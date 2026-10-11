package engine

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

// gateFitsUpdate: an append gate never stands for a forced replace; any
// replace gate stands for one, in every mode; a mismatched PR, head or base
// never fits; with nothing forced only what the history alone gives fits.
func TestGateFitsUpdate(t *testing.T) {
	t.Parallel()
	u := func() *prUpdate { return &prUpdate{PR: 7, Branch: "f", Base: "main", Head: "h"} }
	meta := func(needs bool) feature.Meta {
		return feature.Meta{Name: "f", Promoted: &feature.Promoted{PublicPR: 7, Base: "main", NeedsUpdate: needs}}
	}
	gate := func(style, reason string) *feature.Gate {
		return &feature.Gate{Update: &feature.GateUpdate{PR: 7, Head: "h", Base: "main", Style: style, Reason: reason}}
	}
	baseWas := func(b string) feature.Meta { m := meta(false); m.Promoted.Base = b; return m }
	restores := func() *prUpdate { x := u(); x.Restores = []string{"gone"}; return x }
	moved := func(edit func(*feature.GateUpdate)) *feature.Gate {
		g := gate(plan.StyleReplace, plan.ReasonRebase)
		edit(g.Update)
		return g
	}
	for _, c := range []struct {
		name  string
		gt    *feature.Gate
		u     *prUpdate
		m     feature.Meta
		mode  string
		above bool
		want  bool
	}{
		{"no gate", nil, u(), meta(false), "preserve", false, false},
		{"no PR, no update", &feature.Gate{}, nil, meta(false), "preserve", false, true},
		{"no PR, an update gate", gate(plan.StyleAppend, ""), nil, meta(false), "preserve", false, false},
		{"a PR, a first-publish gate", &feature.Gate{}, u(), meta(false), "preserve", false, false},
		{"append, nothing forced", gate(plan.StyleAppend, ""), u(), meta(false), "preserve", false, true},
		{"append, squash forces a replace", gate(plan.StyleAppend, ""), u(), meta(false), "squash", false, false},
		{"append, curated forces a replace", gate(plan.StyleAppend, ""), u(), meta(false), "curated", false, false},
		{"append, open PRs above", gate(plan.StyleAppend, ""), u(), meta(false), "preserve", true, false},
		{"append, restack flagged", gate(plan.StyleAppend, ""), u(), meta(true), "preserve", false, false},
		{"replace rebase, squash", gate(plan.StyleReplace, plan.ReasonRebase), u(), meta(false), "squash", false, true},
		{"replace restack, squash", gate(plan.StyleReplace, plan.ReasonRestack), u(), meta(false), "squash", true, true},
		{"replace conflict, curated", gate(plan.StyleReplace, plan.ReasonConflict), u(), meta(false), "curated", false, true},
		{"replace squash, open PRs above", gate(plan.StyleReplace, plan.ReasonSquash), u(), meta(false), "preserve", true, true},
		{"replace rebase, restack flagged", gate(plan.StyleReplace, plan.ReasonRebase), u(), meta(true), "preserve", false, true},
		{"replace rebase, nothing forced", gate(plan.StyleReplace, plan.ReasonRebase), u(), meta(false), "preserve", false, true},
		{"replace reworded, nothing forced", gate(plan.StyleReplace, plan.ReasonReworded), u(), meta(false), "preserve", false, true},
		{"replace conflict, nothing forced", gate(plan.StyleReplace, plan.ReasonConflict), u(), meta(false), "preserve", false, true},
		{"replace squash, nothing forced", gate(plan.StyleReplace, plan.ReasonSquash), u(), meta(false), "preserve", false, false},
		{"replace curated, nothing forced", gate(plan.StyleReplace, plan.ReasonCurated), u(), meta(false), "preserve", false, false},
		{"replace restack, nothing forced", gate(plan.StyleReplace, plan.ReasonRestack), u(), meta(false), "preserve", false, false},
		// base_changed: the PR's base is main, as the gate saw it, but the
		// record says it was published on release.
		{"append, base changed", gate(plan.StyleAppend, ""), u(), baseWas("release"), "preserve", false, false},
		{"replace, base changed", gate(plan.StyleReplace, plan.ReasonRebase), u(), baseWas("release"), "preserve", false, true},
		{"append, commits removed on GitHub", gate(plan.StyleAppend, ""), restores(), meta(false), "preserve", false, false},
		{"replace, commits removed on GitHub", gate(plan.StyleReplace, plan.ReasonConflict), restores(), meta(false), "preserve", false, true},
		{"append with a reason", gate(plan.StyleAppend, plan.ReasonRebase), u(), meta(false), "preserve", false, false},
		{"another PR, nothing forced", moved(func(w *feature.GateUpdate) { w.PR = 8 }), u(), meta(false), "preserve", false, false},
		{"another head, nothing forced", moved(func(w *feature.GateUpdate) { w.Head = "h2" }), u(), meta(false), "preserve", false, false},
		{"another base, nothing forced", moved(func(w *feature.GateUpdate) { w.Base = "release" }), u(), meta(false), "preserve", false, false},
		{"another PR", moved(func(w *feature.GateUpdate) { w.PR = 8 }), u(), meta(false), "squash", false, false},
		{"another head", moved(func(w *feature.GateUpdate) { w.Head = "h2" }), u(), meta(false), "squash", false, false},
		{"another base", moved(func(w *feature.GateUpdate) { w.Base = "release" }), u(), meta(false), "squash", false, false},
	} {
		if got := gateFitsUpdate(c.gt, c.u, c.m, c.mode, c.above); got != c.want {
			t.Errorf("%s: gateFitsUpdate = %v, want %v", c.name, got, c.want)
		}
	}
}

