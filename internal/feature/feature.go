// Package feature defines a feature's metadata (feature.json, stored on
// refs/ghostbranch/features/<name> in the shadow repo) and computes its
// state. State is never stored.
package feature

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

const (
	SchemaVersion = "0.1"
	MetaFile      = "feature.json"
)

// Gate check and leak outcomes.
const (
	ChecksPassed  = "passed"
	ChecksFailed  = "failed"
	ChecksNoCI    = "no_ci"
	ChecksTimeout = "timeout"
	ChecksNotRun  = "not_run"
	LeakClean     = "clean"
	LeakFound     = "found"
)

// Gate is the last gate result, pinned to the inputs it was computed from.
type Gate struct {
	FeatureHead   string    `json:"feature_head"`
	PublicMain    string    `json:"public_main"`
	Overlay       string    `json:"overlay"`
	Candidate     string    `json:"candidate"`
	PublishedTree string    `json:"published_tree"`
	Checks        string    `json:"checks"`
	Leak          string    `json:"leak"`
	At            time.Time `json:"at"`
}

// Passed reports a green gate: checks passed and no leak findings.
func (g *Gate) Passed() bool {
	return g != nil && g.Checks == ChecksPassed && g.Leak == LeakClean
}

// FreshFor reports whether the gate was computed from exactly these inputs.
func (g *Gate) FreshFor(head, publicMain, overlay string) bool {
	return g != nil && g.FeatureHead == head && g.PublicMain == publicMain && g.Overlay == overlay
}

// Promoted records the public PR a feature was published as.
type Promoted struct {
	PublicPR int    `json:"public_pr"`
	PlanID   string `json:"plan_id"`
}

// Meta is feature.json.
type Meta struct {
	SchemaVersion string    `json:"schema_version"`
	Name          string    `json:"name"`
	Branch        string    `json:"branch"`
	CreatedBy     string    `json:"created_by"`
	CreatedAt     time.Time `json:"created_at"`
	Base          string    `json:"base"`
	Abandoned     bool      `json:"abandoned"`
	Gate          *Gate     `json:"gate"`
	Promoted      *Promoted `json:"promoted"`
}

// Encode serializes m as indented JSON with a trailing newline.
func Encode(m Meta) []byte {
	b, _ := json.MarshalIndent(m, "", "  ")
	return append(b, '\n')
}

// Decode parses feature.json.
func Decode(b []byte) (Meta, error) {
	var m Meta
	err := json.Unmarshal(b, &m)
	return m, err
}

// State is a feature's computed lifecycle state.
type State string

const (
	StateDraft      State = "draft"
	StateInReview   State = "in_review"
	StateGatePassed State = "gate_passed"
	StatePromoted   State = "promoted"
	StateAbandoned  State = "abandoned"
)

// StateInput is everything state depends on.
type StateInput struct {
	Meta           Meta
	Head           string
	PublicMain     string
	Overlay        string
	PublicPRExists bool
}

// StateOf computes state. The first matching rule wins.
func StateOf(in StateInput) State {
	m := in.Meta
	switch {
	case m.Abandoned:
		return StateAbandoned
	case m.Promoted != nil && in.PublicPRExists:
		return StatePromoted
	case m.Gate.Passed() && m.Gate.FreshFor(in.Head, in.PublicMain, in.Overlay):
		return StateGatePassed
	case m.Gate != nil:
		return StateInReview
	}
	return StateDraft
}

var nameChars = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// ValidateName checks a feature or public branch name. field names the
// input in the error.
func ValidateName(name, field string) error {
	bad := func(why string) error {
		return contract.New(contract.NameInvalid, "invalid name %q: %s", name, why).WithField(field)
	}
	switch {
	case len(name) == 0 || len(name) > 100:
		return bad("must be 1 to 100 characters")
	case !nameChars.MatchString(name):
		return bad("use letters, digits, '.', '_', '-' and '/', starting with a letter or digit")
	case strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{"):
		return bad("must not contain '..', '//' or '@{'")
	case strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock"):
		return bad("must not end with '/', '.' or '.lock'")
	case strings.Contains(name, "/."):
		return bad("no path component may start with '.'")
	case name == "main" || name == "HEAD" || name == "ghostbranch" || strings.HasPrefix(name, "ghostbranch/") || strings.HasPrefix(name, "ghostbranch-"):
		return bad("this name is reserved")
	}
	return nil
}
