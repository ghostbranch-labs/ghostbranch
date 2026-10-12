package engine_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// record points h's engine at a fresh Recorder.
func (h *harness) record() *progress.Recorder {
	rec := &progress.Recorder{}
	h.eng.Progress = rec
	return rec
}

// endedClean fails t when a step is still running after a command that
// succeeded, or when want is not among the steps' endings as done.
func endedClean(t *testing.T, rec *progress.Recorder, command, want string) {
	t.Helper()
	if rec.Running() {
		t.Fatalf("a step is still running after %s:\n%s", command, rec.String())
	}
	if want != "" && !hasEvent(rec.Events(), "done", want) {
		t.Fatalf("%s never ended %q as done:\n%s", command, want, rec.String())
	}
}

// A dry-run gate stops after the scan, and a detached gate after its push:
// neither leaves a step running.
func TestStepsEndAfterADryRunAndADetachedGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	h.eng.DryRun = true
	rec := h.record()
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	h.eng.DryRun = false
	endedClean(t, rec, "gate --dry-run", "Scanning what feat-x would publish for leaks (preserve)")
	if slices.Contains(rec.Steps(), "Pushing the gate candidate to the shadow repo") {
		t.Fatalf("a dry run pushed:\n%s", rec.String())
	}

	rec = h.record()
	res, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "gate --detach", "Pushed the gate candidate")
	want := "gate candidate " + res.Candidate[:12] + " → " + shadowName + ":ghostbranch-gate/feat-x"
	if !slices.Contains(rec.Notes(), want) {
		t.Fatalf("notes lack %q:\n%s", want, rec.String())
	}
}

func TestStepsEndAfterRestackAndFold(t *testing.T) {
	t.Parallel()
	h := threeDeep(t)
	testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	rec := h.record()
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a"}); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "feature restack", "Restacking feat-c onto feat-b")
	if !slices.Contains(rec.Steps(), "Restacking feat-b onto feat-a") {
		t.Fatalf("restack steps:\n%s", rec.String())
	}

	h2 := threeDeep(t)
	h2.gateFeature("feat-b")
	aBefore, bHead := h2.head("feat-a"), h2.head("feat-b")
	rec = h2.record()
	if _, _, err := h2.eng.FeatureFold(h2.ctx, h2.pair, engine.FoldOptions{Feature: "feat-b"}); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "feature fold", "Folding feat-b into feat-a")
	if want := fmt.Sprintf("feat-a: %s → %s", aBefore[:12], bHead[:12]); !slices.Contains(rec.Notes(), want) {
		t.Fatalf("notes lack %q:\n%s", want, rec.String())
	}
}

func TestStepsEndAfterCloseAndUpgrade(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.eng.Approver = nil
	rec := h.record()
	if _, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "feature close", "Deleted 4 ref(s) of feat-x from the shadow repo")

	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	rec = h.record()
	if _, _, err := h.eng.Upgrade(h.ctx, h.pair); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "upgrade", "Upgraded the overlay branch of "+shadowName)
}

// feature pull, then the promote that updates the open PR.
func TestStepsEndAfterPullAndAnUpdate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publishPreserve("a")
	testutil.Commit(t, h.pubGit, "a", "their fix", map[string]string{"fix.txt": "fix\n"})
	testutil.Commit(t, h.shadowGit(), "a", "my review fix", map[string]string{"a.txt": "a, reviewed\n"})
	prNum := h.shadowMeta("a").Promoted.PublicPR
	rec := h.record()
	if _, err := h.pull("a"); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "feature pull", fmt.Sprintf("Brought 1 commit(s) from PR #%d into a", prNum))

	h.gateFeature("a")
	rec = h.record()
	pres, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || pres.Applied == nil {
		t.Fatalf("promote = %+v %v", pres, err)
	}
	endedClean(t, rec, "promote (update)", fmt.Sprintf("Updated PR #%d on %s (append)", prNum, publicName))
}

func TestStepsEndAfterWaiversAdd(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	rec := h.record()
	if _, err := add(h, "test fixture", id); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "waivers add", "Scanning what feat-x would publish for leaks (preserve)")
}
