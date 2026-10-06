//go:build !windows

package console

import (
	"io"
	"os"

	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	_, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	return err == nil
}

// Open opens the controlling terminal.
func Open() (io.ReadWriteCloser, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }
