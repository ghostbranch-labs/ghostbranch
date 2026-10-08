package engine_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// Every command and failure path runs; the public repo may change only
// through promote/apply, and only once here.
func TestOnlyPromoteAndApplyReachThePublicRepo(t *testing.T) {
	t.Parallel()
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
	// Public main moves on with a push-triggered workflow, so sync really
	// pushes and looks for (and cancels) the runs that push started.
	h.eng.CIAppearWindow, h.eng.CIAppearPoll = 3*time.Second, time.Second
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{".github/workflows/push.yml": pushWorkflow})
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	h.forge.Runs[publicName] = []fake.Run{{SHA: next, Run: wfRun(2, "ci", "queued")}}
	baseline = h.publicRefs()
	n = len(h.pushes)
	sres, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	run("sync", err, "")
	if len(h.pushes) == n || sres.ShadowMainAfter != next || len(sres.CancelledRuns) != 1 {
		t.Fatalf("sync did not push and cancel: %+v", sres)
	}
	for _, repo := range append(append([]string(nil), h.forge.PushRunsRepos...), h.forge.CancelRunRepos...) {
		if repo != shadowName {
			t.Fatalf("sync asked about workflow runs in %s", repo)
		}
	}
	if len(h.forge.PushRunsRepos) == 0 || len(h.forge.CancelRunRepos) == 0 || len(h.forge.Cancelled[publicName]) != 0 {
		t.Fatalf("PushRuns %v CancelRun %v cancelled %v", h.forge.PushRunsRepos, h.forge.CancelRunRepos, h.forge.Cancelled)
	}
	n = len(h.pushes)
	_, _, err = h.eng.Audit(h.ctx, h.pair)
	run("audit", err, "")
	if len(h.pushes) != n {
		t.Fatal("audit must never push")
	}
	// A schedule makes upgrade install the sync workflow, so it really pushes.
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	n = len(h.pushes)
	_, _, err = h.eng.Upgrade(h.ctx, h.pair)
	run("upgrade", err, "")
	if len(h.pushes) == n {
		t.Fatal("upgrade did not push")
	}
	for _, p := range h.pushes[n:] {
		if p.remote != "shadow" {
			t.Fatalf("upgrade pushed to %s: %+v", p.remote, p)
		}
	}
	good := h.startFeature("good", map[string]string{"good.txt": "g\n"})
	h.startFeature("leaky", map[string]string{"leak.env": "T=" + testutil.FakeGitHubToken() + "\n"})
	h.startFeature("red", map[string]string{"red.txt": "r\n"})
	h.startFeature("marked", map[string]string{"m.go": "// BEGIN-INTERNAL\n"})

	h.eng.DryRun = true
	_, _, err = h.eng.Gate(h.ctx, h.pair, "good")
	run("gate --dry-run", err, "")
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "squash", Message: "m", Yes: true})
	run("promote --dry-run", err, "")
	h.eng.DryRun = false

	_, _, err = h.eng.Gate(h.ctx, h.pair, "good")
	run("gate", err, "")
	_, _, err = h.eng.Gate(h.ctx, h.pair, "leaky")
	run("gate leaky", err, contract.LeakFound)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "leaky", Mode: "squash", Message: "m", Yes: true})
	run("promote leaky", err, contract.LeakFound)
	_, _, err = h.eng.Gate(h.ctx, h.pair, "marked")
	run("gate marked", err, contract.MarkerInvalid)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "marked", Mode: "squash", Message: "m", Yes: true})
	run("promote marked", err, contract.MarkerInvalid)

	// The history modes' own failures, and a --plan in each of them.
	r1 := h.startFeature("reorder", map[string]string{"r.txt": "1\n"})
	r2 := testutil.Commit(t, h.shadowGit(), "reorder", "edit r", map[string]string{"r.txt": "2\n"})
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "reorder", Mode: "curated", Title: "t", Yes: true,
		Curate: writeCurate(t, `{"commits": [{"from": ["`+r2+`"], "message": "x"}, {"from": ["`+r1+`"], "message": "y"}]}`)})
	run("promote curated conflict", err, contract.CurateConflict)
	// README back to public main's content: replayed first it is clean and
	// empty, so the reorder ends on another tree than the gated one.
	readme, _, _ := h.pubGit.ShowFile(h.ctx, "refs/heads/main", "README.md")
	m1 := h.startFeature("mismatch", map[string]string{"README.md": "changed\n", "y.txt": "y\n"})
	m2 := testutil.Commit(t, h.shadowGit(), "mismatch", "back", map[string]string{"README.md": string(readme)})
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "mismatch", Mode: "curated", Title: "t", Yes: true,
		Curate: writeCurate(t, `{"commits": [{"from": ["`+m2+`"], "message": "b"}, {"from": ["`+m1+`"], "message": "a"}]}`)})
	run("promote curated tree mismatch", err, contract.CurateTreeMismatch)
	h.startFeature("notes", map[string]string{".ghostbranch/notes.md": "private\n"})
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "notes", Yes: true})
	run("promote nothing to publish", err, contract.NothingToPublish)
	mapped := "author_email: map\n  author_map:\n    \"x@y.example\": \"X <x@y.example>\""
	h.setConfig("author_email: noreply", mapped)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Yes: true})
	run("promote preserve, unmapped author", err, contract.AuthorUnmapped)
	h.setConfig(mapped, "author_email: noreply")
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", PlanOnly: true})
	run("promote --plan, preserve", err, "")
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "curated", PlanOnly: true,
		Curate: writeCurate(t, `{"commits": [{"from": ["`+good+`"], "message": "add good"}]}`)})
	run("promote --plan, curated", err, "")

	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "failure"}}
	}
	_, _, err = h.eng.Gate(h.ctx, h.pair, "red")
	run("gate red", err, contract.ChecksFailed)
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "red", Mode: "squash", Message: "m", Yes: true})
	run("promote red", err, contract.ChecksFailed)
	h.forge.Checks = nil

	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "squash", Message: "m", PlanOnly: true})
	run("promote --plan", err, "")
	h.eng.Approver = nil
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "squash", Message: "m", Yes: true})
	run("promote --yes without a console or grant", err, contract.ApprovalRequired)
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
	run("apply --yes without a console or grant", err, contract.ApprovalRequired)
	h.eng.Approver = &approver{err: &approval.Refusal{Reason: approval.NotConfirmed}}
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID})
	run("declined apply", err, contract.ApprovalRequired)
	h.eng.Approver = h.approver

	applied, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID})
	if err != nil {
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

	// status, feature list and feature show only read, a published feature
	// among the ones they report.
	baseline = h.publicRefs()
	n = len(h.pushes)
	_, _, err = h.eng.Status(h.ctx, h.pair)
	run("status", err, "")
	_, _, err = h.eng.FeatureList(h.ctx, h.pair, true)
	run("feature list --all", err, "")
	_, _, err = h.eng.FeatureShow(h.ctx, h.pair, "good")
	run("feature show", err, "")
	if len(h.pushes) != n {
		t.Fatal("status, feature list and feature show must never push")
	}

	// abandon and close write only to the shadow repo: a park and its undo,
	// a close --plan, a close refused for want of a console, a close at the
	// console, and a merged feature's close with no console.
	_, _, err = h.eng.FeatureAbandon(h.ctx, h.pair, "leaky", false)
	run("feature abandon", err, "")
	_, _, err = h.eng.FeatureAbandon(h.ctx, h.pair, "leaky", true)
	run("feature abandon --undo", err, "")
	h.eng.DryRun = true
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "good")
	run("feature close --plan", err, "")
	h.eng.DryRun = false
	h.eng.Approver = nil
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "red")
	run("feature close without a console", err, contract.ApprovalRequired)
	h.eng.Approver = h.approver
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "red")
	run("feature close at the console", err, "")
	h.forge.SetPRMerged(publicName, applied.PublicPR)
	h.eng.Approver = nil
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "good")
	run("feature close, merged", err, "")
	h.eng.Approver = h.approver
	for _, p := range h.pushes[n:] {
		if p.remote != "shadow" {
			t.Fatalf("%s pushed to %s: %+v", p.op, p.remote, p)
		}
	}

	// remove is local: it unregisters the pair and touches no repo. It goes
	// last, since nothing runs on the pair after it.
	baseline = h.publicRefs()
	n = len(h.pushes)
	_, _, err = h.eng.Remove(h.ctx, h.pair)
	run("remove", err, "")
	if len(h.pushes) != n {
		t.Fatal("remove must never push")
	}
}

