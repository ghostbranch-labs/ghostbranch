package engine_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// startOn starts name on top of from (feature start --from) and adds one
// commit to it, as startFeature does. It returns the new head.
func (h *harness) startOn(name, from string, files map[string]string) string {
	h.t.Helper()
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, name, engine.FeatureStartOptions{From: from}); err != nil {
		h.t.Fatalf("feature start %s --from %s: %v", name, from, err)
	}
	return testutil.Commit(h.t, h.shadowGit(), name, "work on "+name, files)
}

// head is a shadow branch's head.
func (h *harness) head(branch string) string {
	h.t.Helper()
	sha, err := h.shadowGit().MustResolve(h.ctx, "refs/heads/"+branch)
	if err != nil {
		h.t.Fatal(err)
	}
	return sha
}

// pushesBy counts the pushes op made, and fails the test if any went
// anywhere but the shadow repo.
func (h *harness) pushesBy(op string) int {
	h.t.Helper()
	n := 0
	for _, p := range h.pushes {
		if p.op != op {
			continue
		}
		if p.remote != "shadow" {
			h.t.Fatalf("%s pushed to %s: %+v", op, p.remote, p)
		}
		n++
	}
	return n
}

// fileAt reads path at rev in the shadow repo; ok is false when it isn't there.
func (h *harness) fileAt(rev, path string) (string, bool) {
	h.t.Helper()
	b, ok, err := h.shadowGit().ShowFile(h.ctx, rev, path)
	if err != nil {
		h.t.Fatal(err)
	}
	return string(b), ok
}

// threeDeep starts feat-a on main, feat-b on feat-a and feat-c on feat-b,
// one commit each.
func threeDeep(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	h.startOn("feat-c", "feat-b", map[string]string{"c.txt": "c\n"})
	return h
}

func TestFoldFastForwardsTheParentAndClosesTheChild(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	h.gateFeature("feat-b")
	if h.gatePR("feat-b") == 0 {
		t.Fatal("the gate left no draft PR for feat-b")
	}
	aBefore, bHead := h.head("feat-a"), h.head("feat-b")
	cFork := h.meta("feat-c").ForkedAt
	res, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-b"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Parent != "feat-a" || res.From != aBefore || res.To != bHead || !reflect.DeepEqual(res.Reparented, []string{"feat-c"}) || res.GatePR == nil {
		t.Fatalf("fold = %+v", res)
	}
	validateResult(t, "feature-fold.schema.json", res)
	if h.head("feat-a") != bHead {
		t.Fatal("feat-a did not fast-forward to feat-b's head")
	}
	refs := h.shadowRefs()
	for _, ref := range []string{"refs/heads/feat-b", "refs/heads/ghostbranch-gate/feat-b", "refs/ghostbranch/features/feat-b"} {
		if _, ok := refs[ref]; ok {
			t.Fatalf("%s survived the fold", ref)
		}
	}
	if h.gatePR("feat-b") != 0 {
		t.Fatal("feat-b's gate PR is still open")
	}
	// feat-c is stacked on feat-a now; its fork point, a commit of feat-b,
	// is on feat-a.
	if c := h.meta("feat-c"); c.Parent != "feat-a" || c.ForkedAt != cFork {
		t.Fatalf("feat-c = parent %q, forked_at %s", c.Parent, c.ForkedAt)
	}
	// One atomic push did it all.
	if n := h.pushesBy("feature fold"); n != 1 {
		t.Fatalf("fold made %d pushes, want 1", n)
	}
	es := h.ledger()
	last := es[len(es)-1]
	if last.Op != "feature fold" || last.Feature != "feat-b" || last.Refs["refs/heads/feat-b"] != bHead || !strings.HasPrefix(last.Summary, "folded feat-b into feat-a") {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestFoldRefusesWhenTheParentMovedOnThenFoldsAfterTheSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	fork := h.meta("feat-b").ForkedAt
	aTip := testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	before := h.shadowRefs()
	_, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-b"})
	ce := wantCode(t, err, contract.FoldNeedsRebase)
	if ce.Details["parent"] != "feat-a" || ce.Details["parent_head"] != aTip || ce.Details["forked_at"] != fork ||
		!reflect.DeepEqual(ce.Details["steps"], engine.RebaseSteps("shadow", "feat-b", "feat-a", fork)) {
		t.Fatalf("details = %+v", ce.Details)
	}
	if h.pushesBy("feature fold") != 0 || !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("a refused fold changed the shadow repo")
	}
	// The person runs the steps: feat-b's commit, rebased onto feat-a's tip.
	sg := h.shadowGit()
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-b", aTip); err != nil {
		t.Fatal(err)
	}
	rebased := testutil.Commit(t, sg, "feat-b", "work on feat-b", map[string]string{"b.txt": "b\n"})
	res, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-b"})
	if err != nil || res.To != rebased || h.head("feat-a") != rebased {
		t.Fatalf("fold after the rebase = %+v %v", res, err)
	}
}

