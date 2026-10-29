package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// RestackOptions are feature restack's inputs.
type RestackOptions struct {
	Feature string
	// Onto re-parents Feature: "main", or the feature to stack it on. Empty
	// restacks the features above Feature onto their parents' tips.
	Onto string
}

// RestackedBranch is one branch a restack moved, or whose record it moved.
type RestackedBranch struct {
	Feature  string `json:"feature"`
	Parent   string `json:"parent"`    // "main", or the feature it is stacked on now
	From     string `json:"from"`      // its head before
	To       string `json:"to"`        // its head now; From when only its record moved
	ForkedAt string `json:"forked_at"` // its fork point now; "" on main
}

// RestackResult is what a restack moved, in the order it pushed.
type RestackResult struct {
	Feature   string            `json:"feature"`
	Onto      string            `json:"onto"`      // "" when it restacked the features above Feature
	Restacked []RestackedBranch `json:"restacked"` // never null
	Pending   []string          `json:"pending"`   // not reached because it stopped, the one it stopped at first; never null
	Preview   *preview.Preview  `json:"preview"`
	// StoppedOnConflict is true when the restack stopped at Pending[0] on a
	// conflict (REBASE_CONFLICT), not on Ctrl-C or a git failure.
	StoppedOnConflict bool `json:"-"`
	// gateFirst is GateFirst's answer.
	gateFirst string
}

// GateFirst is the lowest branch, from the bottom of the restacked stack
// up, whose gate isn't fresh: the first one next gates. It is "" when
// nothing was restacked.
func (r *RestackResult) GateFirst() string { return r.gateFirst }

// FeatureRestack replays, in an isolated worktree, each feature above
// opt.Feature onto its parent's tip, bottom first, or with opt.Onto moves
// opt.Feature itself onto main or another feature first. Each branch and
// its record go in one atomic push leased on what the restack read, so a
// branch someone pushed to meanwhile is left alone. The first conflict
// stops it with the git steps; branches already restacked stay restacked.
// Ghostbranch never resolves a conflict. It touches only the shadow repo.
func (e *Engine) FeatureRestack(ctx context.Context, p workspace.Pair, opt RestackOptions) (res *RestackResult, warns []contract.Warning, err error) {
	name := opt.Feature
	defer func() {
		summary := ""
		if res != nil && res.Preview == nil {
			moved := make([]string, len(res.Restacked))
			for i, b := range res.Restacked {
				moved[i] = b.Feature
			}
			summary = fmt.Sprintf("restacked %d branch(es)", len(moved))
			if len(moved) > 0 {
				summary += ": " + strings.Join(moved, ", ")
			}
			if len(res.Pending) > 0 {
				summary += "; stopped at " + strings.Join(res.Pending, ", ")
			}
		}
		e.record("feature restack", &p, name, "", summary, err)
	}()
	if err := feature.ValidateName(name, "feature", "main"); err != nil {
		return nil, nil, err
	}
	if opt.Onto != "" && opt.Onto != "main" {
		if err := feature.ValidateName(opt.Onto, "onto", "main"); err != nil {
			return nil, nil, err
		}
	}
	pc, err := e.open(ctx, "feature restack", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	_, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	recs, err := e.records(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	self, ok := recs[name]
	if !ok {
		return nil, warns, contract.New(contract.FeatureNotFound,
			"no feature %q in %s; list them with ghostbranch feature list --all", name, pc.pair.Shadow).WithField("feature")
	}
	above := descendants(recs, name)
	order := above
	if opt.Onto != "" {
		// An archived target is refused before checkOnto, which would call
		// its gone branch FEATURE_NOT_FOUND (P25).
		if t, ok := recs[opt.Onto]; ok && opt.Onto != name {
			if err := e.stackOnArchivedRefusal(ctx, pc, opt.Onto, t.meta, "onto"); err != nil {
				return nil, warns, err
			}
		}
		if err := checkOnto(recs, self, above, opt.Onto); err != nil {
			return nil, warns, err
		}
		order = append([]string{name}, above...)
	}
	// Refuse before anything moves: a record this binary can't write, or a
	// branch that is gone (its own, or one the restack would build on).
	for _, n := range append([]string{name}, order...) {
		r := recs[n]
		// name's own record is written only when --onto moves it.
		if r.newer && (n != name || opt.Onto != "") {
			ce := contract.NewerRecord("feature", r.meta.SchemaVersion).WithField("feature")
			ce.Details["feature"] = n
			return nil, warns, ce
		}
		if r.head == "" {
			return nil, warns, contract.New(contract.FeatureNotFound,
				"feature %s's branch is gone from %s, so nothing can be restacked on it; close it (ghostbranch feature close %s) or push its branch back first",
				n, pc.pair.Shadow, n).WithField("feature").WithDetails(map[string]any{"feature": n})
		}
	}
	shadowMain, err := pc.git.MustResolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow main")
	}
	res = &RestackResult{Feature: name, Onto: opt.Onto, Restacked: []RestackedBranch{}, Pending: []string{}}
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"feature_head": self.head, "shadow_main": shadowMain})
	}
	tips := map[string]string{name: self.head} // each feature's head as this restack leaves it
	for i, n := range order {
		r := recs[n]
		parent := parentFeature(r.meta)
		// What carries on after the person rebases n by hand: the same
		// restack, so every branch above name (n's siblings too) is
		// reached; without --onto once name's own move has landed.
		again := []string{"feature", "restack", name}
		reparent := opt.Onto != "" && n == name
		if reparent {
			parent = opt.Onto
			if parent == "main" {
				parent = ""
			}
			again = []string{"feature", "restack", name, "--onto", opt.Onto}
		}
		onto := shadowMain
		if parent != "" {
			if t, ok := tips[parent]; ok {
				onto = t
			} else {
				onto = recs[parent].head
			}
		}
		b, changed, rw, err := e.restackOne(ctx, pc, r, parent, onto, reparent, again, res.Preview)
		warns = addWarnings(warns, rw...)
		if err != nil {
			res.Pending = append(res.Pending, order[i:]...)
			res.StoppedOnConflict = contract.From(err).Code == contract.RebaseConflict
			return res, warns, err
		}
		tips[n] = b.To
		if changed {
			res.Restacked = append(res.Restacked, b)
		}
	}
	if res.Preview == nil && len(res.Restacked) > 0 {
		if pm, err := pc.git.MustResolve(ctx, publicRef("main")); err == nil {
			res.gateFirst = e.firstToGate(ctx, pc, res.Restacked[0].Feature, pm, O)
		}
	}
	return res, warns, nil
}

