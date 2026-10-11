package engine_test

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// publishedStack gates names as a stack and publishes it, squashed, under a
// grant. It returns the PRs, bottom first. Sync never asks a console.
func publishedStack(t *testing.T, names ...string) (*harness, []plan.StackPR) {
	t.Helper()
	h := gatedStack(t, names...)
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: names, Mode: "squash", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote %v = %+v %v", names, res, err)
	}
	h.eng.Approver = nil
	return h, res.Applied.PRs
}

// squashMergeBottom does on the public repo what GitHub does when it
// squash-merges the bottom PR of a native stack: main gets one commit with
// the bottom's changes, the PR above is retargeted to main, and each branch
// above is rebased onto the one below, keeping its changes (new SHAs, the
// same patches).
func (h *harness) squashMergeBottom(prs []plan.StackPR) {
	h.t.Helper()
	ctx, g := h.ctx, h.pubGit
	bottom := prs[0].Feature
	tree, err := g.TreeOf(ctx, "refs/heads/"+bottom)
	if err != nil {
		h.t.Fatal(err)
	}
	squash, err := g.CommitTree(ctx, tree, []string{h.pubTip("main")}, "Squash of "+bottom, testutil.Author(50), testutil.Author(50))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/main", squash); err != nil {
		h.t.Fatal(err)
	}
	h.forge.SetPRMerged(publicName, prs[0].Number)
	h.forge.SetPRBase(publicName, prs[1].Number, "main")
	oldBase, onto := h.pubTip(bottom), squash
	for _, p := range prs[1:] {
		old := h.pubTip(p.Feature)
		commits, err := g.CommitsBetween(ctx, oldBase, old)
		if err != nil {
			h.t.Fatal(err)
		}
		dir := filepath.Join(h.t.TempDir(), "restack-"+strings.ReplaceAll(p.Feature, "/", "-"))
		wt, err := g.WorktreeAdd(ctx, dir, onto)
		if err != nil {
			h.t.Fatal(err)
		}
		rebased, err := wt.CherryPick(ctx, commits, testutil.Author(51))
		if rerr := g.WorktreeRemove(ctx, dir); err != nil || rerr != nil {
			h.t.Fatal(err, rerr)
		}
		tip := rebased[len(rebased)-1]
		if err := g.UpdateRef(ctx, "refs/heads/"+p.Feature, tip); err != nil {
			h.t.Fatal(err)
		}
		oldBase, onto = old, tip
	}
}

func attentionNames(rs []engine.FeatureReport) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

// Review Focus 2: a parent closes only in the same atomic push that moves
// its children onto main; if that push fails, nothing changes.
func TestSyncReparentsChildrenAtomically(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	h.forge.SetPRMerged(publicName, prs[0].Number)
	ma := h.shadowMeta("a")
	wantA := h.featureRefs("a", ma.Promoted.PlanID)
	recorder := h.eng.OnPush

	// Someone moves b's record just as sync pushes: the lease on it fails,
	// so a is not deleted and b still names a.
	h.moveMetaOnPush("b", 1)
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || len(warningsFor(warns, contract.FeatureNotClosed)) != 1 {
		t.Fatalf("closed %+v, warnings %+v", res.Closed, warns)
	}
	after := h.shadowRefs()
	for ref, sha := range wantA {
		if after[ref] != sha {
			t.Fatalf("%s is %q after a failed close, want %s", ref, after[ref], sha)
		}
	}
	if mb := h.shadowMeta("b"); mb.Parent != "a" {
		t.Fatalf("b's parent is %q after a failed close", mb.Parent)
	}

	// Again, with nobody in the way: one push deletes a and rewrites b.
	h.eng.OnPush = recorder
	h.pushes = nil
	res, _ = h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Name != "a" || !reflect.DeepEqual(res.Closed[0].Reparented, []string{"b"}) {
		t.Fatalf("closed = %+v", res.Closed)
	}
	mb := h.shadowMeta("b")
	if mb.ParentName() != "main" || mb.ForkedAt != ma.Promoted.FeatureHead || mb.Promoted.Base != "main" || mb.Promoted.NeedsUpdate {
		t.Fatalf("b's record = parent %q, forked_at %q, promoted %+v; want main, %s", mb.Parent, mb.ForkedAt, mb.Promoted, ma.Promoted.FeatureHead)
	}
	together := 0
	for _, p := range h.pushes {
		del, upd := false, false
		for _, s := range p.refspecs {
			del = del || s == ":refs/ghostbranch/features/a"
			upd = upd || (strings.HasSuffix(s, ":refs/ghostbranch/features/b") && !strings.HasPrefix(s, ":"))
		}
		if del != upd {
			t.Fatalf("a push deleted a's record or rewrote b's, not both: %v", p.refspecs)
		}
		if del {
			together++
		}
	}
	if together != 1 {
		t.Fatalf("%d pushes closed a with b's record, want 1", together)
	}
	if _, ok := h.shadowRefs()["refs/ghostbranch/features/a"]; ok {
		t.Fatal("a's record is still there")
	}
}

