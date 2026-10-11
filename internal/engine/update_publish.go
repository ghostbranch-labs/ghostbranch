package engine

// Promoting and applying the update of an open PR: the checks an update
// plan must pass, the leased push, and the record it leaves.

import (
	"context"
	"fmt"
	"slices"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

// updateFlags refuses what an update can't do: it pushes to the branch the
// PR is on, so --branch may only name that branch. A typed --title or
// --body is fine: apply edits the PR's text once the push lands.
func updateFlags(opt PromoteOptions, u *prUpdate) error {
	if opt.Branch != "" && opt.Branch != u.Branch {
		return contract.New(contract.BadFlag, "--branch %s: feature %s's PR #%d is open on branch %s, which this promote updates; drop --branch",
			opt.Branch, opt.Feature, u.PR, u.Branch).WithField("branch")
	}
	return nil
}

// updateStands checks an update plan against the feature's gate record,
// which lives in the shadow repo and so is trusted where the local plan
// file isn't. The gate must have built this very update (the same PR,
// base, head H, style and reason), and the plan must push to the branch
// the feature was published to. A first-publish plan stands only on a
// gate that built no update.
func updateStands(m feature.Meta, pl plan.Plan) error {
	var gu *feature.GateUpdate
	if m.Gate != nil {
		gu = m.Gate.Update
	}
	ok := !pl.Publish.Update && gu == nil
	if pl.Publish.Update && gu != nil && len(pl.PRs) == 1 && m.Promoted != nil {
		p := pl.PRs[0]
		ok = gu.PR == p.Number && gu.Base == p.Base && gu.Head == pl.Publish.ExpectedHead && gu.Style == pl.Publish.Style &&
			gu.Reason == pl.Publish.ReplaceReason && p.Update && p.Style == gu.Style &&
			m.Promoted.PublicPR == gu.PR && publishedBranch(m) == pl.Publish.TargetBranch &&
			(gu.Style == plan.StyleReplace || len(pl.Publish.Restores) == 0)
	}
	if ok {
		return nil
	}
	return contract.New(contract.PlanStale,
		"plan %s's publish doesn't match the update the recorded gate for %q built; run ghostbranch gate, then make a new plan", pl.ID, pl.Feature).
		WithDetails(stale(pl, []string{"gate"}))
}

// updateLive checks the open PR an update plan expects, before anything is
// asked or pushed. The PR's branch must be at the head the plan saw (H) or
// already at P; at P the push landed and apply only finishes the record
// (landed). Otherwise the PR must still be open on the same base, its
// branch must hold no commit Ghostbranch didn't publish, and it must lack
// the same published commits the plan puts back. A branch that moved is
// PLAN_STALE moved public_branch; a PR closed, merged or retargeted is
// moved public_pr.
func (e *Engine) updateLive(ctx context.Context, pc *pairCtx, pl plan.Plan, m feature.Meta, existing string, exists bool, P string) (bool, error) {
	branch, n, H := pl.Publish.TargetBranch, pl.PRs[0].Number, pl.Publish.ExpectedHead
	if exists && existing == P {
		return true, nil
	}
	branchMoved := func(at string) error {
		return contract.New(contract.PlanStale,
			"public PR #%d's branch %s %s, not at %s as plan %s expects: it changed on GitHub. Nothing was pushed; make a new plan with ghostbranch promote",
			n, branch, at, short(H), pl.ID).WithDetails(stale(pl, []string{"public_branch"}))
	}
	if !exists {
		return false, branchMoved("is gone")
	}
	if existing != H {
		return false, branchMoved("is at " + short(existing))
	}
	u, err := e.prHead(ctx, pc, &m)
	if err != nil {
		return false, err
	}
	if u == nil || u.PR != n || u.Base != pl.PRs[0].Base {
		return false, contract.New(contract.PlanStale,
			"public PR #%d is no longer open on %s as plan %s expects; nothing was pushed. Make a new plan with ghostbranch promote", n, pl.PRs[0].Base, pl.ID).
			WithDetails(stale(pl, []string{"public_pr"}))
	}
	if u.Head != H || u.changed() || !slices.Equal(u.Restores, pl.Publish.Restores) {
		return false, branchMoved("holds other commits")
	}
	return false, nil
}

// pushUpdate pushes P to the open PR's branch under a lease on the head the
// plan saw (--force-with-lease=refs/heads/<branch>:<H>). An append is a
// fast-forward of H and a replace rewrites it. In both, a branch that moved
// since is left as it is: PLAN_STALE.
func (e *Engine) pushUpdate(ctx context.Context, pc *pairCtx, pl plan.Plan, P string) error {
	branch, H, n := pl.Publish.TargetBranch, pl.Publish.ExpectedHead, pl.PRs[0].Number
	e.progressf("updating public PR #%d (%s): %s to %s:%s, leased on %s", n, pl.Publish.Style, short(P), pc.pair.Public, branch, short(H))
	opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + branch, Expect: H}}}
	err := e.push(ctx, pc, remotePublic, opt, P+":refs/heads/"+branch)
	if err != nil && contract.From(err).Code == contract.RefMoved {
		out := contract.New(contract.PlanStale,
			"public PR #%d's branch %s moved while plan %s was being applied, so nothing was pushed; make a new plan with ghostbranch promote", n, branch, pl.ID).
			WithDetails(stale(pl, []string{"public_branch"}))
		out.Err = err
		return out
	}
	return err
}

