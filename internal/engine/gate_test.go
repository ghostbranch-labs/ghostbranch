package engine_test

import (
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func paths(t *testing.T, h *harness, rev string) []string {
	t.Helper()
	return pathsIn(t, h, h.shadowGit(), rev)
}

// mirrorPaths lists a revision in the local mirror; the published tree T is
// never pushed, so it exists only there.
func mirrorPaths(t *testing.T, h *harness, rev string) []string {
	t.Helper()
	return pathsIn(t, h, gitx.New(h.eng.WS.MirrorDir(h.pair)), rev)
}

func pathsIn(t *testing.T, h *harness, g *gitx.Git, rev string) []string {
	t.Helper()
	es, err := g.ListTree(h.ctx, rev)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Path)
	}
	return out
}

func TestGatePassesAndRecordsTheResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	head := h.startFeature("feat-x", map[string]string{"feature.txt": "new\n", ".ghostbranch/notes.md": "private\n"})
	before := h.publicRefs()

	var rec progress.Recorder
	h.eng.Progress = &rec
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if wantNote := "CI runs on shadow PR #1: https://github.example/" + shadowName + "/pull/1 (gate candidate " + res.Candidate[:12] + ")"; !slices.Contains(rec.Notes(), wantNote) {
		t.Errorf("notes lack %q:\n%s", wantNote, rec.String())
	}
	if !slices.Contains(rec.Steps(), "Waiting for CI to start") {
		t.Errorf("no CI step:\n%s", rec.String())
	}
	if err != nil || res.Checks != feature.ChecksPassed || res.Leak != feature.LeakClean || res.FeatureHead != head {
		t.Fatalf("gate = %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/ghostbranch-gate/feat-x"); got != res.Candidate {
		t.Fatalf("gate branch %s != candidate %s", got, res.Candidate)
	}
	cPaths := paths(t, h, res.Candidate)
	for _, want := range []string{".ghostbranch.yml", ".ghostbranch/README.md", "feature.txt"} {
		if !contains(cPaths, want) {
			t.Errorf("candidate is missing %s: %v", want, cPaths)
		}
	}
	if contains(cPaths, ".ghostbranch/notes.md") {
		t.Error("the feature's private notes reached the CI candidate")
	}
	tPaths := mirrorPaths(t, h, res.PublishedTree)
	if !reflect.DeepEqual(tPaths, []string{".github/workflows/ci.yml", "README.md", "feature.txt"}) {
		t.Fatalf("published tree = %v", tPaths)
	}
	prs := h.forge.PRs(shadowName)
	if len(prs) != 1 || !prs[0].Draft || prs[0].Head != "ghostbranch-gate/feat-x" || prs[0].Base != "main" || res.ShadowPR == nil || *res.ShadowPR != 1 {
		t.Fatalf("shadow PRs = %+v", prs)
	}
	raw, _, _ := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	m, _ := feature.Decode(raw)
	if !m.Gate.Passed() || m.Gate.Candidate != res.Candidate || m.Gate.PublishedTree != res.PublishedTree {
		t.Fatalf("recorded gate = %+v", m.Gate)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("gate touched the public repo")
	}

	// A second gate after another commit reuses the same draft PR.
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"feature.txt": "newer\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	if n := len(h.forge.PRs(shadowName)); n != 1 {
		t.Fatalf("want 1 gate PR, got %d", n)
	}
}

func TestGateRebasesOntoTheLatestPublicMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	newMain := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.PublicMain != newMain {
		t.Fatalf("gate = %+v %v", res, err)
	}
	if !contains(mirrorPaths(t, h, res.PublishedTree), "up.txt") {
		t.Fatal("published tree must include upstream changes")
	}
}

func TestGateRebaseConflict(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"README.md": "feature version\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"README.md": "upstream version\n"})
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.RebaseConflict)
	if !reflect.DeepEqual(ce.Details["files"], []string{"README.md"}) {
		t.Fatalf("details = %v", ce.Details)
	}
	if _, ok, _ := h.shadowGit().Resolve(h.ctx, "refs/heads/ghostbranch-gate/feat-x"); ok {
		t.Fatal("no candidate may be pushed on conflict")
	}
}

