package engine_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const archiveRefX = "refs/ghostbranch/archive/feat-x"

// archive runs feature archive and checks a result against its schema.
func (h *harness) archive(name string) (*engine.FeatureArchiveResult, []contract.Warning, error) {
	h.t.Helper()
	res, warns, err := h.eng.FeatureArchive(h.ctx, h.pair, name)
	if res != nil {
		validateResult(h.t, "feature-archive.schema.json", res)
	}
	return res, warns, err
}

// One atomic push to the shadow repo creates the archive ref at the
// branch's head and deletes the branch and its gate branch; then the gate
// PR is closed and the record marked. Nothing touches the public repo.
func TestFeatureArchiveHandsOff(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	before := h.shadowRefs()
	head := before["refs/heads/feat-x"]
	gateTip, hasGate := before["refs/heads/ghostbranch-gate/feat-x"]
	gatePR := h.gatePR("feat-x")
	pubBefore := h.publicRefs()
	res, _, err := h.archive("feat-x")
	if err != nil || !res.Changed || res.PublicPR != pr || res.Branch != "feat-x" || res.Archived == nil ||
		res.Archived.Head != head || res.Archived.Ref != archiveRefX || res.Archived.At.IsZero() {
		t.Fatalf("archive = %+v %v", res, err)
	}
	want := map[string]string{archiveRefX: head, "refs/heads/feat-x": head}
	if hasGate {
		want["refs/heads/ghostbranch-gate/feat-x"] = gateTip
	}
	if !reflect.DeepEqual(res.Moved, want) {
		t.Fatalf("moved = %v, want %v", res.Moved, want)
	}
	after := h.shadowRefs()
	if after[archiveRefX] != head {
		t.Fatalf("archive ref = %q, want %s", after[archiveRefX], head)
	}
	for _, ref := range []string{"refs/heads/feat-x", "refs/heads/ghostbranch-gate/feat-x"} {
		if _, ok := after[ref]; ok {
			t.Errorf("%s survived the archive", ref)
		}
	}
	if gatePR != 0 && (res.GatePR == nil || *res.GatePR != gatePR || h.gatePR("feat-x") != 0) {
		t.Errorf("gate PR #%d: result %v, still open %d", gatePR, res.GatePR, h.gatePR("feat-x"))
	}
	if m := h.meta("feat-x"); m.Archived == nil || m.Archived.Head != head || m.Archived.Ref != archiveRefX {
		t.Fatalf("record = %+v", m.Archived)
	}
	if !reflect.DeepEqual(h.publicRefs(), pubBefore) {
		t.Fatal("feature archive changed the public repo")
	}
	if n := h.pushesBy("feature archive"); n != 2 { // the archive push, then the record
		t.Fatalf("feature archive pushed %d times, want 2", n)
	}
	ents := h.ledger()
	last := ents[len(ents)-1]
	if last.Op != "feature archive" || last.Feature != "feat-x" || !reflect.DeepEqual(last.Refs, want) {
		t.Fatalf("ledger = %+v", last)
	}
	validateResult(t, "ledger-entry.schema.json", last) // its op is in the enum (P8)
	if !h.mirrorHas("refs/shadow-meta/archive/feat-x") || h.mirrorHas("refs/remotes/shadow/feat-x") {
		t.Fatal("the mirror must follow the archive")
	}
}

// Running it again changes nothing and is not an error (R13).
func TestFeatureArchiveIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	if _, _, err := h.archive("feat-x"); err != nil {
		t.Fatal(err)
	}
	pushes := len(h.pushes)
	res, _, err := h.archive("feat-x")
	if err != nil || res.Changed || res.Archived == nil || len(res.Moved) != 0 || len(h.pushes) != pushes {
		t.Fatalf("second archive = %+v %v (pushes %d → %d)", res, err, pushes, len(h.pushes))
	}
}