// recordUpdate reads the updated PR and records the publish: the new head,
// the patch-ids of every commit now on the PR, and needs_update cleared. A
// record that already says so (a resumed apply) is not written again.
func (e *Engine) recordUpdate(ctx context.Context, pc *pairCtx, pl plan.Plan, P string) (forge.PR, []contract.Warning, error) {
	g := pc.git
	n, base := pl.PRs[0].Number, pl.PRs[0].Base
	pr, err := e.Forge.GetPR(ctx, pc.pair.Public, n)
	if err != nil {
		return forge.PR{}, nil, forgeErr(err, "read public PR #%d", n)
	}
	baseSHA, ok, err := g.Resolve(ctx, publicRef(base))
	if err != nil {
		return pr, nil, internalErr(err, "read public branch "+base)
	}
	if !ok {
		return pr, nil, contract.New(contract.Internal, "public PR #%d's base branch %s is not in %s", n, base, pc.pair.Public)
	}
	commits, err := g.CommitsBetween(ctx, baseSHA, P)
	if err != nil {
		return pr, nil, internalErr(err, "list the commits on the updated PR")
	}
	ids, err := g.PatchIDs(ctx, commits)
	if err != nil {
		return pr, nil, internalErr(err, "compute the updated PR's patch-ids")
	}
	// The commits feature pull adopted stay recorded, so a later replace
	// still can't drop them unseen. Those the updated PR no longer carries
	// were folded by a squash or curated replace (approved in its summary;
	// a preserve update that would drop one is refused): they stay too,
	// marked Folded, so the next squash credits their authors again.
	m, _, err := e.readMeta(ctx, pc, pl.Feature)
	if err != nil {
		return pr, nil, err
	}
	var adopted []feature.AdoptedCommit
	if m.Promoted != nil && m.Promoted.PublicPR == n && len(m.Promoted.Adopted) > 0 {
		missing, err := missingAdopted(ctx, g, m.Promoted.Adopted, commits)
		if err != nil {
			return pr, nil, err
		}
		for _, a := range m.Promoted.Adopted {
			a.Folded = a.Folded || slices.Contains(missing, a)
			adopted = append(adopted, a)
		}
	}
	mw, err := e.recordPublished(ctx, pc, pl.Feature, feature.Promoted{PublicPR: n, PlanID: pl.ID, FeatureHead: pl.Fingerprint.FeatureHead,
		Branch: pl.Publish.TargetBranch, Base: base, PublishedHead: P, PatchIDs: ids, Adopted: adopted})
	return pr, mw, err
}

