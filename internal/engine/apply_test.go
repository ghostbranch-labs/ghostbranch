package engine_test

import (
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func planned(t *testing.T) (*harness, string) {
	t.Helper()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Message: "Add feature", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return h, res.Plan.ID
}

// Review Focus 3b: a plan made against old state is refused with exit 6.
func TestApplyRefusesAStalePlan(t *testing.T) {
	h, id := planned(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	ce := wantCode(t, err, contract.PlanStale)
	if !reflect.DeepEqual(ce.Details["moved"], []string{"public_main"}) || ce.Code.Exit() != 6 {
		t.Fatalf("details = %v", ce.Details)
	}
	if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok {
		t.Fatal("stale plan was published")
	}

	h2, id2 := planned(t)
	testutil.Commit(t, h2.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
	_, _, err = h2.eng.Apply(h2.ctx, engine.ApplyOptions{Plan: id2, Yes: true})
	if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"feature_head"}) {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestApplyResumesAfterTheBranchWasPushed(t *testing.T) {
	h, id := planned(t)
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the public push but before the PR was opened.
	mirror := gitx.New(h.eng.WS.MirrorDir(h.pair))
	if err := mirror.Push(h.ctx, "public", gitx.PushOptions{}, pl.Publish.Commit+":refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !res.Resumed || res.PublicPR != 1 {
		t.Fatalf("resume = %+v %v", res, err)
	}
	res, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !res.Resumed || len(h.forge.PRs(publicName)) != 1 {
		t.Fatalf("second resume = %+v %v", res, err)
	}
}

func TestApplyRefusesAPublicBranchAtAnotherCommit(t *testing.T) {
	h, id := planned(t)
	testutil.Commit(t, h.pubGit, "feat-x", "someone else", map[string]string{"x": "x\n"})
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	_ = wantCode(t, err, contract.BranchExists)
}

func TestApplyUnknownPlan(t *testing.T) {
	h := newHarness(t)
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: "nope", Yes: true})
	_ = wantCode(t, err, contract.BadFlag)
}
