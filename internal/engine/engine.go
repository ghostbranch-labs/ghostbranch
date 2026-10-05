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
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const (
	DefaultPollInterval = 15 * time.Second
	DefaultSettleWindow = 60 * time.Second
	// OverlayBranch holds the overlay and is the shadow repo's default branch.
	OverlayBranch = "ghostbranch"
)

// Engine runs operations for one workspace. Prompt and Editor are nil when
// there is no interactive terminal.
type Engine struct {
	Forge        forge.Forge
	WS           *workspace.Workspace
	Actor        string
	Token        string
	Now          func() time.Time
	Sleep        func(time.Duration)
	Rand         io.Reader
	Progress     io.Writer
	Prompt       func(summary string) (bool, error)
	Editor       func(initial string) (string, error)
	NewScanner   func(overlay.Set) (leak.Scanner, error)
	PollInterval time.Duration
	SettleWindow time.Duration
	DryRun       bool
	// OnPush observes every push before it happens (tests).
	OnPush func(op, remote string, refspecs []string)
}

// IsAgent reports whether actor is an agent ("agent:<name>").
func IsAgent(actor string) bool { return strings.HasPrefix(actor, "agent:") }

// needsHumanApprover reports whether actor may not approve a plan that
// requires a human. Agents never may. Until sub-project 2 defines how an app
// approves, an app ("app") is treated like an agent, so it cannot approve its
// own plans either.
func needsHumanApprover(actor string) bool { return IsAgent(actor) || actor == "app" }

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

func leakErr(findings []leak.Finding) error {
	return contract.New(contract.LeakFound,
		"%d leak finding(s) in what would be published; nothing was pushed to the public repo", len(findings)).
		WithDetails(map[string]any{"findings": findings})
}

// record appends to the ledger. Dry runs are not recorded. A ledger failure
// is reported but never masks the command's own result.
func (e *Engine) record(op string, pair *workspace.Pair, feat, planID, summary string, err error) {
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
	if werr := ledger.Append(e.WS.LedgerPath(), ent); werr != nil {
		e.progressf("warning: could not write the ledger: %v", werr)
	}
}
