package engine_test

import (
	"bytes"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
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
	h := newHarness(t)
	h.init()
	head := h.startFeature("feat-x", map[string]string{"feature.txt": "new\n", ".ghostbranch/notes.md": "private\n"})
	before := h.publicRefs()

	var progress bytes.Buffer
	h.eng.Progress = &progress
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if wantLine := "waiting for CI on " + res.Candidate[:12] + " (shadow PR #1: https://github.example/" + shadowName + "/pull/1)"; !strings.Contains(progress.String(), wantLine) {
		t.Errorf("progress lacks %q in %q", wantLine, progress.String())
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
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{".ghostbranch.yml": "mine\n"})
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
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	sg := h.shadowGit()
	metaRef := "refs/ghostbranch/features/feat-x"
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if !strings.HasSuffix(s, ":"+metaRef) {
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
	for _, c := range []struct {
		conclusion string
		want       contract.Code
	}{{"success", ""}, {"failure", contract.ChecksFailed}} {
		t.Run(c.conclusion, func(t *testing.T) {
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
