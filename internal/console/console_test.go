package console_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// device is a fake console: in is what the person types, out is what they see.
type device struct {
	in  io.Reader
	out bytes.Buffer
}

func (d *device) Read(p []byte) (int, error)  { return d.in.Read(p) }
func (d *device) Write(p []byte) (int, error) { return d.out.Write(p) }
func (d *device) Close() error                { return nil }

func challenger(d *device) *console.Challenger {
	return &console.Challenger{Open: func() (io.ReadWriteCloser, error) { return d, nil }, Rand: &testutil.SeqReader{}}
}

func reason(err error) approval.Reason {
	var r *approval.Refusal
	if errors.As(err, &r) {
		return r.Reason
	}
	return ""
}

func TestNewCode(t *testing.T) {
	code, err := console.NewCode(&testutil.SeqReader{})
	if err != nil || code != "ABCDEF" {
		t.Fatalf("code = %q %v", code, err)
	}
	// Bytes >= 248 are rejected so every character is equally likely.
	code, _ = console.NewCode(bytes.NewReader([]byte{255, 250, 248, 0, 1, 2, 3, 4, 5}))
	if code != "ABCDEF" {
		t.Fatalf("rejection sampling: %q", code)
	}
	for _, c := range "0O1IL" {
		if strings.ContainsRune(console.Alphabet, c) {
			t.Fatalf("look-alike %q in the alphabet", c)
		}
	}
}

func TestChallengeAcceptsTheCodeAnyCase(t *testing.T) {
	d := &device{in: strings.NewReader("  abcdef \n")}
	if err := challenger(d).Confirm(context.Background(), "About to publish to acme/widget"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.out.String(), "About to publish to acme/widget") || !strings.Contains(d.out.String(), "ABCDEF") {
		t.Fatalf("the person must see the summary and the code:\n%s", d.out.String())
	}
}

func TestChallengeRefusals(t *testing.T) {
	for in, want := range map[string]approval.Reason{
		"\n":                  approval.NotConfirmed,
		"n\n":                 approval.NotConfirmed,
		"no\n":                approval.NotConfirmed,
		"":                    approval.NotConfirmed, // EOF
		"X\nY\nZ\n":           approval.TooManyAttempts,
		"X\nY\nABCDEF\n":      "", // the third try is still allowed
		"WRONG1\nWRONG2\nn\n": approval.NotConfirmed,
	} {
		err := challenger(&device{in: strings.NewReader(in)}).Confirm(context.Background(), "s")
		if reason(err) != want || (want == "" && err != nil) {
			t.Errorf("input %q: got %v, want %q", in, err, want)
		}
	}
}

func TestChallengeTimesOut(t *testing.T) {
	r, w := io.Pipe() // never written: nobody is at the console
	defer func() { _ = w.Close() }()
	c := challenger(&device{in: r})
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	var asked time.Duration
	c.After = func(d time.Duration) <-chan time.Time { asked = d; return fired }
	if err := c.Confirm(context.Background(), "s"); reason(err) != approval.Timeout || asked != 5*time.Minute {
		t.Fatalf("got %v after %v", err, asked)
	}
}

// stuckDevice simulates Windows: a pending Read blocks Close, and Close only
// returns once the gate is released (as if the person typed another line).
type stuckDevice struct {
	r    *io.PipeReader
	gate chan struct{}
}

func (d *stuckDevice) Read(p []byte) (int, error)  { return d.r.Read(p) }
func (d *stuckDevice) Write(p []byte) (int, error) { return len(p), nil }
func (d *stuckDevice) Close() error {
	<-d.gate
	return d.r.Close()
}

// confirmWithStuckDevice runs Confirm against a device whose Close blocks, with
// the pipe delivering typed and then staying open. It fails if Confirm does not
// return within 2s.
func confirmWithStuckDevice(t *testing.T, ctx context.Context, typed string, after func(time.Duration) <-chan time.Time) error {
	t.Helper()
	r, w := io.Pipe()
	d := &stuckDevice{r: r, gate: make(chan struct{})}
	t.Cleanup(func() { close(d.gate); _ = w.Close() })
	if typed != "" {
		go func() { _, _ = w.Write([]byte(typed)) }()
	}
	c := &console.Challenger{Open: func() (io.ReadWriteCloser, error) { return d, nil }, Rand: &testutil.SeqReader{}, After: after}
	done := make(chan error, 1)
	go func() { done <- c.Confirm(ctx, "s") }()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Confirm did not return: it waited on closing the console device")
		return nil
	}
}

func TestConfirmDoesNotWaitOnClose(t *testing.T) {
	if err := confirmWithStuckDevice(t, context.Background(), "ABCDEF\n", nil); err != nil {
		t.Fatal(err)
	}
}

func TestTimeoutDoesNotWaitOnClose(t *testing.T) {
	fired := make(chan time.Time, 1)
	fired <- time.Time{}
	err := confirmWithStuckDevice(t, context.Background(), "", func(time.Duration) <-chan time.Time { return fired })
	if reason(err) != approval.Timeout {
		t.Fatalf("got %v", err)
	}
}

func TestCancelledContextReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := confirmWithStuckDevice(t, ctx, "", nil)
	if !errors.Is(err, context.Canceled) || reason(err) != "" {
		t.Fatalf("got %v", err)
	}
}

func TestChallengeWithoutADeviceIsNoConsole(t *testing.T) {
	c := &console.Challenger{Open: func() (io.ReadWriteCloser, error) { return nil, errors.New("no tty") }, Rand: &testutil.SeqReader{}}
	if err := c.Confirm(context.Background(), "s"); reason(err) != approval.NoConsole {
		t.Fatal(err)
	}
}

// Pipes and the null device are never a console. NUL on Windows looks like a
// character device to os.Stat, which is the sub-project 1 bug this replaces.
func TestUsable(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close(); _ = w.Close() }()
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = null.Close() }()
	for name, f := range map[string]*os.File{"pipe": r, "null": null} {
		if console.Usable(f, f) {
			t.Errorf("%s must not be a usable console", name)
		}
	}
	if console.Usable(nil, nil) {
		t.Error("nil files are not a console")
	}
}

// A file isn't a terminal: no width, and IsTerminal is false.
func TestWidthOfAFileIsZero(t *testing.T) {
	t.Parallel()
	f, err := os.CreateTemp(t.TempDir(), "w")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if console.IsTerminal(f) || console.Width(f) != 0 || console.Width(nil) != 0 || console.IsTerminal(nil) {
		t.Fatal("a file has no terminal width")
	}
}
