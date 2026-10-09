// Package plan defines a promote plan: exactly what will be published, and
// the repo state it was made against.
package plan

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

const SchemaVersion = "0.8"

type Pair struct {
	Public string `json:"public"`
	Shadow string `json:"shadow"`
}

// Fingerprint is the repo state a plan was made against.
type Fingerprint struct {
	PublicMain    string `json:"public_main"`
	FeatureHead   string `json:"feature_head"`
	Overlay       string `json:"overlay"`
	GateCandidate string `json:"gate_candidate"`
}

// Moved returns the JSON names of the fields that differ in live, in
// declaration order.
func (f Fingerprint) Moved(live Fingerprint) []string {
	var moved []string
	if f.PublicMain != live.PublicMain {
		moved = append(moved, "public_main")
	}
	if f.FeatureHead != live.FeatureHead {
		moved = append(moved, "feature_head")
	}
	if f.Overlay != live.Overlay {
		moved = append(moved, "overlay")
	}
	if f.GateCandidate != live.GateCandidate {
		moved = append(moved, "gate_candidate")
	}
	return moved
}

type Identity struct {
	Name  string    `json:"name"`
	Email string    `json:"email"`
	Date  time.Time `json:"date"`
}

type PR struct {
	Title string `json:"title"`
	Body  string `json:"body"`
}

// Commit is one public commit of the chain.
type Commit struct {
	Commit  string   `json:"commit"`
	Tree    string   `json:"tree"`
	Parent  string   `json:"parent"`
	Sources []string `json:"sources"` // feature branch commits it publishes
	Author  Identity `json:"author"`
	Subject string   `json:"subject"`
}

// Group is one curated public commit: the feature commits it combines, in
// order, and its message.
type Group struct {
	From    []string `json:"from"`
	Message string   `json:"message"`
}

// Publish describes what will be published. Commit is the tip, Tree the
// tip's tree (the gated tree) and Parent public main.
type Publish struct {
	Commit       string   `json:"commit"`
	Tree         string   `json:"tree"`
	Parent       string   `json:"parent"`
	TargetBranch string   `json:"target_branch"`
	PR           PR       `json:"pr"`
	Commits      []Commit `json:"commits"`       // the public commits, oldest first; the last is Commit
	DroppedEmpty []string `json:"dropped_empty"` // feature commits that publish nothing
}

// Waived is a finding the plan was approved with because a person waived it.
type Waived struct {
	ID     string `json:"id"`
	Rule   string `json:"rule"`
	Where  string `json:"where"`
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// Leak is the scan result the plan was approved with. A plan only exists
// when nothing blocks; waived findings and warnings are recorded.
type Leak struct {
	Findings       []map[string]any   `json:"findings"`
	Waived         []Waived           `json:"waived"`
	Warnings       []contract.Warning `json:"warnings"`
	ScannerVersion string             `json:"scanner_version"`
}

// CI is what the gate's CI ran with: the repo guards it rewrote, and those
// it could not, which skipped in the shadow repo.
type CI struct {
	RepoGuards workflow.GuardReport `json:"repo_guards"`
}

type Plan struct {
	SchemaVersion string      `json:"schema_version"`
	ID            string      `json:"id"`
	Kind          string      `json:"kind"`
	Actor         string      `json:"actor"`
	CreatedAt     time.Time   `json:"created_at"`
	Pair          Pair        `json:"pair"`
	Feature       string      `json:"feature"`
	Mode          string      `json:"mode"`
	Fingerprint   Fingerprint `json:"fingerprint"`
	Curate        []Group     `json:"curate"` // null unless curated
	Publish       Publish     `json:"publish"`
	Leak          Leak        `json:"leak"`
	CI            CI          `json:"ci"`
}

// NewID returns a time-sortable plan ID such as 20261005T123000Z-abcdef.
func NewID(now time.Time, rnd io.Reader) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// Save writes p to dir/<id>.json atomically and returns the path.
func Save(dir string, p Plan) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, p.ID+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	fail := func(err error) (string, error) {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	path := filepath.Join(dir, p.ID+".json")
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

var idPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[0-9a-f]{6}$`)

// ErrNotFound means no plan file exists for the reference.
var ErrNotFound = errors.New("plan not found")

// ValidID reports whether s is a plan ID such as 20261005T123000Z-abcdef.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// Load reads a plan by ID in dir, or by a path ending in .json.
func Load(dir, ref string) (Plan, string, error) {
	path := ref
	switch {
	case strings.HasSuffix(ref, ".json"):
	case ValidID(ref):
		path = filepath.Join(dir, ref+".json")
	default:
		return Plan{}, "", contract.New(contract.BadFlag, "%q is not a plan ID or a path to a plan .json file", ref).WithField("plan")
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Plan{}, "", contract.Wrap(contract.BadFlag, ErrNotFound, "no plan %q", ref).WithField("plan")
	}
	if err != nil {
		return Plan{}, "", contract.Wrap(contract.Internal, err, "read plan %q", ref)
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return Plan{}, "", contract.Wrap(contract.BadFlag, err, "plan %q is not valid JSON", ref).WithField("plan")
	}
	// The file is unsigned, and its ID names a ref in the shadow repo.
	if !ValidID(p.ID) {
		return Plan{}, "", contract.New(contract.BadFlag, "plan %q has an invalid id %q", ref, p.ID).WithField("plan")
	}
	return p, path, nil
}
