package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// FeatureAbandonResult is a feature parked or restored.
type FeatureAbandonResult struct {
	Feature   string           `json:"feature"`
	Abandoned bool             `json:"abandoned"` // the new value
	Changed   bool             `json:"changed"`   // false when it already had that value
	Preview   *preview.Preview `json:"preview"`
}

// FeatureCloseResult is what closing a feature deleted.
type FeatureCloseResult struct {
	Feature  string            `json:"feature"`
	State    feature.State     `json:"state"`    // the state it was closed from
	Deleted  map[string]string `json:"deleted"`  // shadow ref -> SHA it pointed at; never null
	GatePR   *int              `json:"gate_pr"`  // the draft PR closed, nil when none was open
	Approval *ledger.Approval  `json:"approval"` // console, or nil when none was needed
	Preview  *preview.Preview  `json:"preview"`
}

// FeatureAbandon parks a feature (or, with undo, restores it) by flipping
// Abandoned in its metadata, a leased push to the shadow repo. Its branch,
// gate and plans stay; nothing is deleted, so no console is asked.
func (e *Engine) FeatureAbandon(ctx context.Context, p workspace.Pair, name string, undo bool) (res *FeatureAbandonResult, warns []contract.Warning, err error) {
	changed := false
	defer func() {
		summary := ""
		if err == nil && res != nil && res.Preview == nil {
			switch {
			case changed && undo:
				summary = "restored"
			case changed:
				summary = "abandoned"
			case undo:
				summary = "already active; nothing changed"
			default:
				summary = "already abandoned; nothing changed"
			}
		}
		e.record("feature abandon", &p, name, "", summary, err)
	}()
	if err := feature.ValidateName(name, "feature"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature abandon", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	m, metaSHA, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, warns, err
	}
	changed = m.Abandoned != !undo
	res = &FeatureAbandonResult{Feature: name, Abandoned: !undo, Changed: changed}
	if e.DryRun {
		res.Preview = preview.New(nil)
		if changed {
			res.Preview.Add(preview.PushRef(pc.pair.Shadow, remoteMetaRef(name), metaSHA, ""))
		}
		return res, warns, nil
	}
	if !changed {
		return res, warns, nil
	}
	m.SchemaVersion = feature.SchemaVersion
	m.Abandoned = !undo
	mw, err := e.updateMeta(ctx, pc, m, metaSHA)
	warns = addWarnings(warns, mw...)
	if err != nil {
		return nil, warns, err
	}
	return res, warns, nil
}