// editUpdated sets the texts the person typed for an update (pl.Typed) on
// its PR; a text not typed is left as it is on GitHub. Editing is safe to
// repeat, so a resumed apply edits again.
func (e *Engine) editUpdated(ctx context.Context, pc *pairCtx, pl plan.Plan) error {
	var title, body *string
	if pl.Typed.Title {
		title = new(pl.Publish.PR.Title)
	}
	if pl.Typed.Body {
		body = new(pl.Publish.PR.Body)
	}
	if title == nil && body == nil {
		return nil
	}
	n := pl.PRs[0].Number
	if err := e.Forge.EditPR(ctx, pc.pair.Public, n, title, body); err != nil {
		return forgeErr(err, "edit public PR #%d's title and body; its branch is updated, and ghostbranch apply %s edits it again", n, pl.ID)
	}
	return nil
}

// previewUpdate adds an update's public steps: the leased push of P over
// H, unless the branch is already at P, then the edit of PR n's texts the
// person typed (editUpdated), which apply makes again on a resume.
func (e *Engine) previewUpdate(ctx context.Context, pc *pairCtx, pv *preview.Preview, branch, H, P string, n int, typed plan.Typed) {
	if tip, ok, _ := pc.git.Resolve(ctx, publicRef(branch)); !ok || tip != P {
		pv.Add(preview.PushRef(pc.pair.Public, "refs/heads/"+branch, H, P))
	}
	if typed.Title || typed.Body {
		pv.Add(preview.EditPR(pc.pair.Public, n, typed.Title, typed.Body))
	}
}

// commitSources maps each public commit to the feature commits it
// publishes, for findings; nil for squash, whose findings name no commits.
func commitSources(cs []plan.Commit, mode string) map[string][]string {
	if mode == config.ModeSquash {
		return nil
	}
	out := make(map[string][]string, len(cs))
	for _, c := range cs {
		out[c.Commit] = c.Sources
	}
	return out
}

// updateHeadline is the first line of an update's approval summary: what
// happens to PR n's branch.
func updateHeadline(n int, pub plan.Publish) string {
	if pub.Style == plan.StyleAppend {
		return fmt.Sprintf("Update PR #%d (adds %s)", n, plural(len(pub.Commits), "commit"))
	}
	return fmt.Sprintf("Replace PR #%d's branch (history changed: %s)", n, visible(pub.ReplaceReason))
}

// plural is "1 commit" or "n commits".
func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// foldedAdopted is what an update plan's replace of feature name's PR n
// does to the commits feature pull adopted from GitHub (m's record): those
// its public commits (pub) don't carry (missingAdopted). In squash or
// curated mode they are folded into the new commits, which the approval
// summary lists. A preserve plan that drops any is refused, as promote
// refuses it: the plan file is local and unsigned. An append keeps the
// PR's commits, so it folds none.
func (e *Engine) foldedAdopted(ctx context.Context, pc *pairCtx, name string, m feature.Meta, mode string, pub plan.Publish, n int) ([]feature.AdoptedCommit, error) {
	if !pub.Update || pub.Style != plan.StyleReplace || m.Promoted == nil {
		return nil, nil
	}
	missing, err := missingAdopted(ctx, pc.git, m.Promoted.Adopted, commitSHAs(pub.Commits))
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	if mode == config.ModePreserve {
		return nil, adoptedMissingErr(name, pub.TargetBranch, n, pub.ExpectedHead, missing)
	}
	return missing, nil
}