func TestGateLeakFailsBeforeCI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	h.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if res.Leak != feature.LeakFound || res.Checks != feature.ChecksNotRun || len(res.Findings) == 0 || calls.Load() != 0 {
		t.Fatalf("gate = %+v, ListChecks calls = %d", res, calls.Load())
	}
	if res.ShadowPR != nil {
		t.Fatalf("no PR was opened, so shadow_pr must be nil, got %d", *res.ShadowPR)
	}
	if _, ok, _ := h.shadowGit().Resolve(h.ctx, "refs/heads/ghostbranch-gate/feat-x"); ok {
		t.Fatal("a leaking candidate must not be pushed to CI")
	}
}

func TestGateCheckOutcomes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		checks func(h *harness) func(string, string) []forge.Check
		want   contract.Code
	}{
		{"failed", func(*harness) func(string, string) []forge.Check {
			return func(string, string) []forge.Check {
				return []forge.Check{{Name: "build", Status: "completed", Conclusion: "success"},
					{Name: "lint", Status: "completed", Conclusion: "failure"}}
			}
		}, contract.ChecksFailed},
		{"no ci", func(*harness) func(string, string) []forge.Check {
			return func(string, string) []forge.Check { return nil }
		}, contract.NoCI},
		{"timeout", func(*harness) func(string, string) []forge.Check {
			return func(string, string) []forge.Check { return []forge.Check{{Name: "slow", Status: "in_progress"}} }
		}, contract.CITimeout},
		{"skipped and neutral pass", func(*harness) func(string, string) []forge.Check {
			return func(string, string) []forge.Check {
				return []forge.Check{{Name: "a", Status: "completed", Conclusion: "skipped"},
					{Name: "b", Status: "completed", Conclusion: "neutral"}}
			}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.forge.Checks = c.checks(h)
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if codeOf(err) != c.want {
				t.Fatalf("want %q, got %v (%+v)", c.want, err, res)
			}
			if c.want == contract.ChecksFailed && !reflect.DeepEqual(res.FailedChecks, []string{"lint"}) {
				t.Fatalf("failed checks = %v", res.FailedChecks)
			}
		})
	}
}

// Checks that appear late must be waited for: the gate may not pass on the
// first green check.
func TestGateWaitsForLateChecksToSettle(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	start := h.clock.Now()
	h.forge.Checks = func(string, string) []forge.Check {
		elapsed := h.clock.Now().Sub(start)
		out := []forge.Check{{Name: "fast", Status: "completed", Conclusion: "success"}}
		switch {
		case elapsed >= 6e9:
			out = append(out, forge.Check{Name: "late", Status: "completed", Conclusion: "failure"})
		case elapsed >= 3e9:
			out = append(out, forge.Check{Name: "late", Status: "in_progress"})
		}
		return out
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.ChecksFailed)
	if !reflect.DeepEqual(res.FailedChecks, []string{"late"}) {
		t.Fatalf("failed = %v", res.FailedChecks)
	}
}

// Review Focus 4.
func TestGateFeatureBranchDeletedButMetadataRemains(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.FeatureNotFound)
	_, _, err = h.eng.Gate(h.ctx, h.pair, "never-started")
	_ = wantCode(t, err, contract.FeatureNotFound)
}

func TestGateOverlayCollision(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	// feature.txt keeps the feature from being overlay-only, which stops
	// earlier as NOTHING_TO_PUBLISH (F-07).
	h.startFeature("feat-x", map[string]string{".ghostbranch.yml": "mine\n", "feature.txt": "x\n"})
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.OverlayCollision)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// When the gate finds a leak but cannot record that result, the leak still
// wins: the error is LEAK_FOUND, with a warning that nothing was recorded.
func TestGateLeakWinsOverAFailureToRecordIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	h.moveMetaOnPush("feat-x", 1) // a leak writes one record
	res, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if res == nil || len(res.Findings) == 0 {
		t.Fatalf("gate = %+v", res)
	}
	var noted bool
	for _, w := range warns {
		noted = noted || w.Code == contract.GateNotRecorded
	}
	if !noted {
		t.Fatalf("want a %s warning, got %+v", contract.GateNotRecorded, warns)
	}
}