// A whole stack merged at once closes bottom first.
func TestSyncClosesAWholeMergedStackBottomUp(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	for _, p := range prs {
		h.forge.SetPRMerged(publicName, p.Number)
	}
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(warns) != 0 || len(res.Closed) != 2 || res.Closed[0].Name != "a" || res.Closed[1].Name != "b" ||
		!reflect.DeepEqual(res.Closed[0].Reparented, []string{"b"}) || len(res.Closed[1].Reparented) != 0 {
		t.Fatalf("closed = %+v, warnings %+v", res.Closed, warns)
	}
	for ref := range h.shadowRefs() {
		if strings.HasSuffix(ref, "/a") || strings.HasSuffix(ref, "/b") {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
}

// Review Focus 3: GitHub's restack of the branches above a squash-merged PR
// is the same changes on a new base; sync records the new heads, and it is
// neither drift nor public_pr_needs_update.
func TestSyncAcceptsGitHubsRestackByPatchID(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b", "c")
	oldB, oldC := h.pubTip("b"), h.pubTip("c")
	h.squashMergeBottom(prs)
	newB, newC := h.pubTip("b"), h.pubTip("c")
	if newB == oldB || newC == oldC {
		t.Fatal("the restack must give new SHAs")
	}
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(warningsFor(warns, contract.PublicBranchChanged)) != 0 || len(res.Attention) != 0 {
		t.Fatalf("GitHub's restack was flagged: warnings %+v, attention %v", warns, attentionNames(res.Attention))
	}
	if len(res.Closed) != 1 || res.Closed[0].Name != "a" {
		t.Fatalf("closed = %+v", res.Closed)
	}
	mb, mc := h.shadowMeta("b"), h.shadowMeta("c")
	if mb.Promoted.PublishedHead != newB || mb.Promoted.Base != "main" || mb.Promoted.NeedsUpdate || mb.ParentName() != "main" {
		t.Fatalf("b's record = parent %q, %+v", mb.ParentName(), mb.Promoted)
	}
	if mc.Promoted.PublishedHead != newC || mc.Promoted.Base != "b" || mc.Parent != "b" {
		t.Fatalf("c's record = parent %q, %+v", mc.Parent, mc.Promoted)
	}
	for _, n := range []string{"b", "c"} {
		if r := h.report(n); r.State == feature.StatePublicPRNeedsUpdate {
			t.Fatalf("%s is %s after GitHub's restack", n, r.State)
		}
	}
	// A second sync finds nothing new.
	if _, warns := h.syncOK(engine.SyncOptions{}); len(warns) != 0 {
		t.Fatalf("second sync warned %+v", warns)
	}
}

// Review Focus 3: a change someone pushed to a stacked public branch is
// flagged and never adopted, beside a merge or alone.
func TestSyncFlagsAForeignChangeOnAStackedBranch(t *testing.T) {
	t.Parallel()
	h, _ := publishedStack(t, "a", "b")
	published := h.shadowMeta("b").Promoted.PublishedHead
	testutil.Commit(t, h.pubGit, "b", "someone else's change", map[string]string{"b.txt": "changed by hand\n"})
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.PublicBranchChanged)
	if len(ws) != 1 || ws[0].Details["feature"] != "b" || ws[0].Details["published_head"] != published {
		t.Fatalf("warnings = %+v", warns)
	}
	if got := h.shadowMeta("b").Promoted.PublishedHead; got != published {
		t.Fatalf("b's published head became %s: a foreign change was adopted", got)
	}
	if !reflect.DeepEqual(attentionNames(res.Attention), []string{"b"}) {
		t.Fatalf("attention = %v", attentionNames(res.Attention))
	}

	// A restack that also changed what b holds: a still closes and b moves
	// onto main, but b's new head is flagged, not recorded.
	h2, prs := publishedStack(t, "a", "b")
	published = h2.shadowMeta("b").Promoted.PublishedHead
	h2.squashMergeBottom(prs)
	testutil.Commit(t, h2.pubGit, "b", "slipped in", map[string]string{"extra.txt": "x\n"})
	res, warns = h2.syncOK(engine.SyncOptions{})
	if len(warningsFor(warns, contract.PublicBranchChanged)) != 1 || len(res.Closed) != 1 || res.Closed[0].Name != "a" {
		t.Fatalf("closed %+v, warnings %+v", res.Closed, warns)
	}
	if mb := h2.shadowMeta("b"); mb.Promoted.PublishedHead != published || mb.ParentName() != "main" {
		t.Fatalf("b's record = parent %q, %+v", mb.ParentName(), mb.Promoted)
	}
}

// Plain chained PRs (no native stack): GitHub only retargets, so after a
// squash merge the child's PR still carries the parent's old commits; after
// a merge commit it doesn't.
func TestSyncMarksAPlainChainedChildNeedsUpdate(t *testing.T) {
	t.Parallel()
	for _, mergeCommit := range []bool{false, true} {
		h := gatedStack(t, "a", "b")
		h.grant(time.Hour)
		h.forge.StacksUnavailable = true
		res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
		if err != nil {
			t.Fatal(err)
		}
		h.eng.Approver = nil
		prs := res.Applied.PRs
		aTip := h.pubTip("a")
		parents := []string{h.pubTip("main")}
		if mergeCommit {
			parents = append(parents, aTip)
		}
		tree, err := h.pubGit.TreeOf(h.ctx, aTip)
		if err != nil {
			t.Fatal(err)
		}
		merged, err := h.pubGit.CommitTree(h.ctx, tree, parents, "merge a", testutil.Author(60), testutil.Author(60))
		if err != nil {
			t.Fatal(err)
		}
		if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/main", merged); err != nil {
			t.Fatal(err)
		}
		h.forge.SetPRMerged(publicName, prs[0].Number)
		h.forge.SetPRBase(publicName, prs[1].Number, "main")
		h.syncOK(engine.SyncOptions{})
		mb := h.shadowMeta("b")
		if mb.Promoted.NeedsUpdate == mergeCommit || mb.ParentName() != "main" {
			t.Fatalf("merge commit %v: b's record = parent %q, %+v", mergeCommit, mb.ParentName(), mb.Promoted)
		}
		if r := h.report("b"); (r.State == feature.StatePublicPRNeedsUpdate) == mergeCommit {
			t.Fatalf("merge commit %v: b is %s", mergeCommit, r.State)
		}
	}
}

