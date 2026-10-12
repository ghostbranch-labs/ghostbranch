package cli_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
)

// cursorShown is true when the last cursor escape in s shows it.
func cursorShown(s string) bool {
	return strings.LastIndex(s, "\x1b[?25l") <= strings.LastIndex(s, "\x1b[?25h")
}

// On a terminal the gate draws a checklist on stderr; stdout is the result,
// unchanged and without escapes; the cursor ends shown.
func TestTerminalRunsDrawTheChecklist(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.env.StderrAnimate = true
	code, out, errOut := te.run("gate", "feat-x")
	if code != 0 || !strings.Contains(out, "Gate for feat-x: checks passed") || strings.Contains(out, "\x1b") {
		t.Fatalf("%d\nstdout: %q", code, out)
	}
	for _, want := range []string{"✓ Reading acme/widget and acme/widget-shadow from GitHub", "✓ Replaying feat-x onto public main", "✓ CI passed", "· CI runs on shadow PR #"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr lacks %q:\n%q", want, errOut)
		}
	}
	if !cursorShown(errOut) {
		t.Fatalf("the cursor stays hidden: %q", errOut)
	}
}

// --json, pipes and TERM=dumb get plain lines: no escape byte anywhere.
func TestJSONRunsPrintPlainStepsWithoutEscapes(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.env.StderrAnimate = true // a terminal, but --json
	code, out, errOut := te.run("gate", "feat-x", "--json")
	if code != 0 || strings.Contains(out+errOut, "\x1b") || !strings.Contains(errOut, "Waiting for CI to start\n") {
		t.Fatalf("%d\nstdout: %q\nstderr: %q", code, out, errOut)
	}
}

// A failing command marks the running step ✗ before the error line.
func TestAFailedStepIsMarkedBeforeTheError(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.forge.Checks = failing
	te.env.StderrAnimate = true
	code, _, errOut := te.run("gate", "feat-x")
	i, j := strings.Index(errOut, "✗ "), strings.Index(errOut, "error: ")
	if code != 3 || i < 0 || j < i {
		t.Fatalf("%d\n%q", code, errOut)
	}
}

// R16: a read-only command on a terminal shows only the spinner: no ✓
// lines, the cursor ends shown, stdout unchanged.
func TestReadOnlyCommandsLeaveNoDoneLines(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.env.StderrAnimate = true
	code, out, errOut := te.run("status")
	if code != 0 || strings.Contains(errOut, "✓") || strings.Contains(out, "\x1b") {
		t.Fatalf("%d\nstdout %q\nstderr %q", code, out, errOut)
	}
	if !cursorShown(errOut) {
		t.Fatalf("the cursor stays hidden: %q", errOut)
	}
}

// A panic in the middle of a step unwinds through Run's deferred Finish:
// the running step is marked ✗ and the cursor comes back before the panic
// leaves Run.
func TestAPanicMidStepMarksItAndShowsTheCursor(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.env.StderrAnimate = true
	te.env.NewScanner = func(leak.Rules) (leak.Scanner, error) { panic("boom") } // inside gate's scan step
	var out, errb bytes.Buffer
	env := te.env
	env.Stdout, env.Stderr = &out, &errb
	recovered := func() (r any) {
		defer func() { r = recover() }()
		cli.Run([]string{"gate", "feat-x"}, env)
		return nil
	}()
	errOut := errb.String()
	if recovered != "boom" {
		t.Fatalf("recovered %v\nstderr %q", recovered, errOut)
	}
	if !strings.Contains(errOut, "✗ Scanning what feat-x would publish for leaks") || !cursorShown(errOut) {
		t.Fatalf("stderr %q", errOut)
	}
}

// A shadow repo GitHub refused to create is a ✗, not a ✓ the next step
// ends, and the dead step isn't redrawn around guided init's questions.
func TestGuidedInitMarksARefusedCreationFailed(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.CreateForbidden = true
	te.env.StderrAnimate = true
	p := atTerminal(te, "", "", "", "")
	p.onWait = func() {
		if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
			t.Error(err)
		}
	}
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	const step = "Creating the private repo acme/widget-shadow"
	i := strings.Index(errOut, "✗ "+step)
	if i < 0 || strings.Contains(errOut, "✓ "+step) {
		t.Fatalf("stderr %q", errOut)
	}
	if strings.Contains(errOut[i+len("✗ "+step):], step) {
		t.Fatalf("the refused step is drawn again: %q", errOut)
	}
	if !cursorShown(errOut) {
		t.Fatalf("the cursor stays hidden: %q", errOut)
	}
}
