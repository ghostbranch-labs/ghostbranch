package engine_test

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func (h *harness) pull(name string) (*engine.PullResult, error) {
	h.t.Helper()
	res, _, err := h.eng.FeaturePull(h.ctx, h.pair, engine.PullOptions{Feature: name})
	if res != nil {
		validateResult(h.t, "feature-pull.schema.json", res)
	}
	return res, err
}

// publishPreserve starts, gates and publishes name in preserve mode (one commit).
func (h *harness) publishPreserve(name string) {
	h.t.Helper()
	h.startFeature(name, map[string]string{name + ".txt": name + "\n"})
	h.gateFeature(name)
	if res, err := promote(h, engine.PromoteOptions{Feature: name, Mode: "preserve", Yes: true}); err != nil || res.Applied == nil {
		h.t.Fatalf("promote %s = %+v %v", name, res, err)
	}
}

// updateBranch is GitHub's "Update branch" on the public branch: a commit
// on public main adding up.txt, merged into branch. It returns the merge.
func (h *harness) updateBranch(branch string) string {
	h.t.Helper()
	P := h.pubTip(branch)
	up := testutil.Commit(h.t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/tmp", P); err != nil {
		h.t.Fatal(err)
	}
	withUp := testutil.Commit(h.t, h.pubGit, "tmp", "tree", map[string]string{"up.txt": "up\n"})
	tree, err := h.pubGit.TreeOf(h.ctx, withUp)
	if err != nil {
		h.t.Fatal(err)
	}
	merge, err := h.pubGit.CommitTree(h.ctx, tree, []string{P, up}, "Merge branch 'main' into "+branch, testutil.Author(70), testutil.Author(70))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/"+branch, merge); err != nil {
		h.t.Fatal(err)
	}
	_ = h.pubGit.DeleteRef(h.ctx, "refs/heads/tmp")
	return merge
}

// mergeByHand is the person's by-hand pull: the public branch merged into
// the shadow feature branch name, which is pushed. theirs is the public
// branch's head; it returns the person's merge.
func (h *harness) mergeByHand(name, theirs string) string {
	h.t.Helper()
	sg := h.shadowGit()
	if _, err := sg.Run(h.ctx, "fetch", h.forge.RepoDir(publicName), "+refs/heads/"+name+":refs/heads/their-"+name); err != nil {
		h.t.Fatal(err)
	}
	F := h.head(name)
	c := testutil.Commit(h.t, sg, name, "merge", map[string]string{"up.txt": "up\n"})
	mtree, err := sg.TreeOf(h.ctx, c)
	if err != nil {
		h.t.Fatal(err)
	}
	mine, err := sg.CommitTree(h.ctx, mtree, []string{F, theirs}, "Merge their "+name, testutil.Author(71), testutil.Author(71))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := sg.UpdateRef(h.ctx, "refs/heads/"+name, mine); err != nil {
		h.t.Fatal(err)
	}
	_ = sg.DeleteRef(h.ctx, "refs/heads/their-"+name)
	return mine
}

// Review Focus 3: a commit someone made on GitHub is refused by both the
// gate and promote, then feature pull brings it in, and the next update
// keeps it: their commit is in the PR's history, unchanged.
func TestPullThenUpdateKeepsTheirCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	theirs := testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	testutil.Commit(t, h.shadowGit(), "a", "my review fix", map[string]string{"a.txt": "a, reviewed\n"})
	_, _, gerr := h.eng.Gate(h.ctx, h.pair, "a")
	_, perr := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if codeOf(gerr) != contract.PRChangedOnGitHub || codeOf(perr) != contract.PRChangedOnGitHub {
		t.Fatalf("gate %v, promote %v: both must refuse with PUBLIC_BRANCH_CHANGED", gerr, perr)
	}
	if h.pubTip("a") != theirs {
		t.Fatal("their commit was overwritten")
	}
	res, err := h.pull("a")
	if err != nil || len(res.Adopted) != 1 || res.Adopted[0].SHA != theirs || res.From == res.To {
		t.Fatalf("pull = %+v %v", res, err)
	}
	m := h.shadowMeta("a")
	if m.Promoted.PublishedHead != theirs || len(m.Promoted.Adopted) != 1 || m.Promoted.Adopted[0].SHA != theirs {
		t.Fatalf("a's record = %+v", m.Promoted)
	}
	if _, ok := h.fileAt("refs/heads/a", "fix.txt"); !ok {
		t.Fatal("the feature branch lacks their change")
	}
	if got, _ := h.fileAt("refs/heads/a", "a.txt"); got != "a, reviewed\n" {
		t.Fatalf("my fix was lost: a.txt = %q", got)
	}
	h.gateFeature("a")
	pr, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || pr.Applied == nil || pr.Plan.Publish.Style != "append" {
		t.Fatalf("promote after pull = %+v %v", pr, err)
	}
	if !h.pubAncestor(theirs, h.pubTip("a")) {
		t.Fatal("the update dropped their commit from the PR's history")
	}
}

