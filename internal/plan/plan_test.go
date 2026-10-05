package plan_test

import (
	"bytes"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

func TestNewIDIsSortableAndDeterministic(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC)
	id, err := plan.NewID(now, bytes.NewReader([]byte{0xab, 0xcd, 0xef}))
	if err != nil || id != "20261005T123000Z-abcdef" {
		t.Fatalf("NewID = %q %v", id, err)
	}
	if !regexp.MustCompile(`^\d{8}T\d{6}Z-[0-9a-f]{6}$`).MatchString(id) {
		t.Fatalf("bad id shape %q", id)
	}
}

func TestFingerprintMoved(t *testing.T) {
	a := plan.Fingerprint{PublicMain: "p", FeatureHead: "h", Overlay: "o", GateCandidate: "c"}
	if m := a.Moved(a); len(m) != 0 {
		t.Fatalf("identical fingerprints moved: %v", m)
	}
	b := a
	b.PublicMain, b.GateCandidate = "p2", "c2"
	if m := a.Moved(b); !reflect.DeepEqual(m, []string{"public_main", "gate_candidate"}) {
		t.Fatalf("Moved = %v", m)
	}
}

func TestSaveLoadByIDAndPath(t *testing.T) {
	dir := t.TempDir()
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261005T123000Z-abcdef", Kind: "promote", Feature: "x",
		CreatedAt: time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC), Leak: plan.Leak{Findings: []map[string]any{}}}
	path, err := plan.Save(dir, p)
	if err != nil || path != filepath.Join(dir, p.ID+".json") {
		t.Fatalf("Save = %q %v", path, err)
	}
	for _, ref := range []string{p.ID, path} {
		got, gotPath, err := plan.Load(dir, ref)
		if err != nil || !reflect.DeepEqual(got, p) || gotPath != path {
			t.Fatalf("Load(%q) = %+v %q %v", ref, got, gotPath, err)
		}
	}
	_, _, err = plan.Load(dir, "nope")
	if ce := contract.From(err); ce == nil || ce.Code != contract.BadFlag || ce.Field != "plan" {
		t.Fatalf("unknown plan: %v", err)
	}
}
