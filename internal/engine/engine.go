// Package engine implements every Ghostbranch operation. It is the only
// package that orchestrates git, the forge and the leak scanner, and the
// only one allowed to push to the public repo: from promote and apply,
// after a green gate on the exact tree being published.
package engine

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
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
	// DefaultCancelVerifyWindow is how long sync waits for the runs it
	// cancelled to finish before it force-cancels the ones still going.
	DefaultCancelVerifyWindow = 30 * time.Second
	// DefaultForceCancelWindow is how long it then waits for the
	// force-cancelled runs to finish.
	DefaultForceCancelWindow = 15 * time.Second
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
	TokenSource string
	// LooksLikeAgent is auth.LooksLikeAgent for this run: it is likely an
	// agent or a script, not a person at a terminal. Doctor and agents
	// status read it to spot an agent on a person's stored login.
	LooksLikeAgent bool
	// AppLogin: Token is the stored login (TokenSource "login") and that
	// login came from the Ghostbranch app (Kind "app"), so it can list the
	// app's installations. Never decided from the token's prefix.
	AppLogin bool
	Now      func() time.Time
	// Sleep waits d, or returns ctx.Err() as soon as ctx is done.
	Sleep func(context.Context, time.Duration) error
	Rand  io.Reader
	// Progress receives the steps and notes of slow work; nil prints nothing.
	Progress     progress.Progress
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
	// OnBase hears the pair's base branch as soon as open has read it, so
	// the CLI can name it in the next steps of an error, which carries no
	// result. A doctor run whose overlay config can't be read never calls it.
	OnBase func(branch string)
	// ledgerWarns holds a LEDGER_NOT_WRITTEN warning for each ledger entry
	// that couldn't be written, until LedgerWarnings hands them over.
	ledgerWarns []contract.Warning
	// tightenFailed holds the state directories whose chmod failed in this
	// run, so each failure is noted once (noteTightened).
	tightenFailed map[string]bool
}

func (e *Engine) now() time.Time { return e.Now().UTC().Truncate(time.Second) }

// reporter is e.Progress, or a Progress that prints nothing.
func (e *Engine) reporter() progress.Progress {
	if e.Progress == nil {
		return progress.Nop()
	}
	return e.Progress
}

// startStep starts a step: plain words, never a SHA (notes carry those).
func (e *Engine) startStep(format string, args ...any) progress.Step {
	return e.reporter().Step(fmt.Sprintf(format, args...))
}

// notef prints a one-off line above the running step.
func (e *Engine) notef(level progress.NoteLevel, format string, args ...any) {
	e.reporter().Note(level, fmt.Sprintf(format, args...))
}

// bot is the identity for commits that only ever live in the shadow repo:
// the Ghostbranch GitHub App's bot (see auth.BotName).
func (e *Engine) bot() gitx.Identity {
	return gitx.Identity{Name: auth.BotName, Email: auth.BotEmail, When: e.now()}
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

// gitAuthFailure matches git's words for a credential or access problem:
// sending the same token again fails the same way.
var gitAuthFailure = regexp.MustCompile(`(?i)(authentication failed|invalid username or (password|token)|could not read (username|password)|terminal prompts disabled|returned error: 40[134]|repository not found|permission to \S+ denied|write access to repository not granted)`)

// remoteErr is a failed fetch, ls-remote or push. It is retryable unless
// the credentials or the repo's visibility are the problem, or the command
// was cancelled.
func remoteErr(err error, what string) error {
	ce := contract.Wrap(contract.ForgeError, err, "%s", what)
	ce.Retryable = !authFailure(err) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	return ce
}

func authFailure(err error) bool {
	var perm *gitx.PermissionError
	if errors.As(err, &perm) {
		return true
	}
	var ge *gitx.Error
	return errors.As(err, &ge) && gitAuthFailure.MatchString(ge.Stderr)
}

func internalErr(err error, what string) error {
	return contract.Wrap(contract.Internal, err, "%s", what)
}

// updateErr passes on a LOCKED from workspace.Update as it is, with who
// holds workspace.json, so the caller can retry; anything else is INTERNAL.
func updateErr(err error, what string) error {
	if contract.From(err).Code == contract.Locked {
		return err
	}
	return internalErr(err, what)
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
// never masks the command's own result: it becomes a LEDGER_NOT_WRITTEN
// warning, which the CLI adds to the result through LedgerWarnings.
func (e *Engine) record(op string, pair *workspace.Pair, feat, planID, summary string, err error) {
	e.recordWith(op, pair, feat, planID, summary, err, nil)
}

// recordWith is record with a hook to fill extra fields.
func (e *Engine) recordWith(op string, pair *workspace.Pair, feat, planID, summary string, err error, mod func(*ledger.Entry)) {
	if e.DryRun || e.WS == nil {
		return
	}
	e.tightenState()
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
		e.ledgerWarns = addWarnings(e.ledgerWarns, ledgerNotWritten(e.WS.LedgerPath(), ent, werr))
	}
}

// LedgerWarnings returns the LEDGER_NOT_WRITTEN warnings since the last
// call, and forgets them.
func (e *Engine) LedgerWarnings() []contract.Warning {
	w := e.ledgerWarns
	e.ledgerWarns = nil
	return w
}

// ledgerNotWritten is the warning for an entry the ledger couldn't take. A
// close's deleted refs ride in details.refs: the ledger is where a person
// looks for the SHAs to push a branch back.
func ledgerNotWritten(path string, ent ledger.Entry, err error) contract.Warning {
	details := map[string]any{"op": ent.Op, "path": path}
	msg := fmt.Sprintf("the ledger entry for %s couldn't be written to %s: %v; the command's own result stands", ent.Op, path, err)
	if ent.Feature != "" {
		details["feature"] = ent.Feature
	}
	if len(ent.Refs) > 0 {
		details["refs"] = maps.Clone(ent.Refs)
		msg += fmt.Sprintf("; it deleted %d ref(s), listed with their SHAs in details.refs (keep them: a deleted branch can be pushed back from its SHA)", len(ent.Refs))
	}
	return contract.Warning{Code: contract.LedgerNotWritten, Message: msg, Details: details}
}
