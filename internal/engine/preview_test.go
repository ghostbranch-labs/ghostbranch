package engine_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func opNames(p *preview.Preview) []string {
	var out []string
	for _, o := range p.Ops {
		out = append(out, o["op"].(string))
	}
	return out
}

// Every mutating command previews with --dry-run and writes nothing.
func TestPreviewsWriteNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.eng.DryRun = true
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || res.Preview == nil {
		t.Fatalf("init preview: %+v %v", res, err)
	}
	if got := opNames(res.Preview); !reflect.DeepEqual(got, []string{"create_repo", "push_ref", "push_ref", "set_default_branch", "register_pair"}) {
		t.Fatalf("init ops = %v", got)
	}
	if h.forge.RepoDir(shadowName) != "" || len(h.pushes) != 0 || len(h.ledger()) != 0 {
		t.Fatal("init --dry-run wrote something")
	}
	h.eng.DryRun = false
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	before, pushes, entries := h.publicRefs(), len(h.pushes), len(h.ledger())

	// feature start before public main moves: on a stale shadow main it
	// refuses, preview or not.
	h.eng.DryRun = true
	fres, _, err := h.eng.FeatureStart(h.ctx, h.pair, "other")
	if err != nil || !reflect.DeepEqual(opNames(fres.Preview), []string{"push_ref", "push_ref"}) {
		t.Fatalf("feature start preview: %+v %v", fres, err)
	}
	h.eng.DryRun = false
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	before = h.publicRefs()

	h.eng.DryRun = true
	sres, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || !reflect.DeepEqual(opNames(sres.Preview), []string{"push_ref"}) || sres.Preview.Fingerprint["public_main"] == "" {
		t.Fatalf("sync preview: %+v %v", sres, err)
	}
	gres, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || !reflect.DeepEqual(opNames(gres.Preview), []string{"push_ref", "open_pr", "wait_checks", "push_ref"}) {
		t.Fatalf("gate preview: %+v %v", gres, err)
	}
	// No fresh gate yet: promote --dry-run says to run the gate, and makes no plan.
	pres, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "m"})
	if err != nil || pres.Plan != nil || !reflect.DeepEqual(opNames(pres.Preview), []string{"run_gate"}) {
		t.Fatalf("promote preview with a stale gate: %+v %v", pres, err)
	}
	h.eng.DryRun = false
	if len(h.pushes) != pushes || !reflect.DeepEqual(h.publicRefs(), before) || len(h.ledger()) != entries {
		t.Fatal("a preview pushed or recorded something")
	}
}

func TestPromoteAndApplyPreviewsWithAFreshGate(t *testing.T) {
	t.Parallel()
	h := gated(t)
	pushes := len(h.pushes)
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"})
	h.eng.DryRun = false
	if err != nil || res.Plan == nil || !reflect.DeepEqual(opNames(res.Preview), []string{"push_ref", "push_ref", "open_pr"}) {
		t.Fatalf("promote preview: %+v %v", res, err)
	}
	if got := res.Preview.Ops[1]; got["repo"] != publicName || got["to"] != res.Plan.Publish.Commit {
		t.Fatalf("public push op = %v", got)
	}
	if len(h.pushes) != pushes {
		t.Fatal("promote --dry-run pushed")
	}

	saved, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	pushes = len(h.pushes)
	h.eng.DryRun = true
	ares, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: saved.Plan.ID})
	h.eng.DryRun = false
	if err != nil || !reflect.DeepEqual(opNames(ares.Preview), []string{"push_ref", "open_pr"}) || len(h.approver.asked) != 0 {
		t.Fatalf("apply preview: %+v %v", ares, err)
	}
	if len(h.pushes) != pushes {
		t.Fatal("apply --dry-run pushed")
	}
}

// promote --plan --dry-run previews only what promote --plan does: the plan
// commit in the shadow repo and the plan file. Nothing public.
func TestPromotePlanOnlyPreviewListsOnlyThePlan(t *testing.T) {
	t.Parallel()
	h := gated(t)
	pushes, entries := len(h.pushes), len(h.ledger())
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	h.eng.DryRun = false
	if err != nil || res.Plan == nil || !reflect.DeepEqual(opNames(res.Preview), []string{"push_ref", "write_file"}) {
		t.Fatalf("promote --plan preview: %+v %v", res, err)
	}
	if got := res.Preview.Ops[0]; got["repo"] != shadowName || got["to"] != res.Plan.Publish.Commit {
		t.Fatalf("plan push op = %v", got)
	}
	if path, _ := res.Preview.Ops[1]["path"].(string); !strings.HasSuffix(path, res.Plan.ID+".json") {
		t.Fatalf("write_file op = %v", res.Preview.Ops[1])
	}
	if len(h.pushes) != pushes || len(h.ledger()) != entries || res.PlanPath != "" {
		t.Fatal("promote --plan --dry-run pushed, recorded or saved something")
	}
	if _, _, err := plan.Load(h.eng.WS.PlansDir(), res.Plan.ID); err == nil {
		t.Fatal("promote --plan --dry-run saved the plan")
	}
}

// A promote preview refuses a public branch that is already at another
// commit, exactly as the real publish would.
func TestPromotePreviewRefusesAnExistingPublicBranch(t *testing.T) {
	t.Parallel()
	h := gated(t)
	other := testutil.Commit(t, h.pubGit, "feat-x", "someone else's", map[string]string{"x.txt": "x\n"})
	pushes := len(h.pushes)
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"})
	h.eng.DryRun = false
	ce := wantCode(t, err, contract.BranchExists)
	if ce.Field != "branch" || ce.Details["branch"] != "feat-x" || ce.Details["sha"] != other {
		t.Fatalf("error = %+v", ce)
	}
	if res != nil && res.Preview != nil {
		t.Fatalf("a refused promote must not return a preview: %v", res.Preview.Ops)
	}
	if len(h.pushes) != pushes {
		t.Fatal("promote --dry-run pushed")
	}
}

// Once the plan is published, previewing the same promote lists no public
// push and no new PR: both already exist.
func TestPromotePreviewAfterPublishOmitsDoneSteps(t *testing.T) {
	t.Parallel()
	h := gated(t)
	saved, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: saved.Plan.ID}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"})
	h.eng.DryRun = false
	if err != nil || res.Plan == nil || res.Plan.Publish.Commit != saved.Plan.Publish.Commit {
		t.Fatalf("the preview must rebuild the published commit: %+v %v", res, err)
	}
	if got := opNames(res.Preview); !reflect.DeepEqual(got, []string{"push_ref"}) || res.Preview.Ops[0]["repo"] != shadowName {
		t.Fatalf("ops = %v", res.Preview.Ops)
	}
}
