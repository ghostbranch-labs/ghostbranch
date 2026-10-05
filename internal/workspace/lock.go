package workspace

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// StaleAfter is how old a lock file must be before another run takes it over.
const StaleAfter = 2 * time.Hour

// Lock takes the per-pair lock, allowing one mutating operation per pair on
// this machine. Safety across machines comes from leased pushes, not from
// this lock.
func Lock(path string, now time.Time) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d %s\n", os.Getpid(), now.UTC().Format(time.RFC3339))
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
		if info, serr := os.Stat(path); serr == nil && now.Sub(info.ModTime()) > StaleAfter {
			_ = os.Remove(path)
			continue
		}
		break
	}
	return nil, contract.New(contract.Locked,
		"another ghostbranch operation holds %s; if none is running, delete that file", path)
}
