package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// I3: a commit made on GitHub that feature pull adopted stays recorded
// through an append, and a later preserve update whose history no longer
// has it (the person dropped it from the feature) is refused naming it,
// leaving the PR as it is.
func TestPreserveReplaceRefusesToDropAnAdoptedCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	published := h.head("a")
	theirs := testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("a")
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || res.Applied == nil || res.Plan.Publish.Style != plan.StyleAppend {
		t.Fatalf("append after the pull = %+v %v", res, err)
	}
	if ad := h.shadowMeta("a").Promoted.Adopted; len(ad) != 1 || ad[0].SHA != theirs {
		t.Fatalf("the append's record lost the adopted commit: %+v", ad)
	}

	// The person rebuilds the feature without their commit.
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/a", published); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("a")
	tip := h.pubTip("a")
	_, err = promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	list, _ := ce.Details["foreign"].([]map[string]any)
	if ce.Details["reason"] != "adopted_missing" || len(list) != 1 || list[0]["sha"] != theirs || list[0]["subject"] != "their fix" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if h.pubTip("a") != tip {
		t.Fatal("the refused update moved the PR")
	}
}

// I3: the same refusal right after the pull: the person drops the commit
// feature pull brought in, and the preserve update that follows would
// replace the PR's branch without it.
func TestPreserveReplaceRefusesToDropACommitJustPulled(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	published := h.head("a")
	theirs := testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/a", published); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("a")
	_, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	if list, _ := ce.Details["foreign"].([]map[string]any); ce.Details["reason"] != "adopted_missing" || len(list) != 1 || list[0]["sha"] != theirs {
		t.Fatalf("details = %+v", ce.Details)
	}
	if h.pubTip("a") != theirs {
		t.Fatal("the refused update moved the PR")
	}
}

// I3: a squash update folds the adopted commits into its one commit; the
// approval summary names each, and the record lets them go once folded.
func TestSquashReplaceListsFoldedAdoptedCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a") // squash
	theirs := testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("a")
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true})
	if err != nil || res.Applied == nil || res.Plan.Publish.Style != plan.StyleReplace {
		t.Fatalf("squash update = %+v %v", res, err)
	}
	s := h.approver.asked[len(h.approver.asked)-1]
	want := "folds 1 commit made on GitHub into the squash commit:\n           " + theirs[:12] + " their fix ("
	if !strings.Contains(s, want) {
		t.Fatalf("summary lacks %q:\n%s", want, s)
	}
	if ad := h.shadowMeta("a").Promoted.Adopted; len(ad) != 1 || ad[0].SHA != theirs || !ad[0].Folded {
		t.Fatalf("the folded commit must stay recorded, marked folded: %+v", ad)
	}
}

// maintainer is the author of the commits made on GitHub in the I6 tests.
var maintainer = gitx.Identity{Name: "Mae Maintainer", Email: "mae@upstream.example", When: testutil.Base.Add(90 * time.Minute)}

// commitAs commits files on branch of g as who, author and committer.
func commitAs(t *testing.T, g *gitx.Git, branch, msg string, files map[string]string, who gitx.Identity) string {
	t.Helper()
	c := testutil.Commit(t, g, branch, msg, files)
	ctx := t.Context()
	tree, err := g.TreeOf(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := g.MustResolve(ctx, c+"^")
	if err != nil {
		t.Fatal(err)
	}
	out, err := g.CommitTree(ctx, tree, []string{parent}, msg, who, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+branch, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// adoptedReplace publishes a in preserve under config (old -> new), takes a
// maintainer's commit on its PR with feature pull, then rebuilds a so the
// next update must replace: its own commit changed, the maintainer's
// replayed on top. It returns the gate's error, then promote's result.
func adoptedReplace(t *testing.T, old, new string) (*harness, error, *engine.PromoteResult, error) {
	t.Helper()
	h := newHarness(t)
	h.init()
	if old != "" {
		h.setConfig(old, new)
	}
	h.publishPreserve("a")
	commitAs(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/a", h.meta("a").Base); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "Add a, reworked", map[string]string{"a.txt": "a, reworked\n"})
	commitAs(t, h.shadowGit(), "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	_, _, gerr := h.eng.Gate(h.ctx, h.pair, "a")
	res, perr := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	return h, gerr, res, perr
}

// wantMaintainerKept checks the PR's tip, the maintainer's commit, kept
// their name and email through a replace.
func wantMaintainerKept(t *testing.T, h *harness, res *engine.PromoteResult) {
	t.Helper()
	if res.Plan.Publish.Style != plan.StyleReplace {
		t.Fatalf("style = %s", res.Plan.Publish.Style)
	}
	tip := h.pubTip("a")
	au, err := h.pubGit.CommitAuthor(h.ctx, tip)
	if err != nil || au.Name != maintainer.Name || au.Email != maintainer.Email {
		t.Fatalf("their commit's public author = %+v %v; want %s <%s>", au, err, maintainer.Name, maintainer.Email)
	}
}

// I6, author_email map: a maintainer's commit pulled in keeps its author
// through a preserve replace; neither gate nor promote finds it unmapped.
func TestAdoptedCommitKeepsItsAuthorUnderMap(t *testing.T) {
	t.Parallel()
	h, gerr, res, perr := adoptedReplace(t, "author_email: noreply",
		"author_email: map\n  author_map:\n    \"author@example.com\": \"Me <me@example.com>\"")
	if gerr != nil || perr != nil || res.Applied == nil {
		t.Fatalf("gate %v, promote %+v %v", gerr, res, perr)
	}
	wantMaintainerKept(t, h, res)
}

// I6, author_email noreply: the maintainer's author isn't rewritten to the
// publisher.
func TestAdoptedCommitKeepsItsAuthorUnderNoreply(t *testing.T) {
	t.Parallel()
	h, gerr, res, perr := adoptedReplace(t, "", "")
	if gerr != nil || perr != nil || res.Applied == nil {
		t.Fatalf("gate %v, promote %+v %v", gerr, res, perr)
	}
	wantMaintainerKept(t, h, res)
	own, err := h.pubGit.CommitAuthor(h.ctx, h.pubTip("a")+"^")
	if err != nil || own.Email != "42+octo@users.noreply.github.com" {
		t.Fatalf("the person's own commit = %+v %v; noreply still applies to it", own, err)
	}
}

// I6, squash: the squash commit credits the maintainer whose commit it
// folds with a Co-authored-by trailer at the end of its message.
func TestSquashUpdateCreditsAdoptedAuthors(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a") // squash
	commitAs(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"}, maintainer)
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 {
		t.Fatalf("pull = %+v %v", res, err)
	}
	testutil.Commit(t, h.shadowGit(), "a", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("a")
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("squash update = %+v %v", res, err)
	}
	msg, err := h.pubGit.CommitMessage(h.ctx, h.pubTip("a"))
	if want := "\n\nCo-authored-by: Mae Maintainer <mae@upstream.example>"; err != nil || !strings.HasSuffix(strings.TrimSpace(msg), want) {
		t.Fatalf("squash message = %q %v; want it to end with %q", msg, err, want)
	}
}
