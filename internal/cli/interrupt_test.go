package cli

import (
	"context"
	"errors"
	"os"
	"slices"
	"syscall"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

func TestAsInterrupted(t *testing.T) {
	t.Parallel()
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	moved := contract.New(contract.FeatureMoved, "refs/heads/feat-x changed").WithDetails(map[string]any{"feature": "feat-x"})
	if asInterrupted(cancelled, nil) != nil {
		t.Fatal("success stays success")
	}
	if got := contract.From(asInterrupted(live, moved)); got.Code != contract.FeatureMoved {
		t.Fatalf("no cancel: %v", got)
	}
	got := contract.From(asInterrupted(cancelled, moved))
	if got.Code != contract.Interrupted || got.Details["feature"] != "feat-x" || got.Details["error_code"] != string(contract.FeatureMoved) {
		t.Fatalf("cancelled: %+v", got)
	}
	if got := contract.From(asInterrupted(cancelled, errors.New("git: signal: interrupt"))); got.Code != contract.Interrupted ||
		got.Details["error_code"] != string(contract.Internal) {
		t.Fatalf("a raw error: %+v", got)
	}
	job := contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": "gate/feat-x/0123456789ab"})
	if got := asInterrupted(cancelled, job); got != error(job) {
		t.Fatalf("an INTERRUPTED of its own must pass through: %v", got)
	}
	failed := contract.New(contract.ChecksFailed, "checks failed")
	if got := contract.From(asInterrupted(cancelled, failed)); got.Code != contract.ChecksFailed {
		t.Fatalf("a settled gate outcome is the answer: %v", got)
	}
	// A stale or missing job is a settled answer too: turning it into
	// INTERRUPTED would have next offer wait on a dead job.
	for _, code := range []contract.Code{contract.JobStale, contract.JobNotFound} {
		dead := contract.New(code, "x").WithDetails(map[string]any{"job": "gate/feat-x/0123456789ab"})
		if got := asInterrupted(cancelled, dead); got != error(dead) {
			t.Fatalf("%s must pass through: %v", code, got)
		}
	}
}

// SIGTERM stops a run like Ctrl-C (the cursor is shown again); SIGHUP too
// where it exists (signals_unix_test.go).
func TestStopSignalsIncludeTERM(t *testing.T) {
	t.Parallel()
	if !slices.Contains(stopSignals, os.Interrupt) || !slices.Contains(stopSignals, os.Signal(syscall.SIGTERM)) {
		t.Fatalf("stopSignals %v", stopSignals)
	}
}