// Unpublished, merged and closed features have no open PR to hand off.
func TestFeatureArchiveNeedsAnOpenPR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-d", map[string]string{"d.txt": "d\n"})
	_, _, err := h.archive("feat-d")
	if ce := wantCode(t, err, contract.NoOpenPR); ce.Details["reason"] != "not_published" {
		t.Fatalf("draft: %+v", ce)
	}
	pr, _ := h.publish("feat-m")
	h.forge.SetPRMerged(publicName, pr)
	_, _, err = h.archive("feat-m")
	if ce := wantCode(t, err, contract.NoOpenPR); ce.Details["reason"] != "merged" {
		t.Fatalf("merged: %+v", ce)
	}
	if _, ok := h.shadowRefs()["refs/heads/feat-m"]; !ok {
		t.Fatal("a refused archive must leave the branch")
	}
	pr, _ = h.publish("feat-c")
	if err := h.forge.ClosePullRequest(h.ctx, publicName, pr); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.archive("feat-c")
	if ce := wantCode(t, err, contract.NoOpenPR); ce.Details["reason"] != "closed" {
		t.Fatalf("closed: %+v", ce)
	}
	if _, ok := h.shadowRefs()["refs/heads/feat-c"]; !ok {
		t.Fatal("a refused archive must leave the branch")
	}
}

// A feature stacked on it would lose the branch it is built on (R8):
// abandoned children count, archived ones don't.
func TestFeatureArchiveRefusesUnderChildren(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	h.startOn("feat-y", "feat-x", map[string]string{"y.txt": "y\n"})
	_, _, err := h.archive("feat-x")
	ce := wantCode(t, err, contract.HasChildren)
	if !reflect.DeepEqual(ce.Details["children"], []string{"feat-y"}) || ce.Details["op"] != "archive" ||
		!strings.Contains(ce.Message, "ghostbranch promote feat-x <child> --plan") || strings.Contains(ce.Message, "feature fold") {
		t.Fatalf("children: %+v", ce) // archive's own remedies (P6): publish, restack, or close an abandoned child
	}
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-y", false); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.archive("feat-x")
	_ = wantCode(t, err, contract.HasChildren)
	if _, ok := h.shadowRefs()["refs/heads/feat-x"]; !ok {
		t.Fatal("a refused archive must leave the branch")
	}
	// An archived child doesn't block (R8).
	h.editRemoteMeta("feat-y", func(m *feature.Meta) {
		m.Archived = &feature.Archived{At: time.Unix(1700000000, 0).UTC(), Ref: "refs/ghostbranch/archive/feat-y", Head: h.head("feat-y")}
	})
	h.eng.DryRun = true
	if _, _, err := h.archive("feat-x"); err != nil {
		t.Fatalf("an archived child blocked: %v", err)
	}
	h.eng.DryRun = false
}

// A record that can't be read may be a child, so it blocks (R8): the guard
// fails closed.
func TestFeatureArchiveRefusesWithAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	h.startFeature("feat-bad", map[string]string{"b.txt": "b\n"})
	h.corruptMeta("feat-bad")
	_, _, err := h.archive("feat-x")
	ce := wantCode(t, err, contract.HasChildren)
	if ce.Details["op"] != "archive" || !reflect.DeepEqual(ce.Details["unreadable"], []string{"feat-bad"}) {
		t.Fatalf("details = %v", ce.Details)
	}
	if _, ok := h.shadowRefs()["refs/heads/feat-x"]; !ok {
		t.Fatal("a refused archive must leave the branch")
	}
}

// An archive that stopped after its push (the record unwritten) resumes:
// the archive ref at the head counts as done and the missing branch is fine.
func TestFeatureArchiveResumesAfterThePush(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	sg := h.shadowGit()
	head := h.head("feat-x")
	if err := sg.UpdateRef(h.ctx, archiveRefX, head); err != nil {
		t.Fatal(err)
	}
	if err := sg.DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.archive("feat-x")
	if err != nil || !res.Changed || res.Archived.Head != head {
		t.Fatalf("resume = %+v %v", res, err)
	}
	if _, ok := res.Moved[archiveRefX]; ok { // it was there already, so this run didn't create it (P16)
		t.Fatalf("moved = %v", res.Moved)
	}
	if m := h.meta("feat-x"); m.Archived == nil || m.Archived.Head != head {
		t.Fatalf("record = %+v", m.Archived)
	}
}

// The branch moved after a partial archive: the new head holds the old
// one, so the archive moves to it and nothing is lost (Review Focus 2).
func TestFeatureArchiveArchivesTheNewHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	sg := h.shadowGit()
	if err := sg.UpdateRef(h.ctx, archiveRefX, h.head("feat-x")); err != nil {
		t.Fatal(err)
	}
	newer := testutil.Commit(t, sg, "feat-x", "later work", map[string]string{"later.txt": "later\n"})
	res, _, err := h.archive("feat-x")
	if err != nil || res.Archived.Head != newer || h.shadowRefs()[archiveRefX] != newer {
		t.Fatalf("archive = %+v %v", res, err)
	}
}

