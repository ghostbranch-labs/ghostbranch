package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// planNotHere is PLAN_NOT_FOUND for plan id, whose file isn't on this
// machine (F-18). It looks for the plan's ref in lookup's shadow repo,
// read-only: it fetches into the mirror and takes no lock. The ref means
// the plan was made on another machine; the feature whose gate published
// the plan commit's tree is named when exactly one matches. A failed
// look-up is said in the message, never returned as its own error.
func (e *Engine) planNotHere(ctx context.Context, id string, lookup *workspace.Pair) error {
	d := map[string]any{"plan": id, "reason": "unknown"}
	notFound := func(format string, args ...any) error {
		return contract.New(contract.PlanNotFound, format, args...).WithField("plan").WithDetails(d)
	}
	if lookup == nil {
		return notFound("no plan %s on this machine; pass --repo <owner/repo> to look for it in that pair's shadow repo", id)
	}
	d["public"] = lookup.Public
	pc, err := e.open(ctx, "apply", *lookup, false)
	if err != nil {
		return notFound("no plan %s on this machine; couldn't check %s: %s", id, lookup.Shadow, contract.From(err).Message)
	}
	defer pc.unlock()
	sha, ok, err := pc.git.Resolve(ctx, planRef(id))
	if err != nil {
		return notFound("no plan %s on this machine; couldn't check %s: %v", id, lookup.Shadow, err)
	}
	if !ok {
		return notFound("no plan %s on this machine or in %s", id, lookup.Shadow)
	}
	d["reason"] = "other_machine"
	if tree, err := pc.git.TreeOf(ctx, sha); err == nil {
		if f := e.featureGatedAs(ctx, pc, tree); f != "" {
			d["feature"] = f
		}
	}
	return notFound("plan %s was made on another machine (the shadow repo has its ref, but plan files stay where they were made): apply it there, or make a new plan here", id)
}

// featureGatedAs is the one feature whose recorded gate published tree, or
// "" when none or several did.
func (e *Engine) featureGatedAs(ctx context.Context, pc *pairCtx, tree string) string {
	recs, err := e.records(ctx, pc)
	if err != nil {
		return ""
	}
	found := ""
	for n, r := range recs {
		if r.meta.Gate == nil || r.meta.Gate.PublishedTree != tree {
			continue
		}
		if found != "" {
			return ""
		}
		found = n
	}
	return found
}