// --plan previews the re-parenting and the recorded restacks, and changes nothing.
func TestSyncPlanPreviewsTheStackBookkeeping(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b", "c")
	h.squashMergeBottom(prs)
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, _ := h.syncOK(engine.SyncOptions{})
	h.eng.DryRun = false
	// Record writes are push_ref ops on a feature's record ref; a's close
	// deletes its record with a delete_ref, which doesn't count.
	var writes []string
	for _, op := range res.Preview.Ops {
		if ref, _ := op["ref"].(string); op["op"] == "push_ref" && strings.HasPrefix(ref, "refs/ghostbranch/features/") {
			writes = append(writes, strings.TrimPrefix(ref, "refs/ghostbranch/features/"))
		}
	}
	// b's and c's recorded restacks, then b's re-parenting in a's close.
	if !reflect.DeepEqual(writes, []string{"b", "c", "b"}) {
		t.Fatalf("record writes previewed: %v in %+v", writes, res.Preview.Ops)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("sync --plan changed the shadow repo")
	}
}

// A commit someone pushed to a single feature's public branch is flagged,
// never adopted, until feature pull brings it in.
func TestSyncFlagsAForeignChangeOnASingleFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("a")
	published := h.shadowMeta("a").Promoted.PublishedHead
	theirs := testutil.Commit(t, h.pubGit, "a", "someone else's change", map[string]string{"fix.txt": "fix\n"})
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.PublicBranchChanged)
	if len(ws) != 1 || ws[0].Details["feature"] != "a" {
		t.Fatalf("warnings = %+v", warns)
	}
	// details.foreign has the errors' shape: the commits, not a count.
	if fs, _ := ws[0].Details["foreign"].([]map[string]any); len(fs) != 1 || fs[0]["sha"] != theirs || fs[0]["subject"] != "someone else's change" {
		t.Fatalf("details.foreign = %#v", ws[0].Details["foreign"])
	}
	if !reflect.DeepEqual(attentionNames(res.Attention), []string{"a"}) || h.shadowMeta("a").Promoted.PublishedHead != published {
		t.Fatalf("attention %v, record %+v", attentionNames(res.Attention), h.shadowMeta("a").Promoted)
	}
	if _, _, err := h.eng.FeaturePull(h.ctx, h.pair, engine.PullOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	if _, warns = h.syncOK(engine.SyncOptions{}); len(warningsFor(warns, contract.PublicBranchChanged)) != 0 {
		t.Fatalf("after feature pull, sync still warns: %+v", warns)
	}
}

