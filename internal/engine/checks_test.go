package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func newCheckEngine(t *testing.T) (*Engine, *fake.Forge, *testutil.Clock) {
	t.Helper()
	f := fake.New(t.TempDir())
	clock := testutil.NewClock(time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	return &Engine{Forge: f, Now: clock.Now, Sleep: clock.SleepCtx, PollInterval: time.Second, SettleWindow: 5 * time.Second}, f, clock
}

var gateCfg = config.Gate{CIAppearTimeout: 10 * time.Second, CITimeout: 60 * time.Second}

func TestWaitChecksNeverReportsNoCIOnceACheckWasSeen(t *testing.T) {
	t.Parallel()
	e, f, clock := newCheckEngine(t)
	start := clock.Now()
	f.Checks = func(string, string) []forge.Check {
		if clock.Now().Sub(start) < 3*time.Second {
			return []forge.Check{{Name: "ci", Status: "in_progress"}}
		}
		return nil // the run was deleted
	}
	out, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{})
	if err != nil || out.status != feature.ChecksTimeout {
		t.Fatalf("want timeout, got %+v %v", out, err)
	}
}

func TestWaitChecksCountsFromTheGivenStart(t *testing.T) {
	t.Parallel()
	e, f, clock := newCheckEngine(t)
	calls := 0
	f.Checks = func(string, string) []forge.Check {
		calls++
		return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "success"}}
	}
	out, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{start: clock.Now().Add(-2 * time.Hour)})
	if err != nil || out.status != feature.ChecksPassed || calls != 1 {
		t.Fatalf("want passed after one poll, got %+v %v (%d polls)", out, err, calls)
	}
}

func TestWaitChecksStopsAtTheDeadline(t *testing.T) {
	t.Parallel()
	e, f, clock := newCheckEngine(t)
	f.Checks = func(string, string) []forge.Check { return []forge.Check{{Name: "ci", Status: "in_progress"}} }
	out, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{deadline: clock.Now().Add(3 * time.Second)})
	if err != nil || out.status != feature.ChecksPending {
		t.Fatalf("want pending, got %+v %v", out, err)
	}
}

func TestWaitChecksIsInterrupted(t *testing.T) {
	t.Parallel()
	e, f, _ := newCheckEngine(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.Checks = func(string, string) []forge.Check {
		cancel()
		return []forge.Check{{Name: "ci", Status: "in_progress"}}
	}
	_, err := e.waitChecks(ctx, "o/r", "sha", gateCfg, waitOpts{job: "gate/x/0123456789ab"})
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.Interrupted || ce.Details["job"] != "gate/x/0123456789ab" {
		t.Fatalf("want INTERRUPTED with the job, got %v", err)
	}
}

func TestWaitChecksEmitsChangesAndTheOutcome(t *testing.T) {
	t.Parallel()
	e, f, clock := newCheckEngine(t)
	start := clock.Now()
	f.Checks = func(string, string) []forge.Check {
		if clock.Now().Sub(start) < 2*time.Second {
			return []forge.Check{{Name: "ci", Status: "in_progress"}}
		}
		return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "failure"}}
	}
	var got []JobEvent
	out, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{job: "j", emit: func(ev JobEvent) { got = append(got, ev) }})
	if err != nil || out.status != feature.ChecksFailed {
		t.Fatalf("%+v %v", out, err)
	}
	var types []string
	for _, ev := range got {
		types = append(types, ev.Type)
		raw, _ := json.Marshal(ev)
		testutil.ValidateSchema(t, "wait-event.schema.json", raw)
	}
	// One event per change, not per poll: in_progress, then completed, then the outcome.
	if want := []string{EventCheck, EventCheck, EventSettled}; !reflect.DeepEqual(types, want) {
		t.Fatalf("events = %v, want %v", types, want)
	}
	if last := got[len(got)-1]; last.Outcome != feature.ChecksFailed || !reflect.DeepEqual(last.FailedChecks, []string{"ci"}) || last.Job != "j" {
		t.Fatalf("settled = %+v", last)
	}
}

// quickEngine is newCheckEngine with a short settle for a complete picture:
// SettleWindow 5s, QuickSettle 1s, polling every second.
func quickEngine(t *testing.T) (*Engine, *fake.Forge, *testutil.Clock) {
	e, f, clock := newCheckEngine(t)
	e.QuickSettle = time.Second
	return e, f, clock
}

func actionsCheck(name string) forge.Check {
	return forge.Check{Name: name, Status: "completed", Conclusion: "success", Actions: true}
}

func prRun(path, event, status string) forge.Run {
	return forge.Run{Workflow: path, Path: path, Event: event, Status: status, Conclusion: "success"}
}

