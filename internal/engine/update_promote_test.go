package engine_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// Review Focus 1 and 4: a new commit goes onto the open PR as one more
// commit, a fast-forward of its head, under the same PR.
func TestPromoteAppendsToAnOpenPR(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	H := h.pubTip("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "Fix the review", map[string]string{"more.txt": "more\n"})
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	pub := res.Plan.Publish
	if !pub.Update || pub.Style != "append" || pub.ReplaceReason != "" || pub.ExpectedHead != H || pub.Parent != H || len(pub.Commits) != 1 {
		t.Fatalf("publish = %+v", pub)
	}
	tip := h.pubTip("feat-x")
	if parent, _ := h.pubGit.MustResolve(h.ctx, tip+"^"); tip != pub.Commit || parent != H {
		t.Fatalf("public branch at %s (parent %s), want %s on %s", tip, parent, pub.Commit, H)
	}
	if prs := h.forge.PRs(publicName); len(prs) != 1 {
		t.Fatalf("public PRs = %+v", prs)
	}
	if got := res.Applied.PRs; len(got) != 1 || got[0].Number != pr || !got[0].Update || got[0].Style != "append" {
		t.Fatalf("applied PRs = %+v", got)
	}
	if m := h.meta("feat-x"); m.Promoted.PublishedHead != tip || m.Promoted.PlanID != res.Plan.ID || len(m.Promoted.PatchIDs) != 2 || m.Promoted.PublicPR != pr {
		t.Fatalf("record = %+v", m.Promoted)
	}
	s := h.approver.asked[len(h.approver.asked)-1]
	for _, want := range []string{fmt.Sprintf("Update PR #%d (adds 1 commit)", pr), "Fix the review", "keeps its title and body", "more.txt"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "feature.txt") {
		t.Fatalf("the summary lists a file the PR already has:\n%s", s)
	}
	if es := h.ledger(); es[len(es)-1].Summary != fmt.Sprintf("updated PR #%d on feat-x", pr) {
		t.Fatalf("ledger = %+v", es[len(es)-1])
	}
	if len(h.forge.Edits) != 0 {
		t.Fatalf("an update with no typed texts edited the PR: %+v", h.forge.Edits)
	}
}

// Review Focus 4: squash mode replaces the branch under a lease; the
// summary says so, and the record's patch-ids are the new history's.
func TestPromoteReplacesAChangedHistory(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	H := h.pubTip("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature, reviewed", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	pub := res.Plan.Publish
	if !pub.Update || pub.Style != "replace" || pub.ReplaceReason != "squash" || pub.ExpectedHead != H ||
		pub.Parent != h.pubTip("main") || len(pub.Commits) != 1 {
		t.Fatalf("publish = %+v", pub)
	}
	tip := h.pubTip("feat-x")
	if in, _ := h.pubGit.IsAncestor(h.ctx, H, tip); tip != pub.Commit || in {
		t.Fatalf("branch at %s, plan %s; old head kept: %v", tip, pub.Commit, in)
	}
	if s := h.approver.asked[len(h.approver.asked)-1]; !strings.Contains(s, fmt.Sprintf("Replace PR #%d's branch (history changed: squash)", pr)) {
		t.Fatalf("summary:\n%s", s)
	}
	if m := h.meta("feat-x"); len(m.Promoted.PatchIDs) != 1 || m.Promoted.PublishedHead != tip || len(h.forge.PRs(publicName)) != 1 {
		t.Fatalf("record = %+v", m.Promoted)
	}
	if es := h.ledger(); es[len(es)-1].Summary != fmt.Sprintf("replaced PR #%d's branch feat-x", pr) {
		t.Fatalf("ledger = %+v", es[len(es)-1])
	}
}

// Acceptance 6.1a-2 bug 1: after a clean rebase onto a moved main, status
// says the PR is behind and promote replaces its branch onto the new main
// (reason rebase), never NOTHING_TO_PUBLISH. Once it lands, the feature is
// no longer behind.
func TestPromoteReplacesACleanRebaseOntoAMovedMain(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	H := h.pubTip("feat-x")
	h.moveMain()
	h.rebaseOntoMain()
	if r := h.report("feat-x"); r.State != feature.StatePRBehind {
		t.Fatalf("before the update, feat-x is %s", r.State)
	}
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	pub := res.Plan.Publish
	if !pub.Update || pub.Style != plan.StyleReplace || pub.ReplaceReason != plan.ReasonRebase || pub.ExpectedHead != H ||
		pub.Parent != h.pubTip("main") {
		t.Fatalf("publish = %+v", pub)
	}
	if !res.Gate.Reused {
		t.Fatal("promote ran the gate again: the person's gate built this replace")
	}
	tip := h.pubTip("feat-x")
	if parent, _ := h.pubGit.MustResolve(h.ctx, tip+"^"); tip != pub.Commit || parent != h.pubTip("main") {
		t.Fatalf("public branch at %s on %s, want %s on main %s", tip, parent, pub.Commit, h.pubTip("main"))
	}
	if s := h.approver.asked[len(h.approver.asked)-1]; !strings.Contains(s, fmt.Sprintf("Replace PR #%d's branch (history changed: rebase)", pr)) {
		t.Fatalf("summary:\n%s", s)
	}
	if r := h.report("feat-x"); r.State != feature.StatePublished {
		t.Fatalf("after the update, feat-x is %s", r.State)
	}
}

// A squash or curated promote reuses the person's preserve gate when that
// gate built a replace (here after a clean rebase): the candidate is the
// same in every mode. The plan and the approval say the mode's reason, not
// the reason the gate recorded. What the mode writes that the gate never
// saw (the squash or group message) is scanned by promote: a planted ID
// refuses. Apply scans again: a tree finding waived for the gate and the
// plan, its waiver revoked before apply, refuses there, and nothing moves.
func TestAModeForcedReplaceOnAReusedGate(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"squash", "curated"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			h, pr := publishedFeature(t)
			h.setBlockPatterns("JIRA-[0-9]+")
			H := h.pubTip("feat-x")
			h.moveMain()
			h.rebaseOntoMain()
			testutil.Commit(t, h.shadowGit(), "feat-x", "add a fixture", fixture())
			gr, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if _ = wantCode(t, err, contract.LeakFound); len(gr.Findings) != 1 || !gr.Findings[0].Waivable {
				t.Fatalf("gate findings = %+v", gr.Findings)
			}
			secret := gr.Findings[0]
			h.waive(secret, "test fixture")
			h.gateFeature("feat-x")
			m := h.meta("feat-x")
			if g := m.Gate; g.ScannedAs != "preserve" || g.Update == nil || g.Update.Style != plan.StyleReplace || g.Update.Reason != plan.ReasonRebase {
				t.Fatalf("gate = scanned as %s, update %+v", g.ScannedAs, g.Update)
			}
			opt := func(msg string, planOnly bool) engine.PromoteOptions {
				o := engine.PromoteOptions{Mode: mode, PlanOnly: planOnly, Yes: !planOnly}
				if mode == "squash" {
					o.Message = msg
				} else {
					o.Curate, o.Groups = "c.json", []plan.Group{{From: gateSources(m), Message: msg}}
				}
				return o
			}
			from := len(h.pushes)
			res, err := promote(h, opt("Fix JIRA-123", true))
			ce := wantCode(t, err, contract.LeakFound)
			if ce.Details["mode"] != mode || res == nil || res.Gate == nil || !res.Gate.Reused || len(res.Findings) == 0 ||
				res.Findings[0].Where != "commit_message" {
				t.Fatalf("refusal %+v; result %+v", ce.Details, res)
			}
			res, err = promote(h, opt("Fix the review", true))
			if err != nil || !res.Gate.Reused || res.Plan.Publish.Style != plan.StyleReplace || res.Plan.Publish.ReplaceReason != mode {
				t.Fatalf("plan = %+v %v", res, err)
			}
			id := res.Plan.ID
			if _, err := h.eng.Waivers.Revoke(publicName, []string{secret.ID}, false, h.clock.Now()); err != nil {
				t.Fatal(err)
			}
			_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
			_ = wantCode(t, err, contract.LeakFound)
			if got := h.publicPushes(from); len(got) != 0 || h.pubTip("feat-x") != H {
				t.Fatalf("a refused update pushed %v publicly", got)
			}
			h.waive(secret, "test fixture")
			ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
			if err != nil || ar == nil || h.pubTip("feat-x") != res.Plan.Publish.Commit {
				t.Fatalf("apply = %+v %v", ar, err)
			}
			if s := h.approver.asked[len(h.approver.asked)-1]; !strings.Contains(s, fmt.Sprintf("Replace PR #%d's branch (history changed: %s)", pr, mode)) {
				t.Fatalf("summary:\n%s", s)
			}
		})
	}
}