// A record that can't be read may be stacked on the merged feature, so
// sync leaves it open, in attention, and says which record is in the way.
func TestSyncLeavesAParentOpenBesideAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	h.forge.SetPRMerged(publicName, prs[0].Number)
	ma := h.shadowMeta("a")
	want := h.featureRefs("a", ma.Promoted.PlanID)
	h.corruptMeta("b")
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 {
		t.Fatalf("closed = %+v", res.Closed)
	}
	ws := warningsFor(warns, contract.FeatureNotClosed)
	if len(ws) != 1 || ws[0].Details["feature"] != "a" || !reflect.DeepEqual(ws[0].Details["unreadable"], []string{"b"}) ||
		!strings.Contains(ws[0].Message, "feature b couldn't be read") {
		t.Fatalf("warnings = %+v", warns)
	}
	if !reflect.DeepEqual(attentionNames(res.Attention), []string{"a", "b"}) {
		t.Fatalf("attention = %v", attentionNames(res.Attention))
	}
	after := h.shadowRefs()
	for ref, sha := range want {
		if after[ref] != sha {
			t.Fatalf("%s is %q, want %s", ref, after[ref], sha)
		}
	}
}

// A parent that moved on after its child forked, then merged without the
// child being restacked: the child doesn't contain the parent's published
// head, so sync keeps the child's own fork point (still an ancestor of the
// parent's head, so forked_at..head is only the child's commits). Its gate
// then replays only its own commits, and restack --onto main works.
func TestSyncKeepsAForkPointTheChildStillHas(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	bHead := h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	fork := h.meta("b").ForkedAt
	testutil.Commit(t, h.shadowGit(), "a", "more a", map[string]string{"a2.txt": "a2\n"})
	h.gateFeature("a")
	h.grant(time.Hour)
	res, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a = %+v %v", res, err)
	}
	h.eng.Approver = nil
	h.forge.SetPRMerged(publicName, res.Applied.PublicPR)
	sres, _ := h.syncOK(engine.SyncOptions{})
	if len(sres.Closed) != 1 || sres.Closed[0].Name != "a" || !reflect.DeepEqual(sres.Closed[0].Reparented, []string{"b"}) {
		t.Fatalf("closed = %+v", sres.Closed)
	}
	if mb := h.shadowMeta("b"); mb.ParentName() != "main" || mb.ForkedAt != fork {
		t.Fatalf("b's record = parent %q, forked_at %q; want main, %s", mb.ParentName(), mb.ForkedAt, fork)
	}
	h.gateFeature("b")
	if got := gateSources(h.meta("b")); !reflect.DeepEqual(got, []string{bHead}) {
		t.Fatalf("b's gate replayed %v, want only b's own commit %s", got, bHead)
	}
	rres, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "b", Onto: "main"})
	if err != nil || len(rres.Restacked) != 1 {
		t.Fatalf("restack b --onto main = %+v %v", rres, err)
	}
	bNew := h.head("b")
	if _, ok := h.fileAt(bNew, "a.txt"); ok {
		t.Fatal("b still carries a's commit")
	}
	if _, ok := h.fileAt(bNew, "b.txt"); !ok {
		t.Fatal("b lost its own commit")
	}
}

// Plain chained PRs: a person merged b's PR into a's public branch, not
// main. Sync leaves b open, in attention, with a FEATURE_NOT_CLOSED that
// says so, and touches neither b nor a.
func TestSyncLeavesAFeatureMergedIntoItsParentsBranchOpen(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	h.forge.StacksUnavailable = true
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil
	prs := res.Applied.PRs
	if prs[1].Base != "a" {
		t.Fatalf("b's PR base = %q, want a", prs[1].Base)
	}
	h.forge.SetPRMerged(publicName, prs[1].Number)
	before := h.shadowRefs()
	sres, warns := h.syncOK(engine.SyncOptions{})
	if len(sres.Closed) != 0 {
		t.Fatalf("closed = %+v", sres.Closed)
	}
	ws := warningsFor(warns, contract.FeatureNotClosed)
	if len(ws) != 1 || ws[0].Details["feature"] != "b" || ws[0].Details["reason"] != "merged_into_parent_branch" ||
		ws[0].Details["base"] != "a" || !strings.Contains(ws[0].Message, "ghostbranch feature close b") {
		t.Fatalf("warnings = %+v", warns)
	}
	if !reflect.DeepEqual(attentionNames(sres.Attention), []string{"b"}) {
		t.Fatalf("attention = %v", attentionNames(sres.Attention))
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("sync changed the shadow repo")
	}
}

