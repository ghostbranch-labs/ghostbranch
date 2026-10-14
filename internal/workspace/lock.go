package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// Lock takes the lock at path for op: one operation at a time on this
// machine. It is an OS file lock (LockFileEx on Windows, flock elsewhere),
// so it frees itself when its process ends, however it ends, and the file
// is never deleted. Safety across machines comes from leased pushes.
func Lock(path string, now time.Time, op string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(f); err != nil {
		_ = f.Close()
		if errors.Is(err, errBusy) {
			return nil, contract.New(contract.Locked, "another ghostbranch operation holds the lock %s", path).
				WithDetails(map[string]any{"held_by": holder(path), "path": path})
		}
		return nil, err
	}
	b, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "op": op, "since": now.UTC().Format(time.RFC3339)})
	if f.Truncate(0) == nil {
		_, _ = f.WriteAt(append(b, '\n'), 0)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

var errBusy = errors.New("lock busy")

// holder reads who holds the lock, best effort: a 5c-format or empty file
// gives what it can.
func holder(path string) map[string]any {
	b, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	var h map[string]any
	if json.Unmarshal(b, &h) == nil {
		if pid, ok := h["pid"].(float64); ok {
			h["pid"] = int(pid)
		}
		return h
	}
	if pid, since, ok := strings.Cut(strings.TrimSpace(string(b)), " "); ok {
		if n, err := strconv.Atoi(pid); err == nil {
			return map[string]any{"pid": n, "since": since}
		}
	}
	return map[string]any{}
}