// Checks that all complete just before ci_timeout, but too recently to have
// settled, report their outcome rather than CI_TIMEOUT.
func TestGateCompletedChecksBeatTheTimeout(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		conclusion string
		want       contract.Code
	}{{"success", ""}, {"failure", contract.ChecksFailed}} {
		t.Run(c.conclusion, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			var start time.Time
			h.forge.Checks = func(string, string) []forge.Check {
				if start.IsZero() {
					start = h.clock.Now()
				}
				out := []forge.Check{{Name: "first", Status: "completed", Conclusion: "success"},
					{Name: "slow", Status: "in_progress"}}
				// The default ci_timeout is 60m; the last check lands 2s before
				// it, inside the 5s settle window.
				if h.clock.Now().Sub(start) >= 60*time.Minute-2*time.Second {
					out[1] = forge.Check{Name: "slow", Status: "completed", Conclusion: c.conclusion}
					out = append(out, forge.Check{Name: "late", Status: "completed", Conclusion: "success"})
				}
				return out
			}
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if codeOf(err) != c.want {
				t.Fatalf("want %q, got %v (%+v)", c.want, err, res)
			}
		})
	}
}

func TestGateWarnsWhenCheckVisibilityIsLimited(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	h.forge.LimitedChecks = true

	res, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPassed {
		t.Fatalf("gate = %+v %v", res, err)
	}
	found := false
	for _, w := range warns {
		if w.Code == contract.ChecksLimited && strings.Contains(w.Message, "GitHub Actions") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no CHECKS_LIMITED warning in %+v", warns)
	}
}

// moveMetaOnPush makes someone else move the feature's metadata ref just as
// the gate pushes it, so the leased push fails. Only metadata pushes
// numbered from or later (counting from 1) are hit; earlier ones succeed.
func (h *harness) moveMetaOnPush(name string, from int) {
	t := h.t
	sg := h.shadowGit()
	metaRef := "refs/ghostbranch/features/" + name
	n := 0
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if !strings.HasSuffix(s, ":"+metaRef) {
				continue
			}
			if n++; n < from {
				continue
			}
			// Someone else moves the metadata ref first, so the leased push fails.
			old, err := sg.MustResolve(h.ctx, metaRef)
			if err != nil {
				t.Fatal(err)
			}
			tree, err := sg.TreeOf(h.ctx, old)
			if err != nil {
				t.Fatal(err)
			}
			moved, err := sg.CommitTree(h.ctx, tree, []string{old}, "concurrent", testutil.Author(2), testutil.Author(2))
			if err != nil {
				t.Fatal(err)
			}
			if err := sg.UpdateRef(h.ctx, metaRef, moved); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestGateRecordsTheRebasedToSourceMapping(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	first := h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	second := testutil.Commit(t, h.shadowGit(), "feat-x", "add b", map[string]string{"b.txt": "b\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	m, _ := feature.Decode(raw)
	if len(m.Gate.Commits) != 2 || m.Gate.Commits[0].Source != first || m.Gate.Commits[1].Source != second {
		t.Fatalf("gate.commits = %+v", m.Gate.Commits)
	}
	R, _ := h.shadowGit().MustResolve(h.ctx, res.Candidate+"^")
	if m.Gate.Commits[1].Rebased != R {
		t.Fatalf("last rebased %s != R %s", m.Gate.Commits[1].Rebased, R)
	}
	if p, _ := h.shadowGit().MustResolve(h.ctx, m.Gate.Commits[1].Rebased+"^"); p != m.Gate.Commits[0].Rebased {
		t.Fatal("rebased commits must chain")
	}
}

func TestGateConflictNamesTheCommitAndLeakIsNotRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	src := h.startFeature("feat-x", map[string]string{"README.md": "feature version\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"README.md": "upstream version\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.RebaseConflict)
	if ce.Details["commit"] != src || !reflect.DeepEqual(ce.Details["files"], []string{"README.md"}) {
		t.Fatalf("details = %v", ce.Details)
	}
	if res == nil || res.Leak != feature.LeakNotRun {
		t.Fatalf("leak must be not_run when the scan never ran: %+v", res)
	}
}

func TestGateCheckFailureWinsOverAFailureToRecordIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}
	}
	// The pending record succeeds; the final write and its one retry fail.
	h.moveMetaOnPush("feat-x", 2)
	_, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.ChecksFailed)
	var w *contract.Warning
	for i := range warns {
		if warns[i].Code == contract.GateNotRecorded {
			w = &warns[i]
		}
	}
	if w == nil {
		t.Fatalf("want %s, got %+v", contract.GateNotRecorded, warns)
	}
	if job, _ := w.Details["job"].(string); !strings.HasPrefix(job, "gate/feat-x/") || !strings.Contains(w.Message, "ghostbranch wait "+job) {
		t.Fatalf("%s must name the job: %+v", contract.GateNotRecorded, *w)
	}
}

