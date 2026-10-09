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