// planReason: a replace says what forces it now when something does
// (squash or curated mode, restack, base_changed); else the gate's reason.
// An append keeps none.
func TestPlanReason(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ style, gated, forced, want string }{
		{plan.StyleReplace, plan.ReasonRestack, plan.ReasonSquash, plan.ReasonSquash},
		{plan.StyleReplace, plan.ReasonRebase, plan.ReasonCurated, plan.ReasonCurated},
		{plan.StyleReplace, plan.ReasonSquash, plan.ReasonRestack, plan.ReasonRestack},
		{plan.StyleReplace, plan.ReasonCurated, plan.ReasonBaseChanged, plan.ReasonBaseChanged},
		{plan.StyleReplace, plan.ReasonRebase, "", plan.ReasonRebase},
		{plan.StyleAppend, "", "", ""},
	} {
		if got := planReason(c.style, c.gated, c.forced); got != c.want {
			t.Errorf("planReason(%s, %s, %s) = %q, want %q", c.style, c.gated, c.forced, got, c.want)
		}
	}
}

// reasonStands: what apply accepts as the plan's reason for the gate's
// update. A preserve replace never shows a mode's reason; squash and
// curated always show their own.
func TestReasonStands(t *testing.T) {
	t.Parallel()
	g := func(style, reason string) feature.GateUpdate { return feature.GateUpdate{Style: style, Reason: reason} }
	for _, c := range []struct {
		gu           feature.GateUpdate
		mode, reason string
		want         bool
	}{
		{g(plan.StyleAppend, ""), "preserve", "", true},
		{g(plan.StyleAppend, ""), "preserve", plan.ReasonRestack, false},
		{g(plan.StyleReplace, plan.ReasonRebase), "squash", plan.ReasonSquash, true},
		{g(plan.StyleReplace, plan.ReasonRebase), "squash", plan.ReasonRebase, false},
		{g(plan.StyleReplace, plan.ReasonRestack), "curated", plan.ReasonCurated, true},
		{g(plan.StyleReplace, plan.ReasonSquash), "preserve", plan.ReasonSquash, false},
		{g(plan.StyleReplace, plan.ReasonSquash), "preserve", plan.ReasonRestack, true},
		{g(plan.StyleReplace, plan.ReasonCurated), "preserve", plan.ReasonBaseChanged, true},
		{g(plan.StyleReplace, plan.ReasonReworded), "preserve", plan.ReasonReworded, true},
		{g(plan.StyleReplace, plan.ReasonConflict), "preserve", plan.ReasonRebase, true},
		{g(plan.StyleReplace, plan.ReasonRebase), "preserve", plan.ReasonConflict, false},
	} {
		if got := reasonStands(c.gu, c.mode, c.reason); got != c.want {
			t.Errorf("reasonStands(%+v, %s, %s) = %v, want %v", c.gu, c.mode, c.reason, got, c.want)
		}
	}
}