// A parent whose published head nothing records can't tell its children's
// fork point: re-parenting them refuses (HAS_CHILDREN, parent_head_unknown)
// rather than write a record with no fork point.
func TestReparentRefusesWhenTheParentsHeadIsUnknown(t *testing.T) {
	t.Parallel()
	h, _ := publishedStack(t, "a", "b")
	err := h.eng.ReparentChildrenForTest(h.ctx, h.pair, "a", func(m *feature.Meta) {
		p := *m.Promoted
		p.FeatureHead, p.PlanID = "", ""
		m.Promoted = &p
	})
	ce := wantCode(t, err, contract.HasChildren)
	if ce.Details["reason"] != "parent_head_unknown" || !reflect.DeepEqual(ce.Details["children"], []string{"b"}) {
		t.Fatalf("details = %v", ce.Details)
	}
	if mb := h.shadowMeta("b"); mb.Parent != "a" {
		t.Fatalf("b's parent is %q", mb.Parent)
	}
}

// With several unreadable records, the warning names a console close for
// each of them.
func TestSyncNamesEveryUnreadableRecord(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b", "c")
	h.forge.SetPRMerged(publicName, prs[0].Number)
	h.corruptMeta("b")
	h.corruptMeta("c")
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.FeatureNotClosed)
	if len(res.Closed) != 0 || len(ws) != 1 || !reflect.DeepEqual(ws[0].Details["unreadable"], []string{"b", "c"}) ||
		!strings.Contains(ws[0].Message, "ghostbranch feature close b; ghostbranch feature close c") {
		t.Fatalf("closed %+v, warnings %+v", res.Closed, warns)
	}
}

// A child whose record a newer Ghostbranch wrote can't be moved onto main:
// sync leaves its merged parent open (HAS_CHILDREN behind the
// FEATURE_NOT_CLOSED) and changes nothing.
func TestSyncLeavesAParentOpenBesideANewerChild(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b")
	h.forge.SetPRMerged(publicName, prs[0].Number)
	h.newerMeta("b")
	before := h.shadowRefs()
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.FeatureNotClosed)
	if len(res.Closed) != 0 || len(ws) != 1 || ws[0].Details["feature"] != "a" || ws[0].Details["error_code"] != string(contract.HasChildren) {
		t.Fatalf("closed %+v, warnings %+v", res.Closed, warns)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("sync changed the shadow repo")
	}
}

// Recording GitHub's restack is leased: a record someone moved meanwhile
// is left as it is, with SYNC_PUSH_FAILED, and the rest of sync goes on.
func TestSyncRestackRecordPushFailureWarns(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b", "c")
	published := h.shadowMeta("c").Promoted.PublishedHead
	h.squashMergeBottom(prs)
	h.moveMetaOnPush("c", 1)
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.SyncPushFailed)
	if len(ws) != 1 || ws[0].Details["branch"] != "refs/ghostbranch/features/c" || !strings.Contains(ws[0].Message, "next sync tries again") {
		t.Fatalf("warnings = %+v", warns)
	}
	if got := h.shadowMeta("c").Promoted.PublishedHead; got != published {
		t.Fatalf("c's published head became %s", got)
	}
	if len(res.Closed) != 1 || res.Closed[0].Name != "a" {
		t.Fatalf("closed = %+v", res.Closed)
	}
}

// A moved stacked branch that can't be compared (its PR's base isn't in
// the public repo) is skipped with PR_STATE_UNKNOWN, never flagged or
// recorded.
func TestSyncSkipsAStackedBranchItCantCompare(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "a", "b", "c")
	published := h.shadowMeta("c").Promoted.PublishedHead
	h.squashMergeBottom(prs)
	h.forge.SetPRBase(publicName, prs[2].Number, "no-such-branch")
	res, warns := h.syncOK(engine.SyncOptions{})
	ws := warningsFor(warns, contract.PRStateUnknown)
	if len(ws) != 1 || ws[0].Details["feature"] != "c" || !strings.Contains(ws[0].Message, "no-such-branch is not in the public repo") ||
		len(warningsFor(warns, contract.PublicBranchChanged)) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	if got := h.shadowMeta("c").Promoted.PublishedHead; got != published {
		t.Fatalf("c's published head became %s", got)
	}
	for _, r := range res.Attention {
		if r.Name == "c" {
			t.Fatal("c was put in attention")
		}
	}
}
