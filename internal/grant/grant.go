// Package grant stores agent grants: a person's permission, made at a
// console, for agents to publish to one repo pair from this machine. The
// file is plain JSON on purpose (spec §7.1, D8): a key on the same machine
// would add no real protection, and the token split is the real boundary.
package grant

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const SchemaVersion = "0.9"

// DefaultFor is the grant length when --for is not given.
const DefaultFor = "6h"

// Durations are the only grant lengths, so "forever" is always typed out as
// --until-revoked.
var Durations = map[string]time.Duration{
	"6h":  6 * time.Hour,
	"24h": 24 * time.Hour,
	"30d": 30 * 24 * time.Hour,
}

type Grant struct {
	ID        string     `json:"id"`
	Public    string     `json:"public"`
	Shadow    string     `json:"shadow"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at"`
	CreatedBy string     `json:"created_by"`
}

// ValidAt reports whether g is in force at now. A nil ExpiresAt never expires.
func (g Grant) ValidAt(now time.Time) bool { return g.ExpiresAt == nil || now.Before(*g.ExpiresAt) }

// For reports whether g is for this exact pair.
func (g Grant) For(public, shadow string) bool {
	return strings.EqualFold(g.Public, public) && strings.EqualFold(g.Shadow, shadow)
}

// ParseFor turns --for and --until-revoked into a lifetime; nil means until
// revoked.
func ParseFor(forFlag string, untilRevoked bool) (*time.Duration, error) {
	if untilRevoked {
		if forFlag != "" {
			return nil, contract.New(contract.BadFlag, "pass --for or --until-revoked, not both").WithField("for")
		}
		return nil, nil
	}
	if forFlag == "" {
		forFlag = DefaultFor
	}
	d, ok := Durations[forFlag]
	if !ok {
		return nil, contract.New(contract.BadFlag, "--for must be 6h, 24h or 30d, or pass --until-revoked; got %q", forFlag).WithField("for")
	}
	return &d, nil
}

// NewID returns g- and six hex digits.
func NewID(r io.Reader) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", err
	}
	return "g-" + hex.EncodeToString(b), nil
}

type file struct {
	SchemaVersion string  `json:"schema_version"`
	Grants        []Grant `json:"grants"`
}

// Store is grants.json in one workspace.
type Store struct{ path, lock string }

func NewStore(path, lockPath string) *Store { return &Store{path: path, lock: lockPath} }

// Path is the grants file, for messages.
func (s *Store) Path() string { return s.path }

// Load returns every stored grant, expired ones included. A missing file
// has none; an unreadable or invalid file is an error, never "no grants".
func (s *Store) Load() ([]Grant, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", s.path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		what := "is not valid JSON"
		if json.Valid(b) {
			what = "has a value Ghostbranch can't read"
		}
		return nil, corruptFile{fmt.Errorf("%s %s; fix it or run `ghostbranch agents revoke --all`: %w", s.path, what, err)}
	}
	if contract.Newer(f.SchemaVersion) {
		return nil, contract.NewerRecord("grants", f.SchemaVersion)
	}
	return f.Grants, nil
}

// Valid returns the grant in force for the pair at now.
func (s *Store) Valid(public, shadow string, now time.Time) (Grant, bool, error) {
	gs, err := s.Load()
	if err != nil {
		return Grant{}, false, err
	}
	for _, g := range gs {
		if g.For(public, shadow) && g.ValidAt(now) {
			return g, true, nil
		}
	}
	return Grant{}, false, nil
}

// Put stores g, replacing any grant for the same pair, and prunes expired
// grants. It returns the grant it replaced, if one was still in force.
func (s *Store) Put(g Grant, now time.Time) (*Grant, error) {
	var replaced *Grant
	err := s.update(now, false, func(gs []Grant) []Grant {
		out := make([]Grant, 0, len(gs)+1)
		for _, old := range gs {
			if old.For(g.Public, g.Shadow) {
				o := old
				replaced = &o
				continue
			}
			out = append(out, old)
		}
		return append(out, g)
	})
	return replaced, err
}

// Revoke removes the grants whose public repo is public, or every grant when
// all is set, and returns the ones that were in force. With all, a file that
// isn't valid JSON or doesn't decode is replaced too, so a corrupt grants.json
// can be cleared; one it couldn't read, or from a newer Ghostbranch, is kept.
func (s *Store) Revoke(public string, all bool, now time.Time) ([]Grant, error) {
	removed := []Grant{}
	err := s.update(now, all, func(gs []Grant) []Grant {
		out := make([]Grant, 0, len(gs))
		for _, g := range gs {
			if all || strings.EqualFold(g.Public, public) {
				removed = append(removed, g)
				continue
			}
			out = append(out, g)
		}
		return out
	})
	return removed, err
}

func (s *Store) update(now time.Time, replaceCorrupt bool, fn func([]Grant) []Grant) error {
	unlock, err := workspace.Lock(s.lock, now, "agents")
	if err != nil {
		return err
	}
	defer unlock()
	gs, err := s.Load()
	if err != nil && (!replaceCorrupt || !unparsable(err)) {
		return err
	}
	live := make([]Grant, 0, len(gs))
	for _, g := range gs {
		if g.ValidAt(now) {
			live = append(live, g)
		}
	}
	return s.save(fn(live))
}

func (s *Store) save(gs []Grant) error {
	if gs == nil {
		gs = []Grant{}
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{SchemaVersion: SchemaVersion, Grants: gs}, "", "  ")
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

// unparsable reports a Load error from a file that was read but didn't decode,
// the only error revoke --all may replace the file over: a file it couldn't
// read, or one from a newer Ghostbranch, stays as it is.
func unparsable(err error) bool {
	var c corruptFile
	return errors.As(err, &c)
}
