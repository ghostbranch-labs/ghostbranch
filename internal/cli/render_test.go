package cli

import (
	"reflect"
	"strings"
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

// tableNames is the NAME column of featureTable's rows. The cell is
// everything before the STATE column; tabwriter pads by runes, so it cuts
// by runes ("└─" is more than one byte).
func tableNames(out string) []string {
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	col := strings.Index(lines[0], "STATE")
	var names []string
	for _, line := range lines[1:] {
		names = append(names, strings.TrimRight(string([]rune(line)[:col]), " "))
	}
	return names
}

func TestFeatureTableShowsStacksAsTrees(t *testing.T) {
	t.Parallel()
	n := 4
	rs := []engine.FeatureReport{
		{Name: "a", State: "published", Parent: "main", Children: []string{"b"}, Drift: engine.Drift{Status: "clean"}, Stack: &n},
		{Name: "b", State: "published", Parent: "a", Children: []string{"c", "d"}, Drift: engine.Drift{Status: "clean"}, Stack: &n},
		{Name: "c", State: "parent_stale", Parent: "b", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
		{Name: "d", State: "draft", Parent: "b", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
		{Name: "solo", State: "draft", Parent: "main", Children: []string{}, Drift: engine.Drift{Status: "clean"}},
	}
	var b strings.Builder
	featureTable(&b, rs)
	want := []string{"a", "└─ b", "  └─ c", "  └─ d", "solo"}
	if names := tableNames(b.String()); !reflect.DeepEqual(names, want) {
		t.Fatalf("NAME column = %q, want %q\n%s", names, want, b.String())
	}
	if !strings.Contains(b.String(), "stack #4") || !strings.Contains(b.String(), "parent_stale") {
		t.Fatalf("the table must show the stack and the state:\n%s", b.String())
	}
}

// Records whose parents loop have no root; every feature is still listed,
// once.
func TestFeatureTableListsALoopFlat(t *testing.T) {
	t.Parallel()
	rs := []engine.FeatureReport{
		{Name: "x", State: "draft", Parent: "y", Drift: engine.Drift{Status: "clean"}},
		{Name: "y", State: "draft", Parent: "x", Drift: engine.Drift{Status: "clean"}},
		{Name: "z", State: "unreadable", Parent: "", Drift: engine.Drift{Status: "unknown"}},
	}
	var b strings.Builder
	featureTable(&b, rs)
	if names, want := tableNames(b.String()), []string{"z", "x", "└─ y"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("NAME column = %q, want %q\n%s", names, want, b.String())
	}
}

// feature show names the branch below and the branches on it, only for a
// feature in a stack.
func TestFeatureShowTextNamesTheStack(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		r    engine.FeatureReport
		want string
	}{
		{engine.FeatureReport{Name: "b", Parent: "a", Children: []string{"c", "d"}}, "  stack: on a; branches on it: c, d\n"},
		{engine.FeatureReport{Name: "c", Parent: "b", Children: []string{}}, "  stack: on b; branches on it: none\n"},
		{engine.FeatureReport{Name: "a", Parent: "main", Children: []string{"b"}}, "  stack: on main; branches on it: b\n"},
		{engine.FeatureReport{Name: "solo", Parent: "main", Children: []string{}}, ""},
	} {
		var b strings.Builder
		featureShowText(&b, &engine.FeatureShowResult{Feature: c.r})
		has := strings.Contains(b.String(), "  stack: ")
		if (c.want == "" && has) || (c.want != "" && !strings.Contains(b.String(), c.want)) {
			t.Errorf("%s: feature show =\n%s\nwant the line %q", c.r.Name, b.String(), c.want)
		}
	}
}