// Review Focus 1, at the gate.
func TestGateScansHistoryForPreserve(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	first := h.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "remove it", map[string]string{"config.env": testutil.Delete, "a.txt": "a\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.LeakFound)
	if res.ScannedAs != "preserve" || len(res.Findings) != 1 || !reflect.DeepEqual(res.Findings[0].Commits, []string{first}) || ce.Details["mode"] != "preserve" {
		t.Fatalf("gate = %+v", res)
	}
	// Under squash the deleted secret never goes public, so the gate passes.
	h.setConfig("default_mode: preserve", "default_mode: squash")
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.ScannedAs != "squash" || len(res.Findings) != 0 {
		t.Fatalf("squash gate = %+v %v", res, err)
	}
	// Curated gates on the final tree too; promote scans the curated chain.
	h.setConfig("default_mode: squash", "default_mode: curated")
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.ScannedAs != "curated" || len(res.Findings) != 0 {
		t.Fatalf("curated gate = %+v %v", res, err)
	}
	raw, _, _ := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	if m, _ := feature.Decode(raw); m.Gate == nil || m.Gate.ScannedAs != "curated" {
		t.Fatalf("recorded gate = %+v", m.Gate)
	}
}

// The gate promote runs scans for the mode promote publishes, not the
// default: a squash promote passes a feature whose secret is only in an
// older commit, and a preserve promote under a squash default stops on it.
func TestPromoteGatesForItsEffectiveMode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "remove it", map[string]string{"config.env": testutil.Delete, "a.txt": "a\n"})
	before := h.publicRefs()
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add a", PlanOnly: true})
	if err != nil || res.Gate == nil || res.Gate.Reused || res.Gate.ScannedAs != "squash" || res.Plan == nil || len(res.Plan.Publish.Commits) != 1 {
		t.Fatalf("squash promote under a preserve default = %+v %v", res, err)
	}

	h2 := newHarness(t)
	h2.init()
	h2.setConfig("default_mode: preserve", "default_mode: squash")
	h2.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	testutil.Commit(t, h2.shadowGit(), "feat-x", "remove it", map[string]string{"config.env": testutil.Delete, "a.txt": "a\n"})
	before2 := h2.publicRefs()
	res, _, err = h2.eng.Promote(h2.ctx, h2.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "preserve", Title: "t", PlanOnly: true})
	ce := wantCode(t, err, contract.LeakFound)
	if ce.Details["mode"] != "preserve" || res.Gate == nil || res.Gate.ScannedAs != "preserve" || res.Gate.Leak != feature.LeakFound ||
		res.Gate.ShadowPR != nil || res.Plan != nil {
		t.Fatalf("preserve promote under a squash default = %+v details = %v", res, ce.Details)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) || !reflect.DeepEqual(h2.publicRefs(), before2) {
		t.Fatal("a --plan promote touched the public repo")
	}
}

// A broken marker in an older commit fails the gate under preserve even
// though the tip fixed it, and the error names the feature and the commit.
func TestGateNamesTheFeatureOnAMarkerInAnOlderCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	first := h.startFeature("feat-x", map[string]string{"app.go": "ok\n// BEGIN-INTERNAL\nx\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "close it", map[string]string{"app.go": "ok\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.MarkerInvalid)
	if ce.Details["feature"] != "feat-x" || ce.Details["commit"] != first {
		t.Fatalf("details = %v", ce.Details)
	}
	if res == nil || res.Leak != feature.LeakNotRun || res.ShadowPR != nil {
		t.Fatalf("gate = %+v", res)
	}
}