// Review Focus 4.
func TestFoldLeaseRefusesAMovedParent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	bMeta := h.shadowRefs()["refs/ghostbranch/features/feat-b"]
	sg := h.shadowGit()
	var meanwhile string
	h.eng.OnPush = func(op, _ string, _ []string) {
		if op == "feature fold" && meanwhile == "" {
			// Someone pushes to feat-a after fold read it.
			meanwhile = testutil.Commit(t, sg, "feat-a", "meanwhile", map[string]string{"m.txt": "m\n"})
		}
	}
	_, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-b"})
	ce := wantCode(t, err, contract.FeatureMoved)
	if ce.Details["ref"] != "refs/heads/feat-a" || !strings.Contains(ce.Message, "folded into feat-a") {
		t.Fatalf("error = %+v", ce)
	}
	refs := h.shadowRefs()
	if refs["refs/heads/feat-a"] != meanwhile || refs["refs/ghostbranch/features/feat-b"] != bMeta || refs["refs/heads/feat-b"] == "" {
		t.Fatalf("the fold changed something after its lease failed: %+v", refs)
	}
}

func TestFoldRefusesAFeatureOnMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	_, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-a"})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "feature" {
		t.Fatalf("field = %q", ce.Field)
	}
}

func TestFoldPlanChangesNothing(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, _, err := h.eng.FeatureFold(h.ctx, h.pair, engine.FoldOptions{Feature: "feat-b"})
	if err != nil || res.Preview == nil || len(res.Deleted) != 0 {
		t.Fatalf("fold --plan = %+v %v", res, err)
	}
	validateResult(t, "feature-fold.schema.json", res)
	if h.pushesBy("feature fold") != 0 || !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("fold --plan changed the shadow repo")
	}
}

func TestRestackRebasesEachBranchAboveInOrder(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	bOld, cOld := h.head("feat-b"), h.head("feat-c")
	aNew := testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"})
	if err != nil {
		t.Fatal(err)
	}
	validateResult(t, "feature-restack.schema.json", res)
	if len(res.Restacked) != 2 || res.Restacked[0].Feature != "feat-b" || res.Restacked[1].Feature != "feat-c" || len(res.Pending) != 0 {
		t.Fatalf("restack = %+v", res)
	}
	bNew, cNew := h.head("feat-b"), h.head("feat-c")
	if bNew == bOld || cNew == cOld || res.Restacked[0].From != bOld || res.Restacked[0].To != bNew || res.Restacked[1].To != cNew {
		t.Fatalf("heads: b %s -> %s, c %s -> %s; result %+v", bOld, bNew, cOld, cNew, res.Restacked)
	}
	if b := h.meta("feat-b"); b.Parent != "feat-a" || b.ForkedAt != aNew {
		t.Fatalf("feat-b = %+v", b)
	}
	if c := h.meta("feat-c"); c.Parent != "feat-b" || c.ForkedAt != bNew {
		t.Fatalf("feat-c = %+v", c)
	}
	for _, f := range []string{"a.txt", "a2.txt", "b.txt", "c.txt"} {
		if _, ok := h.fileAt(cNew, f); !ok {
			t.Fatalf("restacked feat-c lacks %s", f)
		}
	}
	// One atomic push per branch, to the shadow repo only.
	if n := h.pushesBy("feature restack"); n != 2 {
		t.Fatalf("restack made %d pushes, want 2", n)
	}
	// Run again, it finds nothing to do and pushes nothing.
	again, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"})
	if err != nil || len(again.Restacked) != 0 || h.pushesBy("feature restack") != 2 {
		t.Fatalf("second restack = %+v %v", again, err)
	}
}

