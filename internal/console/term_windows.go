//go:build windows

package console

import (
	"io"
	"os"

	"golang.org/x/sys/windows"
)

// isTerminal is true only for a real console handle. NUL and pipes fail
// GetConsoleMode, unlike the character-device file-mode test.
func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &mode) == nil
}

type winConsole struct{ in, out *os.File }

func (c winConsole) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c winConsole) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c winConsole) Close() error {
	err := c.in.Close()
	if err2 := c.out.Close(); err == nil {
		err = err2
	}
	return err
}

// x/sys/windows v0.30.0 has no wrapper for FlushConsoleInputBuffer.
var procFlushConsoleInputBuffer = windows.NewLazySystemDLL("kernel32.dll").NewProc("FlushConsoleInputBuffer")

// flush discards what was typed before the code is shown, so an Enter
// pressed during a long gate cannot answer it. Only the real console is
// flushed, and errors are ignored.
func flush(dev io.ReadWriteCloser) {
	c, ok := dev.(winConsole)
	if !ok || procFlushConsoleInputBuffer.Find() != nil {
		return
	}
	rc, err := c.in.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(h uintptr) { _, _, _ = procFlushConsoleInputBuffer.Call(h) })
}

// Open opens the console's input and output buffers.
func Open() (io.ReadWriteCloser, error) {
	in, err := os.OpenFile("CONIN$", os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	out, err := os.OpenFile("CONOUT$", os.O_RDWR, 0)
	if err != nil {
		_ = in.Close()
		return nil, err
	}
	return winConsole{in, out}, nil
}

// ColorCapable reports whether f is a console that now accepts ANSI escape
// codes: it is a real console handle and enabling virtual-terminal
// processing on it succeeded. A failure means no colour for that stream.
func ColorCapable(f *os.File) bool {
	if !isTerminal(f) {
		return false
	}
	h := windows.Handle(f.Fd())
	var mode uint32
	if windows.GetConsoleMode(h, &mode) != nil {
		return false
	}
	if mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return true
	}
	return windows.SetConsoleMode(h, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) == nil
}
