package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

// StackReadiness is what next offers after a passing gate of a stacked
// feature (F-12). The engine works it out with the test promote uses
// (gateFresh and the tree link), so next never offers a promote that
// refuses stale_gate; the front end only reads it (directive 6).
type StackReadiness struct {
	// GateFirst is the lowest feature below this one whose gate isn't
	// fresh, or doesn't link to the level below it: gate it first. "" when
	// every one below is ready.
	GateFirst string
	// StaleAbove are the features stacked directly on this one whose gates
	// aren't fresh on this gate.
	StaleAbove []string
	// Levels is the longest stack from the bottom, up through this feature
	// and on through its only fresh child at each step, every level fresh
	// and linked by tree to the one below (trap 51). nil with GateFirst.
	Levels []string
	// TitleFor are the levels with more than one gate commit: their PRs
	// need --title-for.
	TitleFor []string
}

// readiness works out res.Ready. chain is the unpublished stack from its
// bottom up to res.Feature, kids the live features stacked on it; recs are
// the records stackView read.
func (e *Engine) readiness(ctx context.Context, pc *pairCtx, recs map[string]stackRecord, res *GateResult, chain, kids []string) StackReadiness {
	pm, O := res.PublicMain, res.Overlay
	var rd StackReadiness
	// The bottom links as promote links it (review M4): to public main, or
	// to its published parent's tree.
	below, check := bottomBelow(recs, chain[0])
	for _, n := range chain[:len(chain)-1] {
		m := recs[n].meta
		if fresh, _ := e.gateFresh(ctx, pc, n, m, pm, O); !fresh || (check && !linked(m, below)) {
			return StackReadiness{GateFirst: n}
		}
		rd.Levels = append(rd.Levels, n)
		if len(m.Gate.Commits) > 1 {
			rd.TitleFor = append(rd.TitleFor, n)
		}
		below, check = m.Gate.PublishedTree, true
	}
	// This feature's gate just passed; it links to the level below by the
	// tree it built on. As the bottom, it just built on what it builds on:
	// a moved published parent is promote's parent_moved, not a re-gate.
	if len(chain) > 1 && baseTree(recs, res) != below {
		return StackReadiness{GateFirst: chain[len(chain)-2]}
	}
	rd.Levels = append(rd.Levels, res.Feature)
	if len(res.Commits) > 1 {
		rd.TitleFor = append(rd.TitleFor, res.Feature)
	}
	tree, level := res.PublishedTree, kids
	for first := true; ; first = false {
		var fresh []string
		for _, k := range level {
			m := recs[k].meta
			if ok, _ := e.gateFresh(ctx, pc, k, m, pm, O); ok && linked(m, tree) {
				fresh = append(fresh, k)
			} else if first {
				rd.StaleAbove = append(rd.StaleAbove, k)
			}
		}
		if len(fresh) != 1 {
			return rd
		}
		k := fresh[0]
		gt := recs[k].meta.Gate
		rd.Levels = append(rd.Levels, k)
		if len(gt.Commits) > 1 {
			rd.TitleFor = append(rd.TitleFor, k)
		}
		tree, level = gt.PublishedTree, liveChildren(recs, childrenOf(recs, k, false))
	}
}

// baseTree is the published tree res's gate built on: the stack it built
// in this run, or, for a result read back from the record (wait), the
// record's BaseTree when the record is this gate's. "" when unknown.
func baseTree(recs map[string]stackRecord, res *GateResult) string {
	if res.stack != nil {
		return res.stack.T
	}
	if g := recs[res.Feature].meta.Gate; g != nil && g.Candidate == res.Candidate {
		return g.BaseTree
	}
	return ""
}

// bottomBelow is the published tree the stack's bottom gate must have
// built on, as resolveStack has it: "" on public main, or the tree its
// published parent's gate published. ok is false when the parent's record
// can't say; promote then refuses on the parent (parent_moved or
// parent_not_public), so readiness doesn't send the bottom to gate.
func bottomBelow(recs map[string]stackRecord, bottom string) (tree string, ok bool) {
	p := stackParent(recs[bottom].meta)
	if p == feature.MainParent {
		return "", true
	}
	pr, found := recs[p]
	if !found || pr.meta.Promoted == nil || pr.meta.Gate == nil || pr.meta.Gate.PublishedTree == "" {
		return "", false
	}
	return pr.meta.Gate.PublishedTree, true
}

// linked reports whether m's gate built on the published tree below it
// ("" for public main), as resolveStack checks it.
func linked(m feature.Meta, below string) bool {
	return m.Gate != nil && m.Gate.BaseTree == below
}
