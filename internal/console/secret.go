package console

import (
	"context"
	"errors"
	"io"

	"golang.org/x/term"
)

// ErrNotTerminal means the device given to ReadSecret isn't the real
// console, so input can't be hidden there.
var ErrNotTerminal = errors.New("the console device isn't a terminal, so hidden input can't be read")

// ReadSecret reads one line from the console device dev with echo off (for
// agents token). The line is never shown, and the caller should clear the
// bytes after use. Ctrl-C (ctx cancelled) turns echo back on before it
// returns, so the terminal isn't left without it.
func ReadSecret(ctx context.Context, dev io.ReadWriteCloser) ([]byte, error) {
	fd, ok := inputFD(dev)
	if !ok {
		return nil, ErrNotTerminal
	}
	old, err := term.GetState(fd)
	if err != nil {
		return nil, ErrNotTerminal
	}
	type read struct {
		b   []byte
		err error
	}
	ch := make(chan read, 1)
	go func() {
		b, err := term.ReadPassword(fd)
		ch <- read{b, err}
	}()
	select {
	case r := <-ch:
		return r.b, r.err
	case <-ctx.Done():
		_ = term.Restore(fd, old)
		return nil, ctx.Err()
	}
}
