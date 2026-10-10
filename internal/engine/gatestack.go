package engine

// Gating up the stack: a stacked feature's gate replays the features below
// it onto public main (R), then its own forked_at..head commits on R. CI
// runs on the top; the leak scan covers only what the feature adds.

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
)

// forkOf is m's fork point when it is stacked, "" when its parent is main.
func forkOf(m feature.Meta) string {
	if parentFeature(m) == "" {
		return ""
	}
	return m.ForkedAt
}

// stackLevel is one feature below the one being gated.
type stackLevel struct {
	name string
	meta feature.Meta
	head string
}

// stackBelow walks from m's parent down to main and returns the features
// below m, bottom first, at their heads now; nil when m is unstacked. Each
// link must hold: the parent's record and branch exist, and both the
// parent and the feature above it contain that feature's fork point.
// Otherwise it is PARENT_REWRITTEN naming the feature above.
func (e *Engine) stackBelow(ctx context.Context, pc *pairCtx, name string, m feature.Meta, head string) ([]stackLevel, error) {
	g := pc.git
	var out []stackLevel
	seen := map[string]bool{name: true}
	child, cm, chead := name, m, head
	for p := parentFeature(cm); p != ""; p = parentFeature(cm) {
		if seen[p] {
			return nil, contract.New(contract.Internal, "features %s and %s are stacked on each other", child, p)
		}
		seen[p] = true
		pm, _, _, err := e.readMetaAny(ctx, pc, p)
		if ce := contract.From(err); ce != nil && ce.Code == contract.FeatureNotFound {
			return nil, parentRewritten(child, p, cm.ForkedAt, reasonParentGone)
		}
		if err != nil {
			return nil, err
		}
		phead, ok, err := g.Resolve(ctx, shadowRef(pm.Branch))
		if err != nil {
			return nil, internalErr(err, "read feature branch "+pm.Branch)
		}
		if !ok {
			return nil, parentRewritten(child, p, cm.ForkedAt, reasonParentGone)
		}
		if cm.ForkedAt == "" {
			return nil, parentRewritten(child, p, "", reasonForkPointLost)
		}
		if in, err := g.IsAncestor(ctx, cm.ForkedAt, phead); err != nil {
			return nil, internalErr(err, "compare "+child+" with "+p)
		} else if !in {
			return nil, parentRewritten(child, p, cm.ForkedAt, reasonParentRewritten)
		}
		if in, err := g.IsAncestor(ctx, cm.ForkedAt, chead); err != nil {
			return nil, internalErr(err, "compare "+child+" with its fork point")
		} else if !in {
			return nil, parentRewritten(child, p, cm.ForkedAt, reasonForkPointLost)
		}
		out = append(out, stackLevel{name: p, meta: pm, head: phead})
		child, cm, chead = p, pm, phead
	}
	slices.Reverse(out)
	return out, nil
}

// stackBase is what a stacked feature's own commits replay onto: R, the
// rebased tip of the features below it, and R's published tree T.
type stackBase struct {
	R, T  string
	below []feature.StackHead
}

// stackBase builds R level by level from public main. A level whose own
// gate built on this same stack is reused (reusableR); any other is
// replayed here, its own commits onto the R below it, without touching
// its record.
func (e *Engine) stackBase(ctx context.Context, pc *pairCtx, cfg config.Config, levels []stackLevel, pm, O string) (stackBase, error) {
	g := pc.git
	R, T := pm, ""
	heads := map[string]string{}
	below := []feature.StackHead{}
	for i, lv := range levels {
		parent := ""
		if i > 0 {
			parent = levels[i-1].name
		}
		if r, t, ok := reusableR(ctx, g, lv, parent, forkOf(lv.meta), heads, pm, O); ok {
			R, T = r, t
		} else {
			own, err := ownSources(ctx, g, lv.name, lv.meta, lv.head, pm)
			if err != nil {
				return stackBase{}, err
			}
			e.progressf("replaying %s (below the gated feature) onto %s", lv.name, short(R))
			rebased, err := e.replay(ctx, pc, own, R)
			if err != nil {
				var ce *gitx.ConflictError
				if errors.As(err, &ce) {
					onto := "public main"
					if parent != "" {
						onto = parent + " (rebased onto public main)"
					}
					return stackBase{}, contract.New(contract.RebaseConflict,
						"feature %s, below the gated feature in its stack, does not rebase cleanly onto %s; commit %s stopped on conflicts in: %s",
						lv.name, onto, short(ce.Commit), strings.Join(ce.Files, ", ")).
						WithDetails(map[string]any{"feature": lv.name, "files": ce.Files, "commit": ce.Commit})
				}
				return stackBase{}, contract.From(err)
			}
			if len(rebased) > 0 {
				R = rebased[len(rebased)-1]
			}
			T = ""
		}
		heads[lv.name] = lv.head
		below = append(below, feature.StackHead{Feature: lv.name, Head: lv.head})
	}
	if T == "" {
		t, _, err := publishedTree(ctx, g, R, cfg)
		if err != nil {
			return stackBase{}, namedMarkerErr(err, levels[len(levels)-1].name)
		}
		T = t
	}
	return stackBase{R: R, T: T, below: below}, nil
}

