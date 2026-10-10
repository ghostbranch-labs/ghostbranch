package engine

// The stack side of sync: GitHub's restack of the public branches above a
// merged PR, and moving a closing feature's children onto main.

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

// refreshPublished compares each open published stacked feature's public
// branch with the head Ghostbranch recorded. A branch that moved and holds
// exactly the published changes (the same patch-ids, on its PR's base as it
// is now) is GitHub's restack after a PR below it merged: the new head, and
// the retargeted base, are recorded. That is neither drift nor
// public_pr_needs_update. A branch that moved and holds anything else was
// changed by someone: PUBLIC_BRANCH_CHANGED, and the feature joins sync's
// attention list; it is never adopted.
//
// Skipped: records from before 0.11 (no patch-ids), records this binary
// can't write, and, in 6.1a, a single feature on main (no native stack, its
// parent and its PR's base main): flagging foreign pushes there is
// deferred. A comparison that fails (a git error, or a base branch the
// public repo doesn't have) is skipped with a PR_STATE_UNKNOWN warning,
// never flagged. A record that can't be written is a SYNC_PUSH_FAILED
// warning, and the next sync tries again. In a dry run (pv set) the writes
// are previewed.
func (e *Engine) refreshPublished(ctx context.Context, pc *pairCtx, rs []FeatureReport, pv *preview.Preview) []contract.Warning {
	g := pc.git
	var warns []contract.Warning
	for i := range rs {
		r := &rs[i]
		m := r.meta
		if m == nil || r.unreadable != "" || r.PRState != prOpen || m.Promoted == nil ||
			m.Promoted.PublishedHead == "" || len(m.Promoted.PatchIDs) == 0 {
			continue
		}
		if m.Promoted.Stack == 0 && m.ParentName() == feature.MainParent && m.Promoted.Base == feature.MainParent {
			continue
		}
		branch := publishedBranch(*m)
		unknown := func(why string) {
			warns = append(warns, contract.Warning{Code: contract.PRStateUnknown,
				Message: fmt.Sprintf("couldn't compare public branch %s of feature %s with what was published (%s); its record was left as it is, and the next sync looks again",
					branch, r.Name, why),
				Details: map[string]any{"feature": r.Name, "public_pr": m.Promoted.PublicPR}})
		}
		tip, ok, err := g.Resolve(ctx, publicRef(branch))
		if err != nil {
			unknown(err.Error())
			continue
		}
		if !ok || tip == m.Promoted.PublishedHead {
			continue
		}
		pr, err := e.Forge.GetPR(ctx, pc.pair.Public, m.Promoted.PublicPR)
		if err != nil {
			warns = append(warns, contract.Warning{Code: contract.PRStateUnknown,
				Message: fmt.Sprintf("couldn't read public PR #%d of feature %s to check its moved branch: %v", m.Promoted.PublicPR, r.Name, err),
				Details: map[string]any{"feature": r.Name, "public_pr": m.Promoted.PublicPR}})
			continue
		}
		base, ok, err := g.Resolve(ctx, publicRef(pr.Base))
		if err != nil {
			unknown(err.Error())
			continue
		}
		if !ok {
			unknown("its PR's base branch " + pr.Base + " is not in the public repo")
			continue
		}
		commits, err := g.CommitsBetween(ctx, base, tip)
		if err != nil {
			unknown(err.Error())
			continue
		}
		ids, err := g.PatchIDs(ctx, commits)
		if err != nil {
			unknown(err.Error())
			continue
		}
		if !slices.Equal(ids, m.Promoted.PatchIDs) {
			r.flagged = true
			warns = append(warns, contract.Warning{Code: contract.PublicBranchChanged,
				Message: fmt.Sprintf("public branch %s of feature %s is at %s, not %s as published, and holds changes Ghostbranch didn't publish; they were not adopted: a person looks at PR #%d",
					branch, r.Name, short(tip), short(m.Promoted.PublishedHead), m.Promoted.PublicPR),
				Details: map[string]any{"feature": r.Name, "branch": branch, "published_head": m.Promoted.PublishedHead,
					"public_head": tip, "public_pr": m.Promoted.PublicPR}})
			continue
		}
		nm := *m
		p := *m.Promoted
		p.PublishedHead, p.Base = tip, pr.Base
		nm.Promoted, nm.SchemaVersion = &p, feature.SchemaVersion
		if pv != nil {
			pv.Add(preview.PushRef(pc.pair.Shadow, remoteMetaRef(r.Name), r.metaSHA, ""))
			continue
		}
		e.progressf("GitHub restacked feature %s's public branch %s onto %s (the same changes, new commits); recording it", r.Name, branch, pr.Base)
		mw, err := e.updateMeta(ctx, pc, nm, r.metaSHA)
		warns = addWarnings(warns, mw...)
		if err != nil {
			ce := contract.From(err)
			warns = append(warns, contract.Warning{Code: contract.SyncPushFailed,
				Message: fmt.Sprintf("sync could not record GitHub's restack of feature %s's public branch %s (%s); the next sync tries again", r.Name, branch, ce.Message),
				Details: map[string]any{"branch": remoteMetaRef(r.Name), "code": string(ce.Code)}})
			continue
		}
		// The closes below lease on the record as it is now.
		if sha, ok, _ := g.Resolve(ctx, metaRef(r.Name)); ok {
			r.meta, r.metaSHA = &nm, sha
		}
	}
	return warns
}