// checkOnto refuses an --onto that would loop, does nothing, or names no
// live feature.
func checkOnto(recs map[string]stackRecord, self stackRecord, above []string, onto string) error {
	bad := func(format string, args ...any) error {
		return contract.New(contract.BadFlag, format, args...).WithField("onto")
	}
	switch {
	case onto == "main" && parentFeature(self.meta) == "" && self.meta.ForkedAt == "":
		// A feature on main with a fork point (sync re-parented it when its
		// parent merged) still carries its old parent's commits: --onto main
		// drops them.
		return bad("feature %s is already on main; --onto moves a stacked feature (leave it out to restack the features above %s)", self.name, self.name)
	case onto == "main":
		return nil
	case onto == self.name:
		return bad("feature %s can't be stacked on itself", onto)
	case slices.Contains(above, onto):
		return bad("feature %s is above %s in its stack, so %s can't be stacked on it: that would make a loop", onto, self.name, self.name)
	}
	t, ok := recs[onto]
	if !ok || t.head == "" {
		return contract.New(contract.FeatureNotFound,
			"no feature %q with a branch in the shadow repo to stack %s on; list them with ghostbranch feature list --all", onto, self.name).WithField("onto")
	}
	if t.meta.Abandoned {
		// As feature start --from: nothing is built on a parked feature.
		return bad("feature %q is abandoned, so nothing can be stacked on it; move %s onto main or another feature", onto, self.name)
	}
	return nil
}

