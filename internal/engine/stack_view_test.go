package engine_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// status and feature list name each feature's parent and children, and the
// native stack its PR is in.
func TestSurveyShowsTheStack(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b", "c")
	h.startFeature("solo", map[string]string{"solo.txt": "s\n"})
	rs, _ := h.survey()
	got := map[string]engine.FeatureReport{}
	for _, r := range rs {
		got[r.Name] = r
		validateResult(t, "feature-report.schema.json", r)
	}
	for name, want := range map[string]struct {
		parent   string
		children []string
	}{"a": {"main", []string{"b"}}, "b": {"a", []string{"c"}}, "c": {"b", []string{}}, "solo": {"main", []string{}}} {
		r := got[name]
		if r.Parent != want.parent || !reflect.DeepEqual(r.Children, want.children) || r.Stack != nil {
			t.Errorf("%s: parent %q, children %v, stack %v; want %q, %v, none", name, r.Parent, r.Children, r.Stack, want.parent, want.children)
		}
	}
	// A survey of one feature still names all its children.
	if r := h.report("a"); !reflect.DeepEqual(r.Children, []string{"b"}) {
		t.Fatalf("report(a).children = %v", r.Children)
	}
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b", "c"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a", "b", "c"} {
		if r := h.report(n); r.Stack == nil || *r.Stack != *res.Applied.StackNumber {
			t.Errorf("%s: stack = %v, want %d", n, r.Stack, *res.Applied.StackNumber)
		}
	}
}

// An abandoned child is not among its parent's children: they are the live
// features stacked on it.
func TestSurveyLeavesAbandonedChildrenOut(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	h.startOn("c", "a", map[string]string{"c.txt": "c\n"})
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "c", false); err != nil {
		t.Fatal(err)
	}
	if r := h.report("a"); !reflect.DeepEqual(r.Children, []string{"b"}) {
		t.Fatalf("a's children = %v, want [b]", r.Children)
	}
}

// The gate tells next where the stack goes: the branches above it, or the
// list a promote of the stack takes. A feature on its own gets none of it.
func TestGateNamesItsStack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	h.startOn("c", "b", map[string]string{"c.txt": "c\n"})
	h.startFeature("solo", map[string]string{"solo.txt": "s\n"})
	gate := func(name string) *engine.GateResult {
		t.Helper()
		res, _, err := h.eng.Gate(h.ctx, h.pair, name)
		if err != nil {
			t.Fatalf("gate %s: %v", name, err)
		}
		validateResult(t, "gate.schema.json", res)
		return res
	}
	if r := gate("a"); r.Parent != "main" || !reflect.DeepEqual(r.Children, []string{"b"}) || !reflect.DeepEqual(r.Chain, []string{"a"}) {
		t.Fatalf("gate a: parent %q, children %v, chain %v", r.Parent, r.Children, r.Chain)
	}
	gate("b")
	if r := gate("c"); r.Parent != "b" || len(r.Children) != 0 || !reflect.DeepEqual(r.Chain, []string{"a", "b", "c"}) {
		t.Fatalf("gate c: parent %q, children %v, chain %v", r.Parent, r.Children, r.Chain)
	}
	if r := gate("solo"); r.Parent != "" || r.Children != nil || r.Chain != nil {
		t.Fatalf("gate solo: parent %q, children %v, chain %v; a feature on its own gets none", r.Parent, r.Children, r.Chain)
	}
	// The record keeps what the gate built on: a bottom feature's gate
	// stacked on nothing, whatever its result says for next.
	if p := h.meta("a").Gate.Parent; p != "" {
		t.Fatalf("a's gate record has parent %q, want none", p)
	}
}

// feature show lists a stacked feature's own commits, not its parent's.
func TestFeatureShowListsOnlyItsOwnCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	res, _, err := h.eng.FeatureShow(h.ctx, h.pair, "b")
	if err != nil {
		t.Fatal(err)
	}
	validateResult(t, "feature-show.schema.json", res)
	if len(res.Commits) != 1 || !reflect.DeepEqual(res.Commits[0].Files, []string{"b.txt"}) {
		t.Fatalf("feature show b lists %+v, want only b's commit", res.Commits)
	}
	if res.Feature.Parent != "a" || !reflect.DeepEqual(h.report("a").Children, []string{"b"}) {
		t.Fatalf("b's parent %q, a's children %v", res.Feature.Parent, h.report("a").Children)
	}
}

// A feature below the gated one that conflicts names the branch it was
// replayed onto, not public main.
func TestStackedGateConflictNamesTheLevelBelow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	h.startOn("b", "a", map[string]string{"shared.txt": "b\n"})
	h.startOn("c", "b", map[string]string{"c.txt": "c\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"shared.txt": "public\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "c")
	ce := wantCode(t, err, contract.RebaseConflict)
	if ce.Details["feature"] != "b" || !strings.Contains(ce.Message, "does not rebase cleanly onto a (rebased onto public main)") {
		t.Fatalf("conflict = %q %+v", ce.Message, ce.Details)
	}
	if res == nil || res.Parent != "b" {
		t.Fatalf("a gate stopped below names its parent: %+v", res)
	}
}
