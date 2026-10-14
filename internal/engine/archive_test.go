package engine_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
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
	for _, want := range []string{"push_ref " + archiveRefX, "delete_ref refs/heads/feat-x", "push_ref refs/ghostbranch/features/feat-x"} {
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
	h.forge.ClosePRErr = errors.New("connection reset")
	_, _, err := h.archive("feat-x")
	if ce := contract.From(err); ce == nil || ce.Code != contract.ForgeError {
		t.Fatalf("err = %v", err)
	}
	if h.meta("feat-x").Archived != nil {
		t.Fatal("the record is the last step")
	}
	h.forge.ClosePRErr = nil
	if res, _, err := h.archive("feat-x"); err != nil || !res.Changed {
		t.Fatalf("resume = %+v %v", res, err)
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
