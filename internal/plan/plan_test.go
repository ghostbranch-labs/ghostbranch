package plan_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
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
	if _, _, err := plan.Load(dir, "20261008T120000Z-abcdef"); !errors.Is(err, plan.ErrNotFound) || contract.From(err).Code != contract.PlanNotFound {
		t.Fatalf("missing plan: %v", err)
	}
	if _, _, err := plan.Load(dir, filepath.Join(dir, "missing.json")); !errors.Is(err, plan.ErrNotFound) || contract.From(err).Code != contract.BadFlag {
		t.Fatalf("a missing .json path stays BAD_FLAG: %v", err)
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

// A plan file from a newer Ghostbranch is refused, not half-read.
func TestLoadRefusesAPlanFromANewerGhostbranch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const id = "20261008T120000Z-abcdef"
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(`{"schema_version":"0.99","id":"`+id+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := plan.Load(dir, id)
	if ce := contract.From(err); ce == nil || ce.Code != contract.UpgradeNeeded || ce.Details["record"] != "plan" ||
		ce.Details["record_version"] != "0.99" || ce.Details["supported"] != contract.RecordVersion {
		t.Fatalf("want UPGRADE_NEEDED for the plan, got %v", err)
	}
}

func TestLoadTellsBadJSONFromABadValue(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, c := range map[string]struct{ raw, want, not string }{
		"not json":    {"{", "not valid JSON", "can't read"},
		"a bad value": {`{"id": 5}`, "can't read", "not valid JSON"},
	} {
		path := filepath.Join(dir, name+".json")
		if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := plan.Load(dir, path)
		if err == nil || !strings.Contains(err.Error(), c.want) || strings.Contains(err.Error(), c.not) {
			t.Errorf("%s: Load = %v", name, err)
		}
	}
}

// A plan from before 0.9 has no typed: both texts count as typed, as they
// did then. A plan that has it keeps it.
func TestLoadTreatsAMissingTypedAsTyped(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const id = "20261009T120000Z-abcdef"
	path := filepath.Join(dir, id+".json")
	for _, c := range []struct {
		raw  string
		want plan.Typed
	}{
		{`{"schema_version":"0.8","id":"` + id + `"}`, plan.Typed{Title: true, Body: true}},
		{`{"schema_version":"0.9","id":"` + id + `","typed":{"title":true,"body":false}}`, plan.Typed{Title: true}},
	} {
		if err := os.WriteFile(path, []byte(c.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		p, _, err := plan.Load(dir, id)
		if err != nil || p.Typed != c.want {
			t.Errorf("%s: typed = %+v %v, want %+v", c.raw, p.Typed, err, c.want)
		}
	}
}

// Plans name repos, branches and commits, so the file is private (the guide
// says so). The mode comes from os.CreateTemp; this pins it.
func TestSaveWritesAPrivateFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix file modes")
	}
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261005T123000Z-abcdef", Kind: "promote", Feature: "x",
		CreatedAt: time.Date(2026, 10, 5, 12, 30, 0, 0, time.UTC), Leak: plan.Leak{Findings: []map[string]any{}}}
	path, err := plan.Save(t.TempDir(), p)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v %v, want 0600", fi.Mode().Perm(), err)
	}
}

func TestAStackPlanRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	n := 4
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261010T120000Z-abcdef", Kind: "promote", Feature: "a",
		Chain:       []plan.Entry{{Feature: "a", Mode: "squash"}, {Feature: "b", Mode: "preserve"}},
		PRs:         []plan.StackPR{{Feature: "a", Base: "main", Number: 1, URL: "u1"}, {Feature: "b", Base: "a", Number: 2, URL: "u2"}},
		StackNumber: &n}
	if _, err := plan.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	got, _, err := plan.Load(dir, p.ID)
	if err != nil || len(got.Chain) != 2 || got.Chain[1].Feature != "b" || got.PRs[1].Base != "a" || got.StackNumber == nil || *got.StackNumber != 4 {
		t.Fatalf("loaded %+v %v", got, err)
	}
}

// An update plan keeps how it updates the PR; a first publish writes none
// of it, so it reads the same as before 0.12.
func TestAnUpdatePlanRoundTrips(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	head := strings.Repeat("a", 40)
	p := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261010T120000Z-abcdef", Kind: "promote", Feature: "a",
		Publish: plan.Publish{Update: true, Style: plan.StyleReplace, ReplaceReason: plan.ReasonRestack, ExpectedHead: head, Restores: []string{head}},
		PRs:     []plan.StackPR{{Feature: "a", Base: "main", Number: 14, URL: "u", Update: true, Style: plan.StyleReplace}}}
	if _, err := plan.Save(dir, p); err != nil {
		t.Fatal(err)
	}
	got, _, err := plan.Load(dir, p.ID)
	if err != nil || !got.Publish.Update || got.Publish.Style != "replace" || got.Publish.ReplaceReason != "restack" ||
		got.Publish.ExpectedHead != head || len(got.Publish.Restores) != 1 || !got.PRs[0].Update || got.PRs[0].Style != "replace" {
		t.Fatalf("loaded %+v %+v %v", got.Publish, got.PRs, err)
	}
	first := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: "20261010T120001Z-abcdef", Kind: "promote", Feature: "b",
		PRs: []plan.StackPR{{Feature: "b", Base: "main"}}}
	path, err := plan.Save(dir, first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"update"`, `"style"`, `"replace_reason"`, `"expected_head"`, `"restores"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("a first publish wrote %s:\n%s", k, b)
		}
	}
}
