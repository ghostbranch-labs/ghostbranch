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
	if err := sg.UpdateRef(h.ctx, "refs/heads/feat-x", h.shadowRefs()[archiveRefX]); err != nil {
		t.Fatal(err)
	}
	back := testutil.Commit(t, sg, "feat-x", "pushed from an old checkout", map[string]string{"old.txt": "old\n"})
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); r.State != feature.StateMergedMoved || !r.Moved || engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || h.shadowRefs()["refs/heads/feat-x"] != back {
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

// The same, archived with commits it never published: the archive holds
// them, so its merge needs a person, as for a marked archive (R9).
func TestSyncLeavesAPartlyArchivedFeatureWithUnpublishedCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "never published", map[string]string{"later.txt": "later\n"})
	h.forge.ClosePRErr = errors.New("connection reset")
	if _, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-x"); err == nil {
		t.Fatal("the archive was meant to stop at the gate PR")
	}
	h.forge.ClosePRErr = nil
	arch := h.shadowRefs()[archiveRefX]
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || h.shadowRefs()[archiveRefX] != arch {
		t.Fatalf("sync closed it unattended: %+v", res.Closed)
	}
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
	if len(h.approver.asked) == 0 || !strings.Contains(h.approver.asked[len(h.approver.asked)-1], arch+":refs/heads/feat-x") {
		t.Fatalf("the console summary must give the archive's recovery push: %q", h.approver.asked)
	}
}
