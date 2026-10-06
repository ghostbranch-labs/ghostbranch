// Package plan defines a promote plan: exactly what will be published, and
// the repo state it was made against.
package plan

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

const SchemaVersion = "0.2"

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

type Publish struct {
	Commit       string   `json:"commit"`
	Tree         string   `json:"tree"`
	Parent       string   `json:"parent"`
	Author       Identity `json:"author"`
	TargetBranch string   `json:"target_branch"`
	PR           PR       `json:"pr"`
}

// Leak is the scan result the plan was approved with. A plan only exists
// when Findings is empty.
type Leak struct {
	Findings       []map[string]any `json:"findings"`
	ScannerVersion string           `json:"scanner_version"`
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
	Publish       Publish     `json:"publish"`
	Leak          Leak        `json:"leak"`
}

// NewID returns a time-sortable plan ID such as 20261005T123000Z-abcdef.
func NewID(now time.Time, rnd io.Reader) (string, error) {
	b := make([]byte, 3)
	if _, err := io.ReadFull(rnd, b); err != nil {
		return "", err
	}
	return now.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b), nil
}

// Save writes p to dir/<id>.json and returns the path.
func Save(dir string, p Plan) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, p.ID+".json")
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load reads a plan by file path or by ID in dir.
func Load(dir, ref string) (Plan, string, error) {
	path := ref
	if !strings.HasSuffix(ref, ".json") {
		path = filepath.Join(dir, ref+".json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Plan{}, "", contract.New(contract.BadFlag, "no plan %q", ref).WithField("plan")
	}
	var p Plan
	if err := json.Unmarshal(b, &p); err != nil {
		return Plan{}, "", contract.Wrap(contract.BadFlag, err, "plan %q is not valid JSON", ref).WithField("plan")
	}
	return p, path, nil
}
