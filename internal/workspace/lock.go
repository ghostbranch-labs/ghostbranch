package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
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
			// Every busy OS lock is transient: its holder ends, or dies and
			// the OS frees it (F-29).
			h := holder(path)
			ce := contract.New(contract.Locked, "%s", lockedMessage(path, h)).
				WithDetails(map[string]any{"held_by": h, "path": path})
			ce.Retryable = true
			return nil, ce
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

// lockedMessage names the lock's holder when its file says who: op, pid
// and since all known. A holder that can't be read is left out.
func lockedMessage(path string, h map[string]any) string {
	op, _ := h["op"].(string)
	pid, _ := h["pid"].(int)
	since, _ := h["since"].(string)
	if op == "" || pid == 0 || since == "" {
		return "another ghostbranch operation holds the lock " + path
	}
	return fmt.Sprintf("another ghostbranch operation (%s, pid %d, since %s) holds the lock %s", op, pid, since, path)
}

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
