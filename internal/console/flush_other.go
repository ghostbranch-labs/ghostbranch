//go:build !windows && !linux && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package console

// flushFD does nothing where no flush is wired up.
func flushFD(int) {}
