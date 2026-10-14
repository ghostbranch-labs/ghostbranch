package cli

import (
	"context"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
)

// animateAllowed is whether stderr may animate: it takes ANSI control and
// TERM isn't dumb. NO_COLOR is not consulted: it removes colour, not the
// animation (spec 6.4a § 2.1).
func animateAllowed(getenv func(string) string, capable bool) bool {
	return capable && getenv("TERM") != "dumb"
}

// plainProgress is progress.Lines with nothing to pause or finish.
type plainProgress struct{ progress.Progress }

func (plainProgress) Pause()      {}
func (plainProgress) Resume()     {}
func (plainProgress) Finish(bool) {}

// newProgress picks how this run shows progress (spec § 2.1): the animated
// checklist on a terminal that takes ANSI control, unless --json; one plain
// line per step and note otherwise. Everything goes to stderr.
func (a *app) newProgress() uiProgress {
	if a.env.StderrAnimate && !a.jsonOut {
		width := a.env.StderrWidth
		if width == nil {
			width = func() int { return 80 }
		}
		c := newChecklist(a.env.Stderr, a.styler(a.env.StderrColor), time.Now, width)
		c.start()
		return c
	}
	return plainProgress{progress.Lines(a.env.Stderr)}
}

// reporter is this run's progress; plain lines when Run didn't set one
// (tests that build an app directly).
func (a *app) reporter() uiProgress {
	if a.prog == nil {
		a.prog = plainProgress{progress.Lines(a.env.Stderr)}
	}
	return a.prog
}

// finishOnCancel finishes the progress as soon as Ctrl-C cancels ctx: the
// running step is marked ✗ and the cursor shown at once, before the command
// winds down and before a second Ctrl-C could kill the process.
func (a *app) finishOnCancel(ctx context.Context) {
	p := a.reporter()
	context.AfterFunc(ctx, func() { p.Finish(true) })
}

// readOnlyCommands only read. On a terminal they show the spinner while
// they wait, and finished steps leave no ✓ line; a ✗ and notes still print
// (owner, 2026-10-11; R16). Plain mode is the same for every command.
// TestEveryCommandIsClassified keeps this and changingCommands complete.
var readOnlyCommands = map[string]bool{
	"status": true, "audit": true, "doctor": true, "version": true, "reference": true, "log": true, "explain": true, "guide": true,
	"feature list": true, "feature show": true, "agents status": true, "waivers list": true,
	"guard status": true, "guard check": true, "auth status": true,
}

// changingCommands change something; they keep the full checklist.
var changingCommands = map[string]bool{
	"init": true, "sync": true, "upgrade": true, "gate": true, "wait": true, "promote": true, "apply": true, "remove": true,
	"feature start": true, "feature abandon": true, "feature close": true, "feature fold": true, "feature restack": true, "feature pull": true, "feature archive": true,
	"agents allow": true, "agents revoke": true, "waivers add": true, "waivers revoke": true,
	"guard install": true, "guard uninstall": true, "login": true, "logout": true,
}

// quietProgress says whether a run draws spinner only: a read-only command,
// or any command under --dry-run, which changes nothing.
func quietProgress(command string, dryRun bool) bool { return dryRun || readOnlyCommands[command] }

// pausingApprover pauses the spinner while the console code is asked.
type pausingApprover struct {
	inner approval.Approver
	p     uiProgress
}

func (w pausingApprover) Confirm(ctx context.Context, summary string) error {
	w.p.Pause()
	defer w.p.Resume()
	return w.inner.Confirm(ctx, summary)
}

// ConfirmChoosing pauses the spinner while apply's choice and code are
// asked. An inner approver that can't ask a choice confirms with the
// default.
func (w pausingApprover) ConfirmChoosing(ctx context.Context, summary string, ch approval.Choice) (int, error) {
	w.p.Pause()
	defer w.p.Resume()
	if c, ok := w.inner.(approval.Chooser); ok {
		return c.ConfirmChoosing(ctx, summary, ch)
	}
	return ch.Default, w.inner.Confirm(ctx, summary)
}

// pausingAsker pauses the spinner around guided init's questions.
type pausingAsker struct {
	inner prompt.Asker
	p     uiProgress
}

func (w pausingAsker) YesNo(ctx context.Context, q string, def bool) (bool, error) {
	w.p.Pause()
	defer w.p.Resume()
	return w.inner.YesNo(ctx, q, def)
}

func (w pausingAsker) Text(ctx context.Context, q, def string) (string, error) {
	w.p.Pause()
	defer w.p.Resume()
	return w.inner.Text(ctx, q, def)
}

func (w pausingAsker) Wait(ctx context.Context, m string) error {
	w.p.Pause()
	defer w.p.Resume()
	return w.inner.Wait(ctx, m)
}

func (w pausingAsker) Say(m string) {
	w.p.Pause()
	defer w.p.Resume()
	w.inner.Say(m)
}
