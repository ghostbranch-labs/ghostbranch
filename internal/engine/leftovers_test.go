package engine_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// A stack promote without titles names every branch that needs one.
func TestStackPromoteNamesEveryMissingTitle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	testutil.Commit(t, h.shadowGit(), "a", "more a", map[string]string{"a2.txt": "a2\n"})
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	h.startOn("c", "b", map[string]string{"c.txt": "c\n"})
	testutil.Commit(t, h.shadowGit(), "c", "more c", map[string]string{"c2.txt": "c2\n"})
	for _, n := range []string{"a", "b", "c"} {
		h.gateFeature(n)
	}
	_, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b", "c"}, Mode: "preserve", PlanOnly: true})
	ce := wantCode(t, err, contract.MissingInput)
	if ce.Field != "title-for" || !reflect.DeepEqual(stringsOf(ce.Details["features"]), []string{"a", "c"}) {
		t.Fatalf("field %q, details %+v", ce.Field, ce.Details)
	}
}

// next never gates a branch above before an ungated one below it.
func TestGateFirstNamesTheLowestUngatedBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "b", engine.FeatureStartOptions{From: "a"})
	if err != nil || res.GateFirst() != "a" {
		t.Fatalf("start --from a: gate first %q, %v", res.GateFirst(), err)
	}
	h.gateFeature("a")
	res, _, err = h.eng.FeatureStart(h.ctx, h.pair, "c", engine.FeatureStartOptions{From: "a"})
	if err != nil || res.GateFirst() != "" {
		t.Fatalf("with a gated: gate first %q, %v", res.GateFirst(), err)
	}
	testutil.Commit(t, h.shadowGit(), "b", "work on b", map[string]string{"b.txt": "b\n"})
	testutil.Commit(t, h.shadowGit(), "a", "a moved", map[string]string{"a2.txt": "a2\n"})
	rr, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"})
	if err != nil || rr.GateFirst() != "a" {
		t.Fatalf("restack a: gate first %q, %v", rr.GateFirst(), err)
	}
}

// Sync says it moves a branch onto main only when that branch stays open.
func TestSyncSaysMovingOntoMainOnlyForBranchesThatStayOpen(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	for _, p := range prs {
		h.forge.SetPRMerged(publicName, p.Number)
	}
	var rec progress.Recorder
	h.eng.Progress = &rec
	h.syncOK(engine.SyncOptions{})
	if strings.Contains(rec.String(), "Moving b onto main") {
		t.Fatalf("b closed in the same sync, yet:\n%s", rec.String())
	}
	if rec.Running() {
		t.Fatalf("a step is still running after sync:\n%s", rec.String())
	}
	h2, prs2 := publishedStack(t, "a", "b")
	h2.squashMergeBottom(prs2)
	var rec2 progress.Recorder
	h2.eng.Progress = &rec2
	h2.syncOK(engine.SyncOptions{})
	if !strings.Contains(rec2.String(), "Moving b onto main") {
		t.Fatalf("b stays open, and the line is missing:\n%s", rec2.String())
	}
	if rec2.Running() {
		t.Fatalf("a step is still running after sync:\n%s", rec2.String())
	}
}

// pr_behind, and foreign commits counted in the report.
func TestReportCountsBehindAndForeign(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(time.Hour)
	h.publish("a")
	testutil.Commit(t, h.shadowGit(), "a", "review fix", map[string]string{"a.txt": "fixed\n"})
	testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	r := h.report("a")
	if r.State != feature.StatePRBehind || r.PR == nil || r.PR.Behind != 1 || r.PR.Foreign != 1 {
		t.Fatalf("report = %s %+v", r.State, r.PR)
	}
	validateResult(t, "feature-report.schema.json", r)
}

// A pushed commit and GitHub's "Update branch" merge are both foreign: the
// merge is a commit Ghostbranch never published.
func TestReportCountsAnUpdateBranchMergeAsForeign(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	testutil.Commit(t, h.pubGit, "main", "main moves on", map[string]string{"main2.txt": "m\n"})
	tree, err := h.pubGit.Run(h.ctx, "merge-tree", "--write-tree", h.pubTip("a"), h.pubTip("main"))
	if err != nil {
		t.Fatal(err)
	}
	tree, _, _ = strings.Cut(strings.TrimSpace(tree), "\n")
	merge, err := h.pubGit.CommitTree(h.ctx, tree, []string{h.pubTip("a"), h.pubTip("main")}, "Merge branch 'main' into a",
		testutil.Author(60), testutil.Author(60))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/a", merge); err != nil {
		t.Fatal(err)
	}
	r := h.report("a")
	if r.PR == nil || r.PR.Foreign != 2 {
		t.Fatalf("report = %s %+v", r.State, r.PR)
	}
}

// GitHub's restack after the PR below squash-merged keeps the same changes
// and messages: nothing on the branch above is foreign.
func TestReportCountsNothingForeignAfterGitHubsRestack(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	h.squashMergeBottom(prs)
	r := h.report("b")
	if r.PR == nil || r.PR.Foreign != 0 {
		t.Fatalf("report = %s %+v", r.State, r.PR)
	}
}

// A feature whose branch is gone has no PR comparison to report.
func TestReportHasNoPRComparisonWhenTheBranchIsGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/a"); err != nil {
		t.Fatal(err)
	}
	r := h.report("a")
	if r.State != feature.StateBranchGone || r.PR != nil {
		t.Fatalf("report = %s %+v", r.State, r.PR)
	}
}

// Once the branch its PR merged into is gone (the feature below merged and
// closed, and its record moved this one onto main), the warning names the
// base its record has now, not only the deleted branch.
func TestMergedIntoParentWarningNamesTheRecordsBaseOnceTheBranchIsGone(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	h.forge.StacksUnavailable = true
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil
	prs := res.Applied.PRs
	h.forge.SetPRMerged(publicName, prs[1].Number)
	h.forge.SetPRMerged(publicName, prs[0].Number)
	h.syncOK(engine.SyncOptions{}) // closes a, and moves b onto main
	if err := h.pubGit.DeleteRef(h.ctx, "refs/heads/a"); err != nil {
		t.Fatal(err)
	}
	_, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.FeatureNotClosed)
	if len(ws) != 1 || ws[0].Details["base"] != "a" || ws[0].Details["record_base"] != "main" ||
		!strings.Contains(ws[0].Message, "public branch main") || !strings.Contains(ws[0].Message, "ghostbranch feature close b") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// A record from before 0.11 (no published head or patch-ids) isn't
// checked for foreign commits: sync skips it too, and without them every
// commit on the PR would read as foreign.
func TestReportSkipsTheForeignCheckForARecordFromBefore011(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	forgeMeta(t, h, "a", func(m *feature.Meta) {
		p := *m.Promoted
		p.PublishedHead, p.PatchIDs = "", nil
		m.Promoted = &p
	})
	testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	if r := h.report("a"); r.PR == nil || r.PR.Foreign != 0 {
		t.Fatalf("report = %s %+v", r.State, r.PR)
	}
	if _, warns := h.syncOK(engine.SyncOptions{}); len(warningsFor(warns, contract.PublicBranchChanged)) != 0 {
		t.Fatalf("sync warns: %+v", warns)
	}
}
