//go:build !windows

package workspace

import (
	"fmt"
	"os"
)

// chmod is os.Chmod; a test swaps it to see a failure.
var chmod = os.Chmod

// EnsurePrivate creates dir 0700 when it is missing and, when it exists
// with any group or other bits, chmods it to 0700. tightened reports the
// chmod; a chmod that left the bits in place is an error.
func EnsurePrivate(dir string) (tightened bool, err error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	if fi.Mode().Perm()&0o077 == 0 {
		return false, nil
	}
	if err := chmod(dir, 0o700); err != nil {
		return false, err
	}
	// A filesystem that ignores chmod (drvfs under WSL, SMB with fixed
	// modes) returns success and keeps the mode: report it, so the caller
	// doesn't claim it made the directory private.
	if fi, err = os.Stat(dir); err != nil {
		return false, err
	}
	if p := fi.Mode().Perm(); p&0o077 != 0 {
		return false, fmt.Errorf("this filesystem ignores chmod; it is still %o", p)
	}
	return true, nil
}
