package engine_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// archived publishes name (kept) and archives it; it returns the PR.
func (h *harness) archived(name string) int {
	h.t.Helper()
	pr, _ := h.publish(name)
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, name); err != nil {
		h.t.Fatalf("archive %s: %v", name, err)
	}
	return pr
}

func TestSurveyReadsAnArchivedFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	r := h.report("feat-x")
	if r.State != feature.StateArchived || r.Head != "" || r.Archived == nil || r.Moved || r.PR != nil || r.Drift.Status != "n/a" {
		t.Fatalf("report = %+v", r)
	}
	validateResult(t, "feature-report.schema.json", r)
	h.forge.SetPRMerged(publicName, pr)
	if r = h.report("feat-x"); r.State != feature.StateMerged || !engine.ClosableWithoutConsole(r) {
		t.Fatalf("merged: %+v", r)
	}
}

// Archived after commits it never published: its merge needs a person (R9).
func TestAnArchiveWithUnpublishedCommitsMergesMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "never published", map[string]string{"later.txt": "later\n"})
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); r.State != feature.StateMergedMoved || engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
}

// An archived feature's shadow branch that comes back (someone pushed it
// from an old checkout, or an older binary wrote it) may hold new private
// commits: the feature reads moved, so its merge is merged_moved and sync
// leaves it, and the branch, to a person (P4).
func TestAnArchivedFeatureWhoseBranchCameBackIsMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	sg := h.shadowGit()
	arch := h.shadowRefs()[archiveRefX]
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-x", arch); err != nil {
		t.Fatal(err)
	}
	back := testutil.Commit(t, sg, "feat-x", "pushed from an old checkout", map[string]string{"old.txt": "old\n"})
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); r.State != feature.StateMergedMoved || !r.Moved || engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if refs := h.shadowRefs(); len(res.Closed) != 0 || refs["refs/heads/feat-x"] != back || refs[archiveRefX] != arch {
		t.Fatalf("sync closed it unattended: %+v", res.Closed)
	}
}

func TestAClosedArchivedPRIsPRClosed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.forge.ClosePR(publicName, h.archived("feat-x"))
	if r := h.report("feat-x"); r.State != feature.StatePRClosed {
		t.Fatalf("report = %+v", r)
	}
}

// Sync closes a merged archived feature, deleting its archive ref in the
// same push, and the ledger keeps the archive's SHA.
func TestSyncClosesAnArchivedFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	arch := h.shadowRefs()[archiveRefX]
	h.forge.SetPRMerged(publicName, pr)
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" || res.Closed[0].Deleted[archiveRefX] != arch {
		t.Fatalf("closed = %+v", res.Closed)
	}
	refs := h.shadowRefs()
	for _, ref := range []string{archiveRefX, "refs/ghostbranch/features/feat-x"} {
		if _, ok := refs[ref]; ok {
			t.Errorf("%s survived the close", ref)
		}
	}
	ents := h.closeEntries()
	if len(ents) != 1 || ents[0].Refs[archiveRefX] != arch {
		t.Fatalf("ledger = %+v", ents)
	}
}

// partlyArchived publishes name and archives it with the gate PR's close
// failing: the archive ref is pushed and the branches are gone, but the
// record is never marked. It returns the PR and the archive's SHA.
func (h *harness) partlyArchived(name string) (int, string) {
	h.t.Helper()
	pr, _ := h.publish(name)
	h.forge.ClosePRErr = errors.New("connection reset")
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, name); err == nil {
		h.t.Fatal("the archive was meant to stop at the gate PR")
	}
	h.forge.ClosePRErr = nil
	if h.meta(name).Archived != nil {
		h.t.Fatal("the record was meant to stay unmarked")
	}
	return pr, h.shadowRefs()[feature.ArchiveRef(name)]
}

// An archive that stopped before its record was marked, whose PR then
// merged: sync closes it as a merged feature and still deletes the archive
// ref, leased on what it holds, in the same push.
func TestSyncClosesAPartlyArchivedFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, arch := h.partlyArchived("feat-x")
	if arch == "" {
		t.Fatal("no archive ref")
	}
	h.forge.SetPRMerged(publicName, pr)
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Deleted[archiveRefX] != arch {
		t.Fatalf("closed = %+v", res.Closed)
	}
	if _, ok := h.shadowRefs()[archiveRefX]; ok {
		t.Fatal("the archive ref survived the close")
	}
}

