package engine_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// publishedFeature publishes feat-x as gated() made it (one commit,
// preserve, the harness console approving) and returns the harness and
// its public PR.
func publishedFeature(t *testing.T) (*harness, int) {
	t.Helper()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	return h, res.Applied.PublicPR
}

// resetFeature moves feat-x back to where it started, as a person's
// rebase or amend does before pushing the rewritten history.
func (h *harness) resetFeature() {
	h.t.Helper()
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/feat-x", h.meta("feat-x").Base); err != nil {
		h.t.Fatal(err)
	}
}

// Review Focus 4: append vs replace is chosen correctly.
func TestUpdateStyle(t *testing.T) {
	t.Parallel()
	more := map[string]string{"more.txt": "more\n"}
	cases := []struct {
		name          string
		prep          func(h *harness, pr int)
		style, reason string // style "": no update, a first publish
	}{
		{"a new commit appends", func(h *harness, _ int) {
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, plan.StyleAppend, ""},
		{"squash mode replaces", func(h *harness, _ int) {
			h.setConfig("default_mode: preserve", "default_mode: squash")
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, plan.StyleReplace, plan.ReasonSquash},
		{"a rebased history replaces", func(h *harness, _ int) {
			h.resetFeature()
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "work on feat-x", map[string]string{"feature.txt": "changed\n"})
		}, plan.StyleReplace, plan.ReasonRebase},
		{"a reworded commit replaces", func(h *harness, _ int) {
			h.resetFeature()
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "work on feat-x, reworded",
				map[string]string{"feature.txt": "new\n", ".ghostbranch/notes.md": "private\n"})
		}, plan.StyleReplace, plan.ReasonReworded},
		{"a restack replaces", func(h *harness, _ int) {
			h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.NeedsUpdate = true })
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, plan.StyleReplace, plan.ReasonRestack},
		{"a changed base replaces", func(h *harness, pr int) {
			if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/release", h.pubTip("main")); err != nil {
				h.t.Fatal(err)
			}
			h.forge.SetPRBase(publicName, pr, "release")
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, plan.StyleReplace, plan.ReasonBaseChanged},
		// The new commit edits up.txt, which public main has and the PR's
		// head doesn't: it applies on main, not on H.
		{"new commits that don't apply on the PR's head replace", func(h *harness, _ int) {
			testutil.Commit(h.t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "a\n"})
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "take up.txt", map[string]string{"up.txt": "a\n"})
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "edit up.txt", map[string]string{"up.txt": "b\n"})
		}, plan.StyleReplace, plan.ReasonConflict},
		// Someone removed the published commit from the PR on GitHub: the
		// update puts it back, so the branch is replaced.
		{"a commit removed on GitHub replaces", func(h *harness, _ int) {
			if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/feat-x", h.pubTip("main")); err != nil {
				h.t.Fatal(err)
			}
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, plan.StyleReplace, plan.ReasonRebase},
		{"a closed PR is a first publish", func(h *harness, pr int) {
			h.forge.ClosePR(publicName, pr)
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "more", more)
		}, "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, pr := publishedFeature(t)
			c.prep(h, pr)
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if err != nil {
				t.Fatalf("gate: %v", err)
			}
			validateResult(t, "gate.schema.json", res)
			m := h.meta("feat-x")
			validateResult(t, "feature.schema.json", m)
			if c.style == "" {
				if res.Update != nil || m.Gate.Update != nil {
					t.Fatalf("a first publish built an update: %+v / %+v", res.Update, m.Gate.Update)
				}
				return
			}
			u := res.Update
			if u == nil || u.PR != pr || u.Style != c.style || u.Reason != c.reason || u.Head != h.pubTip("feat-x") {
				t.Fatalf("update = %+v, want %s %q on #%d at %s", u, c.style, c.reason, pr, h.pubTip("feat-x"))
			}
			if m.Gate.Update == nil || *m.Gate.Update != *u {
				t.Fatalf("recorded %+v, reported %+v", m.Gate.Update, u)
			}
			if c.style == plan.StyleReplace && (res.Base != h.pubTip("main") || m.Gate.Base != "") {
				t.Fatalf("a replace built on %s (recorded %q), not public main", res.Base, m.Gate.Base)
			}
		})
	}
}