// Review Focus 4.
func TestRestackStopsAtAConflictWithSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sg := h.shadowGit()
	h.startFeature("feat-a", map[string]string{"shared.txt": "one\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	h.startOn("feat-c", "feat-b", map[string]string{"shared.txt": "one\nc\n"})
	bOld, cOld := h.head("feat-b"), h.head("feat-c")
	cFork := h.meta("feat-c").ForkedAt
	cMeta := h.shadowRefs()["refs/ghostbranch/features/feat-c"]
	testutil.Commit(t, sg, "feat-a", "change the shared line", map[string]string{"shared.txt": "two\n"})
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"})
	ce := wantCode(t, err, contract.RebaseConflict)
	if ce.Details["feature"] != "feat-c" || ce.Details["parent"] != "feat-b" || !reflect.DeepEqual(ce.Details["files"], []string{"shared.txt"}) ||
		!reflect.DeepEqual(ce.Details["steps"], engine.RebaseSteps("shadow", "feat-c", "feat-b", cFork)) ||
		!reflect.DeepEqual(ce.Details["again"], []string{"feature", "restack", "feat-b"}) {
		t.Fatalf("details = %+v", ce.Details)
	}
	// feat-b, before the conflict, stays restacked; feat-c is untouched.
	if res == nil || len(res.Restacked) != 1 || res.Restacked[0].Feature != "feat-b" || !reflect.DeepEqual(res.Pending, []string{"feat-c"}) {
		t.Fatalf("restack = %+v", res)
	}
	bNew := h.head("feat-b")
	if bNew == bOld || h.meta("feat-b").ForkedAt != h.head("feat-a") {
		t.Fatal("feat-b was not kept restacked")
	}
	if h.head("feat-c") != cOld || h.shadowRefs()["refs/ghostbranch/features/feat-c"] != cMeta {
		t.Fatal("the conflicting branch was changed")
	}
	// The person rebases feat-c by hand onto the new feat-b, resolving the
	// conflict, pushes it, and runs the restack details.again names.
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-c", bNew); err != nil {
		t.Fatal(err)
	}
	resolved := testutil.Commit(t, sg, "feat-c", "work on feat-c", map[string]string{"shared.txt": "two\nc\n"})
	res, _, err = h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-b"})
	if err != nil || len(res.Restacked) != 1 || res.Restacked[0].From != resolved || res.Restacked[0].To != resolved {
		t.Fatalf("restack after the fix = %+v %v", res, err)
	}
	if h.meta("feat-c").ForkedAt != bNew {
		t.Fatal("the restack did not record feat-c's new fork point")
	}
}

func TestRestackLeaseLeavesAMovedBranchAlone(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	sg := h.shadowGit()
	testutil.Commit(t, sg, "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	var meanwhile string
	h.eng.OnPush = func(op, _ string, specs []string) {
		for _, s := range specs {
			if op == "feature restack" && strings.HasSuffix(s, ":refs/heads/feat-c") && meanwhile == "" {
				meanwhile = testutil.Commit(t, sg, "feat-c", "meanwhile", map[string]string{"m.txt": "m\n"})
			}
		}
	}
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"})
	ce := wantCode(t, err, contract.RefMoved)
	if ce.Details["feature"] != "feat-c" || len(res.Restacked) != 1 || h.head("feat-c") != meanwhile {
		t.Fatalf("restack = %+v %+v", res, ce)
	}
}

func TestRestackOntoMainDropsTheParentsCommits(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	main := h.head("main")
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-b", Onto: "main"})
	if err != nil || len(res.Restacked) != 2 || res.Restacked[0].Parent != "main" || res.Restacked[0].ForkedAt != "" {
		t.Fatalf("restack --onto main = %+v %v", res, err)
	}
	bNew, cNew := h.head("feat-b"), h.head("feat-c")
	if _, ok := h.fileAt(bNew, "a.txt"); ok {
		t.Fatal("feat-b still carries feat-a's commit")
	}
	if _, ok := h.fileAt(cNew, "a.txt"); ok {
		t.Fatal("feat-c still carries feat-a's commit")
	}
	if _, ok := h.fileAt(cNew, "b.txt"); !ok {
		t.Fatal("feat-c lost feat-b's commit")
	}
	if b := h.meta("feat-b"); b.Parent != "" || b.ForkedAt != "" || b.Base != main {
		t.Fatalf("feat-b = parent %q, forked_at %q, base %s", b.Parent, b.ForkedAt, b.Base)
	}
	if c := h.meta("feat-c"); c.Parent != "feat-b" || c.ForkedAt != bNew {
		t.Fatalf("feat-c = %+v", c)
	}
	// feat-a has no children now, so it can be abandoned.
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-a", false); err != nil {
		t.Fatal(err)
	}
}