func TestGateScansMessagesAndMappedAuthors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("JIRA-[0-9]+")
	h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	jira := testutil.Commit(t, h.shadowGit(), "feat-x", "fix JIRA-9", map[string]string{"a.txt": "b\n"})
	gr, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if len(gr.Findings) != 1 || gr.Findings[0].Where != "commit_message" || !reflect.DeepEqual(gr.Findings[0].Commits, []string{jira}) {
		t.Fatalf("findings = %+v", gr.Findings)
	}

	h2 := newHarness(t)
	h2.init()
	h2.setConfig("author_email: noreply", "author_email: map\n  author_map:\n    \"x@y.example\": \"X <x@y.example>\"")
	h2.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	res, _, err := h2.eng.Gate(h2.ctx, h2.pair, "feat-x")
	_ = wantCode(t, err, contract.AuthorUnmapped)
	if res == nil || res.Leak != feature.LeakNotRun || res.ShadowPR != nil {
		t.Fatalf("an unmapped author stops the gate before CI: %+v", res)
	}
	// Only a preserve gate scans authors; squash and curated gate the tree,
	// and promote raises AUTHOR_UNMAPPED when it builds a chain that needs it.
	h2.setConfig("default_mode: preserve", "default_mode: squash")
	if res, _, err = h2.eng.Gate(h2.ctx, h2.pair, "feat-x"); err != nil || res.ScannedAs != "squash" {
		t.Fatalf("squash gate with an unmapped author = %+v %v", res, err)
	}
	h2.setConfig("default_mode: squash", "default_mode: curated")
	if res, _, err = h2.eng.Gate(h2.ctx, h2.pair, "feat-x"); err != nil || res.ScannedAs != "curated" {
		t.Fatalf("curated gate with an unmapped author = %+v %v", res, err)
	}
}

func TestGateListsTheCommitsForCurating(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n", ".ghostbranch/n.md": "n\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "add b\n\nmore", map[string]string{"b.txt": "b\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || len(res.Commits) != 2 {
		t.Fatalf("gate = %+v %v", res, err)
	}
	want := []engine.CommitInfo{
		{Source: s1, Subject: "work on feat-x", Author: "Test Author <author@example.com>", Files: []string{"a.txt"}},
		{Source: s2, Subject: "add b", Author: "Test Author <author@example.com>", Files: []string{"b.txt"}},
	}
	if !reflect.DeepEqual(res.Commits, want) {
		t.Fatalf("commits = %+v", res.Commits)
	}
}

// gateLeftNoTrace fails t when the gate pushed anything or opened a gate PR
// since pushes were counted.
func gateLeftNoTrace(t *testing.T, h *harness, pushes int, name string) {
	t.Helper()
	if len(h.pushes) != pushes {
		t.Fatalf("pushed: %+v", h.pushes[pushes:])
	}
	if pr, err := h.forge.FindOpenPR(h.ctx, shadowName, "ghostbranch-gate/"+name, "main"); err != nil || pr != nil {
		t.Fatalf("gate PR %+v %v", pr, err)
	}
}

// F-07: a feature with no commits of its own stops in seconds, before any
// push, gate PR or ten-minute CI wait.
func TestGateWithNoCommitsIsNothingToPublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	pushes := len(h.pushes)
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.NothingToPublish)
	if ce.Details["feature"] != "feat-x" || ce.Details["reason"] != "no_commits" || !strings.Contains(ce.Message, "has no commits of its own yet") {
		t.Fatalf("error = %+v", ce)
	}
	gateLeftNoTrace(t, h, pushes, "feat-x")
}

// The gate promote runs first stops the same way.
func TestPromoteWithNoCommitsStopsBeforeGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	pushes := len(h.pushes)
	_, err := promote(h, engine.PromoteOptions{Mode: "preserve", PlanOnly: true})
	if ce := wantCode(t, err, contract.NothingToPublish); ce.Details["reason"] != "no_commits" {
		t.Fatalf("error = %+v", ce)
	}
	gateLeftNoTrace(t, h, pushes, "feat-x")
}