// An archive ref the branch doesn't hold would be lost if moved (R17).
func TestFeatureArchiveRefusesADivergedArchive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	sg := h.shadowGit()
	other := testutil.Commit(t, sg, "elsewhere", "unrelated", map[string]string{"o.txt": "o\n"})
	if err := sg.UpdateRef(h.ctx, archiveRefX, other); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.archive("feat-x")
	_ = wantCode(t, err, contract.FeatureMoved)
	refs := h.shadowRefs()
	if refs[archiveRefX] != other || refs["refs/heads/feat-x"] == "" {
		t.Fatalf("a refused archive must change nothing: %v", refs)
	}
}

// --plan lists the operations and changes nothing (R16).
func TestFeatureArchivePlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, _, err := h.archive("feat-x")
	if err != nil || res.Preview == nil || res.Changed {
		t.Fatalf("plan = %+v %v", res, err)
	}
	var ops []string
	for _, op := range res.Preview.Ops {
		ops = append(ops, op["op"].(string)+" "+opRef(op))
	}
	for _, want := range []string{"push_ref " + archiveRefX, "delete_ref refs/heads/feat-x",
		"delete_ref refs/heads/ghostbranch-gate/feat-x", "close_pr ", "push_ref refs/ghostbranch/features/feat-x"} {
		if !contains(ops, want) {
			t.Errorf("preview lacks %q: %v", want, ops)
		}
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("--plan changed the shadow repo")
	}
	// --plan refuses what the real run refuses (P7): an archive ref the
	// branch doesn't hold is FEATURE_MOVED here too (R17).
	sg := h.shadowGit()
	other := testutil.Commit(t, sg, "elsewhere", "unrelated", map[string]string{"o.txt": "o\n"})
	if err := sg.UpdateRef(h.ctx, archiveRefX, other); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.archive("feat-x")
	_ = wantCode(t, err, contract.FeatureMoved)
	if refs := h.shadowRefs(); refs[archiveRefX] != other || refs["refs/heads/feat-x"] != before["refs/heads/feat-x"] {
		t.Fatalf("--plan changed the shadow repo: %v", refs)
	}
}

// The gate PR that won't close stops the archive at gate_pr, after the
// push: running it again finishes it.
func TestFeatureArchiveStopsAtTheGatePR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	if h.gatePR("feat-x") == 0 {
		t.Fatal("the gate opens a gate PR") // gate.go always opens a draft gate PR (P15)
	}
	before := h.shadowRefs()
	head, gateTip := before["refs/heads/feat-x"], before["refs/heads/ghostbranch-gate/feat-x"]
	if gateTip == "" {
		t.Fatal("the gate left no gate branch")
	}
	h.forge.ClosePRErr = errors.New("connection reset")
	res, _, err := h.archive("feat-x")
	if ce := contract.From(err); ce == nil || ce.Code != contract.ForgeError {
		t.Fatalf("err = %v", err)
	}
	if h.meta("feat-x").Archived != nil {
		t.Fatal("the record is the last step")
	}
	// The push landed whole: the archive holds the head, both branches are
	// gone, and the result and the ledger name what it moved.
	after := h.shadowRefs()
	if after[archiveRefX] != head {
		t.Fatalf("archive ref = %q, want %s", after[archiveRefX], head)
	}
	for _, ref := range []string{"refs/heads/feat-x", "refs/heads/ghostbranch-gate/feat-x"} {
		if _, ok := after[ref]; ok {
			t.Errorf("%s survived the archive push", ref)
		}
	}
	want := map[string]string{archiveRefX: head, "refs/heads/feat-x": head, "refs/heads/ghostbranch-gate/feat-x": gateTip}
	if res == nil || !reflect.DeepEqual(res.Moved, want) {
		t.Fatalf("result = %+v, want moved %v", res, want)
	}
	ents := h.ledger()
	if last := ents[len(ents)-1]; last.Op != "feature archive" || !reflect.DeepEqual(last.Refs, want) {
		t.Fatalf("ledger = %+v", last)
	}
	h.forge.ClosePRErr = nil
	res, _, err = h.archive("feat-x")
	if err != nil || !res.Changed || res.Archived == nil || res.Archived.Head != head {
		t.Fatalf("resume = %+v %v", res, err)
	}
	if _, ok := res.Moved[archiveRefX]; ok || len(res.Moved) != 0 { // nothing left to push (P16)
		t.Fatalf("resume moved = %v", res.Moved)
	}
	if h.gatePR("feat-x") != 0 {
		t.Fatal("the resume left the gate PR open")
	}
}