// partlyArchivedAhead is partlyArchived for a feature archived with a
// commit it never published: the archive ref holds it. It returns the PR,
// the published head and the archive's SHA.
func (h *harness) partlyArchivedAhead(name string) (int, string, string) {
	h.t.Helper()
	pr, _ := h.publish(name)
	published := h.head(name)
	testutil.Commit(h.t, h.shadowGit(), name, "never published", map[string]string{"later.txt": "later\n"})
	h.forge.ClosePRErr = errors.New("connection reset")
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, name); err == nil {
		h.t.Fatal("the archive was meant to stop at the gate PR")
	}
	h.forge.ClosePRErr = nil
	if h.meta(name).Archived != nil {
		h.t.Fatal("the record was meant to stay unmarked")
	}
	return pr, published, h.shadowRefs()[feature.ArchiveRef(name)]
}

// leftForAPerson checks a merged feature reads merged_moved, and that sync
// closes nothing and leaves the archive ref at arch.
func (h *harness) leftForAPerson(name, arch string) {
	h.t.Helper()
	if r := h.report(name); r.State != feature.StateMergedMoved || !r.Moved || engine.ClosableWithoutConsole(r) {
		h.t.Fatalf("report = %+v", r)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || h.shadowRefs()[feature.ArchiveRef(name)] != arch {
		h.t.Fatalf("sync closed it unattended: %+v", res.Closed)
	}
}

// The same, archived with commits it never published: the archive holds
// them, so its merge needs a person, as for a marked archive (R9).
func TestSyncLeavesAPartlyArchivedFeatureWithUnpublishedCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _, arch := h.partlyArchivedAhead("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.leftForAPerson("feat-x", arch)
}

// And with its shadow branch pushed back at the published head (from a
// working clone, say): the branch reads unmoved, but the archive ref the
// close would delete still holds the unpublished commit.
func TestSyncLeavesAPartialArchiveWhoseBranchCameBackAtThePublishedHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, published, arch := h.partlyArchivedAhead("feat-x")
	if arch == published {
		t.Fatal("the archive was meant to hold an unpublished commit")
	}
	if err := h.shadowGit().UpdateRef(h.ctx, "refs/heads/feat-x", published); err != nil {
		t.Fatal(err)
	}
	h.forge.SetPRMerged(publicName, pr)
	h.leftForAPerson("feat-x", arch)
	if h.shadowRefs()["refs/heads/feat-x"] != published {
		t.Fatal("sync deleted the branch")
	}
}

// A marked archive whose ref no longer holds the archived (and published)
// head: what the close would delete isn't what was published, so a person
// closes it.
func TestSyncLeavesAnArchiveWhoseRefMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	sg := h.shadowGit()
	other := testutil.Commit(t, sg, "elsewhere", "unrelated", map[string]string{"o.txt": "o\n"})
	if err := sg.UpdateRef(h.ctx, archiveRefX, other); err != nil {
		t.Fatal(err)
	}
	h.forge.SetPRMerged(publicName, pr)
	h.leftForAPerson("feat-x", other)
}

// Pushes to an archived feature's PR are expected: no
// PUBLIC_BRANCH_CHANGED, no attention.
func TestSyncDoesNotFlagPushesToAnArchivedPR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.archived("feat-x")
	testutil.Commit(t, h.pubGit, "feat-x", "review fix", map[string]string{"fix.txt": "fix\n"})
	res, warns := h.syncOK(engine.SyncOptions{})
	for _, w := range warns {
		if w.Code == contract.PublicBranchChanged {
			t.Fatalf("flagged: %+v", w)
		}
	}
	for _, r := range res.Attention {
		if r.Name == "feat-x" {
			t.Fatalf("in attention: %+v", r)
		}
	}
}