// Commits that touch only overlay paths or internal sections publish
// nothing; the gate stops before CI in the default mode and in squash.
func TestGateOfOverlayOnlyCommitsStopsBeforeCI(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// setup makes the feature to gate; it returns its name and its own
		// commits, oldest first.
		setup func(h *harness) (string, []string)
	}{
		{"overlay paths only", func(h *harness) (string, []string) {
			return "feat-x", []string{h.startFeature("feat-x", map[string]string{".ghostbranch/notes.md": "private\n"})}
		}},
		{"internal sections only", func(h *harness) (string, []string) {
			return "feat-x", []string{h.startFeature("feat-x", map[string]string{"README.md": "# widget\n# BEGIN-INTERNAL\nship it on Friday\n# END-INTERNAL\n"})}
		}},
		// R13: commits that cancel each other out publish no change either.
		{"commits that cancel out", func(h *harness) (string, []string) {
			add := h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			drop := testutil.Commit(h.t, h.shadowGit(), "feat-x", "undo", map[string]string{"x.txt": testutil.Delete})
			return "feat-x", []string{add, drop}
		}},
		// A stacked level compares with the published tree below it, not
		// its parent's unstripped commit: the parent here has an internal
		// section, so the two differ.
		{"stacked, overlay paths only", func(h *harness) (string, []string) {
			h.startFeature("feat-a", map[string]string{"a.txt": "a\n", "README.md": "# widget\n# BEGIN-INTERNAL\nship it on Friday\n# END-INTERNAL\n"})
			if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x", engine.FeatureStartOptions{From: "feat-a"}); err != nil {
				h.t.Fatal(err)
			}
			return "feat-x", []string{testutil.Commit(h.t, h.shadowGit(), "feat-x", "notes", map[string]string{".ghostbranch/notes.md": "private\n"})}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			name, own := c.setup(h)
			pushes := len(h.pushes)
			_, _, err := h.eng.Gate(h.ctx, h.pair, name)
			ce := wantCode(t, err, contract.NothingToPublish)
			if ce.Details["reason"] != "all_stripped" || !reflect.DeepEqual(ce.Details["dropped_empty"], own) {
				t.Fatalf("gate: %+v", ce)
			}
			gateLeftNoTrace(t, h, pushes, name)
			if strings.HasPrefix(c.name, "stacked") {
				return
			}
			_, err = promote(h, engine.PromoteOptions{Mode: "squash", Message: "x", PlanOnly: true})
			if ce := wantCode(t, err, contract.NothingToPublish); ce.Details["reason"] != "all_stripped" {
				t.Fatalf("promote --mode squash: %+v", ce)
			}
			gateLeftNoTrace(t, h, pushes, name)
		})
	}
}

// A stack level with no commits of its own is refused, naming it.
func TestStackLevelWithNoCommitsIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-b", engine.FeatureStartOptions{From: "feat-a"}); err != nil {
		t.Fatal(err)
	}
	pushes := len(h.pushes)
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-b")
	if ce := wantCode(t, err, contract.NothingToPublish); ce.Details["feature"] != "feat-b" || ce.Details["reason"] != "no_commits" {
		t.Fatalf("error = %+v", ce)
	}
	gateLeftNoTrace(t, h, pushes, "feat-b")
}

// The update of an open PR with nothing new keeps promote's own answer, and
// waivers add (which reads tree findings only) never stops on the check.
func TestEmptyFeatureCheckLeavesUpdatesAndWaiversAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("feat-x")
	_, err := promote(h, engine.PromoteOptions{Mode: "preserve", PlanOnly: true})
	if ce := wantCode(t, err, contract.NothingToPublish); !strings.Contains(ce.Message, "already holds every change") || ce.Details["reason"] != nil {
		t.Fatalf("update: %+v", ce)
	}
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-y"); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.eng.WaiversAdd(h.ctx, h.pair, engine.WaiversAddOptions{Feature: "feat-y", IDs: []string{"f-000000000000"}, Reason: "test"})
	if codeOf(err) == contract.NothingToPublish {
		t.Fatalf("waivers add stopped on the empty-feature check: %v", err)
	}
}

// F-07: NO_CI says why: the candidate has no workflow at all, or it has one
// that never ran. A gate is never refused up front for having no
// workflow: an external CI app posts checks without one.
func TestNoCIMessageNamesMissingWorkflow(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		dropCI       bool
		reason, says string
	}{
		{"no workflow at all", true, "no_workflow", "the gate candidate has no workflow under .github/workflows (public main has none)"},
		{"a workflow that never ran", false, "actions_disabled_or_untriggered", "or no workflow triggers on pull_request"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if c.dropCI {
				testutil.Commit(t, h.pubGit, "main", "drop CI", map[string]string{".github/workflows/ci.yml": testutil.Delete})
			}
			h.init()
			h.forge.Checks = func(string, string) []forge.Check { return nil }
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			ce := wantCode(t, err, contract.NoCI)
			if ce.Details["reason"] != c.reason || !strings.Contains(ce.Message, c.says) {
				t.Fatalf("error = %+v", ce)
			}
		})
	}
}
