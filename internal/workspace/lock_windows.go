//go:build windows

package workspace

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The lock covers one byte at offset 2^32, past any holder info, because a
// byte-range lock also blocks reads of its range on Windows.
func lockFile(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errBusy
	}
	return err
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{OffsetHigh: 1})
}
