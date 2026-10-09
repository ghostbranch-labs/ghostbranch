package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
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