// Review Focus 5.
func TestAShadowRepoTurnedPublicStopsEverything(t *testing.T) {
	t.Parallel()
	for _, mutate := range []func(*harness){
		func(h *harness) { h.forge.SetPrivate(shadowName, false) },
		func(h *harness) { h.forge.SetFork(shadowName, true) },
	} {
		h := newHarness(t)
		h.init()
		h.startFeature("good", map[string]string{"good.txt": "g\n"})
		res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "squash", Message: "m", PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		mutate(h)
		n, before := len(h.pushes), h.publicRefs()
		steps := map[string]error{}
		_, _, steps["init"] = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		_, _, steps["sync"] = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
		_, _, steps["feature start"] = h.eng.FeatureStart(h.ctx, h.pair, "other")
		_, _, steps["gate"] = h.eng.Gate(h.ctx, h.pair, "good")
		_, _, steps["promote"] = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "good", Mode: "squash", Message: "m", Yes: true})
		_, _, steps["apply"] = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		_, _, steps["status"] = h.eng.Status(h.ctx, h.pair)
		_, _, steps["feature list"] = h.eng.FeatureList(h.ctx, h.pair, true)
		_, _, steps["feature show"] = h.eng.FeatureShow(h.ctx, h.pair, "good")
		_, _, steps["feature abandon"] = h.eng.FeatureAbandon(h.ctx, h.pair, "good", false)
		_, _, steps["feature close"] = h.eng.FeatureClose(h.ctx, h.pair, "good")
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