// A person closes an archived feature whose PR closed unmerged at the
// console: the archive ref goes, and the summary says how to push it back.
func TestFeatureCloseDeletesTheArchive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.forge.ClosePR(publicName, h.archived("feat-x"))
	arch := h.shadowRefs()[archiveRefX]
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil || res.Deleted[archiveRefX] != arch || !res.NeedsConsole {
		t.Fatalf("close = %+v %v", res, err)
	}
	if _, ok := h.shadowRefs()[archiveRefX]; ok {
		t.Fatal("the archive ref survived the close")
	}
	if len(h.approver.asked) == 0 || !strings.Contains(h.approver.asked[len(h.approver.asked)-1], arch+":refs/heads/feat-x") {
		t.Fatalf("the console summary must give the archive's recovery push: %q", h.approver.asked)
	}
}

// promote, gate and feature pull refuse an archived feature before any
// shadow-branch lookup (never FEATURE_NOT_FOUND).
func TestArchivedFeatureRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	check := func(what string, err error) {
		t.Helper()
		ce := wantCode(t, err, contract.FeatureArchived)
		if ce.Field != "feature" || ce.Details["feature"] != "feat-x" || ce.Details["public_pr"] != pr ||
			ce.Details["branch"] != "feat-x" || ce.Details["archive_ref"] != archiveRefX || ce.Details["pr_state"] != "open" {
			t.Errorf("%s: %+v", what, ce)
		}
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	check("gate", err)
	_, err = promote(h, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "m", PlanOnly: true})
	check("promote", err)
	_, err = promote(h, engine.PromoteOptions{Features: []string{"feat-x", "feat-y"}, Mode: "squash", PlanOnly: true})
	check("promote a stack", err)
	_, err = h.pull("feat-x")
	check("feature pull", err)
}

// A kept parent with an archived child (R12): promote updates only the
// parent's own PR, and never refuses for the child.
func TestPromoteOfAKeptParentSkipsAnArchivedChild(t *testing.T) {
	t.Parallel()
	h, prs := publishedStack(t, "feat-a", "feat-b")
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-b"); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "feat-a", "review fix", map[string]string{"a2.txt": "a2\n"})
	h.gateFeature("feat-a")
	res, err := promote(h, engine.PromoteOptions{Feature: "feat-a", Mode: "squash", Message: "Add a", PlanOnly: true})
	if err != nil || res.Plan == nil || len(res.Plan.PRs) != 1 || res.Plan.PRs[0].Number != prs[0].Number {
		t.Fatalf("promote feat-a = %+v %v", res, err)
	}
}

// The parent's gate offers nothing for an archived child (R12).
func TestGateReadinessLeavesArchivedChildrenOut(t *testing.T) {
	t.Parallel()
	h, _ := publishedStack(t, "feat-a", "feat-b")
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-b"); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.shadowGit(), "feat-a", "review fix", map[string]string{"a2.txt": "a2\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-a")
	if err != nil || len(res.Ready.StaleAbove) != 0 || contains(res.Ready.Levels, "feat-b") {
		t.Fatalf("gate feat-a = %+v %v", res.Ready, err)
	}
}

// A record that doesn't name its published head (made by a binary from
// before FeatureHead was recorded) takes it from the plan file, for the
// archive as for the branch (review M1): an archive holding exactly that
// head, merged, closes without a person.
func TestAnArchiveOfARecordWithoutItsHeadUsesThePlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.FeatureHead = "" })
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); r.State != feature.StateMerged || r.Moved || !engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
}

// FEATURE_ARCHIVED names the public PR's state (details.pr_state), and its
// message stops offering a push once the PR merged or was closed.
func TestArchivedFeatureRefusalsNameThePRState(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"merged", "closed"} {
		h := newHarness(t)
		h.init()
		pr := h.archived("feat-x")
		if state == "merged" {
			h.forge.SetPRMerged(publicName, pr)
		} else {
			h.forge.ClosePR(publicName, pr)
		}
		_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
		ce := wantCode(t, err, contract.FeatureArchived)
		if ce.Details["pr_state"] != state || strings.Contains(ce.Message, "by pushing") {
			t.Errorf("%s: %+v", state, ce)
		}
		res, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-x")
		if err != nil || res.Changed || res.PRState != state {
			t.Errorf("%s: feature archive = %+v %v", state, res, err)
		}
	}
}
