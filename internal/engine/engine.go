// Package engine implements every Ghostbranch operation. It is the only
// package that orchestrates git, the forge and the leak scanner, and the
// only one allowed to push to the public repo: from promote and apply,
// after a green gate on the exact tree being published.
package engine

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const (
	DefaultPollInterval = 15 * time.Second
	DefaultSettleWindow = 60 * time.Second
	// DefaultCIAppearWindow is how long sync looks for the workflow runs
	// its pushes start in the shadow repo.
	DefaultCIAppearWindow = 30 * time.Second
	// DefaultCIAppearPoll is how often sync looks within that window.
	DefaultCIAppearPoll = 3 * time.Second
	// OverlayBranch holds the overlay and is the shadow repo's default branch.
	OverlayBranch = "ghostbranch"
)

// Engine runs operations for one workspace. Editor is nil when there is no
// interactive terminal. Approver is nil when no person can be asked at a
// console; Grants is nil when grants are not available (tests).
type Engine struct {
	Forge forge.Forge
	WS    *workspace.Workspace
	Actor string
	Token string
	// TokenSource names where Token came from, for error messages.
	TokenSource  string
	Now          func() time.Time
	Sleep        func(time.Duration)
	Rand         io.Reader
	Progress     io.Writer
	Approver     approval.Approver
	Grants       *grant.Store
	Waivers      *waiver.Store
	Editor       func(initial string) (string, error)
	NewScanner   func(leak.Rules) (leak.Scanner, error)
	PollInterval time.Duration
	SettleWindow time.Duration
	// CIAppearWindow is how long sync looks for the workflow runs its
	// pushes start; zero looks once. CIAppearPoll is how often it looks
	// (zero means DefaultCIAppearPoll).
	CIAppearWindow time.Duration
	CIAppearPoll   time.Duration
	DryRun         bool
	// OnPush observes every push before it happens (tests).
	OnPush func(op, remote string, refspecs []string)
}

func (e *Engine) now() time.Time { return e.Now().UTC().Truncate(time.Second) }

func (e *Engine) progressf(format string, args ...any) {
	if e.Progress != nil {
		_, _ = fmt.Fprintf(e.Progress, format+"\n", args...)
	}
}

// bot is the identity for commits that only ever live in the shadow repo.
func (e *Engine) bot() gitx.Identity {
	return gitx.Identity{Name: "ghostbranch", Email: "ghostbranch@invalid", When: e.now()}
}

func (e *Engine) newID(prefix string) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(e.Rand, b); err != nil {
		return "", err
	}
	return prefix + "-" + e.now().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// gitConfig authenticates git over HTTPS with the token, through the
// environment, never argv.
func (e *Engine) gitConfig() [][2]string {
	if e.Token == "" {
		return nil
	}
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + e.Token))
	return [][2]string{{"http.https://github.com/.extraheader", "AUTHORIZATION: basic " + cred}}
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func forgeErr(err error, format string, args ...any) error {
	ce := contract.Wrap(contract.ForgeError, err, format, args...)
	ce.Retryable = errors.Is(err, forge.ErrTransient)
	return ce
}

func remoteErr(err error, what string) error {
	ce := contract.Wrap(contract.ForgeError, err, "%s", what)
	ce.Retryable = true
	return ce
}

func internalErr(err error, what string) error {
	return contract.Wrap(contract.Internal, err, "%s", what)
}

func leakErr(feat string, findings []leak.Finding) error {
	waivable, notWaivable := []string{}, []string{}
	for _, f := range findings {
		if f.Waivable {
			waivable = append(waivable, f.ID)
		} else {
			notWaivable = append(notWaivable, f.ID)
		}
	}
	return contract.New(contract.LeakFound,
		"%d leak finding(s) in what would be published; nothing was pushed to the public repo", len(findings)).
		WithDetails(map[string]any{"feature": feat, "findings": findings, "waivable": waivable, "not_waivable": notWaivable})
}

// record appends to the ledger. Dry runs are not recorded. A ledger failure
// is reported but never masks the command's own result.
func (e *Engine) record(op string, pair *workspace.Pair, feat, planID, summary string, err error) {
	e.recordWith(op, pair, feat, planID, summary, err, nil)
}

// recordWith is record with a hook to fill extra fields.
func (e *Engine) recordWith(op string, pair *workspace.Pair, feat, planID, summary string, err error, mod func(*ledger.Entry)) {
	if e.DryRun || e.WS == nil {
		return
	}
	ent := ledger.Entry{TS: e.now(), Actor: e.Actor, Op: op, Feature: feat, PlanID: planID, Summary: summary}
	if pair != nil {
		ent.Pair = pair.Public
	}
	if ce := contract.From(err); ce != nil {
		ent.ExitCode, ent.ErrorCode = ce.Code.Exit(), string(ce.Code)
		if ent.Summary == "" {
			ent.Summary = ce.Message
		}
	}
	if mod != nil {
		mod(&ent)
	}
	if werr := ledger.Append(e.WS.LedgerPath(), ent); werr != nil {
		e.progressf("warning: could not write the ledger: %v", werr)
	}
}
