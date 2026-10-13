//go:build windows

package workspace

import "os"

// EnsurePrivate creates dir when it is missing. Windows has no mode bits to
// tighten: the default home inherits the profile's private ACL, doctor
// warns about a custom home outside the profile, and Ghostbranch never
// changes an ACL itself (F-28, decision 3).
func EnsurePrivate(dir string) (tightened bool, err error) {
	return false, os.MkdirAll(dir, 0o700)
}
