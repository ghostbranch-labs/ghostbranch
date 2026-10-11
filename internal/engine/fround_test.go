package engine_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// publicAuthors is every author on public branch's first-parent line
// above public main.
func (h *harness) publicAuthors(branch string) []string {
	h.t.Helper()
	out, err := h.pubGit.Run(h.ctx, "log", "--first-parent", "--format=%an <%ae>", "refs/heads/main..refs/heads/"+branch)
	if err != nil {
		h.t.Fatal(err)
	}
	return strings.Split(out, "\n")
}

// rebuildWithOwnCommit rebuilds a from its base with commits of the
// person's own only, the last one titled subject.
func rebuildWithOwnCommit(h *harness, name, subj string) {
	h.t.Helper()
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/"+name, h.meta(name).Base); err != nil {
		h.t.Fatal(err)
	}
	testutil.Commit(h.t, h.shadowGit(), name, "Add "+name+", reworked", map[string]string{name + ".txt": "reworked\n"})
	testutil.Commit(h.t, h.shadowGit(), name, subj, map[string]string{"own.txt": "own\n"})
}

// F1: an adopted commit the mirror doesn't have (another machine) is never
// matched by its subject alone. The person's own commit with the same
// subject keeps author_email (noreply): its private email never goes
// public. Not matched, the adopted commit counts as missing, so the
// preserve update is refused (adopted_missing) rather than guessed.
func TestAnUnreadAdoptedCommitNeverLendsItsSubjectToAPrivateCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	h.editRemoteMeta("a", func(m *feature.Meta) {
		m.Promoted.Adopted = append(m.Promoted.Adopted, feature.AdoptedCommit{SHA: strings.Repeat("ab", 20),
			Author: maintainer.Name + " <" + maintainer.Email + ">", Subject: "their fix"})
	})
	rebuildWithOwnCommit(h, "a", "their fix")
	h.gateFeature("a")
	tip := h.pubTip("a")
	_, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	for _, au := range h.publicAuthors("a") {
		if strings.Contains(au, "author@example.com") {
			t.Fatalf("a private author email went public: %q (promote: %v)", au, err)
		}
	}
	if ce := wantCode(t, err, contract.PRChangedOnGitHub); ce.Details["reason"] != "adopted_missing" || h.pubTip("a") != tip {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// F1, a fresh machine: after a squash folded the maintainer's commit, a
// new mirror no longer has it. A later preserve update of the person's own
// commits, one with the same subject, publishes them as noreply.
func TestAFreshMachineKeepsNoreplyForAPrivateCommitNamedLikeAFoldedOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a") // squash
	theirs := commitAs(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	h.gateFeature("a")
	if res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("squash update = %+v %v", res, err)
	}
	if err := os.RemoveAll(h.eng.WS.MirrorDir(h.pair)); err != nil {
		t.Fatal(err)
	}
	rebuildWithOwnCommit(h, "a", "their fix")
	h.gateFeature("a")
	if _, ok, _ := gitx.New(h.eng.WS.MirrorDir(h.pair)).Resolve(h.ctx, theirs); ok {
		t.Fatal("the fresh mirror must not have their commit")
	}
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("preserve update = %+v %v", res, err)
	}
	for _, au := range h.publicAuthors("a") {
		if au != "octo <42+octo@users.noreply.github.com>" {
			t.Fatalf("public authors %v; want only the noreply identity", h.publicAuthors("a"))
		}
	}
}

// F2: the maintainer's credit survives: two squash updates in a row after
// a pull both end with the maintainer's Co-authored-by trailer.
func TestSquashUpdatesKeepCreditingAFoldedCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a") // squash
	commitAs(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	want := "\n\nCo-authored-by: Mae Maintainer <mae@upstream.example>"
	for i, f := range []string{"more.txt", "even.txt"} {
		testutil.Commit(t, h.shadowGit(), "a", "more "+f, map[string]string{f: "more\n"})
		h.gateFeature("a")
		res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true})
		if err != nil || res.Applied == nil {
			t.Fatalf("squash update %d = %+v %v", i+1, res, err)
		}
		msg, err := h.pubGit.CommitMessage(h.ctx, h.pubTip("a"))
		if err != nil || !strings.HasSuffix(strings.TrimSpace(msg), want) {
			t.Fatalf("squash update %d's message = %q %v; want it to end with %q", i+1, msg, err, want)
		}
	}
}

// landByHand pushes plan id's commit for each branch (bottom first) to the
// public repo, as an apply that stopped after its pushes leaves it.
func (h *harness) landByHand(id string, branches map[string]string) {
	h.t.Helper()
	if _, err := h.pubGit.Run(h.ctx, "fetch", h.forge.RepoDir(shadowName), "+refs/ghostbranch/plans/"+id+":refs/heads/landing"); err != nil {
		h.t.Fatal(err)
	}
	for b, c := range branches {
		if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/"+b, c); err != nil {
			h.t.Fatal(err)
		}
	}
	_ = h.pubGit.DeleteRef(h.ctx, "refs/heads/landing")
}

// F3: a landed update whose record was never written is finished by apply
// even after the feature and public main moved: apply only writes the
// record, and the next promote works.
func TestApplyFinishesALandedUpdateAfterThingsMoved(t *testing.T) {
	t.Parallel()
	h, _ := publishedFeature(t)
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil || res.Plan == nil {
		t.Fatalf("promote --plan = %+v %v", res, err)
	}
	pl := res.Plan
	h.landByHand(pl.ID, map[string]string{"feat-x": pl.Publish.Commit})
	testutil.Commit(t, h.shadowGit(), "feat-x", "later", map[string]string{"later.txt": "later\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if err != nil || !ar.Resumed {
		t.Fatalf("apply = %+v %v", ar, err)
	}
	if m := h.shadowMeta("feat-x"); m.Promoted.PlanID != pl.ID || m.Promoted.PublishedHead != pl.Publish.Commit {
		t.Fatalf("record = %+v", m.Promoted)
	}
	h.gateFeature("feat-x")
	if res, err := promote(h, engine.PromoteOptions{Yes: true}); err != nil || res.Applied == nil {
		t.Fatalf("promote after = %+v %v", res, err)
	}
}

// F3, a stack: every level landed by hand, then things moved; apply
// finishes every record.
func TestApplyFinishesALandedStackUpdateAfterThingsMoved(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	fixBottom(h, "a", "b")
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	if err != nil || res.Plan == nil || len(res.Plan.Chain) != 2 {
		t.Fatalf("promote a --plan = %+v %v", res, err)
	}
	pl := res.Plan
	h.landByHand(pl.ID, map[string]string{"a": pl.Chain[0].Publish.Commit, "b": pl.Chain[1].Publish.Commit})
	testutil.Commit(t, h.shadowGit(), "a", "later", map[string]string{"later.txt": "later\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if err != nil || !ar.Resumed {
		t.Fatalf("apply = %+v %v", ar, err)
	}
	for i, n := range []string{"a", "b"} {
		if m := h.shadowMeta(n); m.Promoted.PlanID != pl.ID || m.Promoted.PublishedHead != pl.Chain[i].Publish.Commit {
			t.Fatalf("%s's record = %+v", n, m.Promoted)
		}
	}

}
