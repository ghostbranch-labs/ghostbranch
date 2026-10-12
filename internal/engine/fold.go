package engine

import (
	"context"
	"fmt"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// FoldOptions are feature fold's inputs.
type FoldOptions struct {
	Feature string // the stacked feature to fold into its parent
}

// FoldResult is a stacked feature folded into its parent.
type FoldResult struct {
	Feature    string            `json:"feature"`    // the folded feature, now closed
	Parent     string            `json:"parent"`     // the feature it was folded into
	From       string            `json:"from"`       // the parent's head before
	To         string            `json:"to"`         // the parent's head now: the folded feature's head
	Reparented []string          `json:"reparented"` // the folded feature's children, now stacked on Parent; never null
	Deleted    map[string]string `json:"deleted"`    // the folded feature's refs deleted, with their SHAs; never null
	GatePR     *int              `json:"gate_pr"`    // its gate draft PR, closed; nil when none was open
	Preview    *preview.Preview  `json:"preview"`
}

// FeatureFold folds a stacked feature into its parent: the parent's branch
// fast-forwards to the feature's head and the feature closes, each of its
// children stacked on the parent instead, all in one atomic push leased on
// what fold read. Every commit of the feature ends up on the parent, so no
// console is asked. A parent whose tip the feature doesn't contain is
// FOLD_NEEDS_REBASE with the git steps: Ghostbranch never rebases, merges
// or resolves a conflict here.
func (e *Engine) FeatureFold(ctx context.Context, p workspace.Pair, opt FoldOptions) (res *FoldResult, warns []contract.Warning, err error) {
	name := opt.Feature
	defer func() {
		summary := ""
		var refs map[string]string
		if res != nil && res.Preview == nil {
			refs = res.Deleted
			if err == nil {
				summary = fmt.Sprintf("folded %s into %s: %s -> %s; deleted %d ref(s)",
					res.Feature, res.Parent, short(res.From), short(res.To), len(res.Deleted))
			}
		}
		e.recordWith("feature fold", &p, name, "", summary, err, func(ent *ledger.Entry) {
			if len(refs) > 0 {
				ent.Refs = refs
			}
		})
	}()
	if err := feature.ValidateName(name, "feature"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature fold", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, warns, err
	}
	parent := parentFeature(m)
	if parent == "" {
		return nil, warns, contract.New(contract.BadFlag,
			"feature %s is not stacked: its parent is main, so there is nothing to fold it into; publish it with ghostbranch promote %s", name, name).
			WithField("feature")
	}
	if m.Promoted != nil {
		return nil, warns, contract.New(contract.BadFlag,
			"feature %s is published as public PR #%d; fold joins only unpublished features, so no public PR loses its branch", name, m.Promoted.PublicPR).
			WithField("feature")
	}
	recs, err := e.readableRecords(ctx, pc, name, "fold")
	if err != nil {
		return nil, warns, err
	}
	pr, ok := recs[parent]
	if !ok || pr.head == "" {
		return nil, warns, parentRewritten(name, parent, m.ForkedAt, reasonParentGone)
	}
	// Fold moves the parent's branch: a newer Ghostbranch's record of it may
	// track what this binary doesn't know, so it is never moved from here.
	if pr.newer {
		ce := contract.NewerRecord("feature", pr.meta.SchemaVersion).WithField("feature")
		ce.Details["feature"] = parent
		return nil, warns, ce
	}
	rs, sw, err := e.survey(ctx, pc, name)
	warns = addWarnings(warns, sw...)
	if err != nil {
		return nil, warns, err
	}
	if len(rs) != 1 {
		return nil, warns, contract.New(contract.FeatureNotFound, "no feature %q in %s", name, pc.pair.Shadow).WithField("feature")
	}
	if rs[0].err != nil {
		return nil, warns, rs[0].err
	}
	r := rs[0]
	if r.Head == "" {
		return nil, warns, contract.New(contract.FeatureNotFound,
			"feature %q has metadata, but its branch %q is gone from %s", name, m.Branch, pc.pair.Shadow).WithField("feature")
	}
	ff, err := pc.git.IsAncestor(ctx, pr.head, r.Head)
	if err != nil {
		return nil, warns, internalErr(err, "compare "+parent+" with "+name)
	}
	if !ff {
		return nil, warns, contract.New(contract.FoldNeedsRebase,
			"%s has moved on since %s was started from it (it is at %s, which %s doesn't contain), so fold can't fast-forward it; rebase %s onto %s yourself (details.steps), push it, and fold again. Ghostbranch never rebases or resolves conflicts",
			parent, name, short(pr.head), name, name, parent).WithField("feature").
			WithDetails(map[string]any{"feature": name, "parent": parent, "parent_head": pr.head, "forked_at": m.ForkedAt,
				"steps": RebaseSteps(remoteShadow, name, parent, m.ForkedAt)})
	}
	// The parent moves to the feature's head, and each child of the feature
	// is stacked on the parent, its fork point unchanged: that commit is on
	// the parent now. All of it goes in the close's atomic push.
	kids := childrenOf(recs, name, true)
	also := []refUpdate{{Ref: "refs/heads/" + pr.meta.Branch, From: pr.head, To: r.Head, Local: shadowRef(pr.meta.Branch)}}
	for _, k := range kids {
		kr := recs[k]
		if kr.newer {
			ce := contract.NewerRecord("feature", kr.meta.SchemaVersion).WithField("feature")
			ce.Details["feature"] = k
			return nil, warns, ce
		}
		km := kr.meta
		km.SchemaVersion = feature.SchemaVersion
		km.Parent = parent
		to := ""
		if !e.DryRun {
			if to, err = e.saveMeta(ctx, pc, km, kr.metaSHA); err != nil {
				return nil, warns, err
			}
		}
		also = append(also, refUpdate{Ref: remoteMetaRef(k), From: kr.metaSHA, To: to, Local: metaRef(k)})
	}
	t, tw, err := e.closeTargets(ctx, pc, r)
	warns = addWarnings(warns, tw...)
	if err != nil {
		return nil, warns, err
	}
	st := e.startStep("Folding %s into %s", name, parent)
	e.notef(progress.Info, "%s: %s → %s", parent, short(pr.head), short(r.Head))
	cres, cw, err := e.closeWith(ctx, pc, r, t, closeOpts{verb: "folded into " + parent, also: also})
	warns = addWarnings(warns, cw...)
	if cres == nil {
		return nil, warns, err
	}
	// The close push's step ended this one; a dry run pushes nothing.
	st.Done("")
	// A non-nil cres with an error: the fold landed, but the gate PR is
	// still open (closeWith's afterDeletes error).
	return &FoldResult{Feature: name, Parent: parent, From: pr.head, To: r.Head, Reparented: kids,
		Deleted: cres.Deleted, GatePR: cres.GatePR, Preview: cres.Preview}, warns, err
}
