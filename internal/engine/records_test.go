package engine_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

// newerMeta makes a feature's record look written by a newer Ghostbranch.
func (h *harness) newerMeta(name string) {
	h.t.Helper()
	h.editRemoteMeta(name, func(m *feature.Meta) { m.SchemaVersion = "0.99" })
}

// warning returns the first warning with code, or nil.
func warning(ws []contract.Warning, code contract.WarningCode) *contract.Warning {
	for i := range ws {
		if ws[i].Code == code {
			return &ws[i]
		}
	}
	return nil
}

func TestANewerRecordRefusesWrites(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.forge.Checks = passing
	h.newerMeta("feat-x")
	const meta = "refs/ghostbranch/features/feat-x"
	before := h.shadowRefs()[meta]
	for _, c := range []struct {
		name string
		run  func() error
	}{
		{"gate", func() error { _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); return err }},
		{"feature abandon", func() error { _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", false); return err }},
		{"promote --plan", func() error {
			_, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add x", PlanOnly: true})
			return err
		}},
	} {
		err := c.run()
		ce := contract.From(err)
		if ce == nil || ce.Code != contract.UpgradeNeeded {
			t.Fatalf("%s: want UPGRADE_NEEDED, got %v", c.name, err)
		}
		if ce.Details["record"] != "feature" || ce.Details["record_version"] != "0.99" || ce.Details["supported"] != contract.SchemaVersion {
			t.Fatalf("%s: details = %+v", c.name, ce.Details)
		}
		if got := h.shadowRefs()[meta]; got != before {
			t.Fatalf("%s moved the metadata ref", c.name)
		}
	}
}

func TestANewerRecordIsShownWithAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.newerMeta("feat-x")
	check := func(what string, ws []contract.Warning) {
		t.Helper()
		w := warning(ws, contract.RecordNewer)
		if w == nil || w.Details["feature"] != "feat-x" || w.Details["record_version"] != "0.99" {
			t.Fatalf("%s warnings = %+v", what, ws)
		}
	}
	res, warns := h.status(contract.RecordNewer)
	if len(res.Features) != 1 || res.Features[0].Name != "feat-x" || res.Features[0].State != feature.StateDraft {
		t.Fatalf("status features = %+v", res.Features)
	}
	check("status", warns)
	show, warns, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil || show.Feature.Name != "feat-x" || len(show.Commits) != 1 {
		t.Fatalf("feature show = %+v %v", show, err)
	}
	check("feature show", warns)
	list, warns, err := h.eng.FeatureList(h.ctx, h.pair, false)
	if err != nil || len(list.Features) != 1 {
		t.Fatalf("feature list = %+v %v", list, err)
	}
	check("feature list", warns)
}

// closeAtTheConsole checks that feat-x, whose record can't be read as a
// whole, closes only at the console, and deletes every ref of it (with
// the plan ref of planID, "" for none). The console's summary says says.
func closeAtTheConsole(t *testing.T, h *harness, state feature.State, planID, says string) {
	t.Helper()
	before := h.shadowRefs()
	want := h.featureRefs("feat-x", planID)

	h.eng.Approver = nil
	_, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.ApprovalRequired)
	if ce.Details["reason"] != string(approval.NoConsole) {
		t.Fatalf("details = %v", ce.Details)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.closePushes() != 0 {
		t.Fatal("closing without a console deleted something")
	}

	h.eng.Approver = h.approver
	res, warns, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	validateResult(t, "feature-close.schema.json", res)
	if res.State != state || res.Approval == nil || res.Approval.Kind != "console" || !res.NeedsConsole {
		t.Fatalf("close = %+v", res)
	}
	if w := warning(warns, contract.RecordUnreadable); w == nil || w.Details["feature"] != "feat-x" {
		t.Fatalf("warnings = %+v", warns)
	}
	if !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("deleted = %v, want %v", res.Deleted, want)
	}
	for ref := range want {
		if _, ok := h.shadowRefs()[ref]; ok {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
	if summary := h.approver.asked[len(h.approver.asked)-1]; !strings.Contains(summary, says) {
		t.Errorf("the summary doesn't say %q:\n%s", says, summary)
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "feature close" || last.Approval == nil || last.Approval.Kind != "console" ||
		!reflect.DeepEqual(last.Refs, want) {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestConsoleClosesACorruptRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.gateFeature("feat-x")
	h.corruptMeta("feat-x")
	if refs := h.featureRefs("feat-x", ""); len(refs) != 3 {
		t.Fatalf("want the branch, the gate branch and the metadata ref, got %v", refs)
	}
	closeAtTheConsole(t, h, feature.StateUnreadable, "", "record couldn't be read")
	if h.gatePR("feat-x") != 0 {
		t.Fatal("the gate PR is still open")
	}
}

// A merged, unmoved feature closes unattended, but not when its record is
// newer than this binary: what it can't read may matter.
func TestConsoleClosesANewerRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); !engine.ClosableWithoutConsole(r) {
		t.Fatalf("set up a merged feature, got %+v", r)
	}
	h.newerMeta("feat-x")
	if r := h.report("feat-x"); r.State != feature.StateMerged || engine.ClosableWithoutConsole(r) {
		t.Fatalf("a newer record closes unattended: %+v", r)
	}
	closeAtTheConsole(t, h, feature.StateMerged, id, "written by a newer Ghostbranch")
}
