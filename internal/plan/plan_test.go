package plan_test

import (
	"bytes"
	"errors"
	"os"
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

func TestLoadRejectsPathsAndKeepsReadErrorsApart(t *testing.T) {
	dir := t.TempDir()
	for _, ref := range []string{"../x", "..\\x", "a/b", "20261008T120000Z-abcdef/../../etc"} {
		if _, _, err := plan.Load(dir, ref); contract.From(err).Code != contract.BadFlag {
			t.Fatalf("%q: want BAD_FLAG, got %v", ref, err)
		}
	}
	if _, _, err := plan.Load(dir, "20261008T120000Z-abcdef"); !errors.Is(err, plan.ErrNotFound) || contract.From(err).Code != contract.BadFlag {
		t.Fatalf("missing plan: %v", err)
	}
	// A path to a plan file still loads, as apply <path> promises.
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261008T120000Z-abcdef", Kind: "promote"}
	path, err := plan.Save(dir, p)
	if err != nil {
		t.Fatal(err)
	}
	if got, _, err := plan.Load(dir, path); err != nil || got.ID != p.ID {
		t.Fatalf("load by path: %+v %v", got, err)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261008T120000Z-abcdef", Kind: "promote"}
	if _, err := plan.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != p.ID+".json" {
		t.Fatalf("leftover temp files: %v", ents)
	}
}

// The plan file is local and unsigned, and its ID names a shadow ref: an ID
// that is not a plan ID is refused on load.
func TestLoadRejectsAPlanWithABadID(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "x.json")
	if err := os.WriteFile(path, []byte(`{"id":"../../heads/main"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := plan.Load(dir, path); contract.From(err) == nil || contract.From(err).Code != contract.BadFlag || contract.From(err).Field != "plan" {
		t.Fatalf("want BAD_FLAG on field plan, got %v", err)
	}
}
