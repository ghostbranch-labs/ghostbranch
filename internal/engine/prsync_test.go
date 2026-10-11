package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// prOf is r's PR counts, failing when the survey left them out.
func prOf(t *testing.T, r engine.FeatureReport) engine.PRSync {
	t.Helper()
	if r.PR == nil {
		t.Fatalf("%s (%s, pr_state %q) has no pr counts", r.Name, r.State, r.PRState)
	}
	return *r.PR
}

// A published feature with new commits is pr_behind, and the report says
// how many; merged, it has no PR counts. The report matches its schema.
func TestSurveyPRBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("f")
	r := h.report("f")
	if r.State != feature.StatePublished || prOf(t, r) != (engine.PRSync{}) {
		t.Fatalf("just published: state %s pr %+v", r.State, r.PR)
	}
	testutil.Commit(t, h.shadowGit(), "f", "review fix 1", map[string]string{"fix1.txt": "1\n"})
	testutil.Commit(t, h.shadowGit(), "f", "review fix 2", map[string]string{"fix2.txt": "2\n"})
	r = h.report("f")
	if r.State != feature.StatePRBehind || !r.Moved || prOf(t, r) != (engine.PRSync{Behind: 2}) {
		t.Fatalf("after two commits: state %s moved %v pr %+v", r.State, r.Moved, r.PR)
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "feature-report.schema.json", b)
	h.forge.SetPRMerged(publicName, pr)
	if r = h.report("f"); r.State != feature.StateMergedMoved || r.PR != nil {
		t.Fatalf("merged: state %s pr %+v", r.State, r.PR)
	}
}

// Rebased onto a newer main, the feature is behind by its own commits
// only, never by main's.
func TestSurveyPRBehindLeavesOutMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("f")
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	h.syncOK(engine.SyncOptions{})
	sg, main := h.shadowGit(), h.head("main")
	out, err := sg.Run(h.ctx, "merge-tree", "--write-tree", main, h.head("f"))
	if err != nil {
		t.Fatal(err)
	}
	tree, _, _ := strings.Cut(out, "\n")
	rebased, err := sg.CommitTree(h.ctx, tree, []string{main}, "f, rebased", testutil.Author(70), testutil.Author(70))
	if err != nil {
		t.Fatal(err)
	}
	if err := sg.UpdateRef(h.ctx, "refs/heads/f", rebased); err != nil {
		t.Fatal(err)
	}
	if r := h.report("f"); r.State != feature.StatePRBehind || prOf(t, r).Behind != 1 {
		t.Fatalf("rebased: state %s pr %+v", r.State, r.PR)
	}
}

// In a stack, a child restacked onto its parent's new commit is behind by
// its own rebased commit, not by the parent's.
func TestSurveyPRBehindLeavesOutTheFeaturesBelow(t *testing.T) {
	t.Parallel()
	h, _ := publishedStack(t, "a", "b")
	testutil.Commit(t, h.shadowGit(), "a", "more a", map[string]string{"a2.txt": "a2\n"})
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b"} {
		if r := h.report(n); r.State != feature.StatePRBehind || prOf(t, r) != (engine.PRSync{Behind: 1}) {
			t.Errorf("%s: state %s pr %+v", n, r.State, r.PR)
		}
	}
}
