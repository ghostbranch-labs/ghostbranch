package engine_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const guardedJob = "on: [pull_request]\njobs:\n  upstream-only:\n    if: github.repository == 'acme/widget'\n    runs-on: x\n    steps:\n      - run: echo hi\n"

func showAt(h *harness, rev, path string) string {
	h.t.Helper()
	b, ok, err := h.shadowGit().ShowFile(h.ctx, rev, path)
	if err != nil || !ok {
		h.t.Fatalf("%s:%s: %v", rev, path, err)
	}
	return string(b)
}

func TestGateRewritesGuardsInTheCandidateOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
	h.init()
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat")
	if err != nil {
		t.Fatal(err)
	}
	if res.RepoGuards == nil || len(res.RepoGuards.Rewritten) != 1 || res.RepoGuards.Mode != "rewrite" {
		t.Fatalf("repo_guards = %+v", res.RepoGuards)
	}
	inC := showAt(h, res.Candidate, ".github/workflows/up.yml")
	if !strings.Contains(inC, "'acme/widget-shadow'") {
		t.Fatalf("C was not rewritten:\n%s", inC)
	}
	// T, what publishes, is untouched
	pres, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := showAt(h, pres.Plan.Publish.Commit, ".github/workflows/up.yml"); got != guardedJob {
		t.Fatalf("the published workflow changed:\n%s", got)
	}
	if len(pres.Plan.CI.RepoGuards.Rewritten) != 1 {
		t.Fatalf("plan ci = %+v", pres.Plan.CI)
	}
	if !pres.Gate.Reused || pres.Gate.RepoGuards == nil || len(pres.Gate.RepoGuards.Rewritten) != 1 {
		t.Fatalf("reused gate = %+v", pres.Gate)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pres.Plan.ID}); err != nil {
		t.Fatalf("apply rejected a rewrite-only difference: %v", err)
	}
}

func TestGateGuardModes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		mode    string
		warns   int
		skipped int
	}{{"warn", 1, 1}, {"off", 0, 0}} {
		h := newHarness(t)
		testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
		h.init()
		h.setConfig("rewrite_repo_guards: rewrite", "rewrite_repo_guards: "+tc.mode)
		h.startFeature("feat", map[string]string{"f.txt": "f\n"})
		res, warns, err := h.eng.Gate(h.ctx, h.pair, "feat")
		if err != nil {
			t.Fatal(err)
		}
		if got := showAt(h, res.Candidate, ".github/workflows/up.yml"); got != guardedJob {
			t.Fatalf("%s: C was rewritten:\n%s", tc.mode, got)
		}
		if res.RepoGuards == nil || res.RepoGuards.Mode != tc.mode || len(res.RepoGuards.Rewritten) != 0 || len(res.RepoGuards.Skipped) != tc.skipped {
			t.Fatalf("%s: repo_guards = %+v", tc.mode, res.RepoGuards)
		}
		if codes(warns)["REPO_GUARD"] != tc.warns {
			t.Fatalf("%s: warnings = %+v", tc.mode, warns)
		}
	}
}

func TestGateReportsUnrewritableGuardsAndShowsThemBeforePublishing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const unrewritable = "on: [pull_request]\njobs:\n  up:\n    if: contains(fromJSON('[\"acme/widget\"]'), github.repository)\n    runs-on: x\n"
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": unrewritable})
	h.init()
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	res, warns, err := h.eng.Gate(h.ctx, h.pair, "feat")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.RepoGuards.Unrewritable) != 1 || codes(warns)["REPO_GUARD_UNREWRITABLE"] != 1 {
		t.Fatalf("repo_guards = %+v, warnings = %+v", res.RepoGuards, warns)
	}
	pres, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pres.Plan.ID}); err != nil {
		t.Fatal(err)
	}
	if len(h.approver.asked) != 1 || !strings.Contains(h.approver.asked[0],
		"  guard    .github/workflows/up.yml job up skips in the shadow repo, so CI did not test it: ") {
		t.Fatalf("summary = %q", h.approver.asked)
	}
}

