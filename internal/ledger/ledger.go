// Package ledger is the local, append-only record of every mutating action
// and who triggered it.
package ledger

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

type Entry struct {
	TS        time.Time `json:"ts"`
	Actor     string    `json:"actor"`
	Op        string    `json:"op"`
	Pair      string    `json:"pair,omitempty"`
	Feature   string    `json:"feature,omitempty"`
	PlanID    string    `json:"plan_id,omitempty"`
	ExitCode  int       `json:"exit_code"`
	ErrorCode string    `json:"error_code,omitempty"`
	Approval  *Approval `json:"approval,omitempty"`
	Grant     *GrantRef `json:"grant,omitempty"`
	// Waivers are the finding ids an entry waived, revoked or relied on.
	Waivers []string `json:"waivers,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	// Refs are the shadow-repo refs an entry deleted, each with the SHA it
	// pointed at.
	Refs    map[string]string `json:"refs,omitempty"`
	Summary string            `json:"summary"`
}

// GrantRef names the grant an entry is about.
type GrantRef struct {
	ID        string     `json:"id"`
	ExpiresAt *time.Time `json:"expires_at"`
}

// Approval records how a publish was approved.
type Approval struct {
	Kind      string `json:"kind"` // "console" or "grant"
	GrantID   string `json:"grant_id,omitempty"`
	CreatedBy string `json:"created_by,omitempty"`
}

// Append writes e as one JSON line.
func Append(path string, e Entry) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// A ledger made before 0.9 was 0644. It names repos, features and
	// actors, so it is private like plans, grants and waivers. Windows has
	// no such bits.
	if runtime.GOOS != "windows" {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm() != 0o600 {
			_ = f.Chmod(0o600) // best effort: a failed chmod must not lose the entry
		}
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Read returns every entry. A missing ledger is empty.
func Read(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var e Entry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, sc.Err()
}
