package cli

import (
	"slices"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

// A resumed update whose push had landed archives nothing (R14), though
// the person may have picked Archive in the run that died: its kept is not
// final, so next offers feature archive for each feature, top first,
// after the usual merge step. (A landed update can't be staged through
// Run without a public push the CLI never makes, so next is called here.)
func TestNextAfterALandedUpdateOffersTheArchive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		prs  []plan.StackPR
		want []string
	}{
		{"one feature", []plan.StackPR{{Feature: "feat-x", Base: "main", Number: 7}}, []string{"feat-x"}},
		{"a stack", []plan.StackPR{{Feature: "feat-a", Base: "main", Number: 7}, {Feature: "feat-b", Base: "feat-a", Number: 8}},
			[]string{"feat-b", "feat-a"}},
	} {
		a := testApp("apply", "p1", "--yes")
		after := map[string]string{}
		for _, p := range tc.prs {
			after[p.Feature] = engine.AfterKept
		}
		top := tc.prs[len(tc.prs)-1]
		a.result = &engine.ApplyResult{PlanID: "p1", PublicBranch: "feat-b", PublicPR: top.Number, PRs: tc.prs,
			Resumed: true, Landed: true, AfterPublish: after}
		next := a.next(nil)
		if len(next) != 1+len(tc.want) || !slices.Equal(next[0].Command, []string{"ghostbranch", "sync", "--repo", "acme/widget"}) {
			t.Fatalf("%s: next = %+v", tc.name, next)
		}
		for i, f := range tc.want {
			n := next[1+i]
			if !slices.Equal(n.Command, []string{"ghostbranch", "feature", "archive", f, "--repo", "acme/widget"}) || n.NeedsHuman || n.Why == "" {
				t.Fatalf("%s: next[%d] = %+v", tc.name, 1+i, n)
			}
		}
		// Without Landed, a kept result is final: no archive step.
		a.result.(*engine.ApplyResult).Landed = false
		if next := a.next(nil); len(next) != 1 {
			t.Fatalf("%s: a kept publish's next = %+v", tc.name, next)
		}
	}
}