// forgeCandidate replaces feature name's recorded candidate with one on the
// same parent whose path is content, as a forged or older gate would have
// left it: gate branch and feature metadata both. It returns the new C.
func forgeCandidate(t *testing.T, h *harness, name, C, path, content string) string {
	t.Helper()
	sh := h.shadowGit()
	R, err := sh.MustResolve(h.ctx, C+"^")
	if err != nil {
		t.Fatal(err)
	}
	blob, err := sh.HashObject(h.ctx, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	cEntries, err := sh.ListTree(h.ctx, C)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sh.MakeTree(h.ctx, withFile(t, cEntries, path, blob))
	if err != nil {
		t.Fatal(err)
	}
	C2, err := sh.CommitTree(h.ctx, tree, []string{R}, "candidate", testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.UpdateRef(h.ctx, "refs/heads/ghostbranch-gate/"+name, C2); err != nil {
		t.Fatal(err)
	}
	forgeMeta(t, h, name, func(m *feature.Meta) { m.Gate.Candidate = C2 })
	return C2
}

// A candidate whose workflow differs from P in a way no rewrite produces,
// forged end to end (gate branch, metadata and plan), so that apply reaches
// assertChain's check of the tip against the candidate. TestRewriteOnly pins
// the rule itself.
func TestAssertPublishRefusesAForgedWorkflowDifference(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
	h.init()
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	pres, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), pres.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	pl.Fingerprint.GateCandidate = forgeCandidate(t, h, "feat", pl.Fingerprint.GateCandidate, ".github/workflows/up.yml", "on: push\njobs: {}\n")
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	before := h.publicRefs()
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	ce := wantCode(t, err, contract.Internal)
	if !reflect.DeepEqual(ce.Details["paths"], []string{".github/workflows/up.yml"}) {
		t.Fatalf("details = %+v", ce.Details)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) || len(h.forge.PRs(publicName)) != 0 {
		t.Fatal("a forged candidate reached the public repo")
	}
}

// Review Focus 5.
func TestGateWithBrokenOrDeletedWorkflow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
	h.init()
	h.startFeature("broken", map[string]string{".github/workflows/up.yml": "jobs: [\n"})
	h.startFeature("deleted", map[string]string{".github/workflows/up.yml": testutil.Delete})
	for _, f := range []string{"broken", "deleted"} {
		res, _, err := h.eng.Gate(h.ctx, h.pair, f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if len(res.RepoGuards.Rewritten) != 0 {
			t.Fatalf("%s: %+v", f, res.RepoGuards)
		}
		if f == "broken" {
			if got := showAt(h, res.Candidate, ".github/workflows/up.yml"); got != "jobs: [\n" {
				t.Fatalf("the broken workflow changed in C: %q", got)
			}
		}
		if _, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: f, Mode: "squash", Message: "m", PlanOnly: true}); err != nil {
			t.Fatalf("%s: promote: %v", f, err)
		}
	}
}

// Planning refinement 5: a gate recorded without the guard rewrite (the
// sub-project 3 binary) is not reused.
func TestPromoteRegatesACandidateBuiltWithoutTheRewrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
	h.init()
	h.setConfig("rewrite_repo_guards: rewrite", "rewrite_repo_guards: off")
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat"); err != nil {
		t.Fatal(err)
	}
	h.setConfig("rewrite_repo_guards: off", "rewrite_repo_guards: rewrite")
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Gate.Reused {
		t.Fatal("promote reused a gate whose candidate lacked the guard rewrite")
	}
}

// Planning refinement 5, with nothing else moved: the recorded candidate is
// what the sub-project 3 binary built (its guarded job skipped), so its tree
// is not the one this binary builds, and promote gates again.
func TestPromoteRegatesWhenTheCandidateLacksTheRewrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{".github/workflows/up.yml": guardedJob})
	h.init()
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	gr, _, err := h.eng.Gate(h.ctx, h.pair, "feat")
	if err != nil {
		t.Fatal(err)
	}
	forgeCandidate(t, h, "feat", gr.Candidate, ".github/workflows/up.yml", guardedJob)
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Gate.Reused {
		t.Fatal("promote reused a gate whose candidate lacked the guard rewrite")
	}
	if got := showAt(h, res.Gate.Candidate, ".github/workflows/up.yml"); !strings.Contains(got, "'acme/widget-shadow'") {
		t.Fatalf("the new candidate was not rewritten:\n%s", got)
	}
}

// In warn mode a rewritable guard isn't rewritten either, so its job skips
// in the shadow repo too: the approval summary lists it beside the guards
// that can't be rewritten.
func TestWarnModeSummaryListsEverySkippedGuard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	const unrewritable = "on: [pull_request]\njobs:\n  up:\n    if: contains(fromJSON('[\"acme/widget\"]'), github.repository)\n    runs-on: x\n"
	testutil.Commit(t, h.pubGit, "main", "guarded", map[string]string{
		".github/workflows/up.yml": guardedJob, ".github/workflows/up2.yml": unrewritable})
	h.init()
	h.setConfig("rewrite_repo_guards: rewrite", "rewrite_repo_guards: warn")
	h.startFeature("feat", map[string]string{"f.txt": "f\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat")
	if err != nil {
		t.Fatal(err)
	}
	if g := res.RepoGuards; len(g.Skipped) != 1 || len(g.Unrewritable) != 1 || len(g.Rewritten) != 0 || g.Skipped[0].Path != ".github/workflows/up.yml" {
		t.Fatalf("repo_guards = %+v", g)
	}
	pres, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || len(pres.Plan.CI.RepoGuards.Skipped) != 1 {
		t.Fatalf("plan = %+v %v", pres, err)
	}
	validateResult(t, "plan.schema.json", pres.Plan)
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pres.Plan.ID}); err != nil {
		t.Fatal(err)
	}
	if len(h.approver.asked) != 1 {
		t.Fatalf("asked %d times", len(h.approver.asked))
	}
	for _, want := range []string{
		"  guard    .github/workflows/up.yml job upstream-only skips in the shadow repo (ci.rewrite_repo_guards is warn), so CI did not test it: ",
		"  guard    .github/workflows/up2.yml job up skips in the shadow repo, so CI did not test it: ",
	} {
		if !strings.Contains(h.approver.asked[0], want) {
			t.Errorf("summary lacks %q:\n%s", want, h.approver.asked[0])
		}
	}
}