// The mirror of TestAModeForcedReplaceOnAReusedGate: a replace gate built
// in squash (default_mode squash) is reused by a preserve promote once a
// restack is flagged, which forces a replace. The plan and the approval
// say restack, the reason forced now, never squash; apply accepts it.
func TestAPreserveUpdateOnASquashGateSaysWhatForcesIt(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	h.setConfig("default_mode: preserve", "default_mode: squash")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	if g := h.meta("feat-x").Gate; g.ScannedAs != "squash" || g.Update == nil || g.Update.Reason != plan.ReasonSquash {
		t.Fatalf("gate = scanned as %s, update %+v", g.ScannedAs, g.Update)
	}
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.NeedsUpdate = true })
	res, err := promote(h, engine.PromoteOptions{Mode: "preserve", Yes: true})
	if err != nil || res.Applied == nil || !res.Gate.Reused {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if pub := res.Plan.Publish; pub.Style != plan.StyleReplace || pub.ReplaceReason != plan.ReasonRestack {
		t.Fatalf("publish = style %s reason %s", pub.Style, pub.ReplaceReason)
	}
	if s := h.approver.asked[len(h.approver.asked)-1]; !strings.Contains(s, fmt.Sprintf("Replace PR #%d's branch (history changed: restack)", pr)) {
		t.Fatalf("summary:\n%s", s)
	}
}

