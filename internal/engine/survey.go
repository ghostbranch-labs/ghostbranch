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
	Name   string        `json:"name"`
	State  feature.State `json:"state"`
	Branch string        `json:"branch"`
	Head   string        `json:"head"` // "" when the branch is gone
	Base   string        `json:"base"`
	// Parent is the feature it is stacked on, or "main"; "" when the record
	// couldn't be read. Children are the live features stacked directly on
	// it, by name, never null. Stack is the native GitHub stack its PR is in.
	Parent    string       `json:"parent"`
	Children  []string     `json:"children"`
	Stack     *int         `json:"stack"`
	Drift     Drift        `json:"drift"`
	Gate      *GateSummary `json:"gate"`      // nil when never gated
	PublicPR  *int         `json:"public_pr"` // nil when never published
	PRState   string       `json:"pr_state"`  // "", open, merged, closed, unknown
	PlanID    string       `json:"plan_id"`
	Moved     bool         `json:"moved"` // since publish; false when unpublished; meaningless when the branch is gone (branch_gone reads only PRState)
	Abandoned bool         `json:"abandoned"`
	// Error is why the feature couldn't be surveyed; set only with state
	// unreadable. feature show returns it as its error.
	Error string `json:"error,omitempty"`
	// err is the error behind Error, for feature show and feature close.
	err error
	// unreadable is why the record couldn't be read as a whole: "" when
	// it could, "newer" (from a newer Ghostbranch), "corrupt" (the record
	// doesn't parse) or "error" (reading the record or the feature's refs
	// failed). Such a feature never closes unattended.
	unreadable string
	// gateForPublished: the feature has no gate, or its gate is for the
	// head that was published and its gate branch is gone or still at the
	// gate's candidate. Otherwise the gate may hold commits never
	// published, and a branch_gone feature doesn't close unattended. Set
	// only for a merged branch_gone feature, the one case
	// ClosableWithoutConsole reads it.
	gateForPublished bool
	// meta is the record as read (nil when it couldn't be), and metaSHA its
	// metadata commit: sync re-parents a closing feature's children from
	// them, leased on that commit.
	meta    *feature.Meta
	metaSHA string
	// flagged: sync found its public branch holding changes Ghostbranch
	// didn't publish (PUBLIC_BRANCH_CHANGED).
	flagged bool
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
// feature). Names that aren't features are left out, not errors. A feature
// whose record or refs can't be read is reported as unreadable, with the
// error (failedReport), and the survey goes on. Only a cancel, or a failure
// that concerns every feature (listing them, the config, public main),
// fails it.
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
			if ctx.Err() != nil {
				return nil, warns, err
			}
			// One feature that can't be read doesn't hide the rest.
			r, ws = e.failedReport(ctx, pc, name, err), nil
		}
		out = append(out, r)
		warns = addWarnings(warns, ws...)
	}
	// Children come from every record, not only the surveyed ones, so that
	// feature show a still names the features stacked on a.
	recs, err := e.records(ctx, pc)
	if err != nil && ctx.Err() != nil {
		return nil, warns, err
	}
	for i := range out {
		out[i].Children = []string{}
		if err == nil {
			out[i].Children = childrenOf(recs, out[i].Name, false)
		}
	}
	return out, warns, nil
}

func (e *Engine) surveyOne(ctx context.Context, pc *pairCtx, name, pm, O string) (FeatureReport, []contract.Warning, error) {
	var warns []contract.Warning
	m, metaSHA, newer, err := e.readMetaAny(ctx, pc, name)
	if err != nil {
		return FeatureReport{}, nil, err
	}
	if newer {
		warns = append(warns, contract.Warning{Code: contract.RecordNewer,
			Message: fmt.Sprintf("feature %s's record was written by a newer Ghostbranch (schema %s); it is shown as far as this binary can read it, and won't be changed",
				name, m.SchemaVersion),
			Details: map[string]any{"feature": name, "record_version": m.SchemaVersion}})
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
	r.meta, r.metaSHA = &m, metaSHA
	r.Parent, r.Children = stackParent(m), []string{}
	if m.Promoted != nil && m.Promoted.Stack != 0 {
		n := m.Promoted.Stack
		r.Stack = &n
	}
	if newer {
		r.unreadable = unreadableNewer
	}
	if g := m.Gate; g != nil {
		r.Gate = &GateSummary{Checks: g.Checks, Leak: g.Leak, ScannedAs: g.ScannedAs, At: g.At,
			Fresh: exists && g.FreshFor(head, pm, O)}
		if g.Pending() {
			job := jobID(name, g.Candidate)
			r.Gate.Job = &job
		}
	}
	st, err := e.stackState(ctx, pc, name, m)
	if err != nil {
		return FeatureReport{}, nil, err
	}
	stackStale := m.Gate != nil && (st.broken || !m.Gate.StackFreshFor(parentFeature(m), forkOf(m), st.heads))
	if r.Gate != nil && stackStale {
		r.Gate.Fresh = false
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
		Overlay: O, PRState: r.PRState, Moved: r.Moved, ParentMoved: st.parentMoved, StackStale: stackStale})
	// Only a merged branch_gone feature needs to know whether its gate is
	// for what was published (ClosableWithoutConsole), so only it reads the
	// gate branch.
	if r.State == feature.StateBranchGone && r.PRState == prMerged {
		if r.gateForPublished, err = gateIsForPublished(ctx, pc, name, m); err != nil {
			return FeatureReport{}, nil, err
		}
	}
	return r, warns, nil
}

// gateIsForPublished reports whether m has no gate, or a gate for the head
// that was published whose gate branch is gone or still at the gate's
// candidate. Otherwise the gate may hold commits never published.
func gateIsForPublished(ctx context.Context, pc *pairCtx, name string, m feature.Meta) (bool, error) {
	g := m.Gate
	if g == nil {
		return true, nil
	}
	if m.Promoted == nil || g.FeatureHead == "" || g.FeatureHead != m.Promoted.FeatureHead {
		return false, nil
	}
	tip, ok, err := pc.git.Resolve(ctx, shadowRef(gateBranch(name)))
	if err != nil {
		return false, internalErr(err, "read gate branch "+gateBranch(name))
	}
	return !ok || tip == g.Candidate, nil
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

// Why a FeatureReport's record couldn't be read as a whole.
const (
	unreadableNewer   = "newer"
	unreadableCorrupt = "corrupt"
	unreadableError   = "error"
)

// ClosableWithoutConsole is the one rule for unattended deletion: published,
// merged and unmoved, or merged with its branch already gone, when the gate
// (if any) is for what was published (nothing left on it to lose). A record
// this binary can't read as a whole never is.
func ClosableWithoutConsole(r FeatureReport) bool {
	if r.unreadable != "" {
		return false
	}
	return r.State == feature.StateMerged ||
		(r.State == feature.StateBranchGone && r.PRState == "merged" && r.gateForPublished)
}
