//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package console

import "golang.org/x/sys/unix"

// fread is FREAD from <sys/fcntl.h>: TIOCFLUSH then discards input only.
const fread = 1

// flushFD discards input typed on the terminal but not yet read.
func flushFD(fd int) { _ = unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, fread) }
