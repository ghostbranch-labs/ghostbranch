package engine

// Updating a stack's open PRs. promote a, with open PRs above a, plans a
// and every open PR above it together, under one approval (openAbove, in
// update.go). Every branch with an open PR below the top of what changes is
// replaced: the bottom because open PRs sit above it (its gate built a
// restack), each branch above as its commits move onto the new commits
// below it. Only a bottom on main with no open PR above it appends, as its
// gate built it; the branches above it then publish for the first time.
// Apply pushes bottom up, each update leased on the PR head the plan saw.

import (
	"context"
	"slices"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

// levelUpdate is what resolveStack needs of a branch whose open PR the plan
// updates: the PR head the plan saw, and whether the plan appends to it
// (only a bottom on main whose gate built an append can).
type levelUpdate struct {
	head   string
	append bool
}

// targetBranch is the public branch m publishes to: its PR's branch once it
// is published, else its name.
func targetBranch(m feature.Meta) string {
	if m.Promoted != nil {
		return publishedBranch(m)
	}
	return m.Name
}

// stackUpdates is, for each branch of the plan, how the plan updates its
// open PR, read now (prHead); nil for a branch published for the first time
// (or whose PR is no longer open: apply then refuses as for any first
// publish to an existing branch). It works in two passes:
//  1. Every branch's PR is read, bottom up, and a PR that holds commits
//     Ghostbranch didn't publish, or an update of it never recorded, stops
//     the plan (prChangedErr, naming the branch) before anything else is
//     checked: they are never overwritten.
//  2. Each update gets its style:
//     - The bottom on main updates as its gate built it (Gate.Update), which
//     must fit this PR, mode and the open PRs above it now (gateFitsUpdate),
//     else stale_gate (updateGateErr). With open PRs above, that is a
//     replace; its reason is what forces it now (restack, or squash or
//     curated in those modes), else the gate's (planReason).
//     - A stacked branch keeps its first-publish gate, so it is replaced.
//     Above the bottom the reason is restack, as the branch below gets new
//     commits in this plan. A stacked bottom (on a published parent) gets
//     squash in squash mode, else rebase.
func (e *Engine) stackUpdates(ctx context.Context, pc *pairCtx, names, modes []string, defaultMode, pm, O string) ([]*prUpdate, error) {
	out := make([]*prUpdate, len(names))
	metas := make([]feature.Meta, len(names))
	for i, n := range names {
		m, _, err := e.readMeta(ctx, pc, n)
		if err != nil {
			return nil, err
		}
		u, err := e.prHead(ctx, pc, &m)
		if err != nil {
			return nil, err
		}
		if u != nil && u.changed() {
			return nil, prChangedErr(n, u)
		}
		metas[i], out[i] = m, u
	}
	for i, n := range names {
		m, u := metas[i], out[i]
		if i == 0 && !m.Stacked() {
			// The bottom on main: its gate built the update (or none), for the
			// open PRs above it as the gate saw them.
			if m.Gate == nil {
				continue // resolveStack reports the missing gate
			}
			above := false
			if u != nil {
				var err error
				if above, err = e.hasOpenAbove(ctx, pc, n); err != nil {
					return nil, err
				}
			}
			if !gateFitsUpdate(m.Gate, u, m, modes[i], above) {
				// A gate built on another feature head, public main or overlay
				// is stale whatever it built: gating again builds anew.
				head, _, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
				if err != nil {
					return nil, internalErr(err, "read feature branch "+m.Branch)
				}
				ce := updateGateErr(n, names, m, u, modes[i], defaultMode, above, m.Gate.FreshFor(head, pm, O))
				if slices.ContainsFunc(out, func(u *prUpdate) bool { return u != nil }) {
					ce.Details["updating"] = true // the new plan updates the open PRs (next says so)
				}
				return nil, ce
			}
			if u != nil {
				u.Style = m.Gate.Update.Style
				u.Reason = planReason(u.Style, m.Gate.Update.Reason, forcedReason(u, &m, modes[i], above))
			}
			continue
		}
		if u == nil {
			continue
		}
		switch {
		case i > 0:
			u.Style, u.Reason = plan.StyleReplace, plan.ReasonRestack
		case modes[i] == config.ModeSquash:
			u.Style, u.Reason = plan.StyleReplace, plan.ReasonSquash
		default:
			u.Style, u.Reason = plan.StyleReplace, plan.ReasonRebase
		}
	}
	return out, nil
}

// updateGateErr is NOT_A_STACK stale_gate for the bottom on main, name,
// whose gate doesn't fit the update of its open PR u in mode now
// (gateFitsUpdate). Gating again can't help only when the gate is fresh
// (built on the feature head, public main and overlay there are now), was
// built in promote.default_mode (def), which a standalone gate always
// builds in, mode is another, and the gate would fit in def: gating again
// would build the same update. Then the refusal says so, with
// details.update_mode, and the way out is to promote name on its own,
// which gates it in mode. In every other case (name, public main or the
// overlay moved since the gate; the PR's head or base, or the PRs open
// above it, changed; or the gate was built in another mode by an earlier
// promote) gating again builds in def what fits.
func updateGateErr(name string, names []string, m feature.Meta, u *prUpdate, mode, def string, above, fresh bool) *contract.Error {
	gt := m.Gate
	if u == nil {
		return staleGate(name, names)
	}
	if !fresh || gt.Update == nil || gt.ScannedAs != def || mode == def || !gateFitsUpdate(gt, u, m, def, above) {
		return notAStack("stale_gate", name, names,
			"%s's gate wasn't built for the update its public PR #%d needs now (%s, public main, the PR's head or base, the PRs open above it, or the history mode changed since %s was gated): gate %s, then the branches above it, then promote again",
			name, u.PR, name, name, name)
	}
	built := "an append to"
	if gt.Update.Style == plan.StyleReplace {
		built = "a replace of"
	}
	ce := notAStack("stale_gate", name, names,
		"%s's gate built %s its public PR #%d in %s mode (promote.default_mode, the mode gate builds in), which a %s update can't use, so gating %s again won't change it. Promote %s on its own first (ghostbranch promote %s --mode %s --plan gates it in %s mode and plans its update), then promote the branches above it",
		name, built, u.PR, def, mode, name, name, name, mode, mode)
	ce.Details["update_mode"] = mode
	return ce
}

// restackedAbove refuses an update plan in which a branch above the bottom
// wasn't restacked onto the branch below it after that branch moved
// (parent_stale). Its gate could still pass, as it replays the branches
// below, but its own branch would stay on the old commits. A branch listed
// out of order is left to resolveStack, which names why.
func (e *Engine) restackedAbove(ctx context.Context, pc *pairCtx, names []string, updates []*prUpdate) error {
	if !slices.ContainsFunc(updates, func(u *prUpdate) bool { return u != nil }) {
		return nil
	}
	for i := 1; i < len(names); i++ {
		m, _, err := e.readMeta(ctx, pc, names[i])
		if err != nil {
			return err
		}
		p := stackParent(m)
		if p != names[i-1] {
			continue
		}
		st, err := e.stackState(ctx, pc, names[i], m)
		if err != nil {
			return err
		}
		if st.parentMoved {
			ce := notAStack("parent_stale", names[i], names,
				"%s's parent %s moved since %s was started or restacked on it: run ghostbranch feature restack %s, gate the branches above it from the bottom, then promote again",
				names[i], p, names[i], p)
			ce.Details["parent"] = p
			return ce
		}
	}
	return nil
}

// levelUpdates is resolveStack's view of a plan's updates, by feature.
func levelUpdates(names []string, updates []*prUpdate) map[string]levelUpdate {
	out := map[string]levelUpdate{}
	for i, u := range updates {
		if u != nil {
			out[names[i]] = levelUpdate{head: u.Head, append: i == 0 && u.Style == plan.StyleAppend}
		}
	}
	return out
}

// planUpdates is levelUpdates read back from a saved stack plan.
func planUpdates(pl plan.Plan) map[string]levelUpdate {
	out := map[string]levelUpdate{}
	for i, en := range pl.Chain {
		if en.Publish.Update {
			out[en.Feature] = levelUpdate{head: en.Publish.ExpectedHead, append: i == 0 && en.Publish.Style == plan.StyleAppend}
		}
	}
	return out
}

// levelPlan is branch i of stack plan pl as the plan of one feature
// (entryPlan), with only that branch's PR in PRs, so the update helpers of
// one feature (updateStands, updateLive, pushUpdate, recordUpdate) act on it.
func levelPlan(pl plan.Plan, i int) plan.Plan {
	ep := entryPlan(pl, i)
	ep.PRs = []plan.StackPR{pl.PRs[i]}
	return ep
}

// stackLevelErr names the whole stack in a PLAN_STALE from one level's
// update helpers, so a re-plan names every branch.
func stackLevelErr(err error, names []string) error {
	if ce := contract.From(err); ce != nil && ce.Code == contract.PlanStale {
		ce.Details["features"] = names
	}
	return err
}

// previewStackPublish adds what publishing entry en still needs. For an
// update, that is the push leased on the PR head the plan saw
// (previewUpdate), then the edit of PR n's typed texts; the PR is already
// open. Anything else goes through previewPublish.
func (e *Engine) previewStackPublish(ctx context.Context, pc *pairCtx, pv *preview.Preview, en plan.Entry, m feature.Meta, base string, n int) error {
	pub := en.Publish
	if !pub.Update {
		return e.previewPublish(ctx, pc, pv, en.Feature, publishedPlan(m), pub.TargetBranch, base, pub.Commit)
	}
	e.previewUpdate(ctx, pc, pv, pub.TargetBranch, pub.ExpectedHead, pub.Commit, n, en.Typed)
	return nil
}

// updateLevelStands checks a stack plan's update of branch i (b) against
// what the shadow repo holds, as updateStands does for one feature: the
// plan file is local and unsigned. The update must name the feature's own
// PR on its published branch. A bottom on main must be the very update its
// gate built (updateStands); any other branch is a replace (its gate is a
// first publish's).
func updateLevelStands(pl plan.Plan, i int, b *stackBranch) error {
	en, pr := pl.Chain[i], pl.PRs[i]
	lp := levelPlan(pl, i)
	if i == 0 && !b.meta.Stacked() {
		return updateStands(b.meta, lp)
	}
	m := b.meta
	if m.Promoted != nil && m.Promoted.PublicPR == pr.Number && pr.Update && pr.Style == en.Publish.Style &&
		en.Publish.Style == plan.StyleReplace && publishedBranch(m) == en.Publish.TargetBranch {
		return nil
	}
	return contract.New(contract.PlanStale,
		"plan %s's update of %s doesn't match its record; make a new plan with ghostbranch promote", pl.ID, en.Feature).
		WithDetails(stale(lp, []string{"prs"}))
}

// openOutside refuses a plan that would move a branch from under an open PR
// it doesn't update: a published level below the top (names[i]) with open
// PRs stacked on it other than names[i+1], the level the plan puts on it.
// Those PRs would stay on the branch's old commits. NOT_A_STACK open_above
// names them in details.children. The top's open PRs are openAbove's.
func (e *Engine) openOutside(ctx context.Context, pc *pairCtx, names []string) error {
	if len(names) < 2 {
		return nil
	}
	recs, err := e.records(ctx, pc)
	if err != nil {
		return err
	}
	for i, n := range names[:len(names)-1] {
		if r, ok := recs[n]; !ok || r.meta.Promoted == nil {
			continue // never published: nothing is published on it
		}
		open, err := e.openChildren(ctx, pc, recs, n, map[string]bool{names[i+1]: true})
		if err != nil {
			return err
		}
		if len(open) > 0 {
			return openAboveErr(n, open, names)
		}
	}
	return nil
}
