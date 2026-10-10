package prompt_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
)

// device is a fake console with a person at it. Each time a question is
// shown (a write ending in a space, as every question does) the person
// types the next line. When the lines run out the console closes (Ctrl-D);
// a silent device never types. Typing only after the question appears
// matters: the asker discards anything typed before.
type device struct {
	pr     *io.PipeReader
	pw     *io.PipeWriter
	mu     sync.Mutex
	out    bytes.Buffer
	typed  []string
	silent bool
}

func newDevice(lines ...string) *device {
	pr, pw := io.Pipe()
	return &device{pr: pr, pw: pw, typed: lines}
}

func (d *device) Read(p []byte) (int, error) { return d.pr.Read(p) }
func (d *device) Write(p []byte) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.out.Write(p)
	if d.silent || !bytes.HasSuffix(p, []byte(" ")) {
		return len(p), nil
	}
	if len(d.typed) == 0 {
		go func() { _ = d.pw.Close() }()
		return len(p), nil
	}
	line := d.typed[0]
	d.typed = d.typed[1:]
	go func() { _, _ = d.pw.Write([]byte(line + "\n")) }()
	return len(p), nil
}
func (d *device) Close() error { return nil }
func (d *device) shown() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.out.String()
}

func onDevice(t *testing.T, lines ...string) (prompt.Asker, *device) {
	t.Helper()
	d := newDevice(lines...)
	a, err := prompt.OnConsole(true, func() (io.ReadWriteCloser, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	return a, d
}

func code(err error) contract.Code {
	if ce := contract.From(err); ce != nil {
		return ce.Code
	}
	return ""
}

func TestYesNoAnswers(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		typed []string
		def   bool
		want  bool
	}{
		{[]string{""}, true, true}, {[]string{""}, false, false}, {[]string{"y"}, false, true}, {[]string{"YES"}, false, true},
		{[]string{" n "}, true, false}, {[]string{"No"}, true, false}, {[]string{"maybe", "y"}, false, true},
	} {
		a, d := onDevice(t, c.typed...)
		got, err := a.YesNo(t.Context(), "Create it?", c.def)
		if err != nil || got != c.want {
			t.Errorf("%q def=%v: %v %v", c.typed, c.def, got, err)
		}
		hint := "[y/N]"
		if c.def {
			hint = "[Y/n]"
		}
		if !strings.Contains(d.shown(), "Create it? "+hint) {
			t.Errorf("%q: shown %q", c.typed, d.shown())
		}
	}
}

func TestYesNoGivesUpAfterThreeTries(t *testing.T) {
	t.Parallel()
	a, d := onDevice(t, "maybe", "perhaps", "sure", "y")
	_, err := a.YesNo(t.Context(), "Proceed?", true)
	if code(err) != contract.BadFlag {
		t.Fatalf("%v", err)
	}
	if n := strings.Count(d.shown(), "Proceed? [Y/n]"); n != prompt.MaxTries {
		t.Fatalf("asked %d times:\n%s", n, d.shown())
	}
}

func TestTextAndWaitAndSay(t *testing.T) {
	t.Parallel()
	a, d := onDevice(t, "", "  acme/other  ", "")
	if got, err := a.Text(t.Context(), "Shadow repo", "acme/widget-shadow"); err != nil || got != "acme/widget-shadow" {
		t.Fatalf("default: %q %v", got, err)
	}
	if got, err := a.Text(t.Context(), "Shadow repo", "acme/widget-shadow"); err != nil || got != "acme/other" {
		t.Fatalf("typed: %q %v", got, err)
	}
	if err := a.Wait(t.Context(), "Create the repo, then press Enter."); err != nil {
		t.Fatal(err)
	}
	a.Say("Done.")
	shown := d.shown()
	for _, s := range []string{"Shadow repo [acme/widget-shadow]: ", "Create the repo, then press Enter.", "Done.\n"} {
		if !strings.Contains(shown, s) {
			t.Fatalf("missing %q in:\n%s", s, shown)
		}
	}
}

func TestCtrlCIsInterrupted(t *testing.T) {
	t.Parallel()
	d := newDevice()
	d.silent = true // nobody types
	a, err := prompt.OnConsole(true, func() (io.ReadWriteCloser, error) { return d, nil })
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, cancel)
	if _, err := a.YesNo(ctx, "Proceed?", true); code(err) != contract.Interrupted {
		t.Fatalf("%v", err)
	}
}

