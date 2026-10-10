// Package console asks a person to confirm on the console device, never on
// stdin or stdout, so piping `yes` or capturing output cannot answer it.
package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
)

// Alphabet has no look-alike characters (no 0 O 1 I L).
const Alphabet = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"

const (
	DefaultTimeout  = 5 * time.Minute
	DefaultAttempts = 3
	codeLength      = 6
)

// Usable reports whether stdin and stderr are both terminals: the first
// condition for asking a person anything (spec §6.3).
func Usable(stdin, stderr *os.File) bool { return isTerminal(stdin) && isTerminal(stderr) }

// Flush discards what was typed on the console device before a question, so
// an Enter pressed earlier can't answer it. Only a real console device is
// flushed; anything else is left alone.
func Flush(dev io.ReadWriteCloser) { flush(dev) }

// NewCode returns a random code. Bytes of 248 and above are rejected so each
// of the 31 characters is equally likely.
func NewCode(r io.Reader) (string, error) {
	out := make([]byte, 0, codeLength)
	b := make([]byte, 1)
	for len(out) < codeLength {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[0] >= 248 {
			continue
		}
		out = append(out, Alphabet[int(b[0])%len(Alphabet)])
	}
	return string(out), nil
}

// Challenger implements approval.Approver on the console device.
type Challenger struct {
	Open     func() (io.ReadWriteCloser, error)
	Rand     io.Reader
	Timeout  time.Duration
	Attempts int
	After    func(time.Duration) <-chan time.Time
}

func refuse(r approval.Reason) error { return &approval.Refusal{Reason: r} }

// Confirm shows summary and a one-time code on the console, and returns nil
// only when the person types the code back.
func (c *Challenger) Confirm(ctx context.Context, summary string) error {
	dev, err := c.Open()
	if err != nil {
		return refuse(approval.NoConsole)
	}
	// On Windows a pending console read blocks Close until the person types
	// another line, so Confirm never waits for the close. The reader goroutine
	// and the handle are released when the process exits.
	defer func() { go func() { _ = dev.Close() }() }()
	code, err := NewCode(c.Rand)
	if err != nil {
		return err
	}
	timeout, attempts, after := c.Timeout, c.Attempts, c.After
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if attempts <= 0 {
		attempts = DefaultAttempts
	}
	if after == nil {
		after = time.After
	}
	// Anything typed before the code appears (an Enter during a long gate)
	// must not count as an answer.
	flush(dev)
	_, _ = fmt.Fprintf(dev, "%s\n\nTo confirm, type this code and press Enter: %s\nLeave it empty, or type n, to cancel.\n", summary, code)

	// The reader goroutine closes lines at EOF, so a closed channel means
	// "nothing more will be typed". After a decision, timeout or cancel it may
	// stay blocked in Read until the process exits.
	lines := make(chan string, 1)
	go func() {
		defer close(lines)
		sc := bufio.NewScanner(dev)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	deadline := after(timeout)
	for left := attempts; ; {
		_, _ = fmt.Fprint(dev, "> ")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			_, _ = fmt.Fprintln(dev, "\nNo answer in time; nothing was confirmed.")
			return refuse(approval.Timeout)
		case line, ok := <-lines:
			if !ok {
				return refuse(approval.NotConfirmed)
			}
			switch ans := strings.ToUpper(strings.TrimSpace(line)); ans {
			case "", "N", "NO":
				_, _ = fmt.Fprintln(dev, "Cancelled.")
				return refuse(approval.NotConfirmed)
			case code:
				return nil
			}
			left--
			if left == 0 {
				_, _ = fmt.Fprintln(dev, "That code doesn't match. Too many tries; nothing was confirmed.")
				return refuse(approval.TooManyAttempts)
			}
			_, _ = fmt.Fprintf(dev, "That code doesn't match; %d tries left.\n", left)
		}
	}
}
