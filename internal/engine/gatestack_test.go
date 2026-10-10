package engine_test

import (
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// rOf is a gate record's rebased tip R.
func rOf(t *testing.T, g *feature.Gate) string {
	t.Helper()
	if g == nil || len(g.Commits) == 0 {
		t.Fatalf("gate %+v has no rebased commits", g)
	}
	return g.Commits[len(g.Commits)-1].Rebased
}

// Review Focus 1's precondition: b's gate tests a+b and scans only b.
func TestStackedGateTestsTheChainAndScansOnlyItsOwnCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("JIRA-[0-9]+")
	h.startFeature("feat-a", map[string]string{"a.txt": "JIRA-1\n"})
	aHead := testutil.Commit(t, h.shadowGit(), "feat-a", "fix JIRA-2", map[string]string{"a2.txt": "a2\n"})
	bHead := h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-a")
	_ = wantCode(t, err, contract.LeakFound)

	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-b")
	if err != nil || res.Checks != feature.ChecksPassed || res.Leak != feature.LeakClean || len(res.Findings) != 0 {
		t.Fatalf("gate feat-b = %+v %v", res, err)
	}
	validateResult(t, "gate.schema.json", res)
	if res.Parent != "feat-a" || len(res.Commits) != 1 || res.Commits[0].Source != bHead {
		t.Fatalf("gate feat-b: parent %q, commits %+v", res.Parent, res.Commits)
	}
	// CI ran on public main + a + b.
	for _, f := range []string{"a.txt", "a2.txt", "b.txt"} {
		if _, ok := h.fileAt(res.Candidate, f); !ok {
			t.Fatalf("the candidate lacks %s", f)
		}
	}
	g := h.meta("feat-b").Gate
	if g.Parent != "feat-a" || g.ForkedAt != aHead || !reflect.DeepEqual(g.Below, []feature.StackHead{{Feature: "feat-a", Head: aHead}}) ||
		len(g.Commits) != 1 || g.Commits[0].Source != bHead || g.BaseTree == "" {
		t.Fatalf("record = %+v", g)
	}
	// The base is feat-a's gate's R, reused: replayed a's two commits on public main.
	if g.Base != rOf(t, h.meta("feat-a").Gate) || g.Base != res.Base {
		t.Fatalf("base %s, want feat-a's R %s", g.Base, rOf(t, h.meta("feat-a").Gate))
	}
	pm, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if got, _ := h.shadowGit().MustResolve(h.ctx, g.Base+"~2"); got != pm {
		t.Fatalf("the base doesn't sit on public main: %s", got)
	}

	// b's own leak is found, and only b's.
	bad := testutil.Commit(t, h.shadowGit(), "feat-b", "fix JIRA-7", map[string]string{"b.txt": "b2\n"})
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-b")
	_ = wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 1 || !reflect.DeepEqual(res.Findings[0].Commits, []string{bad}) {
		t.Fatalf("findings = %+v", res.Findings)
	}
}

// History modes are per feature: preserve scans b's own history, squash
// only b's final changes; a's history is never scanned under b.
func TestStackedGateScansItsOwnHistoryPerMode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("JIRA-[0-9]+")
	h.startFeature("feat-a", map[string]string{"a.txt": "JIRA-1\n"})
	testutil.Commit(t, h.shadowGit(), "feat-a", "drop it", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "JIRA-5\n"})
	testutil.Commit(t, h.shadowGit(), "feat-b", "drop it", map[string]string{"b.txt": "b\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-b") // preserve, the default
	_ = wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 1 || res.Findings[0].Path != "b.txt" || res.ScannedAs != "preserve" {
		t.Fatalf("preserve findings = %+v", res.Findings)
	}
	h.setConfig("default_mode: preserve", "default_mode: squash")
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-b")
	if err != nil || res.Leak != feature.LeakClean || res.ScannedAs != "squash" {
		t.Fatalf("squash gate = %+v %v", res, err)
	}
}

