package engine_test

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func detached(t *testing.T, h *harness, name string) string {
	t.Helper()
	res, _, err := h.eng.GateDetached(h.ctx, h.pair, name)
	if err != nil || res.Job == nil {
		t.Fatalf("detached gate: %+v %v", res, err)
	}
	return *res.Job
}

func TestWaitFinishesADetachedGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	job := detached(t, h, "feat-x")
	var events []engine.JobEvent
	res, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{Events: func(ev engine.JobEvent) { events = append(events, ev) }})
	if err != nil || res.Checks != feature.ChecksPassed || res.Job == nil || *res.Job != job || len(res.Commits) != 1 {
		t.Fatalf("wait = %+v %v", res, err)
	}
	if len(events) < 3 || events[0].Type != engine.EventWaiting || events[0].ShadowPR != *res.ShadowPR || events[len(events)-1].Type != engine.EventSettled {
		t.Fatalf("events = %+v", events)
	}
	if !h.meta("feat-x").Gate.Passed() {
		t.Fatal("the result was not recorded")
	}
	// Again: the finished job answers at once, with no events and no write.
	pushes := len(h.pushes)
	events = nil
	again, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{Events: func(ev engine.JobEvent) { events = append(events, ev) }})
	if err != nil || again.Checks != feature.ChecksPassed || len(events) != 0 || len(h.pushes) != pushes {
		t.Fatalf("second wait = %+v %v, %d events, %d pushes", again, err, len(events), len(h.pushes)-pushes)
	}
	es := h.ledger()
	last := es[len(es)-1]
	if last.Op != "wait" || last.Feature != "feat-x" || last.Summary != "checks passed" || last.ExitCode != 0 {
		t.Fatalf("ledger = %+v", last)
	}
	raw, _ := json.Marshal(last)
	validateSchema(t, "ledger-entry.schema.json", raw)
}

func TestWaitReplaysAFailedOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}
	}
	job := detached(t, h, "feat-x")
	_, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{})
	_ = wantCode(t, err, contract.ChecksFailed)
	res, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{})
	_ = wantCode(t, err, contract.ChecksFailed)
	if !reflect.DeepEqual(res.FailedChecks, []string{"lint"}) {
		t.Fatalf("replayed failed checks = %v", res.FailedChecks)
	}
}

func TestWaitTimeoutLeavesTheJobPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	h.forge.Checks = func(string, string) []forge.Check { return []forge.Check{{Name: "ci", Status: "in_progress"}} }
	job := detached(t, h, "feat-x")
	res, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{Timeout: 3 * time.Second})
	if err != nil || res.Checks != feature.ChecksPending || !h.meta("feat-x").Gate.Pending() {
		t.Fatalf("want pending, got %+v %v", res, err)
	}
}

// Review Focus 4.
func TestWaitPastTheBudgetSettlesAfterOnePoll(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		status string
		want   string
	}{{"completed", feature.ChecksPassed}, {"in_progress", feature.ChecksTimeout}} {
		t.Run(c.status, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			job := detached(t, h, "feat-x")
			h.clock.Sleep(13 * time.Hour) // the next morning
			polls := 0
			h.forge.Checks = func(string, string) []forge.Check {
				polls++
				return []forge.Check{{Name: "ci", Status: c.status, Conclusion: map[bool]string{true: "success"}[c.status == "completed"]}}
			}
			res, _, _ := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{})
			if res.Checks != c.want || polls != 1 {
				t.Fatalf("want %s after 1 poll, got %s after %d", c.want, res.Checks, polls)
			}
		})
	}
}

// Review Focus 1.
func TestWaitResumesAnInterruptedGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	ctx, cancel := context.WithCancel(h.ctx)
	h.forge.Checks = func(string, string) []forge.Check {
		cancel()
		return []forge.Check{{Name: "ci", Status: "in_progress"}}
	}
	res, _, err := h.eng.Gate(ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.Interrupted)
	if res.Job == nil || ce.Details["job"] != *res.Job || !h.meta("feat-x").Gate.Pending() {
		t.Fatalf("want INTERRUPTED with a pending job: %+v %+v", ce.Details, res)
	}
	gatePushes := 0
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if strings.HasSuffix(s, ":refs/heads/ghostbranch-gate/feat-x") {
				gatePushes++
			}
		}
	}
	h.forge.Checks = passing
	done, _, err := h.eng.Wait(h.ctx, h.pair, *res.Job, engine.WaitOptions{})
	if err != nil || done.Checks != feature.ChecksPassed || gatePushes != 0 {
		t.Fatalf("resume = %+v %v (%d candidate pushes)", done, err, gatePushes)
	}
}

func TestAnInterruptBeforeTheRecordHasNoJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	_, _, err := h.eng.Gate(ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.Interrupted)
	if _, has := ce.Details["job"]; has || ce.Details["feature"] != "feat-x" {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// Review Focus 3, at wait.
func TestWaitOnAReplacedJobIsStale(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	old := detached(t, h, "feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"feature.txt": "y\n"})
	current := detached(t, h, "feat-x")
	if old == current {
		t.Fatal("a changed feature reused the old job")
	}
	_, _, err := h.eng.Wait(h.ctx, h.pair, old, engine.WaitOptions{})
	if ce := wantCode(t, err, contract.JobStale); ce.Details["current_job"] != current {
		t.Fatalf("details = %+v", ce.Details)
	}
}

func TestGateReusesAPendingJobForTheSameInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	job := detached(t, h, "feat-x")
	pushes := len(h.pushes)
	if again := detached(t, h, "feat-x"); again != job || len(h.pushes) != pushes {
		t.Fatalf("second detached gate: job %s (want %s), %d new pushes", again, job, len(h.pushes)-pushes)
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x") // blocking: waits on that job
	if err != nil || *res.Job != job || res.Checks != feature.ChecksPassed {
		t.Fatalf("blocking gate = %+v %v", res, err)
	}
}

func TestWaitErrors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	if _, _, err := h.eng.Wait(h.ctx, h.pair, "nope", engine.WaitOptions{}); codeOf(err) != contract.BadFlag {
		t.Errorf("bad id: %v", err)
	}
	if _, _, err := h.eng.Wait(h.ctx, h.pair, "gate/feat-x/0123456789ab", engine.WaitOptions{}); codeOf(err) != contract.JobNotFound {
		t.Errorf("never gated: %v", err)
	}
	if _, _, err := h.eng.Wait(h.ctx, h.pair, "gate/gone/0123456789ab", engine.WaitOptions{}); codeOf(err) != contract.FeatureNotFound {
		t.Errorf("no such feature: %v", err)
	}
}
