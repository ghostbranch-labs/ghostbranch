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

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const SchemaVersion = "0.9"

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
	// Approval is how a person approved it: always {kind: console}, since
	// only the console can waive; absent on waivers made before 0.9.
	Approval *ledger.Approval `json:"approval,omitempty"`
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
	// The version first: a newer Ghostbranch may have changed how values
	// look, and such a file must be refused as newer, not cleared as corrupt.
	var head struct {
		SchemaVersion string `json:"schema_version"`
	}
	if err := json.Unmarshal(b, &head); err != nil {
		return nil, s.corrupt(b, err)
	}
	if contract.Newer(head.SchemaVersion) {
		return nil, contract.NewerRecord("waivers", head.SchemaVersion)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, s.corrupt(b, err)
	}
	return f.Waivers, nil
}

// corrupt wraps a decode error of b, marking it as one revoke --all may
// replace the file over.
func (s *Store) corrupt(b []byte, err error) error {
	what := "is not valid JSON"
	if json.Valid(b) {
		what = "has a value Ghostbranch can't read"
	}
	return corruptFile{fmt.Errorf("%s %s; fix it or run `ghostbranch waivers revoke --all`: %w", s.path, what, err)}
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
	return s.update(now, false, func(old []Waiver) ([]Waiver, error) {
		out := make([]Waiver, 0, len(old)+len(ws))
		for _, o := range old {
			if !slices.ContainsFunc(ws, func(w Waiver) bool { return w.ID == o.ID && strings.EqualFold(w.Public, o.Public) }) {
				out = append(out, o)
			}
		}
		return append(out, ws...), nil
	})
}

// Unknown returns the ids, in order, that name no stored waiver for public.
func Unknown(ws []Waiver, public string, ids []string) []string {
	var out []string
	for _, id := range ids {
		if !slices.ContainsFunc(ws, func(w Waiver) bool { return Matches(w, public, []string{id}, false) }) {
			out = append(out, id)
		}
	}
	return out
}

// UnknownError is WAIVER_UNKNOWN for a revoke that names ids with no waiver
// for public.
func UnknownError(public string, ids []string) *contract.Error {
	return contract.New(contract.WaiverUnknown,
		"%s: no such waiver for %s; ghostbranch waivers list --repo %s shows the stored ones",
		strings.Join(ids, ", "), public, public).WithField("id").WithDetails(map[string]any{"ids": ids})
}

// Revoke removes the waivers Matches selects and returns them; an id with no
// waiver is skipped, which is what remove wants. With all, a file that isn't
// valid JSON or doesn't decode is replaced too, so a corrupt waivers.json can
// be cleared; one it couldn't read, or from a newer Ghostbranch, is kept.
func (s *Store) Revoke(public string, ids []string, all bool, now time.Time) ([]Waiver, error) {
	removed := []Waiver{}
	err := s.update(now, all, func(old []Waiver) ([]Waiver, error) {
		out := make([]Waiver, 0, len(old))
		for _, w := range old {
			if Matches(w, public, ids, all) {
				removed = append(removed, w)
				continue
			}
			out = append(out, w)
		}
		return out, nil
	})
	return removed, err
}

// RevokeKnown removes public's waivers named by ids and returns them. Every
// id must name a stored waiver: if one doesn't, it changes nothing and
// returns UnknownError naming those ids. (Revoke stays lenient for remove,
// which revokes whatever is there.)
func (s *Store) RevokeKnown(public string, ids []string, now time.Time) ([]Waiver, error) {
	removed := []Waiver{}
	err := s.update(now, false, func(old []Waiver) ([]Waiver, error) {
		if unknown := Unknown(old, public, ids); len(unknown) > 0 {
			return nil, UnknownError(public, unknown)
		}
		out := make([]Waiver, 0, len(old))
		for _, w := range old {
			if Matches(w, public, ids, false) {
				removed = append(removed, w)
				continue
			}
			out = append(out, w)
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	return removed, nil
}

func (s *Store) update(now time.Time, replaceCorrupt bool, fn func([]Waiver) ([]Waiver, error)) error {
	unlock, err := workspace.Lock(s.lock, now, "waivers")
	if err != nil {
		return err
	}
	defer unlock()
	ws, err := s.Load()
	if err != nil && (!replaceCorrupt || !Corrupt(err)) {
		return err
	}
	out, err := fn(ws)
	if err != nil {
		return err
	}
	return s.save(out)
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

// corruptFile marks a Load error from a file whose bytes were read but didn't
// decode: not JSON, or JSON with a value of the wrong kind or a bad time.
type corruptFile struct{ err error }

func (c corruptFile) Error() string { return c.err.Error() }
func (c corruptFile) Unwrap() error { return c.err }

// Corrupt reports a Load error from a file that was read but didn't decode:
// the only kind revoke --all replaces.
func Corrupt(err error) bool {
	var c corruptFile
	return errors.As(err, &c)
}
