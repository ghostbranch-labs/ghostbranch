package engine_test

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// inOrder reports whether want appears in got in this order (other steps may
// come between).
func inOrder(got, want []string) bool {
	i := 0
	for _, g := range got {
		if i < len(want) && g == want[i] {
			i++
		}
	}
	return i == len(want)
}

func TestGateReportsItsStepsInOrder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	var rec progress.Recorder
	h.eng.Progress = &rec
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Reading " + publicName + " and " + shadowName + " from GitHub",
		"Replaying feat-x onto public main",
		"Scanning what feat-x would publish for leaks (preserve)",
		"Pushing the gate candidate to the shadow repo",
		"Waiting for CI to start",
	}
	if !inOrder(rec.Steps(), want) {
		t.Fatalf("steps out of order:\n%s", rec.String())
	}
	if rec.Running() {
		t.Fatalf("a step is still running after a passing gate:\n%s", rec.String())
	}
	ev := rec.Events()
	if last := ev[len(ev)-1]; last.Kind != "done" || last.Text != "CI passed" {
		t.Fatalf("the CI step ended %+v", last)
	}
}

// The CI step counts finished checks from each poll.
func TestCIWaitCountsChecksInTheStep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check {
		switch calls.Add(1) {
		case 1:
			return nil
		case 2:
			return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "success"}, {Name: "test", Status: "in_progress"}}
		default:
			return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "success"}, {Name: "test", Status: "completed", Conclusion: "success"}}
		}
	}
	var rec progress.Recorder
	h.eng.Progress = &rec
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	if !inOrder(rec.Updates(), []string{"Waiting for CI to start", "Waiting for CI (1 of 2 checks done)", "Waiting for CI (2 of 2 checks done)"}) {
		t.Fatalf("CI updates:\n%s", rec.String())
	}
}

// A failed CI marks the CI step ✗.
func TestFailedChecksFailTheCIStep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}
	}
	var rec progress.Recorder
	h.eng.Progress = &rec
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err == nil {
		t.Fatal("the gate should fail")
	}
	ev := rec.Events()
	if last := ev[len(ev)-1]; last.Kind != "fail" || last.Text != "CI failed: lint" {
		t.Fatalf("the CI step ended %+v:\n%s", last, rec.String())
	}
}

// R15: piped, the CI step's ending is one more plain line.
func TestPipedGatePrintsTheCIEnding(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		checks []forge.Check
		want   string
	}{
		{"passed", []forge.Check{{Name: "lint", Status: "completed", Conclusion: "success"}}, "ok: CI passed\n"},
		{"failed", []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}, "failed: CI failed: lint\n"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
			h.forge.Checks = func(string, string) []forge.Check { return c.checks }
			var buf bytes.Buffer
			h.eng.Progress = progress.Lines(&buf)
			_, _, _ = h.eng.Gate(h.ctx, h.pair, "feat-x")
			if out := buf.String(); !strings.HasSuffix(out, c.want) {
				t.Fatalf("piped gate output:\n%s", out)
			}
		})
	}
}

// R13: wait ends the CI step, both when CI settles and at --timeout.
func TestWaitEndsTheCIStep(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		checks  []forge.Check
		timeout time.Duration
		want    string
	}{
		{"passed", nil, 0, "CI passed"},
		{"timeout", []forge.Check{{Name: "ci", Status: "in_progress"}}, 3 * time.Second, "CI still running; the job stays pending"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			if c.checks != nil {
				h.forge.Checks = func(string, string) []forge.Check { return c.checks }
			}
			job := detached(t, h, "feat-x")
			var rec progress.Recorder
			h.eng.Progress = &rec
			if _, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{Timeout: c.timeout}); err != nil {
				t.Fatal(err)
			}
			if !hasEvent(rec.Events(), "done", c.want) || rec.Running() {
				t.Fatalf("wait:\n%s", rec.String())
			}
		})
	}
}

func TestInitReportsItsSteps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var rec progress.Recorder
	h.eng.Progress = &rec
	h.init()
	if !inOrder(rec.Steps(), []string{"Creating the private repo " + shadowName, "Creating the overlay branch ghostbranch",
		"Reading " + publicName + " and " + shadowName + " from GitHub"}) ||
		!slices.ContainsFunc(rec.Steps(), func(s string) bool { return len(s) > 9 && s[:9] == "Mirroring" }) {
		t.Fatalf("init steps:\n%s", rec.String())
	}
	if rec.Running() {
		t.Fatalf("a step is still running after init:\n%s", rec.String())
	}
}