// settleTime is how long waitChecks took to pass on checks, with runs as
// the candidate's workflow runs and files as the workflow files that run
// on the PR.
func settleTime(t *testing.T, e *Engine, f *fake.Forge, clock *testutil.Clock, files []string, runs []forge.Run, checks ...forge.Check) time.Duration {
	t.Helper()
	f.Checks = func(string, string) []forge.Check { return checks }
	f.RunsFor = func(string, string) []forge.Run { return runs }
	start := clock.Now()
	out, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{workflows: files})
	if err != nil || out.status != feature.ChecksPassed {
		t.Fatalf("want passed, got %+v %v", out, err)
	}
	return clock.Now().Sub(start)
}

// Every workflow file that runs on the PR has a finished pull_request run,
// and every check is done and from GitHub Actions: the gate settles after
// QuickSettle, not the whole SettleWindow.
func TestWaitChecksSettlesQuicklyWhenEveryWorkflowReported(t *testing.T) {
	t.Parallel()
	e, f, clock := quickEngine(t)
	files := []string{".github/workflows/ci.yml", ".github/workflows/lint.yml"}
	runs := []forge.Run{prRun(files[0], "pull_request", "completed"), prRun(files[1], "pull_request_target", "completed"),
		prRun(files[0], "push", "completed")}
	if took := settleTime(t, e, f, clock, files, runs, actionsCheck("build"), actionsCheck("test"), actionsCheck("lint")); took >= 5*time.Second {
		t.Fatalf("took %s, want under the 5s settle window", took)
	}
}

// Anything short of the complete picture keeps the whole settle window.
// The first case is the one that matters most: on: [push, pull_request]
// with the push guard, the gate's push makes a run whose jobs are all
// skipped, which finishes at once; it must never stand in for the
// pull_request run that hasn't started yet.
func TestWaitChecksKeepsTheSettleWindowWithoutTheCompletePicture(t *testing.T) {
	t.Parallel()
	ci := ".github/workflows/ci.yml"
	skipped := forge.Check{Name: "build", Status: "completed", Conclusion: "skipped", Actions: true}
	for _, tc := range []struct {
		name    string
		files   []string
		limited bool
		runs    []forge.Run
		checks  []forge.Check
	}{
		{"only the skipped push run so far", []string{ci}, false, []forge.Run{prRun(ci, "push", "completed")}, []forge.Check{skipped}},
		{"the pull_request run not finished", []string{ci}, false, []forge.Run{prRun(ci, "pull_request", "in_progress")}, []forge.Check{actionsCheck("build")}},
		{"another workflow not reported", []string{ci, ".github/workflows/lint.yml"}, false, []forge.Run{prRun(ci, "pull_request", "completed")}, []forge.Check{actionsCheck("build")}},
		{"a commit status", []string{ci}, false, []forge.Run{prRun(ci, "pull_request", "completed")},
			[]forge.Check{actionsCheck("build"), {Name: "ci/circleci", Status: "completed", Conclusion: "success"}}},
		{"another app's check run", []string{ci}, false, []forge.Run{prRun(ci, "pull_request", "completed")},
			[]forge.Check{actionsCheck("build"), {Name: "buildkite", Status: "completed", Conclusion: "success"}}},
		{"workflows unknown", nil, false, []forge.Run{prRun(ci, "pull_request", "completed")}, []forge.Check{actionsCheck("build")}},
		{"other apps' checks invisible", []string{ci}, true, []forge.Run{prRun(ci, "pull_request", "completed")}, []forge.Check{actionsCheck("build")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, f, clock := quickEngine(t)
			f.LimitedChecks = tc.limited
			if took := settleTime(t, e, f, clock, tc.files, tc.runs, tc.checks...); took < 5*time.Second {
				t.Fatalf("took %s, want the whole 5s settle window", took)
			}
		})
	}
}

// A check event is sent once per change of status or conclusion, even when
// two checks share a name (a workflow's push and pull_request runs).
func TestWaitChecksSendsOneEventPerChange(t *testing.T) {
	t.Parallel()
	e, f, _ := newCheckEngine(t)
	f.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "build", Status: "completed", Conclusion: "success", Actions: true},
			{Name: "build", Status: "completed", Conclusion: "success"}}
	}
	events := 0
	emit := func(ev JobEvent) {
		if ev.Type == EventCheck {
			events++
		}
	}
	if _, err := e.waitChecks(context.Background(), "o/r", "sha", gateCfg, waitOpts{emit: emit}); err != nil {
		t.Fatal(err)
	}
	if events != 1 {
		t.Fatalf("sent %d check events over several polls, want 1", events)
	}
}