// The fast path: a feature head inside the PR's branch fast-forwards to it.
func TestPullFastForwardsAFeatureHeadInsideThePR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	sg := h.shadowGit()
	// The feature branch reset to what its PR published.
	if _, err := sg.Run(h.ctx, "fetch", h.forge.RepoDir(publicName), "+refs/heads/a:refs/heads/a"); err != nil {
		t.Fatal(err)
	}
	theirs := testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	res, err := h.pull("a")
	if err != nil || res.To != theirs || h.head("a") != theirs {
		t.Fatalf("pull = %+v %v; a at %s", res, err, h.head("a"))
	}
	if m := h.shadowMeta("a"); m.Promoted.FeatureHead != theirs || m.Promoted.PublishedHead != theirs {
		t.Fatalf("record = %+v", m.Promoted)
	}
}

// A conflict, or a merge commit (GitHub's "Update branch"), is the person's:
// git steps, and nothing changes. Once they merge and push it, pull records it.
func TestPullRefusesWithGitStepsThenRecordsAByHandMerge(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	testutil.Commit(t, h.pubGit, "a", "their change", map[string]string{"a.txt": "theirs\n"})
	meta := h.shadowMeta("a")
	testutil.Commit(t, h.shadowGit(), "a", "my change", map[string]string{"a.txt": "mine\n"})
	before := h.head("a")
	_, err := h.pull("a")
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	steps, _ := ce.Details["steps"].([]string)
	if ce.Details["reason"] != "conflict" || len(steps) != 5 || steps[2] != "git merge origin/a" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if h.head("a") != before || !reflect.DeepEqual(h.shadowMeta("a").Promoted, meta.Promoted) {
		t.Fatal("a refused pull changed the branch or the record")
	}

	// "Update branch": a merge of main into the PR's branch.
	h2 := newHarness(t)
	h2.init()
	h2.publish("a")
	merge := h2.updateBranch("a")
	_, err = h2.pull("a")
	if ce := wantCode(t, err, contract.PRChangedOnGitHub); ce.Details["reason"] != "merge_commit" {
		t.Fatalf("details = %+v", ce.Details)
	}
	// The person merges the PR's branch into the feature and pushes it.
	mine := h2.mergeByHand("a", merge)
	oldFeatureHead := h2.shadowMeta("a").Promoted.FeatureHead
	res, err := h2.pull("a")
	if err != nil || res.From != mine || res.To != mine || len(res.Adopted) == 0 {
		t.Fatalf("pull after the merge = %+v %v", res, err)
	}
	if m := h2.shadowMeta("a"); m.Promoted.PublishedHead != merge || m.Promoted.FeatureHead != oldFeatureHead {
		t.Fatalf("record = %+v", m.Promoted)
	}
}

// M8: an "Update branch" merge brought in by hand, then gated and
// promoted: the PR's new tree keeps the merge's content (what main added
// through it). The merge commit object itself may leave the PR when the
// update replaces; its content is what is kept.
func TestPullByHandMergeKeepsItsContentOnTheNextUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	merge := h.updateBranch("a")
	if _, err := h.pull("a"); codeOf(err) != contract.PRChangedOnGitHub {
		t.Fatalf("pull before the by-hand merge: %v", err)
	}
	h.mergeByHand("a", merge)
	if res, err := h.pull("a"); err != nil || len(res.Adopted) != 1 || res.Adopted[0].SHA != merge {
		t.Fatalf("pull after the by-hand merge = %+v %v", res, err)
	}
	h.gateFeature("a")
	pr, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || pr.Applied == nil {
		t.Fatalf("promote after the pull = %+v %v", pr, err)
	}
	tip := h.pubTip("a")
	t.Logf("update style %s (%s); merge commit still in the PR's history: %v",
		pr.Plan.Publish.Style, pr.Plan.Publish.ReplaceReason, h.pubAncestor(merge, tip))
	for path, want := range map[string]string{"up.txt": "up\n", "a.txt": "a\n"} {
		got, ok, err := h.pubGit.ShowFile(h.ctx, tip, path)
		if err != nil || !ok || string(got) != want {
			t.Fatalf("the PR's new tree lost %s: %q %v %v", path, got, ok, err)
		}
	}
}

