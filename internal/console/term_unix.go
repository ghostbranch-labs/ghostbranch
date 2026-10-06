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

// flush discards what was typed before the code is shown, so an Enter
// pressed during a long gate cannot answer it. Only a real terminal file is
// flushed, and errors are ignored. SyscallConn leaves the file's blocking
// mode alone, unlike Fd.
func flush(dev io.ReadWriteCloser) {
	f, ok := dev.(*os.File)
	if !ok {
		return
	}
	rc, err := f.SyscallConn()
	if err != nil {
		return
	}
	_ = rc.Control(func(fd uintptr) { flushFD(int(fd)) })
}