// A ref that moves while the archive pushes loses its lease: the one atomic
// push changes nothing, so no archive ref is made, both branches and the
// gate PR stay, and the record is unmarked (R6).
func TestFeatureArchiveLosesALease(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	gate := h.gatePR("feat-x")
	before := h.shadowRefs()
	if before["refs/heads/ghostbranch-gate/feat-x"] == "" || gate == 0 {
		t.Fatal("the gate left no gate branch or gate PR")
	}
	record := h.eng.OnPush
	var raced string
	var archivePushes [][]string
	h.eng.OnPush = func(op, remote string, specs []string) {
		if op == "feature archive" {
			archivePushes = append(archivePushes, specs)
			if raced == "" {
				raced = testutil.Commit(t, h.shadowGit(), "feat-x", "racing commit", map[string]string{"race.txt": "r\n"})
			}
		}
		record(op, remote, specs)
	}
	_, _, err := h.archive("feat-x")
	ce := wantCode(t, err, contract.FeatureMoved)
	if raced == "" {
		t.Fatal("the race never ran")
	}
	if ce.Details["feature"] != "feat-x" || ce.Details["ref"] != "refs/heads/feat-x" {
		t.Fatalf("details = %v", ce.Details)
	}
	if len(archivePushes) != 1 || len(archivePushes[0]) != 3 {
		t.Fatalf("want one push with all three refspecs, got %v", archivePushes)
	}
	after := h.shadowRefs()
	if _, ok := after[archiveRefX]; ok {
		t.Fatal("the archive ref was made although the push lost its lease")
	}
	if after["refs/heads/feat-x"] != raced {
		t.Fatalf("feat-x = %q, want the raced %s", after["refs/heads/feat-x"], raced)
	}
	if after["refs/heads/ghostbranch-gate/feat-x"] != before["refs/heads/ghostbranch-gate/feat-x"] {
		t.Fatal("the gate branch changed")
	}
	if h.gatePR("feat-x") != gate {
		t.Fatal("the gate PR was closed although nothing was archived")
	}
	if h.meta("feat-x").Archived != nil {
		t.Fatal("the record was marked although nothing was archived")
	}
}

// A published feature whose shadow branch is gone and which has no archive
// ref has nothing to archive: FEATURE_NOT_FOUND, in the run and --plan (P20).
func TestFeatureArchiveWithNothingToArchive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{true, false} {
		h.eng.DryRun = dry
		_, _, err := h.archive("feat-x")
		_ = wantCode(t, err, contract.FeatureNotFound)
	}
	h.eng.DryRun = false
	if h.meta("feat-x").Archived != nil {
		t.Fatal("the record was marked")
	}
}

// A record that names another branch is refused by --plan too (P7).
func TestFeatureArchiveRefusesAnotherBranchInThePlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Branch = "feat-other" })
	before := h.shadowRefs()
	for _, dry := range []bool{true, false} {
		h.eng.DryRun = dry
		_, _, err := h.archive("feat-x")
		_ = wantCode(t, err, contract.Internal)
	}
	h.eng.DryRun = false
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("a refused archive changed the shadow repo")
	}
}

// feature archive and its --plan leave no step running, and the archive
// push ends its step as done (P21). Uses stepends_test.go's record and
// endedClean; progress_steps_test.go stays untouched (the 6.4b overlap).
func TestStepsEndAfterFeatureArchive(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.publish("feat-x")
	h.eng.DryRun = true
	rec := h.record()
	if _, _, err := h.archive("feat-x"); err != nil {
		t.Fatal(err)
	}
	h.eng.DryRun = false
	endedClean(t, rec, "feature archive --plan", "")
	rec = h.record()
	if _, _, err := h.archive("feat-x"); err != nil {
		t.Fatal(err)
	}
	endedClean(t, rec, "feature archive", "Archived feat-x at "+archiveRefX)
}

func opRef(op preview.Op) string { s, _ := op["ref"].(string); return s }
