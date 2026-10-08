// Package waiver stores leak-finding waivers: a person's decision, made at
// a console, that one finding's exact content may be published. Like
// grants, the file is local plain JSON and is friction, not a boundary
// (spec §7.5): the token split is what stops a determined agent.
package waiver

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const SchemaVersion = "0.6"

// Waiver lets one finding through: the same rule, place and match hash. It
// never holds the secret itself.
type Waiver struct {
	ID          string    `json:"id"`
	Public      string    `json:"public"`
	Rule        string    `json:"rule"`
	Where       string    `json:"where"`
	Path        string    `json:"path"`
	MatchSHA256 string    `json:"match_sha256"`
	Reason      string    `json:"reason"`
	CreatedAt   time.Time `json:"created_at"`
	CreatedBy   string    `json:"created_by"`
}

// Matches reports whether a revoke of ids for public, or of all, selects w.
func Matches(w Waiver, public string, ids []string, all bool) bool {
	if all {
		return true
	}
	return strings.EqualFold(w.Public, public) && slices.Contains(ids, w.ID)
}

type file struct {
	SchemaVersion string   `json:"schema_version"`
	Waivers       []Waiver `json:"waivers"`
}

// Store is waivers.json in one workspace.
type Store struct{ path, lock string }

func NewStore(path, lockPath string) *Store { return &Store{path: path, lock: lockPath} }

// Path is the waivers file, for messages.
func (s *Store) Path() string { return s.path }

// Load returns every stored waiver. A missing file has none; an unreadable
// or invalid file is an error, never "no waivers".
func (s *Store) Load() ([]Waiver, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s is not valid JSON; fix it or run `ghostbranch waivers revoke --all`: %w", s.path, err)
	}
	return f.Waivers, nil
}

// For returns the waivers for one public repo, keyed by finding id.
func (s *Store) For(public string) (map[string]Waiver, error) {
	ws, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := map[string]Waiver{}
	for _, w := range ws {
		if strings.EqualFold(w.Public, public) {
			out[w.ID] = w
		}
	}
	return out, nil
}

// Add stores ws, replacing any waiver with the same pair and id.
func (s *Store) Add(ws []Waiver, now time.Time) error {
	return s.update(now, false, func(old []Waiver) []Waiver {
		out := make([]Waiver, 0, len(old)+len(ws))
		for _, o := range old {
			if !slices.ContainsFunc(ws, func(w Waiver) bool { return w.ID == o.ID && strings.EqualFold(w.Public, o.Public) }) {
				out = append(out, o)
			}
		}
		return append(out, ws...)
	})
}

// Revoke removes the waivers Matches selects and returns them. With all, an
// unreadable file is replaced too, so a corrupt waivers.json can be cleared.
func (s *Store) Revoke(public string, ids []string, all bool, now time.Time) ([]Waiver, error) {
	removed := []Waiver{}
	err := s.update(now, all, func(old []Waiver) []Waiver {
		out := make([]Waiver, 0, len(old))
		for _, w := range old {
			if Matches(w, public, ids, all) {
				removed = append(removed, w)
				continue
			}
			out = append(out, w)
		}
		return out
	})
	return removed, err
}

func (s *Store) update(now time.Time, replaceCorrupt bool, fn func([]Waiver) []Waiver) error {
	unlock, err := workspace.Lock(s.lock, now)
	if err != nil {
		return err
	}
	defer unlock()
	ws, err := s.Load()
	if err != nil && !replaceCorrupt {
		return err
	}
	return s.save(fn(ws))
}

func (s *Store) save(ws []Waiver) error {
	if ws == nil {
		ws = []Waiver{}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{SchemaVersion: SchemaVersion, Waivers: ws}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