func TestSyncReportsTheFastForwardAndItsSHAs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	var rec progress.Recorder
	h.eng.Progress = &rec
	h.syncOK(engine.SyncOptions{})
	if !slices.Contains(rec.Steps(), "Fast-forwarding shadow main") ||
		!slices.ContainsFunc(rec.Notes(), func(n string) bool { return len(n) > 12 && n[:12] == "shadow main:" }) {
		t.Fatalf("sync:\n%s", rec.String())
	}
	if rec.Running() {
		t.Fatalf("a step is still running after sync:\n%s", rec.String())
	}
}

// Sync shows a step while it waits for the runs it cancelled to stop: ✓
// when they did, ✗ with a count when some kept running.
func TestSyncShowsTheWaitForCancelledRuns(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		run  func(next string) fake.Run
		kind string
		want string
	}{
		{"stopped", func(next string) fake.Run { return fake.Run{SHA: next, Run: wfRun(1, "ci", "queued")} },
			"done", "Checked that the cancelled CI runs stopped"},
		{"kept running", func(next string) fake.Run {
			return fake.Run{SHA: next, Run: wfRun(7, "ci", "in_progress"), IgnoreCancel: true, IgnoreForceCancel: true}
		}, "fail", "1 cancelled CI run(s) didn't stop"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, next := pushRunsHarness(t)
			seedRun(h, c.run(next))
			var rec progress.Recorder
			h.eng.Progress = &rec
			if _, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{}); err != nil {
				t.Fatal(err)
			}
			if !inOrder(rec.Steps(), []string{"Looking for CI runs the sync started in " + shadowName, "Checking that the cancelled CI runs stopped"}) ||
				!hasEvent(rec.Events(), c.kind, c.want) || rec.Running() {
				t.Fatalf("sync:\n%s", rec.String())
			}
		})
	}
}

func TestPromoteReportsThePlanAndThePublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(time.Hour)
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	var rec progress.Recorder
	h.eng.Progress = &rec
	if _, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add feature", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !inOrder(rec.Steps(), []string{"Building the publish plan for feat-x", "Publishing feat-x to " + publicName + " (branch feat-x)"}) {
		t.Fatalf("promote steps:\n%s", rec.String())
	}
	if rec.Running() {
		t.Fatalf("a step is still running after promote:\n%s", rec.String())
	}
}

// A promote that gates first ends the CI step before it plans.
func TestPromoteThatGatesFirstEndsTheCIStep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(time.Hour)
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	var rec progress.Recorder
	h.eng.Progress = &rec
	if _, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add feature", Yes: true}); err != nil {
		t.Fatal(err)
	}
	if !inOrder(rec.Steps(), []string{"Waiting for CI to start", "Building the publish plan for feat-x"}) || !hasEvent(rec.Events(), "done", "CI passed") || rec.Running() {
		t.Fatalf("promote:\n%s", rec.String())
	}
}

// A stack promote plans every branch in one step; without native stacks the
// register step ends ✓ (the PRs stay chained), after the host's answer.
func TestStackPromoteKeepsPRsChainedWithoutNativeStacks(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	h.forge.StacksUnavailable = true
	var rec progress.Recorder
	h.eng.Progress = &rec
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	prs := res.Applied.PRs
	kept := fmt.Sprintf("Kept PRs #%d, #%d chained (no native stacked PRs)", prs[0].Number, prs[1].Number)
	if !slices.Contains(rec.Steps(), "Building the publish plan for a, b") || !hasEvent(rec.Events(), "done", kept) || rec.Running() ||
		slices.ContainsFunc(rec.Events(), func(e progress.Event) bool { return e.Kind == "fail" }) ||
		!slices.ContainsFunc(rec.Events(), func(e progress.Event) bool {
			return e.Kind == "note" && e.Level == progress.Warn && strings.Contains(e.Text, "has no native stacked PRs")
		}) {
		t.Fatalf("stack promote:\n%s", rec.String())
	}
}
