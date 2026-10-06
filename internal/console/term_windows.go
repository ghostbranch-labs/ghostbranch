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