// bottomUp orders rs parents first, so a stack closes from its bottom: by
// depth below main, then by name.
func bottomUp(rs []FeatureReport) []int {
	parent := map[string]string{}
	for _, r := range rs {
		if r.meta != nil {
			parent[r.Name] = stackParent(*r.meta)
		}
	}
	depth := func(n string) int {
		d, seen := 0, map[string]bool{n: true}
		for {
			p, ok := parent[n]
			if !ok || p == feature.MainParent || seen[p] {
				return d
			}
			seen[p] = true
			d++
			n = p
		}
	}
	idx := make([]int, len(rs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(x, y int) bool {
		a, b := rs[idx[x]], rs[idx[y]]
		if da, db := depth(a.Name), depth(b.Name); da != db {
			return da < db
		}
		return a.Name < b.Name
	})
	return idx
}

// childReports is every surveyed feature whose record names name as its
// parent, abandoned ones included: closing name moves them all onto main.
func childReports(rs []FeatureReport, name string) []FeatureReport {
	var out []FeatureReport
	for _, r := range rs {
		if r.meta != nil && r.Name != name && stackParent(*r.meta) == name {
			out = append(out, r)
		}
	}
	return out
}

// unreadableReports names the surveyed features whose record couldn't be
// read at all (corrupt, or reading it failed): this binary can't tell what
// they are stacked on.
func unreadableReports(rs []FeatureReport) []string {
	out := []string{}
	for _, r := range rs {
		if r.meta == nil {
			out = append(out, r.Name)
		}
	}
	return out
}

// closeBlocked is the FEATURE_NOT_CLOSED warning for a merged feature sync
// leaves open because the records in unread can't be read: any of them may
// be stacked on it, and closing it would strand that feature on a deleted
// branch. feature close and fold refuse for the same reason.
func closeBlocked(name string, unread []string) contract.Warning {
	return contract.Warning{Code: contract.FeatureNotClosed,
		Message: fmt.Sprintf("sync left merged feature %s open: the record of feature %s couldn't be read as a whole, so it can't be told whether it is stacked on %s; a person closes %s at the console (ghostbranch feature close %s), or fixes its record, and the next sync closes %s",
			name, strings.Join(unread, ", "), name, unread[0], unread[0], name),
		Details: map[string]any{"feature": name, "unreadable": unread}}
}

// reparent is a child's record rewritten to stand on main. It is saved but
// not pushed: the parent's close pushes it in the same atomic push.
type reparent struct {
	name string
	old  string // the metadata commit the lease expects
	new  string // the rewritten record's metadata commit; "" in a dry run
}

// reparentChildren rewrites the records of a closing feature's children to
// stand on main, without pushing them. The close pushes them in the same
// atomic push that deletes the parent, so no child is ever left naming a
// deleted parent, and if that push fails nothing changes.
//
// A child keeps its own commits. Its forked_at becomes the parent's
// published feature head, so its next gate replays only the commits after
// it, and its PR's base becomes main, as GitHub retargets it. When the PRs
// were plain chained PRs (no native stack) and the parent's published head
// isn't in public main (a squash or rebase merge), the child's open public
// PR still carries the parent's old commits: it is marked
// public_pr_needs_update. A child whose record this binary can't rewrite
// stops the close (HAS_CHILDREN). In a dry run nothing is written.
func (e *Engine) reparentChildren(ctx context.Context, pc *pairCtx, parent FeatureReport, kids []FeatureReport) ([]reparent, error) {
	if len(kids) == 0 {
		return nil, nil
	}
	g := pc.git
	pm, err := g.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return nil, internalErr(err, "read public main")
	}
	forkedAt, inMain := "", false
	if pmeta := parent.meta; pmeta != nil && pmeta.Promoted != nil {
		forkedAt = e.publishedHead(*pmeta)
		if h := pmeta.Promoted.PublishedHead; h != "" {
			if inMain, err = g.IsAncestor(ctx, h, pm); err != nil {
				return nil, internalErr(err, "check whether "+parent.Name+"'s published head is in public main")
			}
		}
	}
	out := make([]reparent, 0, len(kids))
	for _, k := range kids {
		if k.meta == nil || k.unreadable != "" {
			return nil, contract.New(contract.HasChildren,
				"feature %s has the child %s, whose record this Ghostbranch can't rewrite; %s stays until %s is upgraded here or closed",
				parent.Name, k.Name, parent.Name, k.Name).WithField("feature").
				WithDetails(map[string]any{"feature": parent.Name, "children": []string{k.Name}, "op": "close"})
		}
		m := *k.meta
		m.SchemaVersion, m.Parent = feature.SchemaVersion, ""
		if forkedAt != "" {
			m.ForkedAt = forkedAt
		}
		if m.Promoted != nil {
			p := *m.Promoted
			p.Base = feature.MainParent
			p.NeedsUpdate = p.NeedsUpdate || (p.Stack == 0 && !inMain)
			m.Promoted = &p
		}
		sha := ""
		if !e.DryRun {
			if sha, err = e.saveMeta(ctx, pc, m, k.metaSHA); err != nil {
				return nil, err
			}
		}
		out = append(out, reparent{name: k.Name, old: k.metaSHA, new: sha})
	}
	return out, nil
}
