//go:build !windows

package workspace

import "os"

// chmod is os.Chmod; a test swaps it to see a failure.
var chmod = os.Chmod

// EnsurePrivate creates dir 0700 when it is missing and, when it exists
// with any group or other bits, chmods it to 0700. tightened reports the
// chmod.
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
	return true, nil
}