// FeatureClose deletes a feature from the shadow repo: its branch, gate
// branch, metadata and plan refs, in one atomic push leased on what the
// survey saw, then closes its gate draft PR and cleans up this machine.
// Only a published, merged and unmoved feature (or a merged one whose
// branch is gone) closes without a person at a console; --plan asks no one
// and changes nothing.
func (e *Engine) FeatureClose(ctx context.Context, p workspace.Pair, name string) (res *FeatureCloseResult, warns []contract.Warning, err error) {
	// The console's yes is recorded even when the close then fails.
	var appr *ledger.Approval
	defer func() {
		summary := ""
		var refs map[string]string
		// A close can fail after its deletes (the gate PR); the ledger
		// still records every ref deleted.
		if res != nil && res.Preview == nil {
			refs = res.Deleted
			if err == nil {
				summary = closeLedgerSummary(res)
			}
		}
		e.recordWith("feature close", &p, name, "", summary, err, func(ent *ledger.Entry) {
			if len(refs) > 0 {
				ent.Refs = refs
			}
			ent.Approval = appr
		})
	}()
	if err := feature.ValidateName(name, "feature"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature close", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	rs, sw, err := e.survey(ctx, pc, name)
	warns = addWarnings(warns, sw...)
	if err != nil {
		return nil, warns, err
	}
	if len(rs) != 1 {
		return nil, warns, contract.New(contract.FeatureNotFound,
			"no feature %q in %s; list them with ghostbranch feature list --all", name, pc.pair.Shadow).WithField("feature")
	}
	r := rs[0]
	t, err := e.closeTargets(ctx, pc, r)
	if err != nil {
		return nil, warns, err
	}
	// The console comes before anything changes.
	if !e.DryRun && !ClosableWithoutConsole(r) {
		if e.Approver == nil {
			return nil, warns, noConsole("closing feature " + name + " (" + string(r.State) +
				"; only a merged feature with no newer commits closes unattended)")
		}
		if err := e.confirm(ctx, "", closeSummary(pc, r, t)); err != nil {
			return nil, warns, err
		}
		appr = &ledger.Approval{Kind: "console"}
	}
	res, cw, err := e.closeWith(ctx, pc, r, t)
	warns = addWarnings(warns, cw...)
	if res != nil {
		res.Approval = appr
	}
	return res, warns, err
}

// closeFeature deletes one surveyed feature. It asks no console: sync's
// auto-close calls it only for ClosableWithoutConsole reports. The caller
// records the ledger entry, with Refs = res.Deleted. A non-nil res with an
// error means the refs were deleted but the gate PR couldn't be closed.
// The warnings are MIRROR_NOT_UPDATED from the local cleanup.
func (e *Engine) closeFeature(ctx context.Context, pc *pairCtx, r FeatureReport) (*FeatureCloseResult, []contract.Warning, error) {
	t, err := e.closeTargets(ctx, pc, r)
	if err != nil {
		return nil, nil, err
	}
	return e.closeWith(ctx, pc, r, t)
}

// closeTargets is what closing a feature deletes.
type closeTargets struct {
	refs  map[string]string // shadow-repo ref -> the SHA the mirror holds
	local []string          // the mirror's copies of those refs
	plans []string          // this feature's plan files on this machine
}

// sorted lists the refs to delete in a stable order.
func (t closeTargets) sorted() []string {
	out := make([]string, 0, len(t.refs))
	for ref := range t.refs {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// closeTargets collects every ref of r that exists, with the SHA the mirror
// (fetched by open, so what the survey saw) holds for it: the branch, the
// gate branch, the metadata ref and each plan ref of the feature. A plan
// ref counts when the metadata or a plan file of this pair names it for
// this feature, and only if the mirror has it.
func (e *Engine) closeTargets(ctx context.Context, pc *pairCtx, r FeatureReport) (closeTargets, error) {
	t := closeTargets{refs: map[string]string{}}
	// The branch name comes from metadata in the shadow repo: delete only
	// the branch feature start made, which is always named for the feature.
	if r.Branch != r.Name {
		return t, contract.New(contract.Internal,
			"feature %s's metadata names the branch %q, not %q; refusing to delete another branch", r.Name, r.Branch, r.Name).
			WithDetails(map[string]any{"feature": r.Name, "branch": r.Branch})
	}
	add := func(remote, local string) error {
		sha, ok, err := pc.git.Resolve(ctx, local)
		if err != nil {
			return internalErr(err, "read "+local)
		}
		if ok {
			t.refs[remote], t.local = sha, append(t.local, local)
		}
		return nil
	}
	if r.Head != "" {
		// The survey's own head: the lease is on what the state was worked out from.
		t.refs["refs/heads/"+r.Branch], t.local = r.Head, append(t.local, shadowRef(r.Branch))
	}
	if err := add("refs/heads/"+gateBranch(r.Name), shadowRef(gateBranch(r.Name))); err != nil {
		return t, err
	}
	if err := add(remoteMetaRef(r.Name), metaRef(r.Name)); err != nil {
		return t, err
	}
	ids := map[string]bool{}
	if plan.ValidID(r.PlanID) {
		ids[r.PlanID] = true
	}
	files, err := planFiles(e.WS.PlansDir())
	if err != nil {
		return t, internalErr(err, "list plan files")
	}
	for _, f := range files {
		// The ID names a ref, so it must be a plan ID.
		if f.Feature == r.Name && strings.EqualFold(f.Pair.Public, pc.pair.Public) && plan.ValidID(f.ID) {
			ids[f.ID] = true
			t.plans = append(t.plans, f.Path)
		}
	}
	sort.Strings(t.plans)
	for id := range ids {
		if err := add(remotePlanRef(id), planRef(id)); err != nil {
			return t, err
		}
	}
	return t, nil
}

// closeWith deletes t. The deletes go first, in one atomic push: if any ref
// moved, nothing is deleted and the gate PR stays open. Only then is the
// gate PR closed and this machine cleaned up; a local cleanup that fails is
// a MIRROR_NOT_UPDATED warning.
func (e *Engine) closeWith(ctx context.Context, pc *pairCtx, r FeatureReport, t closeTargets) (*FeatureCloseResult, []contract.Warning, error) {
	sh := pc.pair.Shadow
	res := &FeatureCloseResult{Feature: r.Name, State: r.State, Deleted: map[string]string{}}
	pr, err := e.Forge.FindOpenPR(ctx, sh, gateBranch(r.Name), "main")
	if err != nil {
		return nil, nil, forgeErr(err, "find the gate PR of feature %s", r.Name)
	}
	refs := t.sorted()
	if e.DryRun {
		fp := map[string]string{}
		if r.Head != "" {
			fp["feature_head"] = r.Head
		}
		res.Preview = preview.New(fp)
		for _, ref := range refs {
			res.Preview.Add(preview.DeleteRef(sh, ref, t.refs[ref]))
		}
		if pr != nil {
			res.Preview.Add(preview.ClosePR(sh, pr.Number))
		}
		for _, path := range t.plans {
			res.Preview.Add(preview.RemoveFile(path))
		}
		return res, nil, nil
	}

	opt := gitx.PushOptions{Atomic: true}
	specs := make([]string, 0, len(refs))
	for _, ref := range refs {
		opt.Leases = append(opt.Leases, gitx.Lease{Ref: ref, Expect: t.refs[ref]})
		specs = append(specs, ":"+ref)
	}
	if len(specs) > 0 {
		e.progressf("deleting %d ref(s) of feature %s from %s", len(specs), r.Name, sh)
		if err := e.push(ctx, pc, remoteShadow, opt, specs...); err != nil {
			return nil, nil, movedErr(r.Name, sh, err)
		}
	}
	for _, ref := range refs {
		res.Deleted[ref] = t.refs[ref]
	}
	// The remote is the truth: a mirror ref left behind goes on the next
	// fetch (it prunes), and a plan file left behind names a plan ref that
	// no longer exists, so apply refuses it. Either is a warning.
	var warns []contract.Warning
	for _, local := range t.local {
		warns = addWarnings(warns, e.deleteMirror(ctx, pc.git, local)...)
	}
	for _, path := range t.plans {
		warns = addWarnings(warns, removeLocal(path)...)
	}
	if pr != nil {
		// GitHub may already have closed it with its head branch; closing a
		// closed PR is not an error.
		err := e.Forge.ClosePullRequest(ctx, sh, pr.Number)
		switch {
		case err == nil:
			n := pr.Number
			res.GatePR = &n
		case errors.Is(err, forge.ErrNotFound):
		case errors.Is(err, forge.ErrForbidden):
			return res, warns, afterDeletes(forgeErr(err, "deleted %d ref(s) of feature %s, but the token may not close its gate PR #%d in %s; close it by hand",
				len(res.Deleted), r.Name, pr.Number, sh))
		default:
			return res, warns, afterDeletes(forgeErr(err, "deleted %d ref(s) of feature %s, but couldn't close its gate PR #%d in %s; close it by hand",
				len(res.Deleted), r.Name, pr.Number, sh))
		}
	}
	return res, warns, nil
}

// afterDeletes makes an error that follows the deletes never retryable:
// the feature is gone, so a retry only finds FEATURE_NOT_FOUND.
func afterDeletes(err error) error {
	ce := contract.From(err)
	ce.Retryable = false
	return ce
}

// movedErr turns a lost lease on the delete push into FEATURE_MOVED. Other
// push errors pass through: the push is atomic, so none deleted anything.
func movedErr(name, shadow string, err error) error {
	if contract.From(err).Code != contract.RefMoved {
		return err
	}
	details := map[string]any{"feature": name}
	ref := ""
	var rej *gitx.RejectedError
	if errors.As(err, &rej) && len(rej.Refs) > 0 {
		ref = rej.Refs[0]
		if _, dst, ok := strings.Cut(ref, ":"); ok {
			ref = dst
		}
		details["ref"] = ref
	}
	what := "a ref of feature " + name
	if ref != "" {
		what = ref
	}
	ce := contract.New(contract.FeatureMoved,
		"%s changed in %s while feature %s was being closed, so nothing was deleted; look again with ghostbranch feature show %s",
		what, shadow, name, name).WithField("feature").WithDetails(details)
	ce.Err = err
	return ce
}

// closeSummary is what the person sees before typing the code.
func closeSummary(pc *pairCtx, r FeatureReport, t closeTargets) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Close feature %s (state %s): delete these refs from %s:\n", visible(r.Name), r.State, visible(pc.pair.Shadow))
	refs := t.sorted()
	for _, ref := range refs {
		fmt.Fprintf(&b, "  %s at %s\n", visible(ref), short(t.refs[ref]))
	}
	b.WriteString("and close its gate draft PR, if one is open.\n")
	var branches []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			branches = append(branches, ref)
		}
	}
	if len(branches) > 0 {
		b.WriteString("Recover a branch from a clone that still has its commits (your working clone, for example) with:\n")
		for _, ref := range branches {
			fmt.Fprintf(&b, "  git push %s %s:%s\n", visible(pc.shadow.CloneURL), t.refs[ref], visible(ref))
		}
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// closeLedgerSummary is the ledger's line for a close.
func closeLedgerSummary(res *FeatureCloseResult) string {
	s := fmt.Sprintf("closed %s (%s): deleted %d ref(s)", res.Feature, res.State, len(res.Deleted))
	if res.GatePR != nil {
		s += fmt.Sprintf("; closed gate PR #%d", *res.GatePR)
	}
	return s
}