func TestRestackOntoRefusesALoopAndANoOp(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	for _, c := range []struct{ feature, onto string }{
		{"feat-a", "feat-c"}, // feat-c is above feat-a
		{"feat-a", "feat-a"},
		{"feat-a", "main"}, // already on main
	} {
		_, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: c.feature, Onto: c.onto})
		if ce := wantCode(t, err, contract.BadFlag); ce.Field != "onto" {
			t.Fatalf("%s --onto %s: field %q", c.feature, c.onto, ce.Field)
		}
	}
	if h.pushesBy("feature restack") != 0 {
		t.Fatal("a refused restack pushed")
	}
}

func TestRestackPlanPushesNothing(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"})
	if err != nil || res.Preview == nil || len(res.Restacked) != 2 || len(res.Preview.Ops) != 4 {
		t.Fatalf("restack --plan = %+v %v", res, err)
	}
	validateResult(t, "feature-restack.schema.json", res)
	if h.pushesBy("feature restack") != 0 || !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("restack --plan changed the shadow repo")
	}
}

// Review Focus 2.
func TestAbandonRefusesWithChildren(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	aMeta := h.shadowRefs()["refs/ghostbranch/features/feat-a"]
	_, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-a", false)
	ce := wantCode(t, err, contract.HasChildren)
	if ce.Field != "feature" || !reflect.DeepEqual(ce.Details["children"], []string{"feat-b"}) || ce.Details["op"] != "abandon" {
		t.Fatalf("error = %+v", ce)
	}
	if h.shadowRefs()["refs/ghostbranch/features/feat-a"] != aMeta || h.meta("feat-a").Abandoned {
		t.Fatal("a refused abandon changed the record")
	}
	// An abandoned child doesn't stop an abandon of its parent...
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-b", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-a", false); err != nil {
		t.Fatalf("abandon with only an abandoned child: %v", err)
	}
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-a", true); err != nil {
		t.Fatalf("undo: %v", err)
	}
	// ...but it does stop a close, before any console is asked: closing
	// feat-a would delete the branch feat-b is built on.
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "feat-a")
	ce = wantCode(t, err, contract.HasChildren)
	if ce.Details["op"] != "close" || !reflect.DeepEqual(ce.Details["children"], []string{"feat-b"}) || len(h.approver.asked) != 0 {
		t.Fatalf("close = %+v, console asked %d time(s)", ce, len(h.approver.asked))
	}
	if _, ok := h.shadowRefs()["refs/heads/feat-a"]; !ok {
		t.Fatal("a refused close deleted feat-a")
	}
	// Moving feat-b onto main frees feat-a.
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-b", Onto: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-a"); err != nil {
		t.Fatalf("close after the restack: %v", err)
	}
}

// A feature on main that still records a fork point (sync re-parents a
// child onto main that way when its parent merges) moves --onto main with
// only its own commits: forked_at..head, not everything main lacks.
func TestRestackOntoMainHonoursAForkPointOnAMainParent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	fork := h.meta("feat-b").ForkedAt
	h.editRemoteMeta("feat-b", func(m *feature.Meta) { m.Parent = "" })
	if b := h.meta("feat-b"); b.Parent != "" || b.ForkedAt != fork {
		t.Fatalf("setup: feat-b = %+v", b)
	}
	main := h.head("main")
	res, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-b", Onto: "main"})
	if err != nil || len(res.Restacked) != 1 || res.Restacked[0].Parent != "main" || res.Restacked[0].ForkedAt != "" {
		t.Fatalf("restack --onto main = %+v %v", res, err)
	}
	bNew := h.head("feat-b")
	if _, ok := h.fileAt(bNew, "a.txt"); ok {
		t.Fatal("feat-b still carries feat-a's commit")
	}
	if _, ok := h.fileAt(bNew, "b.txt"); !ok {
		t.Fatal("feat-b lost its own commit")
	}
	if b := h.meta("feat-b"); b.Parent != "" || b.ForkedAt != "" || b.Base != main {
		t.Fatalf("feat-b = parent %q, forked_at %q, base %s", b.Parent, b.ForkedAt, b.Base)
	}
}