// landedUpdate reports whether the update of PR pl.PRs[i] that pub plans
// already landed: its push is on the PR's branch (the branch is at the
// plan's commit, which the plan ref holds), and m's record names that PR on
// that branch. Apply then only finishes the record (finishLanded), whatever
// moved since (the feature, public main, the gate): the push was approved,
// scanned and made when it happened, and nothing new goes public.
func (e *Engine) landedUpdate(ctx context.Context, pc *pairCtx, pl plan.Plan, pub plan.Publish, m feature.Meta, i int) (bool, error) {
	if !pub.Update || i >= len(pl.PRs) || m.Promoted == nil || m.Promoted.PublicPR != pl.PRs[i].Number ||
		publishedBranch(m) != pub.TargetBranch || pub.Commit == "" {
		return false, nil
	}
	g := pc.git
	tip, ok, err := g.Resolve(ctx, publicRef(pub.TargetBranch))
	if err != nil {
		return false, internalErr(err, "read public branch "+pub.TargetBranch)
	}
	if !ok || tip != pub.Commit {
		return false, nil
	}
	// The plan file is local and unsigned: the plan ref must hold the commit.
	planTip, ok, err := g.Resolve(ctx, planRef(pl.ID))
	if err != nil {
		return false, internalErr(err, "read plan "+pl.ID+"'s commit")
	}
	if !ok {
		return false, nil
	}
	return g.IsAncestor(ctx, pub.Commit, planTip)
}

// finishLanded finishes plan pl, whose every update already landed
// (landedUpdate): it writes each PR's record and sets the PR texts the
// person typed, both safe to repeat. Those texts are scanned first, as they
// go public now. No approval is asked: the push it finishes was approved.
func (e *Engine) finishLanded(ctx context.Context, pc *pairCtx, cfg config.Config, pl plan.Plan, warns []contract.Warning) (*ApplyResult, []contract.Warning, error) {
	levels := []plan.Plan{pl}
	if len(pl.Chain) > 0 {
		levels = make([]plan.Plan, len(pl.Chain))
		for i := range pl.Chain {
			levels[i] = levelPlan(pl, i)
		}
	} else {
		levels[0].PRs = []plan.StackPR{pl.PRs[0]}
	}
	for _, lp := range levels {
		if !lp.Typed.Title && !lp.Typed.Body {
			continue
		}
		var texts []leak.Text
		if lp.Typed.Title {
			texts = append(texts, leak.Text{Where: "pr_title", Content: lp.Publish.PR.Title})
		}
		if lp.Typed.Body {
			texts = append(texts, leak.Text{Where: "pr_body", Content: lp.Publish.PR.Body})
		}
		rep, _, err := e.updateScan(ctx, pc, cfg, lp.Publish.Commit, lp.Publish.Commit, scanOpts{texts: texts})
		if err != nil {
			return nil, warns, err
		}
		blocking, _, ww := e.splitFindings(pc.pair, rep.Findings)
		warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
		if len(blocking) > 0 {
			return nil, warns, leakErr(lp.Feature, blocking)
		}
	}
	top := levels[len(levels)-1]
	res := &ApplyResult{PlanID: pl.ID, PublicBranch: top.Publish.TargetBranch, Resumed: true, Waived: []WaivedFinding{},
		PRs: append([]plan.StackPR(nil), pl.PRs...)}
	if e.DryRun {
		res.Preview = preview.New(nil)
		for _, lp := range levels {
			e.previewUpdate(ctx, pc, res.Preview, lp.Publish.TargetBranch, lp.Publish.ExpectedHead, lp.Publish.Commit, lp.PRs[0].Number, lp.Typed)
		}
		return res, warns, nil
	}
	e.progressf("plan %s's update already landed; recording it", pl.ID)
	for i, lp := range levels {
		pr, mw, err := e.recordUpdate(ctx, pc, lp, lp.Publish.Commit)
		warns = addWarnings(warns, mw...)
		if err != nil {
			return res, warns, err
		}
		res.PRs[i].Number, res.PRs[i].URL = pr.Number, pr.URL
		if err := e.editUpdated(ctx, pc, lp); err != nil {
			return res, warns, err
		}
	}
	res.PublicPR, res.PRURL = res.PRs[len(res.PRs)-1].Number, res.PRs[len(res.PRs)-1].URL
	return res, warns, nil
}
