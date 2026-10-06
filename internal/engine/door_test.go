package engine_test

import (
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// Every command and failure path runs; the public repo may change only
// through promote/apply, and only once here.
func TestOnlyPromoteAndApplyReachThePublicRepo(t *testing.T) {
	h := newHarness(t)
	h.init()
	baseline := h.publicRefs()
	still := func(step string) {
		t.Helper()
		if !reflect.DeepEqual(h.publicRefs(), baseline) {
			t.Fatalf("%s changed the public repo", step)
		}
	}
	run := func(step string, err error, want contract.Code) {
		t.Helper()
		if codeOf(err) != want {
			t.Fatalf("%s: want %q, got %v", step, want, err)
		}
		still(step)
	}
	n := len(h.pushes)
	_, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{})
	run("agents allow", err, "")
	_, _, err = h.eng.AgentsStatus(h.ctx, &h.pair)
	run("agents status", err, "")
	_, _, err = h.eng.AgentsRevoke(h.ctx, &h.pair, false)
	run("agents revoke", err, "")
	if len(h.pushes) != n {
		t.Fatal("agents commands must never push")
	}
	_, _, err = h.eng.Sync(h.ctx, h.pair)
	run("sync", err, "")
	h.startFeature("good", map[string]string{"good.txt": "g\n"})
	h.startFeature("leaky", map[string]string{"leak.env": "T=" + testutil.FakeGitHubToken() + "\n"})
	h.startFeature("red", map[string]string{"red.txt": "r\n"})

	h.eng.DryRun = true
	_, _, err = h.eng.Gate(h.ctx, h.pair, "good")
	run("gate --dry-run", err, "")
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Message: "m", Yes: true})
	run("promote --dry-run", err, "")
	h.eng.DryRun = false

	_, _, err = h.eng.Gate(h.ctx, h.pair, "good")
	run("gate", err, "")
	_, _, err = h.eng.Gate(h.ctx, h.pair, "leaky")
	run("gate leaky", err, contract.LeakFound)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "leaky", Message: "m", Yes: true})
	run("promote leaky", err, contract.LeakFound)

	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "failure"}}
	}
	_, _, err = h.eng.Gate(h.ctx, h.pair, "red")
	run("gate red", err, contract.ChecksFailed)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "red", Message: "m", Yes: true})
	run("promote red", err, contract.ChecksFailed)
	h.forge.Checks = nil

	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Message: "m", PlanOnly: true})
	run("promote --plan", err, "")
	h.eng.Approver = nil
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Message: "m", Yes: true})
	run("promote --yes without a console or grant", err, contract.ApprovalRequired)
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
	run("apply --yes without a console or grant", err, contract.ApprovalRequired)
	h.eng.Approver = &approver{err: &approval.Refusal{Reason: approval.NotConfirmed}}
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID})
	run("declined apply", err, contract.ApprovalRequired)
	h.eng.Approver = h.approver

	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var public []pushEvent
	for _, p := range h.pushes {
		if p.remote == "public" {
			public = append(public, p)
		}
	}
	if len(public) != 1 || public[0].op != "apply" {
		t.Fatalf("public pushes = %+v", public)
	}
}

// Review Focus 5.
func TestAShadowRepoTurnedPublicStopsEverything(t *testing.T) {
	for _, mutate := range []func(*harness){
		func(h *harness) { h.forge.SetPrivate(shadowName, false) },
		func(h *harness) { h.forge.SetFork(shadowName, true) },
	} {
		h := newHarness(t)
		h.init()
		h.startFeature("good", map[string]string{"good.txt": "g\n"})
		res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Message: "m", PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		mutate(h)
		n, before := len(h.pushes), h.publicRefs()
		steps := map[string]error{}
		_, _, steps["init"] = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		_, _, steps["sync"] = h.eng.Sync(h.ctx, h.pair)
		_, _, steps["feature start"] = h.eng.FeatureStart(h.ctx, h.pair, "other")
		_, _, steps["gate"] = h.eng.Gate(h.ctx, h.pair, "good")
		_, _, steps["promote"] = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Message: "m", Yes: true})
		_, _, steps["apply"] = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		for step, err := range steps {
			if codeOf(err) != contract.ShadowNotPrivate {
				t.Errorf("%s: want SHADOW_NOT_PRIVATE, got %v", step, err)
			}
		}
		if len(h.pushes) != n || !reflect.DeepEqual(h.publicRefs(), before) {
			t.Fatal("something was pushed after the shadow repo stopped being private")
		}
	}
}