// A restack replaces, and the update that lands clears needs_update.
func TestAnUpdateClearsNeedsUpdate(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.NeedsUpdate = true })
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	res, err := promote(h, engine.PromoteOptions{Title: "", Yes: true})
	if err != nil || res.Plan.Publish.ReplaceReason != "restack" {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if h.meta("feat-x").Promoted.NeedsUpdate {
		t.Fatal("needs_update survived the update")
	}
}

// Review Focus 2: a PR head that moved between plan and apply is never
// overwritten: PLAN_STALE, and the re-plan names the foreign commit.
func TestApplyUpdateLeaseRefusesAMovedPR(t *testing.T) {
	t.Parallel()
	t.Run("moved before apply", func(t *testing.T) {
		t.Parallel()
		h, _ := publishedFeature(t)
		testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
		res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		theirs := testutil.Commit(t, h.pubGit, "feat-x", "maintainer fix", map[string]string{"fix.txt": "fix\n"})
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"public_branch"}) {
			t.Fatalf("details = %+v", ce.Details)
		}
		if h.pubTip("feat-x") != theirs {
			t.Fatal("their commit was overwritten")
		}
		_, err = promote(h, engine.PromoteOptions{PlanOnly: true})
		ce := wantCode(t, err, contract.PRChangedOnGitHub)
		if f, _ := ce.Details["foreign"].([]map[string]any); len(f) != 1 || f[0]["sha"] != theirs {
			t.Fatalf("details = %+v", ce.Details)
		}
	})
	t.Run("moved between the approval and the push", func(t *testing.T) {
		t.Parallel()
		h, _ := publishedFeature(t)
		testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
		res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		var theirs string
		record := h.eng.OnPush
		h.eng.OnPush = func(op, remote string, specs []string) {
			record(op, remote, specs)
			if remote == "public" && theirs == "" {
				theirs = testutil.Commit(t, h.pubGit, "feat-x", "racing push", map[string]string{"race.txt": "r\n"})
			}
		}
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"public_branch"}) {
			t.Fatalf("details = %+v", ce.Details)
		}
		if theirs == "" || h.pubTip("feat-x") != theirs {
			t.Fatal("the lease didn't hold: their commit was overwritten")
		}
	})
	t.Run("closed before apply", func(t *testing.T) {
		t.Parallel()
		h, pr := publishedFeature(t)
		testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
		res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		H := h.pubTip("feat-x")
		h.forge.ClosePR(publicName, pr)
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"public_pr"}) {
			t.Fatalf("details = %+v", ce.Details)
		}
		if h.pubTip("feat-x") != H {
			t.Fatal("an update of a closed PR pushed")
		}
	})
}

