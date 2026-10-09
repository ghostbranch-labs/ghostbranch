package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

// FeatureReport is one feature as the survey sees it.
type FeatureReport struct {
	Name      string        `json:"name"`
	State     feature.State `json:"state"`
	Branch    string        `json:"branch"`
	Head      string        `json:"head"` // "" when the branch is gone
	Base      string        `json:"base"`
	Drift     Drift         `json:"drift"`
	Gate      *GateSummary  `json:"gate"`      // nil when never gated
	PublicPR  *int          `json:"public_pr"` // nil when never published
	PRState   string        `json:"pr_state"`  // "", open, merged, closed, unknown
	PlanID    string        `json:"plan_id"`
	Moved     bool          `json:"moved"` // since publish; false when unpublished; meaningless when the branch is gone (branch_gone reads only PRState)
	Abandoned bool          `json:"abandoned"`
}

// Drift is whether a feature still merges cleanly onto public main.
type Drift struct {
	Status string   `json:"status"` // clean | conflicts | unknown | n/a (branch gone)
	Files  []string `json:"files"`
}

// GateSummary is a feature's last gate result.
type GateSummary struct {
	Checks    string    `json:"checks"`
	Leak      string    `json:"leak"`
	ScannedAs string    `json:"scanned_as"`
	Fresh     bool      `json:"fresh"`
	At        time.Time `json:"at"`
	Job       *string   `json:"job"` // non-nil exactly when the gate is pending: what wait takes
}

// PR states in a FeatureReport.
const (
	prOpen    = "open"
	prMerged  = "merged"
	prClosed  = "closed"
	prUnknown = "unknown"
)

// Drift statuses in a FeatureReport.
const (
	driftClean     = "clean"
	driftConflicts = "conflicts"
	driftUnknown   = "unknown"
	driftNA        = "n/a"
)

// survey reports every feature of the pair (sorted by name), or only
// names when given. pc must be open (mirror fetched). It only reads: the
// mirror, the plan files and the public PRs (one GetPR per published
// feature). Names that aren't features are left out, not errors.
func (e *Engine) survey(ctx context.Context, pc *pairCtx, names ...string) ([]FeatureReport, []contract.Warning, error) {
	const prefix = "refs/shadow-meta/features/"
	refs, err := pc.git.Refs(ctx, strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, nil, internalErr(err, "list features")
	}
	var list []string
	for ref := range refs {
		list = append(list, strings.TrimPrefix(ref, prefix))
	}
	if len(names) > 0 {
		var keep []string
		for _, n := range names {
			if _, ok := refs[prefix+n]; ok {
				keep = append(keep, n)
			}
		}
		list = keep
	}
	sort.Strings(list)
	out := []FeatureReport{}
	if len(list) == 0 {
		return out, nil, nil
	}
	// The caller reports config warnings; the survey needs only O.
	_, O, _, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, nil, err
	}
	pm, err := pc.git.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return nil, nil, internalErr(err, "read public main")
	}
	var warns []contract.Warning
	for _, name := range list {
		r, ws, err := e.surveyOne(ctx, pc, name, pm, O)
		if err != nil {
			return nil, warns, err
		}
		out = append(out, r)
		warns = addWarnings(warns, ws...)
	}
	return out, warns, nil
}

func (e *Engine) surveyOne(ctx context.Context, pc *pairCtx, name, pm, O string) (FeatureReport, []contract.Warning, error) {
	var warns []contract.Warning
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return FeatureReport{}, nil, err
	}
	head, exists, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
	if err != nil {
		return FeatureReport{}, nil, internalErr(err, "read feature branch "+m.Branch)
	}
	if !exists {
		head = ""
	}
	r := FeatureReport{Name: name, Branch: m.Branch, Head: head, Base: m.Base, Abandoned: m.Abandoned,
		Drift: Drift{Status: driftNA, Files: []string{}}}
	if g := m.Gate; g != nil {
		r.Gate = &GateSummary{Checks: g.Checks, Leak: g.Leak, ScannedAs: g.ScannedAs, At: g.At,
			Fresh: exists && g.FreshFor(head, pm, O)}
		if g.Pending() {
			job := jobID(name, g.Candidate)
			r.Gate.Job = &job
		}
	}
	if m.Promoted != nil {
		n := m.Promoted.PublicPR
		r.PublicPR, r.PlanID = &n, m.Promoted.PlanID
		pr, err := e.Forge.GetPR(ctx, pc.pair.Public, n)
		switch {
		case err != nil:
			r.PRState = prUnknown
			warns = append(warns, contract.Warning{Code: contract.PRStateUnknown,
				Message: fmt.Sprintf("couldn't read public PR #%d of feature %s: %v", n, name, err),
				Details: map[string]any{"feature": name, "public_pr": n}})
		case pr.Merged:
			r.PRState = prMerged
		case pr.State == prClosed:
			r.PRState = prClosed
		default:
			r.PRState = prOpen
		}
		base := e.publishedHead(m)
		r.Moved = base == "" || head != base
	}
	if exists {
		clean, files, err := pc.git.MergeTreeCheck(ctx, pm, head)
		switch {
		case err != nil:
			r.Drift.Status = driftUnknown
			warns = append(warns, contract.Warning{Code: contract.DriftUnknown,
				Message: fmt.Sprintf("couldn't check whether feature %s merges cleanly onto public main: %v", name, err),
				Details: map[string]any{"feature": name}})
		case clean:
			r.Drift.Status = driftClean
		default:
			r.Drift = Drift{Status: driftConflicts, Files: append([]string{}, files...)}
		}
	}
	r.State = feature.StateOf(feature.StateInput{Meta: m, BranchExists: exists, Head: head, PublicMain: pm,
		Overlay: O, PRState: r.PRState, Moved: r.Moved})
	return r, warns, nil
}

// publishedHead is the feature head a published feature was published
// from, or "" when nothing records it (which counts as moved). In order:
// the head apply recorded; the plan file's fingerprint (the plan may have
// been made on another machine, so a missing or unreadable plan is
// skipped); the gate record's head, but only when the plan file loaded and
// names the gate's candidate, which must not be empty, as the one it
// published. It never guesses safe: a re-gate overwrites the gate record,
// so without the plan to tie it to the publish, the gate's head may hold
// commits never published.
func (e *Engine) publishedHead(m feature.Meta) string {
	if h := m.Promoted.FeatureHead; h != "" {
		return h
	}
	var pl *plan.Plan
	// Only a plan ID, never a path: Load would read any .json file a path named.
	if id := m.Promoted.PlanID; plan.ValidID(id) {
		if p, _, err := plan.Load(e.WS.PlansDir(), id); err == nil && p.ID == id && p.Feature == m.Name {
			pl = &p
		}
	}
	if pl != nil && pl.Fingerprint.FeatureHead != "" {
		return pl.Fingerprint.FeatureHead
	}
	if g := m.Gate; pl != nil && g != nil && g.Candidate != "" && g.Candidate == pl.Fingerprint.GateCandidate {
		return g.FeatureHead
	}
	return ""
}

// ClosableWithoutConsole is the one rule for unattended deletion: published,
// merged and unmoved, or merged with its branch already gone (nothing left
// on it to lose).
func ClosableWithoutConsole(r FeatureReport) bool {
	return r.State == feature.StateMerged ||
		(r.State == feature.StateBranchGone && r.PRState == "merged")
}
