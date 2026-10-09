package cli

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func TestGateCell(t *testing.T) {
	t.Parallel()
	job := "gate/feat-x/0123456789ab"
	for _, c := range []struct {
		g    *engine.GateSummary
		want string
	}{
		{nil, "-"},
		{&engine.GateSummary{Checks: "pending", Leak: "clean", Fresh: true, Job: &job}, "pending (job " + job + ")"},
		{&engine.GateSummary{Checks: "pending", Leak: "clean", Fresh: false, Job: &job}, "pending (job " + job + ") (stale)"},
		{&engine.GateSummary{Checks: "passed", Leak: "clean", Fresh: true}, "passed"},
		{&engine.GateSummary{Checks: "passed", Leak: "not_run", Fresh: true}, "passed, scan not_run"},
		{&engine.GateSummary{Checks: "failed", Leak: "not_run", Fresh: false}, "failed, scan not_run (stale)"},
		{&engine.GateSummary{Checks: "not_run", Leak: "not_run", Fresh: true}, "not_run"},
		{&engine.GateSummary{Checks: "failed", Leak: "found", Fresh: true}, "leak"},
	} {
		if got := gateCell(c.g); got != c.want {
			t.Errorf("gateCell(%+v) = %q, want %q", c.g, got, c.want)
		}
	}
}
