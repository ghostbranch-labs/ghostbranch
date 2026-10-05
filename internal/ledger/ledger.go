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
	Summary   string    `json:"summary"`
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
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close() //nolint:errcheck
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
	defer f.Close() //nolint:errcheck
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
