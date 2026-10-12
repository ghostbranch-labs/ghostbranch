package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
)

// syncBuffer is a bytes.Buffer safe for the AfterFunc goroutine.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func styleOff() style.Styler { return style.New(false, false) }

// Spec § 2.1: the checklist only on a terminal that takes ANSI control,
// with TERM not dumb, and not under --json. NO_COLOR and --no-color drop
// colour only.
func TestProgressChoice(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name                string
		capable             bool
		vars                map[string]string
		json, noColor, want bool
	}{
		{"terminal", true, nil, false, false, true},
		{"no ANSI control", false, nil, false, false, false},
		{"TERM=dumb", true, map[string]string{"TERM": "dumb"}, false, false, false},
		{"--json", true, nil, true, false, false},
		{"NO_COLOR keeps the animation", true, map[string]string{"NO_COLOR": "1"}, false, false, true},
		{"--no-color keeps the animation", true, nil, false, true, true},
	} {
		getenv := func(k string) string { return c.vars[k] }
		var b bytes.Buffer
		a := &app{env: Env{Stderr: &b, Getenv: getenv, StderrAnimate: animateAllowed(getenv, c.capable)}, jsonOut: c.json, noColor: c.noColor}
		p := a.newProgress()
		_, isChecklist := p.(*checklist)
		p.Finish(false)
		if isChecklist != c.want {
			t.Errorf("%s: checklist %v, want %v", c.name, isChecklist, c.want)
		}
	}
}

type fakeUI struct {
	plainProgress
	log *[]string
}

func (f fakeUI) Pause()  { *f.log = append(*f.log, "pause") }
func (f fakeUI) Resume() { *f.log = append(*f.log, "resume") }

type approverFunc func(context.Context, string) error

func (f approverFunc) Confirm(ctx context.Context, s string) error { return f(ctx, s) }

// The console code is asked with the spinner paused, and the spinner comes
// back after the answer, whatever it was.
func TestPausingApproverPausesAroundTheCode(t *testing.T) {
	t.Parallel()
	var log []string
	ui := fakeUI{log: &log}
	refused := errors.New("refused")
	w := pausingApprover{inner: approverFunc(func(context.Context, string) error {
		log = append(log, "confirm")
		return refused
	}), p: ui}
	if err := w.Confirm(context.Background(), "summary"); !errors.Is(err, refused) {
		t.Fatal(err)
	}
	if strings.Join(log, ",") != "pause,confirm,resume" {
		t.Fatalf("log = %v", log)
	}
}

// On a real checklist the approver's question meets a clean line: the
// spinner line is cleared and the cursor shown before Confirm runs, and the
// spinner is drawn again after it.
func TestPausingApproverClearsTheSpinnerLine(t *testing.T) {
	t.Parallel()
	var b syncBuffer
	c := newChecklist(&b, styleOff(), time.Now, func() int { return 80 })
	c.Step("Publishing")
	var during string
	w := pausingApprover{inner: approverFunc(func(context.Context, string) error {
		during = b.String()
		return nil
	}), p: c}
	if err := w.Confirm(context.Background(), "summary"); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(during, clearLine+showCursor) {
		t.Fatalf("asked over a spinner line: %q", during)
	}
	if !strings.HasSuffix(b.String(), hideCursor+clearLine+"⠋ Publishing") {
		t.Fatalf("spinner not back: %q", b.String())
	}
	c.Finish(false)
}

// Ctrl-C finishes the checklist at once: the running step is marked ✗ and
// the cursor is shown, before the command winds down.
func TestCtrlCFinishesTheChecklist(t *testing.T) {
	t.Parallel()
	var b syncBuffer
	c := newChecklist(&b, styleOff(), time.Now, func() int { return 80 })
	a := &app{prog: c}
	ctx, cancel := context.WithCancel(context.Background())
	a.finishOnCancel(ctx)
	c.Step("Waiting for CI to start")
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.HasSuffix(b.String(), "✗ Waiting for CI to start\n"+showCursor) {
		if time.Now().After(deadline) {
			t.Fatalf("not finished after Ctrl-C: %q", b.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Every command a person can run is either read-only (spinner only) or
// changing (full checklist), never both and never neither: a new command
// must be classified when it is added.
func TestEveryCommandIsClassified(t *testing.T) {
	t.Parallel()
	a := &app{}
	seen := 0
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, sub := range c.Commands() {
			name := commandName(sub)
			if sub.Name() == "help" || sub.Name() == "completion" || strings.HasPrefix(sub.Name(), "__complete") {
				continue
			}
			if sub.Runnable() && len(sub.Commands()) == 0 {
				seen++
				if readOnlyCommands[name] == changingCommands[name] {
					t.Errorf("%q must be in exactly one of readOnlyCommands and changingCommands", name)
				}
			}
			walk(sub)
		}
	}
	walk(a.root())
	if want := len(readOnlyCommands) + len(changingCommands); seen != want {
		t.Errorf("the tree has %d runnable commands, the lists name %d: a listed command no longer exists", seen, want)
	}
}

func TestQuietProgressChoice(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		command string
		dryRun  bool
		want    bool
	}{
		{"status", false, true}, {"doctor", false, true}, {"version", false, true}, {"feature show", false, true},
		{"gate", false, false}, {"promote", false, false}, {"feature pull", false, false}, {"wait", false, false},
		{"gate", true, true}, // a dry run changes nothing
	} {
		if got := quietProgress(c.command, c.dryRun); got != c.want {
			t.Errorf("%s dry-run %v: quiet %v", c.command, c.dryRun, got)
		}
	}
}
