package engine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// landUnrecorded promotes an append to feat-x's open PR whose push lands
// but whose record push fails (someone moved the record just then, as a
// meta-lease failure or an apply killed after the push leaves it). It
// returns the plan and the record as it was before.
func landUnrecorded(t *testing.T) (*harness, *plan.Plan, feature.Meta) {
	t.Helper()
	h, _ := publishedFeature(t)
	before := h.shadowMeta("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "more\n"})
	h.gateFeature("feat-x")
	moved := false
	observe := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		if observe != nil {
			observe(op, remote, specs)
		}
		for _, s := range specs {
			if !moved && op == "promote" && remote == "shadow" && strings.HasSuffix(s, ":refs/ghostbranch/features/feat-x") {
				moved = true
				h.editRemoteMeta("feat-x", func(*feature.Meta) {})
			}
		}
	}
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	h.eng.OnPush = observe
	if codeOf(err) != contract.RefMoved || res == nil || res.Plan == nil || res.Plan.Publish.Style != plan.StyleAppend {
		t.Fatalf("promote with the record push failing = %+v %v", res, err)
	}
	if h.pubTip("feat-x") != res.Plan.Publish.Commit || h.shadowMeta("feat-x").Promoted.PlanID == res.Plan.ID {
		t.Fatal("the update must have landed without its record")
	}
	return h, res.Plan, before
}

// wantUnrecorded checks err is PUBLIC_BRANCH_CHANGED unrecorded_update
// naming plan id, with nothing foreign.
func wantUnrecorded(t *testing.T, what string, err error, id string) {
	t.Helper()
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	if ce.Details["reason"] != "unrecorded_update" || ce.Details["plan"] != id || reflect.ValueOf(ce.Details["foreign"]).Len() != 0 {
		t.Fatalf("%s: details = %+v", what, ce.Details)
	}
}

// I1: an update that landed but was never recorded is Ghostbranch's own.
// Promote and gate refuse naming its plan, also on the other machine (no
// plan file: the plan ref alone names it).
func TestUnrecordedUpdateRefusesPromoteAndGate(t *testing.T) {
	t.Parallel()
	h, pl, _ := landUnrecorded(t)
	id := pl.ID
	_, perr := promote(h, engine.PromoteOptions{Yes: true})
	wantUnrecorded(t, "promote", perr, id)
	_, _, gerr := h.eng.Gate(h.ctx, h.pair, "feat-x")
	wantUnrecorded(t, "gate", gerr, id)
	file := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	if err := os.Rename(file, file+".away"); err != nil {
		t.Fatal(err)
	}
	_, perr = promote(h, engine.PromoteOptions{Yes: true})
	wantUnrecorded(t, "promote without the plan file", perr, id)
}

// I1: feature pull never adopts Ghostbranch's own unrecorded update: it
// refuses naming the plan and changes nothing.
func TestUnrecordedUpdatePullChangesNothing(t *testing.T) {
	t.Parallel()
	h, pl, before := landUnrecorded(t)
	refs := h.shadowRefs()
	_, err := h.pull("feat-x")
	wantUnrecorded(t, "pull", err, pl.ID)
	if !reflect.DeepEqual(h.shadowRefs(), refs) || h.shadowMeta("feat-x").Promoted.PlanID != before.Promoted.PlanID {
		t.Fatal("a refused pull changed the shadow repo")
	}
}

// I1: status and sync count nothing foreign on an unrecorded update, and
// warn naming the plan whose apply finishes it.
func TestUnrecordedUpdateCountsNothingForeign(t *testing.T) {
	t.Parallel()
	h, pl, _ := landUnrecorded(t)
	id := pl.ID
	rs, warns := h.survey("feat-x")
	if len(rs) != 1 || rs[0].PR == nil {
		t.Fatalf("report = %+v", rs)
	}
	if rs[0].PR.Foreign != 0 {
		t.Fatalf("status counts %d foreign commit(s) on Ghostbranch's own update", rs[0].PR.Foreign)
	}
	if !hasUnrecordedWarning(warns, id) {
		t.Fatalf("status warnings = %+v", warns)
	}
	_, swarns := h.syncOK(engine.SyncOptions{})
	if !hasUnrecordedWarning(swarns, id) {
		t.Fatalf("sync warnings = %+v", swarns)
	}
	for _, w := range swarns {
		if w.Code == contract.PublicBranchChanged && w.Details["reason"] != "unrecorded_update" {
			t.Fatalf("sync took the update for someone else's: %+v", w)
		}
	}
}

// I1: apply of the plan finds the push landed and finishes the record;
// the next promote then updates the PR as usual.
func TestUnrecordedUpdateApplyFinishesIt(t *testing.T) {
	t.Parallel()
	h, pl, _ := landUnrecorded(t)
	id := pl.ID
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !ar.Resumed {
		t.Fatalf("apply %s = %+v %v", id, ar, err)
	}
	if m := h.shadowMeta("feat-x"); m.Promoted.PlanID != id || m.Promoted.PublishedHead != pl.Publish.Commit {
		t.Fatalf("record after apply = %+v", m.Promoted)
	}
	testutil.Commit(t, h.shadowGit(), "feat-x", "even more", map[string]string{"more2.txt": "more\n"})
	h.gateFeature("feat-x")
	res, err := promote(h, engine.PromoteOptions{Yes: true})
	if err != nil || res.Applied == nil || res.Plan.Publish.Style != plan.StyleAppend {
		t.Fatalf("promote after the record = %+v %v", res, err)
	}
}

func hasUnrecordedWarning(ws []contract.Warning, id string) bool {
	for _, w := range ws {
		if w.Code == contract.PublicBranchChanged && w.Details["reason"] == "unrecorded_update" && w.Details["plan"] == id {
			return true
		}
	}
	return false
}
