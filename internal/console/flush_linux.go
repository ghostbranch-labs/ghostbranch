//go:build linux

package console

import "golang.org/x/sys/unix"

// flushFD discards input typed on the terminal but not yet read.
func flushFD(fd int) { _ = unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