// An apply whose push already landed only finishes the record, and never
// pushes again.
func TestApplyResumesAnUpdateWhosePushLanded(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	id, P := res.Plan.ID, res.Plan.Publish.Commit
	mirror := gitx.New(h.eng.WS.MirrorDir(h.pair))
	if err := mirror.Push(h.ctx, "public", gitx.PushOptions{}, P+":refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	h.pushes = nil
	for range 2 {
		ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
		if err != nil || !ar.Resumed || ar.PublicPR != pr {
			t.Fatalf("resume = %+v %v", ar, err)
		}
	}
	for _, p := range h.pushes {
		if p.remote == "public" {
			t.Fatalf("a resumed apply pushed again: %+v", p)
		}
	}
	if m := h.meta("feat-x"); m.Promoted.PublishedHead != P || m.Promoted.PlanID != id {
		t.Fatalf("record = %+v", m.Promoted)
	}
}

// R11: an update keeps the PR's branch. A typed title or body is planned,
// shown, and edited on the PR once the push lands; a text not typed is
// left as it is on GitHub.
func TestPromoteUpdateFlags(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	_, err := promote(h, engine.PromoteOptions{Branch: "other", PlanOnly: true})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "branch" {
		t.Fatalf("--branch other: field %s", ce.Field)
	}
	if _, err := promote(h, engine.PromoteOptions{Branch: "feat-x", PlanOnly: true}); err != nil {
		t.Fatalf("--branch naming the PR's own branch: %v", err)
	}
	res, err := promote(h, engine.PromoteOptions{Title: "New title", Body: "New body", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	if pl := res.Plan; pl.Publish.PR != (plan.PR{Title: "New title", Body: "New body"}) || pl.Typed != (plan.Typed{Title: true, Body: true}) {
		t.Fatalf("plan texts = %+v typed %+v", pl.Publish.PR, pl.Typed)
	}
	s := h.approver.asked[len(h.approver.asked)-1]
	if !strings.Contains(s, "PR title New title") || !strings.Contains(s, "New body") || strings.Contains(s, "keeps its title and body") {
		t.Fatalf("summary:\n%s", s)
	}
	if e := h.forge.Edits; len(e) != 1 || e[0].Number != pr || e[0].Repo != publicName ||
		e[0].Title == nil || *e[0].Title != "New title" || e[0].Body == nil || *e[0].Body != "New body" {
		t.Fatalf("edits = %+v", e)
	}
	// Only the title typed: the body is left alone.
	testutil.Commit(t, h.shadowGit(), "feat-x", "even more", map[string]string{"even.txt": "even\n"})
	res, err = promote(h, engine.PromoteOptions{Title: "Newer title", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if pl := res.Plan; pl.Publish.PR != (plan.PR{Title: "Newer title"}) || pl.Typed != (plan.Typed{Title: true}) {
		t.Fatalf("plan texts = %+v typed %+v", pl.Publish.PR, pl.Typed)
	}
	if e := h.forge.Edits; len(e) != 2 || e[1].Title == nil || *e[1].Title != "Newer title" || e[1].Body != nil {
		t.Fatalf("edits = %+v", e)
	}
}

// A closed PR is refused as before: BRANCH_EXISTS only stays for a branch
// Ghostbranch didn't publish for an open PR of this feature.
func TestAClosedPRStillHitsBranchExists(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	h.forge.ClosePR(publicName, pr)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	_, err := promote(h, engine.PromoteOptions{Title: "Again", Yes: true})
	_ = wantCode(t, err, contract.BranchExists)
}

// An append with nothing new says so.
func TestPromoteUpdateWithNothingNew(t *testing.T) {
	t.Parallel()
	h, pr := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "private only", map[string]string{".ghostbranch/notes.md": "more notes\n"})
	_, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if ce := wantCode(t, err, contract.NothingToPublish); ce.Details["public_pr"] != pr {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// R10: a published commit someone removed from the PR on GitHub comes back
// in a replace, and the plan and the approval summary both say so.
func TestPromotePutsBackCommitsRemovedOnGitHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"one.txt": "1\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "second", map[string]string{"two.txt": "2\n"})
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{Title: "Two", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	pr := res.Applied.PublicPR
	second := h.pubTip("feat-x")
	first, err := h.pubGit.MustResolve(h.ctx, second+"^")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/feat-x", first); err != nil {
		t.Fatal(err)
	}
	h.clock.Sleep(time.Minute)
	res, err = promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
	pub := res.Plan.Publish
	if pub.Style != plan.StyleReplace || pub.ReplaceReason != plan.ReasonRebase || pub.ExpectedHead != first ||
		!reflect.DeepEqual(pub.Restores, []string{second}) || len(pub.Commits) != 2 {
		t.Fatalf("publish = %+v", pub)
	}
	s := h.approver.asked[len(h.approver.asked)-1]
	if want := fmt.Sprintf("puts back 1 commit that was removed from PR #%d on GitHub", pr); !strings.Contains(s, want) {
		t.Fatalf("summary lacks %q:\n%s", want, s)
	}
	if h.pubTip("feat-x") != pub.Commit {
		t.Fatal("the replace didn't land")
	}
}

// Carried item a: a gate built to append doesn't stand for a promote that
// must replace (here a restack, flagged after the gate): CI never ran on
// the replaced branch, so promote gates again.
func TestPromoteRegatesAnAppendGateWhenItMustReplace(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	if u := h.meta("feat-x").Gate.Update; u == nil || u.Style != plan.StyleAppend {
		t.Fatalf("gate update = %+v", u)
	}
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.NeedsUpdate = true })
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if res.Gate.Reused {
		t.Fatal("promote reused the append gate for a replace")
	}
	if pub := res.Plan.Publish; pub.Style != plan.StyleReplace || pub.ReplaceReason != plan.ReasonRestack || pub.Parent != h.pubTip("main") {
		t.Fatalf("publish = %+v", pub)
	}
	if u := h.meta("feat-x").Gate.Update; u == nil || u.Style != plan.StyleReplace {
		t.Fatalf("the new gate = %+v", u)
	}
}

// Carried item b: a pending update job stands only for the same open PR on
// the same base, with the same style; otherwise the gate runs again.
func TestAPendingUpdateJobIsReusedOnlyForTheSamePR(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		change func(h *harness, pr int)
		check  func(t *testing.T, u *feature.GateUpdate)
	}{
		{"the PR was closed", func(h *harness, pr int) { h.forge.ClosePR(publicName, pr) },
			func(t *testing.T, u *feature.GateUpdate) {
				if u != nil {
					t.Fatalf("a closed PR's job was reused: %+v", u)
				}
			}},
		{"the PR's base changed", func(h *harness, pr int) {
			testutil.Commit(h.t, h.pubGit, "release", "release", map[string]string{"r.txt": "r\n"})
			h.forge.SetPRBase(publicName, pr, "release")
		}, func(t *testing.T, u *feature.GateUpdate) {
			if u == nil || u.Base != "release" || u.Reason != plan.ReasonBaseChanged {
				t.Fatalf("update = %+v", u)
			}
		}},
		{"a restack was flagged", func(h *harness, _ int) {
			h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.NeedsUpdate = true })
		}, func(t *testing.T, u *feature.GateUpdate) {
			if u == nil || u.Style != plan.StyleReplace || u.Reason != plan.ReasonRestack {
				t.Fatalf("update = %+v", u)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, pr := publishedFeature(t)
			testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
			job, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-x")
			if err != nil || job.Update == nil || job.Update.Style != plan.StyleAppend {
				t.Fatalf("job = %+v %v", job, err)
			}
			c.change(h, pr)
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			if res.Candidate == job.Candidate && res.Update != nil && *res.Update == *job.Update {
				t.Fatal("the pending job was reused")
			}
			c.check(t, res.Update)
		})
	}
}

// Carried item c: under author_email: map, promote scans the chain it
// publishes with the mapped identities in it, so a blocked string in a
// mapped identity stops the update.
func TestPromoteUpdateScansMappedAuthors(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	h.setBlockPatterns("marker-map")
	h.setConfig("author_email: noreply", "author_email: map\n  author_map:\n    \"author@example.com\": \"Mapped <marker-map@x.example>\"")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	H := h.pubTip("feat-x")
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	_ = wantCode(t, err, contract.LeakFound)
	if res.Gate == nil || res.Gate.Update == nil || res.Gate.Update.Style != plan.StyleAppend {
		t.Fatalf("gate = %+v", res.Gate)
	}
	if len(res.Findings) != 1 || res.Findings[0].Where != "author" {
		t.Fatalf("findings = %+v", res.Findings)
	}
	if h.pubTip("feat-x") != H {
		t.Fatal("a leaking update was pushed")
	}
}
