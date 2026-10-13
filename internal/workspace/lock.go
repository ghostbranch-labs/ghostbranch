package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
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

// lockedMessage names the lock's holder when its file says who (HolderText).
// A holder that can't be read is left out.
func lockedMessage(path string, h map[string]any) string {
	if who := HolderText(h); who != "" {
		return fmt.Sprintf("another ghostbranch operation (%s) holds the lock %s", who, path)
	}
	return "another ghostbranch operation holds the lock " + path
}

// holderOp is an operation name as Ghostbranch writes it in a lock file.
var holderOp = regexp.MustCompile(`^[a-z][a-z -]{0,40}$`)

// HolderText is "op, pid N, since T" for a lock holder (holder's map, or
// LOCKED's details.held_by), or "" unless every part looks as Ghostbranch
// writes it: op a short lower-case name, pid above 0, since RFC 3339. The
// file's text reaches a terminal, so nothing else is shown (review M8).
func HolderText(h map[string]any) string {
	op, _ := h["op"].(string)
	pid, _ := h["pid"].(int)
	since, _ := h["since"].(string)
	if !holderOp.MatchString(op) || pid <= 0 {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, since); err != nil {
		return ""
	}
	return fmt.Sprintf("%s, pid %d, since %s", op, pid, since)
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
