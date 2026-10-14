package cli

import (
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
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

// A stack's hand-off runs top first and stops at the first failed level
// (P5): the levels below it stay kept although Archive was chosen for every
// level. next finishes the failed level, then offers feature archive for
// each kept level below it, top first (so none is refused as HAS_CHILDREN),
// and the merge step names only the archived levels as handed off.
func TestNextAfterAStackWhoseHandOffStopped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		after    map[string]string
		archived []string
		failed   string
		want     []string // feature archive steps, in order
		handed   string   // in the merge step's why; "" for none
	}{
		{"two levels, the top fails", map[string]string{"feat-a": engine.AfterKept, "feat-b": engine.AfterIncomplete},
			nil, "feat-b", []string{"feat-b", "feat-a"}, ""},
		{"three levels, the middle fails",
			map[string]string{"feat-a": engine.AfterKept, "feat-b": engine.AfterIncomplete, "feat-c": engine.AfterArchived},
			[]string{"feat-c"}, "feat-b", []string{"feat-b", "feat-a"}, "feat-c (branch feat-c)"},
	} {
		prs := []plan.StackPR{{Feature: "feat-a", Base: "main", Number: 7}, {Feature: "feat-b", Base: "feat-a", Number: 8}}
		if len(tc.after) == 3 {
			prs = append(prs, plan.StackPR{Feature: "feat-c", Base: "feat-b", Number: 9})
		}
		top := prs[len(prs)-1]
		a := testApp("apply", "p1", "--yes")
		a.warnings = []contract.Warning{{Code: contract.ArchiveIncomplete, Details: map[string]any{"feature": tc.failed, "step": "gate_pr"}}}
		a.result = &engine.ApplyResult{PlanID: "p1", PublicBranch: top.Feature, PublicPR: top.Number, PRs: prs,
			AfterPublish: tc.after, Archived: tc.archived}
		next := a.next(nil)
		if len(next) != len(tc.want)+1 {
			t.Fatalf("%s: next = %+v", tc.name, next)
		}
		for i, f := range tc.want {
			if !slices.Equal(next[i].Command, []string{"ghostbranch", "feature", "archive", f, "--repo", "acme/widget"}) || next[i].Why == "" {
				t.Fatalf("%s: next[%d] = %+v", tc.name, i, next[i])
			}
		}
		merge := next[len(tc.want)]
		if !slices.Equal(merge.Command, []string{"ghostbranch", "sync", "--repo", "acme/widget"}) {
			t.Fatalf("%s: merge step = %+v", tc.name, merge)
		}
		if strings.Contains(merge.Why, "The PRs are handed off") || strings.Contains(merge.Why, "feat-a (branch") || strings.Contains(merge.Why, "feat-b (branch") {
			t.Errorf("%s: the merge step calls a kept level handed off: %s", tc.name, merge.Why)
		}
		if tc.handed != "" && !strings.Contains(merge.Why, tc.handed) {
			t.Errorf("%s: the merge step doesn't name %s: %s", tc.name, tc.handed, merge.Why)
		}
	}
}