// ownSources are the commits name's gate replays: forked_at..head whenever
// m records a fork point (a stacked feature, or one moved onto main that
// still carries its old parent's commits), else every commit head has that
// public main pm lacks. A feature that no longer contains its fork point is
// PARENT_REWRITTEN (fork_point_lost).
func ownSources(ctx context.Context, g *gitx.Git, name string, m feature.Meta, head, pm string) ([]string, error) {
	own, ok, err := ownCommits(ctx, g, m, head, pm)
	if err != nil {
		return nil, internalErr(err, "list "+name+"'s commits")
	}
	if !ok {
		return nil, parentRewritten(name, m.ParentName(), m.ForkedAt, reasonForkPointLost)
	}
	return own, nil
}

// reusableR is the rebased tip R and published tree of lv's own gate, when
// that gate built on exactly this stack: lv's head, public main and the
// overlay (the strip rules decide T); lv's parent and fork point; and each
// feature below at the head it has now (below). R must still be in the
// mirror. Any recorded replay counts, whatever its checks or leak: R
// depends only on what was replayed.
func reusableR(ctx context.Context, g *gitx.Git, lv stackLevel, parent, forkedAt string, below map[string]string, pm, O string) (string, string, bool) {
	gt := lv.meta.Gate
	if gt == nil || gt.Commits == nil || gt.PublishedTree == "" || !gt.FreshFor(lv.head, pm, O) || !gt.StackFreshFor(parent, forkedAt, below) {
		return "", "", false
	}
	R := gt.Base
	if R == "" {
		R = pm
	}
	if n := len(gt.Commits); n > 0 {
		R = gt.Commits[n-1].Rebased
	}
	if _, ok, err := g.Resolve(ctx, R); err != nil || !ok {
		return "", "", false
	}
	return R, gt.PublishedTree, true
}

// scanOwn scans what a stacked feature's published tree adds over the
// published tree below it (baseTree): only entries that are new or differ.
// That tree was scanned by the gates below.
func (e *Engine) scanOwn(ctx context.Context, g *gitx.Git, cfg config.Config, entries []gitx.TreeEntry, baseTree string) (leak.Report, error) {
	old, err := blobsByPath(ctx, g, baseTree)
	if err != nil {
		return leak.Report{}, err
	}
	var changed []gitx.TreeEntry
	for _, en := range entries {
		if old[en.Path] != en.SHA {
			changed = append(changed, en)
		}
	}
	rep, _, err := e.scan(ctx, g, cfg, changed, baseTree, nil)
	return rep, err
}

// stackState is what the survey needs of the features below one: their
// heads now; whether any of them moved since the feature above it forked
// from it (parentMoved); and whether the stack is broken (a feature below
// is gone, or the parents loop).
type stackState struct {
	heads       map[string]string
	parentMoved bool
	broken      bool
}

// stackState walks m's parents down to main. It never fails on a broken
// stack: it reports it.
func (e *Engine) stackState(ctx context.Context, pc *pairCtx, name string, m feature.Meta) (stackState, error) {
	st := stackState{heads: map[string]string{}}
	seen := map[string]bool{name: true}
	cm := m
	for p := parentFeature(cm); p != ""; p = parentFeature(cm) {
		if seen[p] {
			st.broken, st.parentMoved = true, true
			return st, nil
		}
		seen[p] = true
		pm, _, _, err := e.readMetaAny(ctx, pc, p)
		if err != nil {
			var bad *unreadableRecord
			if ce := contract.From(err); ce.Code == contract.FeatureNotFound || errors.As(err, &bad) {
				st.broken, st.parentMoved = true, true
				return st, nil
			}
			return st, err
		}
		head, ok, err := pc.git.Resolve(ctx, shadowRef(pm.Branch))
		if err != nil {
			return st, internalErr(err, "read feature branch "+pm.Branch)
		}
		if !ok {
			st.broken, st.parentMoved = true, true
			return st, nil
		}
		if head != cm.ForkedAt {
			st.parentMoved = true
		}
		st.heads[p] = head
		cm = pm
	}
	return st, nil
}

// gateStackFresh reports whether m's gate built on the stack as it is now
// (Gate.StackFreshFor), false without a gate. Task 6's reusableGate and
// the chain checks use it beside FreshFor.
func (e *Engine) gateStackFresh(ctx context.Context, pc *pairCtx, name string, m feature.Meta) (bool, error) {
	if m.Gate == nil {
		return false, nil
	}
	st, err := e.stackState(ctx, pc, name, m)
	if err != nil {
		return false, err
	}
	return !st.broken && m.Gate.StackFreshFor(parentFeature(m), forkOf(m), st.heads), nil
}
