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
		{"another PR", moved(func(w *feature.GateUpdate) { w.PR = 8 }), u(), meta(false), "squash", false, false},
		{"another head", moved(func(w *feature.GateUpdate) { w.Head = "h2" }), u(), meta(false), "squash", false, false},
		{"another base", moved(func(w *feature.GateUpdate) { w.Base = "release" }), u(), meta(false), "squash", false, false},
	} {
		if got := gateFitsUpdate(c.gt, c.u, c.m, c.mode, c.above); got != c.want {
			t.Errorf("%s: gateFitsUpdate = %v, want %v", c.name, got, c.want)
		}
	}
}

// planReason: a replace in squash or curated mode says the mode; anything
// else keeps the gate's reason.
func TestPlanReason(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ style, gated, mode, want string }{
		{plan.StyleReplace, plan.ReasonRestack, "squash", plan.ReasonSquash},
		{plan.StyleReplace, plan.ReasonRebase, "curated", plan.ReasonCurated},
		{plan.StyleReplace, plan.ReasonRebase, "preserve", plan.ReasonRebase},
		{plan.StyleAppend, "", "preserve", ""},
	} {
		if got := planReason(c.style, c.gated, c.mode); got != c.want {
			t.Errorf("planReason(%s, %s, %s) = %q, want %q", c.style, c.gated, c.mode, got, c.want)
		}
	}
}
