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
	SchemaVersion = "0.12"
	MetaFile      = "feature.json"
)

// Gate check and leak outcomes.
const (
	ChecksPassed  = "passed"
	ChecksFailed  = "failed"
	ChecksNoCI    = "no_ci"
	ChecksTimeout = "timeout"
	ChecksNotRun  = "not_run"
	ChecksPending = "pending"
	LeakClean     = "clean"
	LeakFound     = "found"
	LeakNotRun    = "not_run"
)

// GateCommit maps one commit of the gate's rebased chain to the feature
// branch commit it replays.
type GateCommit struct {
	Source  string `json:"source"`
	Rebased string `json:"rebased"`
}

// Gate is the last gate result, pinned to the inputs it was computed from.
type Gate struct {
	FeatureHead   string       `json:"feature_head"`
	PublicMain    string       `json:"public_main"`
	Overlay       string       `json:"overlay"`
	Candidate     string       `json:"candidate"`
	PublishedTree string       `json:"published_tree"`
	Commits       []GateCommit `json:"commits"`
	Checks        string       `json:"checks"`
	Leak          string       `json:"leak"`
	At            time.Time    `json:"at"`
	// ScannedAs is the mode the gate's leak scan covered; empty in a gate
	// recorded before it was stored, which promote does not reuse.
	ScannedAs string `json:"scanned_as,omitempty"`
	// FailedChecks names the checks that failed; absent before 0.7.
	FailedChecks []string `json:"failed_checks,omitempty"`
	// ShadowPR is the gate's draft PR in the shadow repo; 0 before 0.7.
	ShadowPR int `json:"shadow_pr,omitempty"`
	// StartedAt is when the gate pushed its candidate. CI time budgets
	// count from it, so a wait in another process keeps the same budget.
	// Zero before 0.7.
	StartedAt time.Time `json:"started_at,omitzero"`
	// Parent is the feature the gate stacked on; empty when it built on
	// public main.
	Parent string `json:"parent,omitempty"`
	// ForkedAt is the fork point the gate replayed the feature's own
	// commits from; empty when unstacked.
	ForkedAt string `json:"forked_at,omitempty"`
	// Base is the commit the feature's own commits were replayed onto: R,
	// the rebased tip of the features below; empty when unstacked (public
	// main).
	Base string `json:"base,omitempty"`
	// BaseTree is Base's published tree: what the leak scan compared the
	// feature's own commits with. Empty when unstacked.
	BaseTree string `json:"base_tree,omitempty"`
	// Below lists every feature under this one with the head the gate
	// built on, bottom first; nil when unstacked.
	Below []StackHead `json:"below,omitempty"`
	// Update is how the gate built the update of the feature's open PR;
	// nil for a first publish and in a gate recorded before 0.12. An append
	// gate's Base is the PR's head H, and its Commits map the new feature
	// commits to the public commits appended on H.
	Update *GateUpdate `json:"update,omitempty"`
}

// StackHead is one feature below a stacked feature, at the head its gate
// built on.
type StackHead struct {
	Feature string `json:"feature"`
	Head    string `json:"head"`
}

// GateUpdate is how a gate built the update of a feature's open PR: on its
// head H, by appending the new commits or replacing the branch.
type GateUpdate struct {
	PR     int    `json:"pr"`
	Head   string `json:"head"`
	Base   string `json:"base"`
	Style  string `json:"style"` // append | replace
	Reason string `json:"replace_reason,omitempty"`
}

// Pending reports a gate whose CI wait hasn't finished: a job. A pending
// record without its shadow PR was rewritten by an older binary and is no
// job: gate it again.
func (g *Gate) Pending() bool { return g != nil && g.Checks == ChecksPending && g.ShadowPR != 0 }

// Passed reports a green gate: checks passed and no leak findings.
func (g *Gate) Passed() bool {
	return g != nil && g.Checks == ChecksPassed && g.Leak == LeakClean
}

// FreshFor reports whether the gate was computed from exactly these inputs.
func (g *Gate) FreshFor(head, publicMain, overlay string) bool {
	return g != nil && g.FeatureHead == head && g.PublicMain == publicMain && g.Overlay == overlay
}

// StackFreshFor reports whether the gate built on the stack as it is now:
// the same parent ("" for main) and fork point ("" when unstacked), and
// every feature below (below maps each to its head now) at the head it
// recorded. An unstacked gate of an unstacked feature is fresh; FreshFor
// checks the feature's own inputs.
func (g *Gate) StackFreshFor(parent, forkedAt string, below map[string]string) bool {
	if g == nil || g.Parent != parent || g.ForkedAt != forkedAt || len(g.Below) != len(below) {
		return false
	}
	for _, b := range g.Below {
		if h, ok := below[b.Feature]; !ok || h != b.Head {
			return false
		}
	}
	return true
}

