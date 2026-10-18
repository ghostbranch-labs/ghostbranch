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

// inputFD is the file descriptor to read hidden input from: the device
// itself, when it is the opened terminal.
func inputFD(dev io.ReadWriteCloser) (int, bool) {
	f, ok := dev.(*os.File)
	if !ok || !isTerminal(f) {
		return 0, false
	}
	return int(f.Fd()), true
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

// ColorCapable reports whether f is a terminal, which takes ANSI escape
// codes.
func ColorCapable(f *os.File) bool { return isTerminal(f) }

// Width is f's terminal width in columns, or 0 when f is not a terminal.
func Width(f *os.File) int {
	if f == nil {
		return 0
	}
	ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0
	}
	return int(ws.Col)
}