// restackOne moves one branch onto onto, the tip of parent ("" for main),
// and records it, in one atomic push leased on the head and the record
// the restack read. With reparent the branch's own commits are always
// replayed, as it changes parent. Otherwise a branch that already holds
// onto (it was rebased by hand) has only its record moved. Under --plan
// (pv non-nil) it replays but only previews the push. It returns the
// branch as it is now and whether anything changed.
func (e *Engine) restackOne(ctx context.Context, pc *pairCtx, r stackRecord, parent, onto string, reparent bool, again []string, pv *preview.Preview) (RestackedBranch, bool, []contract.Warning, error) {
	g := pc.git
	m := r.meta
	parentName := parent
	if parentName == "" {
		parentName = "main"
	}
	b := RestackedBranch{Feature: r.name, Parent: parentName, From: r.head, To: r.head}
	has, err := g.IsAncestor(ctx, onto, r.head)
	if err != nil {
		return b, false, nil, internalErr(err, "compare "+r.name+" with "+parentName)
	}
	newHead := r.head
	// st is the restack's step, started only when commits are replayed; it
	// ends once the leased push lands (or the run stops short of pushing).
	st := progress.Nop().Step("")
	if reparent || !has {
		own, ok, err := ownCommits(ctx, g, m, r.head, onto)
		switch {
		case err != nil:
			return b, false, nil, internalErr(err, "list "+r.name+"'s own commits")
		case ok:
			st = e.startStep("Restacking %s onto %s", r.name, parentName)
			rebased, err := e.replay(ctx, pc, own, onto)
			if err != nil {
				var ce *gitx.ConflictError
				if errors.As(err, &ce) {
					return b, false, nil, restackStopped(r.name, parentName, upstreamOf(m), ce.Commit, ce.Files, again)
				}
				return b, false, nil, contract.From(err)
			}
			newHead = onto
			if len(rebased) > 0 {
				newHead = rebased[len(rebased)-1]
			}
		case !has:
			// Neither on its fork point nor on the parent's tip: rebased by
			// hand somewhere else.
			return b, false, nil, restackStopped(r.name, parentName, "", "", []string{}, again)
		}
		// !ok && has (only with reparent): rebased by hand onto onto already.
	}
	nm := m
	nm.SchemaVersion = feature.SchemaVersion
	if parent == "" {
		nm.Parent, nm.ForkedAt, nm.Base = "", "", onto
	} else {
		nm.Parent, nm.ForkedAt = parent, onto
	}
	b.To, b.ForkedAt = newHead, nm.ForkedAt
	if newHead == r.head && parentFeature(nm) == parentFeature(m) && nm.ForkedAt == m.ForkedAt && nm.Base == m.Base {
		st.Done("")
		return b, false, nil, nil
	}
	sh := pc.pair.Shadow
	branchRef, metaRemote := "refs/heads/"+m.Branch, remoteMetaRef(r.name)
	if pv != nil {
		if newHead != r.head {
			pv.Add(preview.PushRef(sh, branchRef, r.head, newHead))
		}
		pv.Add(preview.PushRef(sh, metaRemote, r.metaSHA, ""))
		st.Done("")
		return b, true, nil, nil
	}
	sha, err := e.saveMeta(ctx, pc, nm, r.metaSHA)
	if err != nil {
		return b, false, nil, err
	}
	opt := gitx.PushOptions{Atomic: true, Leases: []gitx.Lease{{Ref: metaRemote, Expect: r.metaSHA}}}
	specs := []string{sha + ":" + metaRemote}
	if newHead != r.head {
		opt.Leases = append(opt.Leases, gitx.Lease{Ref: branchRef, Expect: r.head})
		specs = append(specs, newHead+":"+branchRef)
	}
	if err := e.push(ctx, pc, remoteShadow, opt, specs...); err != nil {
		return b, false, nil, restackMoved(r.name, sh, err)
	}
	st.Done("")
	var warns []contract.Warning
	if newHead != r.head {
		warns = addWarnings(warns, e.updateMirror(ctx, g, shadowRef(m.Branch), newHead)...)
	}
	warns = addWarnings(warns, e.updateMirror(ctx, g, metaRef(r.name), sha)...)
	return b, true, warns, nil
}

// ownCommits are the commits a branch adds over its parent, oldest first:
// forked_at..head whenever it records a fork point (a stacked branch, or
// one sync moved onto main when its parent merged); for a branch on main
// without one (moving onto a feature), the commits head has that onto
// lacks. ok is false when the branch no longer contains its fork point, or
// is stacked without one.
func ownCommits(ctx context.Context, g *gitx.Git, m feature.Meta, head, onto string) ([]string, bool, error) {
	if m.ForkedAt == "" {
		if parentFeature(m) != "" {
			return nil, false, nil
		}
		s, err := g.SourceCommits(ctx, onto, head)
		return s, err == nil, err
	}
	in, err := g.IsAncestor(ctx, m.ForkedAt, head)
	if err != nil || !in {
		return nil, false, err
	}
	s, err := g.SourceCommits(ctx, m.ForkedAt, head)
	return s, err == nil, err
}

// upstreamOf is where m's own commits start, for RebaseSteps, as
// ownCommits counts them: its fork point when it records one, "main" when
// on main without one.
func upstreamOf(m feature.Meta) string {
	if m.ForkedAt == "" && parentFeature(m) == "" {
		return "main"
	}
	return m.ForkedAt
}

// restackStopped is the REBASE_CONFLICT that stops a restack at name: a
// conflict in commit (files), or, with no commit, a branch rebased by hand
// somewhere the restack can't follow. details.again is the ghostbranch
// argv that carries on once the person has rebased and pushed it.
func restackStopped(name, parent, upstream, commit string, files, again []string) error {
	why := fmt.Sprintf("commit %s stopped on conflicts in %s", short(commit), strings.Join(files, ", "))
	if commit == "" {
		why = "it no longer contains its fork point and isn't on " + parent + "'s tip either (it was rebased by hand)"
	}
	return contract.New(contract.RebaseConflict,
		"%s does not restack cleanly onto %s: %s. The branches restacked before it stay restacked, and %s is unchanged; Ghostbranch never resolves conflicts: rebase it yourself (details.steps), push it, then run ghostbranch %s",
		name, parent, why, name, strings.Join(again, " ")).WithField("feature").
		WithDetails(map[string]any{"feature": name, "parent": parent, "upstream": upstream, "commit": commit, "files": files,
			"steps": RebaseSteps(remoteShadow, name, parent, upstream), "again": again})
}

// restackMoved names the branch whose lease failed. Other push errors pass
// through.
func restackMoved(name, shadow string, err error) error {
	ce := contract.From(err)
	if ce.Code != contract.RefMoved {
		return err
	}
	out := contract.New(contract.RefMoved,
		"feature %s changed in %s while it was being restacked, so it was left as it is (the branches restacked before it stay restacked); look at it, then run the restack again",
		name, shadow).WithField("feature").WithDetails(map[string]any{"feature": name, "refs": ce.Details["refs"]})
	out.Err = err
	return out
}