// R16: a feature with an open PR stacked on it always replaces (restack),
// even when it only gained commits; once that PR above is closed it no
// longer does.
func TestABottomWithAnOpenPRAboveReplaces(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	testutil.Commit(t, h.shadowGit(), "a", "more a", map[string]string{"more-a.txt": "m\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "a")
	if err != nil {
		t.Fatalf("gate a: %v", err)
	}
	validateResult(t, "gate.schema.json", res)
	if u := res.Update; u == nil || u.PR != prs[0].Number || u.Style != plan.StyleReplace || u.Reason != plan.ReasonRestack {
		t.Fatalf("a's update = %+v, want replace (restack) of #%d", u, prs[0].Number)
	}
	h.forge.ClosePR(publicName, prs[1].Number)
	res, _, err = h.eng.Gate(h.ctx, h.pair, "a")
	if err != nil {
		t.Fatalf("gate a again: %v", err)
	}
	if u := res.Update; u == nil || u.Reason == plan.ReasonRestack {
		t.Fatalf("with nothing open above, a's update = %+v", u)
	}
}

// R10: published commits someone removed from the PR on GitHub are listed
// as Restores (by patch-id and message), and are not foreign.
func TestUpdateListsCommitsRemovedOnGitHub(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"one.txt": "1\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "second", map[string]string{"two.txt": "2\n"})
	h.gateFeature("feat-x")
	h.grant(time.Hour)
	if res, err := promote(h, engine.PromoteOptions{Title: "Two", Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	u, err := h.eng.PRUpdateForTest(h.ctx, h.pair, "feat-x")
	if err != nil || u == nil || len(u.Public) != 2 || len(u.Foreign) != 0 || len(u.Restores) != 0 {
		t.Fatalf("as published: %+v %v", u, err)
	}
	second := h.pubTip("feat-x")
	first, err := h.pubGit.MustResolve(h.ctx, second+"^")
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/feat-x", first); err != nil {
		t.Fatal(err)
	}
	u, err = h.eng.PRUpdateForTest(h.ctx, h.pair, "feat-x")
	if err != nil || u == nil || u.Head != first || len(u.Foreign) != 0 || !reflect.DeepEqual(u.Restores, []string{second}) {
		t.Fatalf("after the removal: %+v %v", u, err)
	}
}

// An append gate's candidate is the PR's head H plus the new commits,
// exactly what the PR will hold, with the private overlay left out.
func TestAppendCandidateIsThePRHeadPlusTheNewCommits(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	H := h.pubTip("feat-x")
	src := testutil.Commit(t, h.shadowGit(), "feat-x", "fix review",
		map[string]string{"more.txt": "m\n", ".ghostbranch/notes.md": "still private\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPassed {
		t.Fatalf("gate = %+v %v", res, err)
	}
	if res.Base != H || res.Update == nil || res.Update.Style != plan.StyleAppend || res.Update.Head != H {
		t.Fatalf("base %s, update %+v; want append on %s", res.Base, res.Update, H)
	}
	if len(res.Commits) != 1 || res.Commits[0].Source != src || !reflect.DeepEqual(res.Commits[0].Files, []string{"more.txt"}) {
		t.Fatalf("commits = %+v", res.Commits)
	}
	m := h.meta("feat-x")
	if m.Gate.Base != H || len(m.Gate.Commits) != 1 || m.Gate.Commits[0].Source != src {
		t.Fatalf("gate record = %+v", m.Gate)
	}
	sg := h.shadowGit()
	R, err := sg.MustResolve(h.ctx, res.Candidate+"^")
	if err != nil || R != m.Gate.Commits[0].Rebased {
		t.Fatalf("candidate parent %s, mapping %+v (%v)", R, m.Gate.Commits, err)
	}
	if parent, err := sg.MustResolve(h.ctx, R+"^"); err != nil || parent != H {
		t.Fatalf("the appended commit sits on %s, not H %s (%v)", parent, H, err)
	}
	if _, ok := h.fileAt(R, "more.txt"); !ok {
		t.Fatal("the appended commit lacks the new file")
	}
	if _, ok := h.fileAt(R, ".ghostbranch/notes.md"); ok {
		t.Fatal("the appended commit holds an overlay path")
	}
	// wait reports the recorded update as the gate did.
	job := "gate/feat-x/" + res.Candidate[:12]
	w, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{})
	if err != nil || w.Update == nil || *w.Update != *res.Update || w.Base != H {
		t.Fatalf("wait = %+v %v", w, err)
	}
}

// Review Focus 1: an append's gate scans only what isn't public, but every
// new commit: a match already public at H is not raised again, and one a
// later new commit removes still is (preserve publishes each commit).
func TestUpdateGateScansOnlyWhatIsNew(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"old.txt": "marker-old\n"})
	h.gateFeature("feat-x")
	if res, err := promote(h, engine.PromoteOptions{Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	h.setBlockPatterns("marker-old", "marker-new")
	added := testutil.Commit(t, h.shadowGit(), "feat-x", "add new", map[string]string{"new.txt": "marker-new\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "fix new", map[string]string{"new.txt": "fine\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if res.Update == nil || res.Update.Style != plan.StyleAppend {
		t.Fatalf("update = %+v", res.Update)
	}
	if len(res.Findings) != 1 || res.Findings[0].Path != "new.txt" || !reflect.DeepEqual(res.Findings[0].Commits, []string{added}) {
		t.Fatalf("findings = %+v", res.Findings)
	}
	// A new commit's message is new too.
	h2 := newHarness(t)
	h2.init()
	h2.startFeature("feat-x", map[string]string{"old.txt": "fine\n"})
	h2.gateFeature("feat-x")
	if res, err := promote(h2, engine.PromoteOptions{Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	h2.setBlockPatterns("marker-msg")
	said := testutil.Commit(t, h2.shadowGit(), "feat-x", "fix: marker-msg", map[string]string{"x.txt": "x\n"})
	res, _, err = h2.eng.Gate(h2.ctx, h2.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 1 || res.Findings[0].Where != "commit_message" || !reflect.DeepEqual(res.Findings[0].Commits, []string{said}) {
		t.Fatalf("findings = %+v", res.Findings)
	}
}

// Review Focus 1: a replace scans the net change since H. What H already
// holds is not raised again, and a squash publishes only the final tree.
func TestReplaceScansTheNetChange(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"old.txt": "marker-old\n"})
	h.gateFeature("feat-x")
	if res, err := promote(h, engine.PromoteOptions{Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("publish = %+v %v", res, err)
	}
	h.setBlockPatterns("marker-old", "marker-new") // rewrites the config: default_mode is preserve again
	h.setConfig("default_mode: preserve", "default_mode: squash")
	testutil.Commit(t, h.shadowGit(), "feat-x", "add new", map[string]string{"new.txt": "marker-new\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if res.Update == nil || res.Update.Style != plan.StyleReplace || res.Update.Reason != plan.ReasonSquash {
		t.Fatalf("update = %+v", res.Update)
	}
	if len(res.Findings) != 1 || res.Findings[0].Path != "new.txt" {
		t.Fatalf("findings = %+v", res.Findings)
	}
	testutil.Commit(t, h.shadowGit(), "feat-x", "fix new", map[string]string{"new.txt": "fine\n"})
	h.gateFeature("feat-x")
}

// An update's preserve gate refuses an author with no mapping, as a first
// publish's does, before any CI runs.
func TestUpdateGateRefusesAnUnmappedAuthor(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	h.setConfig("author_email: noreply", "author_email: map\n  author_map:\n    \"x@y.example\": \"X <x@y.example>\"")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.AuthorUnmapped)
	if res == nil || res.Leak != feature.LeakNotRun || res.ShadowPR != nil {
		t.Fatalf("an unmapped author stops the update gate before CI: %+v", res)
	}
}

// Review Focus 3: commits someone made on GitHub stop the update before
// any candidate is pushed. A commit counts as Ghostbranch's only when its
// patch-id and its message both match a published one.
func TestUpdateRefusesCommitsMadeOnGitHub(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		onPR func(h *harness) (sha, subject string)
	}{
		{"a pushed fix", func(h *harness) (string, string) {
			return testutil.Commit(h.t, h.pubGit, "feat-x", "maintainer fix", map[string]string{"fix.txt": "fix\n"}), "maintainer fix"
		}},
		{"a commit reworded on GitHub", func(h *harness) (string, string) {
			// The same change as published, with another message.
			H := h.pubTip("feat-x")
			parent, err := h.pubGit.MustResolve(h.ctx, H+"^")
			if err != nil {
				h.t.Fatal(err)
			}
			tree, err := h.pubGit.TreeOf(h.ctx, H)
			if err != nil {
				h.t.Fatal(err)
			}
			c, err := h.pubGit.CommitTree(h.ctx, tree, []string{parent}, "reworded on GitHub", testutil.Author(9), testutil.Author(9))
			if err != nil {
				h.t.Fatal(err)
			}
			if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/feat-x", c); err != nil {
				h.t.Fatal(err)
			}
			return c, "reworded on GitHub"
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h, pr := publishedFeature(t)
			gateTip := h.head("ghostbranch-gate/feat-x")
			theirs, subj := c.onPR(h)
			testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
			_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			ce := wantCode(t, err, contract.PRChangedOnGitHub)
			foreign, _ := ce.Details["foreign"].([]map[string]any)
			if len(foreign) != 1 || foreign[0]["sha"] != theirs || foreign[0]["subject"] != subj ||
				ce.Details["public_pr"] != pr || ce.Details["branch"] != "feat-x" || ce.Details["feature"] != "feat-x" {
				t.Fatalf("details = %+v", ce.Details)
			}
			if got := h.head("ghostbranch-gate/feat-x"); got != gateTip {
				t.Fatal("a refused update pushed a gate candidate")
			}
		})
	}
}

// A pending update job is not reused once the PR's branch moved: the gate
// runs again, and sees the foreign commit.
func TestAnUpdateJobIsNotReusedOnceThePRMoves(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
	if _, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.pubGit, "feat-x", "maintainer fix", map[string]string{"fix.txt": "fix\n"})
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.PRChangedOnGitHub)
}

// A feature stacked on one whose gate is an append never builds on that
// append candidate (public commits): it replays the parent privately.
func TestAChildGateNeverStandsOnAnAppendCandidate(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
	h.gateFeature("feat-x")
	a := h.meta("feat-x").Gate
	if a.Update == nil || a.Update.Style != plan.StyleAppend {
		t.Fatalf("feat-x's gate = %+v", a.Update)
	}
	h.startOn("feat-y", "feat-x", map[string]string{"y.txt": "y\n"})
	h.gateFeature("feat-y")
	b := h.meta("feat-y").Gate
	if b.Base == a.Commits[len(a.Commits)-1].Rebased {
		t.Fatal("feat-y's gate stood on feat-x's append candidate")
	}
	if in, err := h.shadowGit().IsAncestor(h.ctx, a.Update.Head, b.Candidate); err != nil || in {
		t.Fatalf("feat-y's candidate contains the PR head %s (%v)", a.Update.Head, err)
	}
}