// Nothing foreign: nothing changes. No open PR: NO_OPEN_PR.
func TestPullWithNothingToPullAndWithoutAnOpenPR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	before := h.shadowRefs()
	res, err := h.pull("a")
	if err != nil || len(res.Adopted) != 0 || res.From != res.To || !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatalf("pull = %+v %v", res, err)
	}
	h.startFeature("b", map[string]string{"b.txt": "b\n"})
	_, err = h.pull("b")
	_ = wantCode(t, err, contract.NoOpenPR)
}

// --plan previews the leased pushes and changes nothing.
func TestPullPlanChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, err := h.pull("a")
	if err != nil || res.Preview == nil || len(res.Preview.Ops) != 2 || !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatalf("pull --plan = %+v %v", res, err)
	}
}

// I2: a pull that leaves the feature branch where it is (a by-hand merge
// already holds the PR) still leases it: a branch pushed to between the
// read and the push makes the pull refuse, recording nothing.
func TestPullLeasesABranchItDoesNotMove(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	merge := h.updateBranch("a")
	h.mergeByHand("a", merge)
	meta := h.shadowMeta("a")
	var moved string
	observe := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		if observe != nil {
			observe(op, remote, specs)
		}
		if op == "feature pull" && remote == "shadow" && moved == "" {
			moved = testutil.Commit(t, h.shadowGit(), "a", "pushed meanwhile", map[string]string{"late.txt": "late\n"})
		}
	}
	_, err := h.pull("a")
	_ = wantCode(t, err, contract.RefMoved)
	if h.head("a") != moved || !reflect.DeepEqual(h.shadowMeta("a").Promoted, meta.Promoted) {
		t.Fatal("a pull whose branch moved must record nothing")
	}
}

// F-14: feature pull names why there is no open PR, so next can say what
// to do instead.
func TestPullWithoutOpenPR(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, reason string
		setup        func(h *harness)
	}{
		{"never published", "not_published", func(h *harness) { h.startFeature("a", map[string]string{"a.txt": "a\n"}) }},
		{"merged", "merged", func(h *harness) { pr, _ := h.publish("a"); h.forge.SetPRMerged(publicName, pr) }},
		{"closed", "closed", func(h *harness) {
			pr, _ := h.publish("a")
			if err := h.forge.ClosePullRequest(h.ctx, publicName, pr); err != nil {
				h.t.Fatal(err)
			}
		}},
		{"branch gone", "branch_gone", func(h *harness) {
			h.publish("a")
			branch := h.shadowMeta("a").Promoted.Branch
			if branch == "" {
				branch = "a"
			}
			if err := h.pubGit.DeleteRef(h.ctx, "refs/heads/"+branch); err != nil {
				h.t.Fatal(err)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			c.setup(h)
			_, err := h.pull("a")
			ce := wantCode(t, err, contract.NoOpenPR)
			if ce.Field != "feature" || ce.Details["feature"] != "a" || ce.Details["reason"] != c.reason {
				t.Fatalf("error = %+v", ce)
			}
			if _, has := ce.Details["public_pr"]; has == (c.reason == "not_published") {
				t.Fatalf("public_pr in %v", ce.Details)
			}
		})
	}
}

// Review Focus 2: a GitHub read that fails while working out the reason is
// that read's error, never a guessed reason.
func TestPullWithoutOpenPRNeverGuessesFromAFailedRead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("a")
	if err := h.forge.ClosePullRequest(h.ctx, publicName, pr); err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	h.forge.GetPRErr = func(_ string, n int) error {
		if n == pr && reads.Add(1) > 1 {
			return fmt.Errorf("%w: rate limited", forge.ErrTransient)
		}
		return nil
	}
	_, err := h.pull("a")
	if ce := wantCode(t, err, contract.ForgeError); !ce.Retryable {
		t.Fatalf("error = %+v", ce)
	}
}

// Review M1: apply pushed the branch and opened the PR but never wrote the
// record. The reason stays not_published (the record says so), but the
// error names the plan whose apply finishes the record, not a new promote.
func TestPullWithoutOpenPRNamesAnUnrecordedPublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	_, id := h.publish("a")
	h.editRemoteMeta("a", func(m *feature.Meta) { m.Promoted = nil })
	_, err := h.pull("a")
	ce := wantCode(t, err, contract.NoOpenPR)
	if ce.Details["reason"] != "not_published" || ce.Details["plan"] != id || !strings.Contains(ce.Message, "apply "+id) {
		t.Fatalf("error = %+v", ce)
	}
}
