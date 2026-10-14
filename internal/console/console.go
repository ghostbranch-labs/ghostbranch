// Package console asks a person to confirm on the console device, never on
// stdin or stdout, so piping `yes` or capturing output cannot answer it.
package console

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
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

// ChoicePrompt starts the question a Choice asks when more than one option
// can be picked.
const ChoicePrompt = "Press Enter for "

// Confirm shows summary and a one-time code on the console, and returns nil
// only when the person types the code back.
func (c *Challenger) Confirm(ctx context.Context, summary string) error {
	_, err := c.challenge(ctx, summary, nil)
	return err
}

// ConfirmChoosing is Confirm with ch asked between the summary and the code
// (R2): Enter picks ch.Default, an option's key, name or number picks it,
// and an unavailable option asks again. With one option left, ch is shown
// and nothing is asked. It returns the option chosen.
func (c *Challenger) ConfirmChoosing(ctx context.Context, summary string, ch approval.Choice) (int, error) {
	return c.challenge(ctx, summary, &ch)
}

// ChoiceText is how ch shows: its title, then one line per option, the
// default marked ❯ and an unavailable one with its reason.
func ChoiceText(ch approval.Choice) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", ch.Title)
	for i, o := range ch.Options {
		mark, text := " ", o.Text
		if i == ch.Default {
			mark = "❯"
		}
		if o.Unavailable != "" {
			text = "unavailable: " + o.Unavailable
		}
		fmt.Fprintf(&b, "%s %-8s — %s\n", mark, o.Name, text)
	}
	return b.String()
}

// choiceQuestion is the question ch asks, "" when at most one option can be
// picked.
func choiceQuestion(ch approval.Choice) string {
	var others []string
	for i, o := range ch.Options {
		if i != ch.Default && o.Unavailable == "" {
			others = append(others, o.Key+" for "+o.Name)
		}
	}
	if len(others) == 0 || ch.Options[ch.Default].Unavailable != "" {
		return ""
	}
	return ChoicePrompt + ch.Options[ch.Default].Name + ", or type " + strings.Join(others, ", ") + ": "
}

// pickOption reads an answer to ch: the option's index, or why the answer
// can't stand ("" when it can).
func pickOption(ch approval.Choice, line string) (int, string) {
	ans := strings.ToLower(strings.TrimSpace(line))
	if ans == "" {
		return ch.Default, ""
	}
	var keys []string
	for i, o := range ch.Options {
		if ans == o.Key || ans == strings.ToLower(o.Name) || ans == strconv.Itoa(i+1) {
			if o.Unavailable != "" {
				return 0, o.Name + " isn't available: " + o.Unavailable + "."
			}
			return i, ""
		}
		if o.Unavailable == "" {
			keys = append(keys, o.Key)
		}
	}
	return 0, "Press Enter, or type " + strings.Join(keys, " or ") + "."
}

// challenge is Confirm, with ch asked first when it is set. pick is
// ch.Default on a refusal.
func (c *Challenger) challenge(ctx context.Context, summary string, ch *approval.Choice) (int, error) {
	pick := 0
	if ch != nil {
		pick = ch.Default
	}
	dev, err := c.Open()
	if err != nil {
		return pick, refuse(approval.NoConsole)
	}
	// On Windows a pending console read blocks Close until the person types
	// another line, so the close never waits. The reader goroutine and the
	// handle are released when the process exits.
	defer func() { go func() { _ = dev.Close() }() }()
	code, err := NewCode(c.Rand)
	if err != nil {
		return pick, err
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
	// Anything typed before the question appears (an Enter during a long
	// gate) must not count as an answer.
	flush(dev)
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
	next := func() (string, error) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-deadline:
			_, _ = fmt.Fprintln(dev, "\nNo answer in time; nothing was confirmed.")
			return "", refuse(approval.Timeout)
		case line, ok := <-lines:
			if !ok {
				return "", refuse(approval.NotConfirmed)
			}
			return line, nil
		}
	}
	_, _ = fmt.Fprintf(dev, "%s\n\n", summary)
	if ch != nil {
		_, _ = fmt.Fprint(dev, ChoiceText(*ch))
		if q := choiceQuestion(*ch); q != "" {
			for left := attempts; ; {
				_, _ = fmt.Fprint(dev, q)
				line, err := next()
				if err != nil {
					return pick, err
				}
				i, why := pickOption(*ch, line)
				if why == "" {
					pick = i
					break
				}
				if left--; left == 0 {
					_, _ = fmt.Fprintln(dev, why+" Too many tries; nothing was confirmed.")
					return ch.Default, refuse(approval.TooManyAttempts)
				}
				_, _ = fmt.Fprintln(dev, why)
			}
		}
		_, _ = fmt.Fprintln(dev)
	}
	_, _ = fmt.Fprintf(dev, "To confirm, type this code and press Enter: %s\nLeave it empty, or type n, to cancel.\n", code)
	for left := attempts; ; {
		_, _ = fmt.Fprint(dev, "> ")
		line, err := next()
		if err != nil {
			return pick, err
		}
		switch ans := strings.ToUpper(strings.TrimSpace(line)); ans {
		case "", "N", "NO":
			_, _ = fmt.Fprintln(dev, "Cancelled.")
			return pick, refuse(approval.NotConfirmed)
		case code:
			return pick, nil
		}
		left--
		if left == 0 {
			_, _ = fmt.Fprintln(dev, "That code doesn't match. Too many tries; nothing was confirmed.")
			return pick, refuse(approval.TooManyAttempts)
		}
		_, _ = fmt.Fprintf(dev, "That code doesn't match; %d tries left.\n", left)
	}
}

// IsTerminal reports whether f is a terminal (a real console handle on
// Windows).
func IsTerminal(f *os.File) bool { return isTerminal(f) }
