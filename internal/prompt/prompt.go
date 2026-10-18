// Package prompt asks a person questions on the console device (/dev/tty,
// or CONIN$ and CONOUT$ on Windows), never on stdin or stdout. Piped input
// can't answer for a person, and captured output never holds a question.
// It is the approval code's device (internal/console), reused.
package prompt

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// Asker asks one person questions.
type Asker interface {
	// YesNo asks a yes/no question; an empty answer is def.
	YesNo(ctx context.Context, question string, def bool) (bool, error)
	// Text asks for a line; an empty answer is def.
	Text(ctx context.Context, question, def string) (string, error)
	// Wait shows message and waits for Enter.
	Wait(ctx context.Context, message string) error
	// Say shows a line.
	Say(message string)
}

// MaxTries is how often YesNo asks before it gives up on unclear answers.
const MaxTries = 3

var ErrNoConsole = errors.New("prompt: no usable console")

// Styled returns a with its questions shown through bold (a colour on a
// terminal, nothing otherwise). Only the console device styles; any other
// Asker comes back unchanged. The text of an error never holds the styling.
func Styled(a Asker, bold func(string) string) Asker {
	if d, ok := a.(*device); ok && bold != nil {
		return &device{dev: d.dev, bold: bold}
	}
	return a
}

// Console is the console device, when stdin and stderr are both terminals
// (the approval code's test).
func Console() (Asker, error) { return OnConsole(console.Usable(os.Stdin, os.Stderr), console.Open) }

// OnConsole is Console with the test's answer and the device opener given,
// as the CLI's Env holds them. Without a usable console the device is never
// opened.
func OnConsole(usable bool, open func() (io.ReadWriteCloser, error)) (Asker, error) {
	if !usable || open == nil {
		return nil, ErrNoConsole
	}
	dev, err := open()
	if err != nil || dev == nil {
		return nil, fmt.Errorf("%w: the console device can't be opened", ErrNoConsole)
	}
	return &device{dev: dev}, nil
}

// device asks on an open console. It is never closed: on Windows a pending
// read blocks Close until the person types a line, so the handle and its
// reader goroutine end with the process (one command).
type device struct {
	dev io.ReadWriteCloser
	// bold styles a question as it is shown (nil: as is).
	bold  func(string) string
	once  sync.Once
	lines chan string
}

func (d *device) start() {
	d.once.Do(func() {
		d.lines = make(chan string, 1)
		go func() {
			defer close(d.lines)
			sc := bufio.NewScanner(d.dev)
			for sc.Scan() {
				d.lines <- sc.Text()
			}
		}()
	})
}

// ask shows text and reads one answer. Lines typed before the question
// appeared are discarded first: those still in the device (Flush) and those
// the reader already took (drained, best effort).
func (d *device) ask(ctx context.Context, text string) (string, error) {
	return d.show(ctx, text, text)
}

// show asks with shown on screen; text is what errors quote.
func (d *device) show(ctx context.Context, text, shown string) (string, error) {
	console.Flush(d.dev)
	d.start()
	for drained := false; !drained; {
		select {
		case _, ok := <-d.lines:
			drained = !ok
		default:
			drained = true
		}
	}
	_, _ = fmt.Fprint(d.dev, shown)
	select {
	case <-ctx.Done():
		_, _ = fmt.Fprintln(d.dev)
		return "", interrupted(ctx, text)
	case line, ok := <-d.lines:
		if !ok {
			return "", contract.New(contract.Interrupted, "the console closed before an answer to %q", strings.TrimSpace(text))
		}
		return strings.TrimSpace(line), nil
	}
}

func (d *device) Say(message string) { _, _ = fmt.Fprintln(d.dev, message) }

func (d *device) YesNo(ctx context.Context, q string, def bool) (bool, error) {
	return yesNo(ctx, q, def, d.askQuestion, d.Say)
}

func (d *device) Text(ctx context.Context, q, def string) (string, error) {
	return text(ctx, q, def, d.askQuestion)
}

func (d *device) Wait(ctx context.Context, message string) error {
	_, err := d.ask(ctx, message+" ")
	return err
}

// askQuestion is ask with the question in bold; its trailing space stays
// plain.
func (d *device) askQuestion(ctx context.Context, text string) (string, error) {
	return d.show(ctx, text, d.styledQuestion(text))
}

func (d *device) styledQuestion(text string) string {
	if d.bold == nil {
		return text
	}
	body := strings.TrimRight(text, " ")
	return d.bold(body) + text[len(body):]
}

func interrupted(ctx context.Context, text string) error {
	e := contract.New(contract.Interrupted, "interrupted (Ctrl-C) at: %s", strings.TrimSpace(text))
	e.Err = ctx.Err()
	return e
}

type askFunc func(ctx context.Context, text string) (string, error)

// yesNo is YesNo for any asker: empty is def; y, yes, n, no in any case;
// anything else asks again, up to MaxTries.
func yesNo(ctx context.Context, q string, def bool, ask askFunc, say func(string)) (bool, error) {
	hint := "[y/N, Enter = no]"
	if def {
		hint = "[Y/n, Enter = yes]"
	}
	for try := 1; ; try++ {
		ans, err := ask(ctx, q+" "+hint+" ")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(ans) {
		case "":
			return def, nil
		case "y", "yes":
			return true, nil
		case "n", "no":
			return false, nil
		}
		if try == MaxTries {
			return false, contract.New(contract.BadFlag, "no yes or no answer to %q after %d tries", q, MaxTries)
		}
		say("Please answer y or n.")
	}
}

func text(ctx context.Context, q, def string, ask askFunc) (string, error) {
	shown := q + ": "
	if def != "" {
		shown = q + " [" + def + "]: "
	}
	ans, err := ask(ctx, shown)
	if err != nil {
		return "", err
	}
	if ans == "" {
		return def, nil
	}
	return ans, nil
}
