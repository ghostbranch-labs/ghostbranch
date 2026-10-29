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
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
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
	Feature      string            `json:"feature"`
	State        feature.State     `json:"state"`         // the state it was closed from
	Deleted      map[string]string `json:"deleted"`       // shadow ref -> SHA it pointed at; never null
	GatePR       *int              `json:"gate_pr"`       // the draft PR closed (by us, or by GitHub as its branch went), nil when none was open
	NeedsConsole bool              `json:"needs_console"` // closing takes a person at a console (not ClosableWithoutConsole); set under --plan too
	Approval     *ledger.Approval  `json:"approval"`      // console; nil under --plan, and when none was needed
	// Reparented are the children moved onto main in the push that closed
	// this feature; never null.
	Reparented []string         `json:"reparented"`
	Preview    *preview.Preview `json:"preview"`
	// GatePRLeftOpen is the gate PR the close couldn't close, 0 when none;
	// for human output only.
	GatePRLeftOpen int `json:"-"`
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
	if err := feature.ValidateName(name, "feature", "main"); err != nil {
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
	// Parking a feature with live children would leave them stacked on a
	// feature nobody works on: refuse, before --plan too.
	if !undo && !m.Abandoned {
		recs, err := e.records(ctx, pc)
		if err != nil {
			return nil, warns, err
		}
		if kids := childrenOf(recs, name, false); len(kids) > 0 {
			return nil, warns, hasChildren(name, kids, "abandon")
		}
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
	_, mw, err := e.updateMeta(ctx, pc, m, metaSHA)
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
// and changes nothing. A feature whose record can't be read (corrupt, or
// from a newer Ghostbranch) closes too, but only at a console.
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
	if err := feature.ValidateName(name, "feature", "main"); err != nil {
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
	if r.err != nil {
		var bad *unreadableRecord
		if !errors.As(r.err, &bad) {
			return nil, warns, r.err
		}
		// The record doesn't parse: close what the refs say, at the console.
		// The branch is read again, so the delete is leased on a head that was read.
		if r, err = e.unreadableReport(ctx, pc, name); err != nil {
			return nil, warns, err
		}
	}
	// Closing deletes the branch the features stacked on it are built on,
	// abandoned ones too (Review Focus 2). Another feature whose record
	// can't be read may be one of them, so that refuses too.
	recs, err := e.readableRecords(ctx, pc, name, "close")
	if err != nil {
		return nil, warns, err
	}
	if kids := childrenOf(recs, name, true); len(kids) > 0 {
		return nil, warns, hasChildren(name, kids, "close")
	}
	t, tw, err := e.closeTargets(ctx, pc, r)
	warns = addWarnings(warns, tw...)
	if err != nil {
		return nil, warns, err
	}
	// The console comes before anything changes.
	if !e.DryRun && !ClosableWithoutConsole(r) {
		if e.Approver == nil {
			why := "only a merged feature with no newer commits closes unattended"
			if r.unreadable != "" {
				why = "its record couldn't be read as a whole: " + unreadableWhy(r)
			}
			return nil, warns, noConsole("closing feature " + name + " (" + string(r.State) + "; " + why + ")")
		}
		if err := e.confirm(ctx, "", closeSummary(pc, r, t)); err != nil {
			return nil, warns, err
		}
		appr = &ledger.Approval{Kind: "console"}
	}
	res, cw, err := e.closeWith(ctx, pc, r, t, closeOpts{})
	warns = addWarnings(warns, cw...)
	if res != nil {
		res.Approval = appr
	}
	if res != nil && res.Preview == nil && r.unreadable != "" {
		warns = addWarnings(warns, contract.Warning{Code: contract.RecordUnreadable,
			Message: fmt.Sprintf("feature %s's record couldn't be read as a whole (%s); its refs were deleted at the console, and the ledger lists them with their SHAs",
				name, unreadableWhy(r)),
			Details: map[string]any{"feature": name}})
	}
	return res, warns, err
}

// unreadableReport stands in for the survey of a feature whose record
// doesn't parse: its branch is the feature's name (feature start's only
// choice), and its head is whatever the mirror has.
func (e *Engine) unreadableReport(ctx context.Context, pc *pairCtx, name string) (FeatureReport, error) {
	r := FeatureReport{Name: name, Branch: name, State: feature.StateUnreadable, unreadable: unreadableCorrupt,
		Drift: Drift{Status: driftUnknown, Files: []string{}}, Children: []string{}}
	head, ok, err := pc.git.Resolve(ctx, shadowRef(name))
	if err != nil {
		return r, internalErr(err, "read feature branch "+name)
	}
	if ok {
		r.Head = head
	}
	return r, nil
}

// failedReport stands in for a feature the survey couldn't read: its record
// is corrupt, or reading it or its refs failed. It is unreadable, so it never
// closes unattended, and it carries the error. Its head is the mirror's
// branch named for the feature, "" when that can't be read either.
func (e *Engine) failedReport(ctx context.Context, pc *pairCtx, name string, cause error) FeatureReport {
	r, _ := e.unreadableReport(ctx, pc, name)
	var bad *unreadableRecord
	if !errors.As(cause, &bad) {
		r.unreadable = unreadableError
	}
	r.Error, r.err = contract.From(cause).Message, cause
	return r
}

// unreadableWhy says why r's record couldn't be read as a whole.
func unreadableWhy(r FeatureReport) string {
	if r.unreadable == unreadableNewer {
		return "it was written by a newer Ghostbranch"
	}
	return "it is corrupt"
}

// closeFeature deletes one surveyed feature. It asks no console: sync's
// auto-close calls it only for ClosableWithoutConsole reports. kids are
// the features stacked on it: their records are rewritten onto main
// (reparentChildren) and pushed in the same atomic push as the deletes, so
// the close and the re-parenting land together or not at all. closing names
// the features this same sync closes: a child among them isn't said to
// move onto main, as it is about to close too. The caller
// records the ledger entry, with Refs = res.Deleted. A non-nil res with an
// error means the refs were deleted (and the children moved) but the gate
// PR couldn't be closed. The warnings are PLAN_FILE_UNREADABLE from listing
// the plan files and MIRROR_NOT_UPDATED from the local cleanup.
func (e *Engine) closeFeature(ctx context.Context, pc *pairCtx, r FeatureReport, kids []FeatureReport, closing map[string]bool) (*FeatureCloseResult, []contract.Warning, error) {
	t, tw, err := e.closeTargets(ctx, pc, r)
	if err != nil {
		return nil, tw, err
	}
	rps, err := e.reparentChildren(ctx, pc, r, kids)
	if err != nil {
		return nil, tw, err
	}
	var o closeOpts
	names := make([]string, 0, len(rps))
	var staying []string
	for _, rp := range rps {
		o.also = append(o.also, refUpdate{Ref: remoteMetaRef(rp.name), From: rp.old, To: rp.new, Local: metaRef(rp.name)})
		names = append(names, rp.name)
		if !closing[rp.name] {
			staying = append(staying, rp.name)
		}
	}
	if len(staying) > 0 && !e.DryRun {
		e.notef(progress.Info, "Moving %s onto main in the same push that closes %s", strings.Join(staying, ", "), r.Name)
	}
	res, cw, err := e.closeWith(ctx, pc, r, t, o)
	// The push landed once res is non-nil outside a dry run, even when the
	// gate PR then wouldn't close.
	if res != nil && res.Preview == nil {
		res.Reparented = append(res.Reparented, names...)
	}
	return res, addWarnings(tw, cw...), err
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
func (e *Engine) closeTargets(ctx context.Context, pc *pairCtx, r FeatureReport) (closeTargets, []contract.Warning, error) {
	t := closeTargets{refs: map[string]string{}}
	// The branch name comes from metadata in the shadow repo: delete only
	// the branch feature start made, which is always named for the feature.
	if r.Branch != r.Name {
		return t, nil, contract.New(contract.Internal,
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
		return t, nil, err
	}
	if err := add(remoteMetaRef(r.Name), metaRef(r.Name)); err != nil {
		return t, nil, err
	}
	// The archive ref goes whenever it exists, also for an archive that
	// stopped before its record was marked.
	if err := add(remoteArchiveRef(r.Name), archiveRef(r.Name)); err != nil {
		return t, nil, err
	}
	ids := map[string]bool{}
	if plan.ValidID(r.PlanID) {
		ids[r.PlanID] = true
	}
	files, unreadable, err := planFiles(e.WS.PlansDir())
	if err != nil {
		return t, nil, internalErr(err, "list plan files")
	}
	var warns []contract.Warning
	for _, u := range unreadable {
		warns = append(warns, planFileUnreadable(u, fmt.Sprintf(
			"if it is a plan of feature %s, its plan ref stays in %s and the file stays on this machine", r.Name, pc.pair.Shadow)))
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
			return t, warns, err
		}
	}
	return t, warns, nil
}

// refUpdate is one more ref a close's atomic push moves: To is pushed to
// Ref, leased on From ("" means the ref must not exist). Local is the
// mirror's copy, moved to To after the push. To is "" under --plan, where
// a record is not written.
type refUpdate struct{ Ref, From, To, Local string }

// closeOpts are what fold and sync add to a close. verb words the
// moved-ref error ("closed" when empty); also are refs updated in the same
// atomic push as the deletes, so the close and they land together or not
// at all.
type closeOpts struct {
	verb string
	also []refUpdate
}

// closeWith deletes t, and moves o.also, in one atomic push first: if any
// ref moved, nothing is deleted or moved and the gate PR stays open. Only
// then is the gate PR closed and this machine cleaned up; a local cleanup
// that fails is a MIRROR_NOT_UPDATED warning.
func (e *Engine) closeWith(ctx context.Context, pc *pairCtx, r FeatureReport, t closeTargets, o closeOpts) (*FeatureCloseResult, []contract.Warning, error) {
	sh := pc.pair.Shadow
	res := &FeatureCloseResult{Feature: r.Name, State: r.State, Deleted: map[string]string{}, NeedsConsole: !ClosableWithoutConsole(r),
		Reparented: []string{}}
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
		for _, u := range o.also {
			res.Preview.Add(preview.PushRef(sh, u.Ref, u.From, u.To))
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
	specs := make([]string, 0, len(refs)+len(o.also))
	for _, ref := range refs {
		opt.Leases = append(opt.Leases, gitx.Lease{Ref: ref, Expect: t.refs[ref]})
		specs = append(specs, ":"+ref)
	}
	for _, u := range o.also {
		opt.Leases = append(opt.Leases, gitx.Lease{Ref: u.Ref, Expect: u.From})
		specs = append(specs, u.To+":"+u.Ref)
	}
	verb := o.verb
	if verb == "" {
		verb = "closed"
	}
	if len(specs) > 0 {
		st := e.startStep("Deleting %s's branches and records from the shadow repo", r.Name)
		if err := e.push(ctx, pc, remoteShadow, opt, specs...); err != nil {
			// Sync's auto-close goes on to the next feature, so end the step
			// here: the next step would otherwise mark it done.
			st.Fail()
			return nil, nil, movedErr(r.Name, sh, verb, err)
		}
		st.Done(fmt.Sprintf("Deleted %d ref(s) of %s from the shadow repo", len(refs), r.Name))
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
	for _, u := range o.also {
		warns = addWarnings(warns, e.updateMirror(ctx, pc.git, u.Local, u.To)...)
	}
	for _, path := range t.plans {
		warns = addWarnings(warns, removeLocal(path)...)
	}
	if pr != nil {
		// GitHub may already have closed it with its head branch; closing a
		// closed PR is not an error. GitHub closing it while ours is in
		// flight can still fail ours, so on any other error look again: a
		// PR that is closed now is done.
		err := e.Forge.ClosePullRequest(ctx, sh, pr.Number)
		if err != nil && !errors.Is(err, forge.ErrNotFound) && !errors.Is(err, forge.ErrForbidden) && e.prClosed(ctx, sh, pr.Number) {
			err = nil
		}
		switch {
		case err == nil:
			n := pr.Number
			res.GatePR = &n
		case errors.Is(err, forge.ErrNotFound):
		case errors.Is(err, forge.ErrForbidden):
			res.GatePRLeftOpen = pr.Number
			// This code is chosen on purpose: the refs are gone, so a person
			// closes the PR by hand. The cause goes into the message, not the
			// chain, so the CLI never turns it into TOKEN_PERMISSION_MISSING
			// (whose "run this command again" would find no feature). A named
			// permission still reaches the caller in details.
			ce := contract.New(contract.ForgeError, "deleted %d ref(s) of feature %s, but the token may not close its gate PR #%d in %s; close it by hand: %v",
				len(res.Deleted), r.Name, pr.Number, sh, err)
			var pe *forge.PermissionError
			if errors.As(err, &pe) {
				ce = ce.WithDetails(map[string]any{"permission": pe.Permission, "repo": pe.Repo})
			}
			return res, warns, afterDeletes(ce)
		default:
			res.GatePRLeftOpen = pr.Number
			return res, warns, afterDeletes(forgeErr(err, "deleted %d ref(s) of feature %s, but couldn't close its gate PR #%d in %s; close it by hand",
				len(res.Deleted), r.Name, pr.Number, sh))
		}
	}
	return res, warns, nil
}

// prClosed reports whether PR number in full is closed now. An error is
// not closed.
func (e *Engine) prClosed(ctx context.Context, full string, number int) bool {
	got, err := e.Forge.GetPR(ctx, full, number)
	return err == nil && got.State == "closed"
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
// verb words what was happening to the feature ("closed", "folded into x").
func movedErr(name, shadow, verb string, err error) error {
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
		"%s changed in %s while feature %s was being %s, so nothing was deleted; look again with ghostbranch feature show %s",
		what, shadow, name, verb, name).WithField("feature").WithDetails(details)
	ce.Err = err
	return ce
}

// closeSummary is what the person sees before typing the code.
func closeSummary(pc *pairCtx, r FeatureReport, t closeTargets) string {
	var b strings.Builder
	switch r.unreadable {
	case unreadableCorrupt:
		fmt.Fprintf(&b, "Feature %s's record couldn't be read, so its state is unknown; its refs are found by name.\n", visible(r.Name))
	case unreadableNewer:
		fmt.Fprintf(&b, "Feature %s's record was written by a newer Ghostbranch; its state is what this binary could read of it.\n", visible(r.Name))
	}
	fmt.Fprintf(&b, "Close feature %s (state %s): delete these refs from %s:\n", visible(r.Name), r.State, visible(pc.pair.Shadow))
	refs := t.sorted()
	for _, ref := range refs {
		fmt.Fprintf(&b, "  %s at %s\n", visible(ref), short(t.refs[ref]))
	}
	b.WriteString("and close its gate draft PR, if one is open.\n")
	var branches []string
	for _, ref := range refs {
		if strings.HasPrefix(ref, "refs/heads/") || strings.HasPrefix(ref, feature.ArchiveRefPrefix) {
			branches = append(branches, ref)
		}
	}
	if len(branches) > 0 {
		b.WriteString("Recover a branch from a clone that still has its commits (your working clone, for example) with:\n")
		for _, ref := range branches {
			to := ref
			if name, ok := strings.CutPrefix(ref, feature.ArchiveRefPrefix); ok {
				to = "refs/heads/" + name
			}
			fmt.Fprintf(&b, "  git push %s %s:%s\n", visible(pc.shadow.CloneURL), t.refs[ref], visible(to))
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
	if len(res.Reparented) > 0 {
		s += fmt.Sprintf("; moved %s onto main", strings.Join(res.Reparented, ", "))
	}
	return s
}
