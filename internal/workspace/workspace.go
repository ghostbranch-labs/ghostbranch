// Package workspace manages local state: the state dir, workspaces and
// their repo pairs, per-pair mirror paths, the pair lock, and working out
// which pair a command targets.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

const SchemaVersion = "0.12"

// Pair is a public repo and its shadow repo, as owner/name.
type Pair struct {
	Public string `json:"public"`
	Shadow string `json:"shadow"`
}

// Key names the pair's state folder.
func (p Pair) Key() string { return strings.ReplaceAll(strings.ToLower(p.Public), "/", "__") }

// Workspace is a named set of pairs on this machine.
type Workspace struct {
	Name  string
	Dir   string
	Pairs []Pair
}

type file struct {
	SchemaVersion string `json:"schema_version"`
	Pairs         []Pair `json:"pairs"`
}

// HomeDir returns the state dir: $GHOSTBRANCH_HOME, else the platform's
// local state location.
func HomeDir(getenv func(string) string, goos string, userHome func() (string, error)) (string, error) {
	if h := getenv("GHOSTBRANCH_HOME"); h != "" {
		return h, nil
	}
	switch goos {
	case "windows":
		if d := getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ghostbranch"), nil
		}
		return "", errors.New("LOCALAPPDATA is not set; set GHOSTBRANCH_HOME")
	case "darwin":
		h, err := userHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, "Library", "Application Support", "ghostbranch"), nil
	default:
		if d := getenv("XDG_STATE_HOME"); d != "" {
			return filepath.Join(d, "ghostbranch"), nil
		}
		h, err := userHome()
		if err != nil {
			return "", err
		}
		return filepath.Join(h, ".local", "state", "ghostbranch"), nil
	}
}

var wsName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// Open loads (or starts) the workspace called name under home.
func Open(home, name string) (*Workspace, error) {
	if !wsName.MatchString(name) {
		return nil, contract.New(contract.BadFlag, "invalid workspace name %q: use lowercase letters, digits and '-'", name).WithField("workspace")
	}
	w := &Workspace{Name: name, Dir: filepath.Join(home, "workspaces", name)}
	b, err := os.ReadFile(filepath.Join(w.Dir, "workspace.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return w, nil
	}
	if err != nil {
		return nil, err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("read workspace.json: %w", err)
	}
	w.Pairs = f.Pairs
	return w, nil
}

// Save writes workspace.json atomically.
func (w *Workspace) Save() error {
	if err := os.MkdirAll(w.Dir, 0o755); err != nil {
		return err
	}
	pairs := w.Pairs
	if pairs == nil {
		pairs = []Pair{}
	}
	b, err := json.MarshalIndent(file{SchemaVersion: SchemaVersion, Pairs: pairs}, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(w.Dir, "workspace.json.tmp")
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(w.Dir, "workspace.json"))
}

// How often, and for how long, Update retries a busy workspace lock. Its
// holds last milliseconds, so two runs registering at once just queue.
const (
	updateLockPoll = 25 * time.Millisecond
	updateLockWait = 5 * time.Second
)

// Update changes workspace.json under its own lock: it re-reads the file,
// applies fn, saves when fn reports a change, and leaves the fresh pairs
// in w. Two runs registering pairs at once both keep theirs. A lock still
// busy after updateLockWait is LOCKED.
func (w *Workspace) Update(now time.Time, fn func(*Workspace) bool) error {
	path := filepath.Join(w.Dir, "workspace.lock")
	drop, err := Lock(path, now, "workspace")
	busy := func(err error) bool { return err != nil && contract.From(err).Code == contract.Locked }
	for deadline := time.Now().Add(updateLockWait); busy(err) && time.Now().Before(deadline); {
		time.Sleep(updateLockPoll)
		drop, err = Lock(path, now, "workspace")
	}
	if err != nil {
		return err
	}
	defer drop()
	fresh, err := Open(filepath.Dir(filepath.Dir(w.Dir)), w.Name)
	if err != nil {
		return err
	}
	if fn(fresh) {
		if err := fresh.Save(); err != nil {
			return err
		}
	}
	w.Pairs = fresh.Pairs
	return nil
}

// AddPair adds p unless a pair with the same public repo exists.
func (w *Workspace) AddPair(p Pair) bool {
	if _, ok := w.Find(p.Public); ok {
		return false
	}
	w.Pairs = append(w.Pairs, p)
	return true
}

// RemovePair drops the pair whose public repo is public and reports whether
// it was registered. The caller saves.
func (w *Workspace) RemovePair(public string) bool {
	n := len(w.Pairs)
	w.Pairs = slices.DeleteFunc(w.Pairs, func(p Pair) bool { return strings.EqualFold(p.Public, public) })
	return len(w.Pairs) != n
}

// Find returns the pair whose public or shadow repo is fullName.
func (w *Workspace) Find(fullName string) (Pair, bool) {
	for _, p := range w.Pairs {
		if strings.EqualFold(p.Public, fullName) || strings.EqualFold(p.Shadow, fullName) {
			return p, true
		}
	}
	return Pair{}, false
}

func (w *Workspace) PairDir(p Pair) string   { return filepath.Join(w.Dir, "pairs", p.Key()) }
func (w *Workspace) MirrorDir(p Pair) string { return filepath.Join(w.PairDir(p), "mirror.git") }
func (w *Workspace) LockPath(p Pair) string  { return filepath.Join(w.Dir, "locks", p.Key()+".lock") }
func (w *Workspace) WorkDir(p Pair, opID string) string {
	return filepath.Join(w.PairDir(p), "work", opID)
}
func (w *Workspace) LedgerPath() string      { return filepath.Join(w.Dir, "ledger.jsonl") }
func (w *Workspace) PlansDir() string        { return filepath.Join(w.Dir, "plans") }
func (w *Workspace) GrantsPath() string      { return filepath.Join(w.Dir, "grants.json") }
func (w *Workspace) GrantsLockPath() string  { return filepath.Join(w.Dir, "grants.lock") }
func (w *Workspace) WaiversPath() string     { return filepath.Join(w.Dir, "waivers.json") }
func (w *Workspace) WaiversLockPath() string { return filepath.Join(w.Dir, "waivers.lock") }