func TestAClosedConsoleIsInterrupted(t *testing.T) {
	t.Parallel()
	a, _ := onDevice(t) // closes at the first question (Ctrl-D)
	if _, err := a.Text(t.Context(), "Name", "x"); code(err) != contract.Interrupted {
		t.Fatalf("%v", err)
	}
}

func TestNoConsoleNeverOpensTheDevice(t *testing.T) {
	t.Parallel()
	opened := false
	_, err := prompt.OnConsole(false, func() (io.ReadWriteCloser, error) { opened = true; return nil, nil })
	if !errors.Is(err, prompt.ErrNoConsole) || opened {
		t.Fatalf("%v (opened: %v)", err, opened)
	}
	_, err = prompt.OnConsole(true, func() (io.ReadWriteCloser, error) { return nil, errors.New("no tty") })
	if !errors.Is(err, prompt.ErrNoConsole) {
		t.Fatalf("open failure: %v", err)
	}
}

// Review Focus 2: piped input never answers for a person. A real process
// with "y" piped into stdin calls Console(): stdin is not a terminal, so no
// console is usable and nothing is asked or read. The answer can come only
// from the console device, shown here with a device that says "n" while the
// question defaults to yes.
func TestConsoleNeverReadsStdin(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GB_PROMPT_HELPER=1")
	cmd.Stdin = strings.NewReader("y\nyes\ny\n")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "no console") {
		t.Fatalf("piped stdin got a console: %s", out)
	}
	a, _ := onDevice(t, "n")
	if yes, err := a.YesNo(t.Context(), "Proceed?", true); err != nil || yes {
		t.Fatalf("the device said n: %v %v", yes, err)
	}
}

func TestScript(t *testing.T) {
	t.Parallel()
	s := prompt.NewScript("", "n", "acme/x", "")
	if v, err := s.YesNo(t.Context(), "Create?", true); err != nil || !v {
		t.Fatalf("default: %v %v", v, err)
	}
	if v, err := s.YesNo(t.Context(), "Guard?", true); err != nil || v {
		t.Fatalf("n: %v %v", v, err)
	}
	if v, err := s.Text(t.Context(), "Shadow", "d"); err != nil || v != "acme/x" {
		t.Fatalf("text: %q %v", v, err)
	}
	if err := s.Wait(t.Context(), "Press Enter"); err != nil {
		t.Fatal(err)
	}
	s.Say("hi")
	if len(s.Asked) != 4 || !strings.Contains(s.Asked[0], "Create? [Y/n]") || s.Said[0] != "hi" {
		t.Fatalf("asked %q said %q", s.Asked, s.Said)
	}
	s.Check(t) // every answer used, none missing
	if _, err := s.YesNo(t.Context(), "One more?", true); !errors.Is(err, prompt.ErrNoAnswers) {
		t.Fatalf("run out: %v", err)
	}
	rec := &recorder{}
	s.Check(rec)
	if len(rec.errs) != 1 || !strings.Contains(rec.errs[0], "One more?") {
		t.Fatalf("Check must report the unanswered question: %q", rec.errs)
	}
	rec = &recorder{}
	prompt.NewScript("y").Check(rec)
	if len(rec.errs) != 1 {
		t.Fatalf("Check must report unused answers: %q", rec.errs)
	}
}

func TestScriptCtrlC(t *testing.T) {
	t.Parallel()
	s := prompt.NewScript("y", prompt.CtrlC)
	_, _ = s.YesNo(t.Context(), "First?", true)
	if _, err := s.Text(t.Context(), "Second", ""); code(err) != contract.Interrupted {
		t.Fatalf("%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := prompt.NewScript("y").YesNo(ctx, "Cancelled?", true); code(err) != contract.Interrupted {
		t.Fatalf("cancelled ctx: %v", err)
	}
}

type recorder struct{ errs []string }

func (r *recorder) Helper()                      {}
func (r *recorder) Errorf(f string, args ...any) { r.errs = append(r.errs, fmt.Sprintf(f, args...)) }
