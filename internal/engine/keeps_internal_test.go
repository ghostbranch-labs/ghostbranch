package engine

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

// F-31: a history worth keeping is a PR of more than one commit in
// preserve or curated mode; "Squash and merge" would flatten it.
func TestKeepsHistory(t *testing.T) {
	t.Parallel()
	one, two := []plan.Commit{{}}, []plan.Commit{{}, {}}
	entry := func(mode string, cs []plan.Commit) plan.Entry {
		return plan.Entry{Mode: mode, Publish: plan.Publish{Commits: cs}}
	}
	for _, c := range []struct {
		name string
		pl   plan.Plan
		want bool
	}{
		{"preserve, two commits", plan.Plan{Mode: "preserve", Publish: plan.Publish{Commits: two}}, true},
		{"preserve, one commit", plan.Plan{Mode: "preserve", Publish: plan.Publish{Commits: one}}, false},
		{"curated, two groups", plan.Plan{Mode: "curated", Publish: plan.Publish{Commits: two}}, true},
		{"squash", plan.Plan{Mode: "squash", Publish: plan.Publish{Commits: one}}, false},
		{"stack: squash bottom, preserve top of two", plan.Plan{Mode: "squash", Publish: plan.Publish{Commits: one},
			Chain: []plan.Entry{entry("squash", one), entry("preserve", two)}}, true},
		{"stack: all squash", plan.Plan{Mode: "squash", Publish: plan.Publish{Commits: one},
			Chain: []plan.Entry{entry("squash", one), entry("squash", one)}}, false},
		{"stack: curated level of one", plan.Plan{Mode: "curated", Publish: plan.Publish{Commits: one},
			Chain: []plan.Entry{entry("curated", one), entry("squash", one)}}, false},
		// Review M2: an append adds its commits on top of the open PR's own,
		// so the PR holds more than one commit even when the plan adds one.
		{"preserve, append of one onto an open PR", plan.Plan{Mode: "preserve",
			Publish: plan.Publish{Commits: one, Update: true, Style: plan.StyleAppend}}, true},
		{"preserve, replace with one", plan.Plan{Mode: "preserve",
			Publish: plan.Publish{Commits: one, Update: true, Style: plan.StyleReplace}}, false},
		{"squash, append of one", plan.Plan{Mode: "squash",
			Publish: plan.Publish{Commits: one, Update: true, Style: plan.StyleAppend}}, false},
		{"stack: preserve level appending one", plan.Plan{Mode: "preserve", Publish: plan.Publish{Commits: one},
			Chain: []plan.Entry{entry("squash", one), {Mode: "preserve", Publish: plan.Publish{Commits: one, Update: true, Style: plan.StyleAppend}}}}, true},
	} {
		if got := keepsHistory(c.pl); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
