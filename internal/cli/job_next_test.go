package cli

import (
	"slices"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func TestWithoutSwitch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ in, want []string }{
		{[]string{"promote", "feat-x", "--detach", "--plan"}, []string{"promote", "feat-x", "--plan"}},
		{[]string{"promote", "feat-x", "--detach=true", "--plan"}, []string{"promote", "feat-x", "--plan"}},
		{[]string{"promote", "feat-x", "--detached", "--detach"}, []string{"promote", "feat-x", "--detached"}},
	} {
		if got := withoutSwitch(tc.in, "--detach"); !slices.Equal(got, tc.want) {
			t.Errorf("withoutSwitch(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// The job errors a cancel or the lock can leave say how to finish the job.
// (A cancel can't be sent through Run in a test, so next is called here.)
func TestNextForJobErrors(t *testing.T) {
	t.Parallel()
	const job = "gate/feat-x/0123456789ab"
	wait := []string{"ghostbranch", "wait", job, "--repo", "acme/widget"}
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"interrupted with a job", contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": job}), wait},
		{"interrupted before CI", contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"feature": "feat-x"}),
			[]string{"ghostbranch", "gate", "feat-x", "--json"}},
		{"locked after CI", contract.New(contract.Locked, "x").WithDetails(map[string]any{"job": job}), wait},
		{"stale, none current", contract.New(contract.JobStale, "x").WithDetails(map[string]any{"job": job, "current_job": ""}),
			[]string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}},
		{"not found after a job", contract.New(contract.JobNotFound, "x").WithDetails(map[string]any{"job": job}),
			[]string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}},
	} {
		a := &app{env: Env{Getenv: func(string) string { return "" }}, args: []string{"gate", "feat-x", "--json"},
			pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
		next := a.next(tc.err)
		if len(next) != 1 || !slices.Equal(next[0].Command, tc.want) {
			t.Errorf("%s: next = %+v, want %v", tc.name, next, tc.want)
		}
	}
	a := &app{env: Env{Getenv: func(string) string { return "" }}}
	if next := a.next(contract.New(contract.Locked, "x")); len(next) != 0 {
		t.Errorf("a plain LOCKED offers nothing new: %+v", next)
	}
	// An interrupted promote --detach: wait on the job, then promote again
	// without --detach, which reuses the green gate.
	a = &app{env: Env{Getenv: func(string) string { return "" }}, command: "promote",
		args:     []string{"promote", "feat-x", "--detach", "--plan", "--json"},
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
	next := a.next(contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"job": job}))
	if len(next) != 2 || !slices.Equal(next[0].Command, wait) ||
		!slices.Equal(next[1].Command, []string{"ghostbranch", "promote", "feat-x", "--plan", "--json"}) {
		t.Errorf("interrupted promote: next = %+v", next)
	}
}

// A promote interrupted after its plan was saved resumes with apply <plan>,
// and a public branch at an earlier plan's commits names that plan: making
// new commits would only end in BRANCH_EXISTS.
func TestNextPointsAtTheSavedPlan(t *testing.T) {
	t.Parallel()
	const id = "20261009T120000Z-abcdef"
	apply := []string{"ghostbranch", "apply", id}
	a := &app{env: Env{Getenv: func(string) string { return "" }}, command: "promote",
		args:     []string{"promote", "feat-x", "--mode", "squash", "-m", "Add x", "--json"},
		pairFull: workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}}
	next := a.next(contract.New(contract.Interrupted, "x").WithDetails(map[string]any{"plan_id": id, "feature": "feat-x"}))
	if len(next) != 1 || !slices.Equal(next[0].Command, apply) {
		t.Errorf("interrupted after the plan: next = %+v, want %v", next, apply)
	}
	a.command, a.args = "apply", []string{"apply", "20261009T130000Z-123456", "--json"}
	next = a.next(contract.New(contract.BranchExists, "x").WithDetails(map[string]any{"branch": "feat-x", "sha": "abc", "plan_id": id}))
	if len(next) == 0 || !slices.Equal(next[0].Command, apply) {
		t.Errorf("branch at an earlier plan: next = %+v, want %v first", next, apply)
	}
	if next := a.next(contract.New(contract.BranchExists, "x").WithDetails(map[string]any{"branch": "feat-x", "sha": "abc"})); len(next) != 0 {
		t.Errorf("branch at another commit: next = %+v, want none", next)
	}
}
