package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// After a cancel, a settled outcome is still the answer; any other error
// names the job only while the record is pending, and the feature once the
// gate's result is recorded.
func TestInterruptedKeepsOutcomesAndNamesOnlyAPendingJob(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	job := "gate/feat-x/0123456789ab"
	pending := &GateResult{Job: &job, Checks: feature.ChecksPending}
	outcomes := 0
	for _, code := range contract.AllCodes() {
		if code.Exit() != contract.ExitGateFailed {
			continue
		}
		outcomes++
		err := contract.New(code, "x")
		if got := interrupted(ctx, pending, "feat-x", err); got != err {
			t.Errorf("%s: want it unchanged, got %v", code, got)
		}
	}
	if outcomes != 5 { // CHECKS_FAILED, NO_CI, CI_TIMEOUT, LEAK_FOUND, MARKER_INVALID
		t.Errorf("want 5 outcome codes, got %d", outcomes)
	}
	plain := errors.New("the push failed")
	ce := contract.From(interrupted(ctx, pending, "feat-x", plain))
	if ce.Code != contract.Interrupted || ce.Details["job"] != job {
		t.Errorf("pending: want INTERRUPTED with the job, got %v %+v", ce, ce.Details)
	}
	passed := &GateResult{Job: &job, Checks: feature.ChecksPassed}
	ce = contract.From(interrupted(ctx, passed, "feat-x", plain))
	if _, has := ce.Details["job"]; ce.Code != contract.Interrupted || has || ce.Details["feature"] != "feat-x" {
		t.Errorf("passed: want INTERRUPTED with the feature, got %v %+v", ce, ce.Details)
	}
}

// Only a busy lock is worth retrying; any other lock failure is a plain
// error. Both name the job.
func TestRelockRetriesOnlyABusyLock(t *testing.T) {
	t.Parallel()
	const job = "gate/feat-x/0123456789ab"
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e := &Engine{Now: time.Now, Sleep: func(context.Context, time.Duration) error { return nil }}
	// The lock's folder is a file, so taking the lock fails, and not as LOCKED.
	err := e.relock(context.Background(), &pairCtx{lockPath: filepath.Join(file, "pair.lock"), op: "wait"}, job)
	if ce := contract.From(err); ce == nil || ce.Code == contract.Locked || ce.Retryable || ce.Details["job"] != job {
		t.Fatalf("a broken lock: %+v", ce)
	}
	busy := filepath.Join(dir, "busy.lock")
	drop, err := workspace.Lock(busy, time.Now(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer drop()
	err = e.relock(context.Background(), &pairCtx{lockPath: busy, op: "wait"}, job)
	if ce := contract.From(err); ce == nil || ce.Code != contract.Locked || !ce.Retryable || ce.Details["job"] != job || ce.Details["held_by"] == nil {
		t.Fatalf("a busy lock: %+v", ce)
	}
}