func TestGateUpAThreeDeepStack(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	cHead := h.head("feat-c")
	for _, n := range []string{"feat-a", "feat-b", "feat-c"} {
		h.gateFeature(n)
	}
	a, b, c := h.meta("feat-a").Gate, h.meta("feat-b").Gate, h.meta("feat-c").Gate
	// Each gate builds on the gate below it.
	if b.Base != rOf(t, a) || c.Base != rOf(t, b) || a.Base != "" {
		t.Fatalf("bases: a %q, b %s (a's R %s), c %s (b's R %s)", a.Base, b.Base, rOf(t, a), c.Base, rOf(t, b))
	}
	if len(c.Below) != 2 || c.Below[0].Feature != "feat-a" || c.Below[1].Feature != "feat-b" ||
		len(c.Commits) != 1 || c.Commits[0].Source != cHead {
		t.Fatalf("feat-c's record = %+v", c)
	}
	for _, f := range []string{"a.txt", "b.txt", "c.txt"} {
		if _, ok := h.fileAt(c.Candidate, f); !ok {
			t.Fatalf("feat-c's candidate lacks %s", f)
		}
	}
	for _, n := range []string{"feat-a", "feat-b", "feat-c"} {
		if s := h.report(n).State; s != feature.StateGatePassed {
			t.Fatalf("%s = %s", n, s)
		}
	}
	// A commit on a makes a's gate stale, and b and c parent_stale (c
	// through b), their gates no longer fresh.
	aNew := testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	ra, rb, rc := h.report("feat-a"), h.report("feat-b"), h.report("feat-c")
	if ra.State != feature.StateInReview || rb.State != feature.StateParentStale || rc.State != feature.StateParentStale ||
		rb.Gate.Fresh || rc.Gate.Fresh {
		t.Fatalf("after a moved: a %s, b %s (fresh %v), c %s (fresh %v)", ra.State, rb.State, rb.Gate.Fresh, rc.State, rc.Gate.Fresh)
	}
	// Gating c now replays the new a below it, gated or not.
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-c")
	if err != nil || res.Base == c.Base {
		t.Fatalf("gate feat-c = %+v %v", res, err)
	}
	if _, ok := h.fileAt(res.Candidate, "a2.txt"); !ok {
		t.Fatal("feat-c's candidate lacks a's new commit")
	}
	if g := h.meta("feat-c").Gate; g.Below[0].Head != aNew {
		t.Fatalf("below = %+v", g.Below)
	}
	// Restacking a clears parent_stale up the stack.
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"}); err != nil {
		t.Fatal(err)
	}
	if rb, rc := h.report("feat-b"), h.report("feat-c"); rb.State == feature.StateParentStale || rc.State == feature.StateParentStale {
		t.Fatalf("after restack: b %s, c %s", rb.State, rc.State)
	}
}

func TestStackedGateRefusesARewrittenParent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sg := h.shadowGit()
	h.startFeature("feat-a", map[string]string{"a.txt": "1\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	fork := h.meta("feat-b").ForkedAt
	// feat-a is rewritten: amended, so it no longer holds feat-b's fork point.
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-a", h.head("main")); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, sg, "feat-a", "rewritten", map[string]string{"a.txt": "2\n"})
	if s := h.report("feat-b").State; s != feature.StateParentStale {
		t.Fatalf("feat-b = %s", s)
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-b")
	ce := wantCode(t, err, contract.ParentRewritten)
	if ce.Details["feature"] != "feat-b" || ce.Details["parent"] != "feat-a" || ce.Details["forked_at"] != fork || ce.Details["reason"] != "parent_rewritten" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("feat-b")
	if got, _ := h.fileAt(h.meta("feat-b").Gate.Candidate, "a.txt"); got != "2\n" {
		t.Fatalf("feat-b's candidate has a.txt %q, want the rewritten 2", got)
	}
}

func TestPendingStackedJobIsNotReusedAfterTheParentMoves(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	first, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-b")
	if err != nil || first.Job == nil {
		t.Fatalf("detach = %+v %v", first, err)
	}
	testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	second, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-b")
	if err != nil || second.Job == nil || *second.Job == *first.Job {
		t.Fatalf("the job built on the old feat-a was reused: %+v %v", second, err)
	}
}

// Until promote takes a whole stack (Task 6), a stacked feature is never
// published on its own: its gate scanned only its own commits.
func TestPromoteRefusesAStackedFeatureAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	h.gateFeature("feat-b")
	_, err := promote(h, engine.PromoteOptions{Feature: "feat-b", Mode: "squash", Message: "Add b", PlanOnly: true})
	_ = wantCode(t, err, contract.NotAStack)
}

// A feature moved onto main that still records a fork point (sync re-parents
// a feature when its parent merges) replays only forked_at..head onto public
// main, and is refused when it no longer contains that fork point.
func TestGateReplaysOnlyItsOwnCommitsAfterReparentingToMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	bHead := h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	fork := h.meta("feat-b").ForkedAt
	h.editRemoteMeta("feat-b", func(m *feature.Meta) { m.Parent = "" })
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-b")
	if err != nil || res.Checks != feature.ChecksPassed || res.Parent != "" || len(res.Commits) != 1 || res.Commits[0].Source != bHead {
		t.Fatalf("gate feat-b = %+v %v", res, err)
	}
	pm, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if res.Base != pm {
		t.Fatalf("base %s, want public main %s", res.Base, pm)
	}
	if _, ok := h.fileAt(res.Candidate, "a.txt"); ok {
		t.Fatal("the candidate replayed feat-a's commit too")
	}
	if g := h.meta("feat-b").Gate; g.Parent != "" || g.ForkedAt != "" || g.Base != "" || g.Below != nil {
		t.Fatalf("record = %+v", g)
	}
	// The branch is rebuilt by hand without its fork point.
	sg := h.shadowGit()
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-b", h.head("main")); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, sg, "feat-b", "redo", map[string]string{"b.txt": "b2\n"})
	_, _, err = h.eng.Gate(h.ctx, h.pair, "feat-b")
	ce := wantCode(t, err, contract.ParentRewritten)
	if ce.Details["reason"] != "fork_point_lost" || ce.Details["forked_at"] != fork {
		t.Fatalf("details = %+v", ce.Details)
	}
}