// Promoted records the public PR a feature was published as.
type Promoted struct {
	PublicPR int    `json:"public_pr"`
	PlanID   string `json:"plan_id"`
	// FeatureHead is the head the publish was planned from; empty in a
	// record written before 0.6. A feature has moved when its branch head
	// is no longer FeatureHead.
	FeatureHead string `json:"feature_head,omitempty"`
	// Branch is the public branch it was published to; empty in a record
	// written before 0.11 (the feature's name then, unless --branch named one).
	Branch string `json:"branch,omitempty"`
	// Base is its PR's base branch: main, or the public branch of the
	// feature below it in a stack. Sync records GitHub's retargeting.
	Base string `json:"base,omitempty"`
	// PublishedHead is the public branch's head as Ghostbranch last knew it:
	// the plan's tip at publish, and GitHub's restack of it once sync saw
	// one with the same patch-ids.
	PublishedHead string `json:"published_head,omitempty"`
	// PatchIDs are the published commits' stable patch-ids, oldest first.
	PatchIDs []string `json:"patch_ids,omitempty"`
	// Stack is the native GitHub stack its PR is in; 0 when none.
	Stack int `json:"stack,omitempty"`
	// NeedsUpdate: the PR below it merged by squash or rebase while the PRs
	// were plain chained PRs (no native stack), so its open public PR still
	// carries that parent's old commits. Republishing it is 6.1a-2.
	NeedsUpdate bool `json:"needs_update,omitempty"`
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
	// Parent is the feature this one is stacked on; "" means main, as in
	// every record before 0.11. ParentName reads it.
	Parent string `json:"parent,omitempty"`
	// ForkedAt is the parent's commit the feature was branched from or last
	// restacked onto: the feature's own commits are ForkedAt..head. Empty
	// for a feature started from main.
	ForkedAt string `json:"forked_at,omitempty"`
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

// MainParent is the parent of a feature that isn't stacked on another.
const MainParent = "main"

// ParentName is the feature's parent: a feature name, or "main".
func (m Meta) ParentName() string {
	if m.Parent == "" {
		return MainParent
	}
	return m.Parent
}

// Stacked reports whether the feature is stacked on another feature.
func (m Meta) Stacked() bool { return m.ParentName() != MainParent }

// State is a feature's computed lifecycle state.
type State string

const (
	StateDraft       State = "draft"
	StateInReview    State = "in_review"
	StateGatePassed  State = "gate_passed"
	StatePublished   State = "published"
	StateMerged      State = "merged"
	StateMergedMoved State = "merged_moved"
	StatePRClosed    State = "pr_closed"
	StateBranchGone  State = "branch_gone"
	StateAbandoned   State = "abandoned"
	// StateUnreadable is a feature whose record couldn't be read: the survey
	// reports it with an error, and only a console close removes it.
	StateUnreadable State = "unreadable"
	// StateParentStale is a stacked feature where a feature below moved
	// since the feature above it forked from it, or is gone; feature
	// restack on the lowest one fixes it.
	StateParentStale State = "parent_stale"
	// StatePublicPRNeedsUpdate is a feature published as a plain chained PR
	// (not a native stack) whose PR below was squash- or rebase-merged: its
	// public branch still carries the old parent commits.
	StatePublicPRNeedsUpdate State = "public_pr_needs_update"
	// StatePRBehind is a published feature whose PR is open and whose
	// branch has commits the PR doesn't: gate it, then promote updates the
	// PR.
	StatePRBehind State = "pr_behind"
)

// StateInput is everything state depends on.
type StateInput struct {
	Meta         Meta
	BranchExists bool
	Head         string
	PublicMain   string
	Overlay      string
	// PRState is the forge's view of the published PR: "", "open",
	// "merged", "closed" or "unknown".
	PRState string
	// Moved reports that the branch has moved since it was published.
	Moved bool
	// ParentMoved reports that a feature below moved since the feature
	// above it forked from it, or is gone; false when unstacked.
	ParentMoved bool
	// StackStale reports that the gate built on a stack that has changed
	// since (Gate.StackFreshFor is false); false when unstacked.
	StackStale bool
	// Behind counts the branch's commits since it was published that its
	// open PR doesn't have; 0 when it isn't published or the PR isn't open.
	Behind int
}

// StateOf computes state. The first matching rule wins.
func StateOf(in StateInput) State {
	m := in.Meta
	switch {
	case m.Abandoned:
		return StateAbandoned
	case !in.BranchExists:
		return StateBranchGone
	case m.Promoted != nil && in.PRState == "merged" && !in.Moved:
		return StateMerged
	case m.Promoted != nil && in.PRState == "merged":
		return StateMergedMoved
	case m.Promoted != nil && in.PRState == "closed":
		return StatePRClosed
	case m.Promoted != nil && m.Promoted.NeedsUpdate:
		return StatePublicPRNeedsUpdate
	case m.Promoted != nil && in.PRState == "open" && in.Behind > 0:
		return StatePRBehind
	case m.Promoted != nil:
		return StatePublished
	case in.ParentMoved:
		return StateParentStale
	case m.Gate.Passed() && m.Gate.FreshFor(in.Head, in.PublicMain, in.Overlay) && !in.StackStale:
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
	case strings.Contains(name, ".lock/"):
		return bad("no path component may end with '.lock'")
	case name == "main" || name == "HEAD" || name == "ghostbranch" || strings.HasPrefix(name, "ghostbranch/") || strings.HasPrefix(name, "ghostbranch-"):
		return bad("this name is reserved")
	}
	return nil
}
