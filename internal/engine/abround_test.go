package engine_test

import (
	"os"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// A: once an approved update finished, the plan file (local, unsigned)
// edited to type a PR title can't change the PR without approval: apply
// writes nothing public, warns PR_TEXT_NOT_SET, and the PR is not edited.
func TestALandedApplyNeverEditsThePRWithoutApproval(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("update = %+v %v", res, err)
	}
	edits := len(h.forge.Edits)
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), res.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	pl.Typed.Title, pl.Publish.PR.Title = true, "Sneaky title"
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil
	_, warns, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if len(h.forge.Edits) != edits {
		t.Fatalf("the PR was edited without approval: %+v", h.forge.Edits[edits:])
	}
	found := false
	for _, w := range warns {
		found = found || string(w.Code) == "PR_TEXT_NOT_SET"
	}
	if err != nil || !found {
		t.Fatalf("apply: %v, warnings %+v; want the record finished and PR_TEXT_NOT_SET", err, warns)
	}
	// With a person at the console, the same apply asks, and sets it.
	h.eng.Approver = h.approver
	asked := len(h.approver.asked)
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID})
	if err != nil || ar.Approval == nil || len(h.approver.asked) != asked+1 || len(h.forge.Edits) != edits+1 ||
		*h.forge.Edits[edits].Title != "Sneaky title" {
		t.Fatalf("approved apply = %+v %v; edits %+v", ar, err, h.forge.Edits)
	}
}

// A: a never-approved append plan whose file names the PR's head H as its
// commit (an ancestor of its plan ref) is not a landed update: it goes the
// normal way, which refuses it.
func TestAnAppendPlanNamingThePRHeadIsNotLanded(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	H := h.pubTip("feat-x")
	recorded := h.shadowMeta("feat-x").Promoted.PlanID
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil || res.Plan == nil || res.Plan.Publish.Style != plan.StyleAppend {
		t.Fatalf("promote --plan = %+v %v", res, err)
	}
	pl := *res.Plan
	pl.Publish.Commit = H
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if err == nil || h.shadowMeta("feat-x").Promoted.PlanID != recorded {
		t.Fatalf("apply of a plan naming H = %+v %v; it must not be finished as landed", ar, err)
	}
}

// B: after a preserve replace, the adopted commit's record names the public
// commit that carries it now, so a machine whose mirror never had the
// original commit still updates the PR (no adopted_missing), keeping the
// maintainer's author.
func TestAnAdoptedCommitFollowsItsPublicCommitAcrossAReplace(t *testing.T) {
	t.Parallel()
	h, gerr, res, perr := adoptedReplace(t, "", "")
	if gerr != nil || perr != nil || res.Applied == nil {
		t.Fatalf("gate %v, promote %+v %v", gerr, res, perr)
	}
	ad := h.shadowMeta("a").Promoted.Adopted
	if len(ad) != 1 || ad[0].SHA != h.pubTip("a") || ad[0].Subject != "their fix" {
		t.Fatalf("adopted after the replace = %+v; want the public commit %s", ad, h.pubTip("a"))
	}
	if err := os.RemoveAll(h.eng.WS.MirrorDir(h.pair)); err != nil {
		t.Fatal(err)
	}
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/a", h.meta("a").Base); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "Add a, reworked again", map[string]string{"a.txt": "a, again\n"})
	commitAs(t, h.shadowGit(), "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	h.gateFeature("a")
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if codeOf(err) == contract.PRChangedOnGitHub || err != nil || res.Applied == nil {
		t.Fatalf("second preserve update = %+v %v", res, err)
	}
	wantMaintainerKept(t, h, res)
	if _, ok, _ := gitx.New(h.eng.WS.MirrorDir(h.pair)).Resolve(h.ctx, ad[0].SHA); !ok {
		t.Fatal("the recorded commit must be the public one, which the fresh mirror has")
	}
}
