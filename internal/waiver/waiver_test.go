package waiver_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

var now = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*waiver.Store, string) {
	t.Helper()
	dir := t.TempDir()
	return waiver.NewStore(filepath.Join(dir, "waivers.json"), filepath.Join(dir, "waivers.lock")), dir
}

func mk(public, id, reason string) waiver.Waiver {
	return waiver.Waiver{ID: id, Public: public, Rule: "github-pat", Where: "tree", Path: "test/a.env",
		MatchSHA256: strings.Repeat("ab", 32), Reason: reason, CreatedAt: now, CreatedBy: "human:octo"}
}

func TestStoreLifecycle(t *testing.T) {
	s, _ := newStore(t)
	if ws, err := s.Load(); err != nil || ws != nil {
		t.Fatalf("missing file: %v %v", ws, err)
	}
	if err := s.Add([]waiver.Waiver{mk("acme/widget", "f-000000000001", "fixture"), mk("other/repo", "f-000000000002", "doc")}, now); err != nil {
		t.Fatal(err)
	}
	got, err := s.For("ACME/widget")
	if err != nil || len(got) != 1 || got["f-000000000001"].Reason != "fixture" {
		t.Fatalf("For = %v %v", got, err)
	}
	if err := s.Add([]waiver.Waiver{mk("acme/widget", "f-000000000001", "renamed fixture")}, now); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Load()
	if len(all) != 2 {
		t.Fatalf("re-adding the same id must replace it: %+v", all)
	}
	removed, err := s.Revoke("acme/widget", []string{"f-000000000001"}, false, now)
	if err != nil || len(removed) != 1 || removed[0].Reason != "renamed fixture" {
		t.Fatalf("Revoke = %+v %v", removed, err)
	}
	if left, _ := s.For("acme/widget"); len(left) != 0 {
		t.Fatalf("still waived: %+v", left)
	}
	if left, _ := s.For("other/repo"); len(left) != 1 {
		t.Fatal("revoking one pair's waiver touched another pair")
	}
}

// Review Focus 5.
func TestCorruptWaiversFile(t *testing.T) {
	s, _ := newStore(t)
	if err := os.WriteFile(s.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "waivers revoke --all") {
		t.Fatalf("Load = %v", err)
	}
	if _, err := s.For("acme/widget"); err == nil {
		t.Fatal("For must fail on a corrupt file, never return no waivers")
	}
	if err := s.Add([]waiver.Waiver{mk("acme/widget", "f-000000000001", "x")}, now); err == nil {
		t.Fatal("Add must not overwrite a corrupt file")
	}
	if _, err := s.Revoke("acme/widget", []string{"f-000000000001"}, false, now); err == nil {
		t.Fatal("a scoped revoke must not overwrite a corrupt file")
	}
	if removed, err := s.Revoke("", nil, true, now); err != nil || len(removed) != 0 {
		t.Fatalf("revoke --all must clear it: %v %v", removed, err)
	}
	if ws, err := s.Load(); err != nil || len(ws) != 0 {
		t.Fatalf("after revoke --all: %v %v", ws, err)
	}
}

func TestFileIsPrivateAtomicAndHoldsNoSecret(t *testing.T) {
	s, dir := newStore(t)
	if err := s.Add([]waiver.Waiver{mk("acme/widget", "f-000000000001", "fixture")}, now); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v, want 0600", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(dir, "waivers.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("a temp file was left behind")
	}
	var raw struct {
		SchemaVersion string           `json:"schema_version"`
		Waivers       []map[string]any `json:"waivers"`
	}
	b, _ := os.ReadFile(s.Path())
	if err := json.Unmarshal(b, &raw); err != nil || raw.SchemaVersion != "0.6" || len(raw.Waivers) != 1 {
		t.Fatalf("%s %v", b, err)
	}
	var keys []string
	for k := range raw.Waivers[0] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	want := []string{"created_at", "created_by", "id", "match_sha256", "path", "public", "reason", "rule", "where"}
	if !reflect.DeepEqual(keys, want) {
		t.Fatalf("stored keys %v, want %v", keys, want)
	}
}

func TestLockedStore(t *testing.T) {
	s, dir := newStore(t)
	unlock, err := workspace.Lock(filepath.Join(dir, "waivers.lock"), now)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	err = s.Add([]waiver.Waiver{mk("acme/widget", "f-000000000001", "x")}, now)
	if ce := contract.From(err); ce == nil || ce.Code != contract.Locked {
		t.Fatalf("want LOCKED, got %v", err)
	}
}

func TestMatches(t *testing.T) {
	w := mk("acme/widget", "f-000000000001", "x")
	cases := []struct {
		public string
		ids    []string
		all    bool
		want   bool
	}{
		{"acme/widget", []string{"f-000000000001"}, false, true},
		{"ACME/Widget", []string{"f-000000000001"}, false, true},
		{"acme/widget", []string{"f-000000000002"}, false, false},
		{"other/repo", []string{"f-000000000001"}, false, false},
		{"", nil, true, true},
	}
	for _, c := range cases {
		if got := waiver.Matches(w, c.public, c.ids, c.all); got != c.want {
			t.Errorf("Matches(%q, %v, %v) = %v", c.public, c.ids, c.all, got)
		}
	}
}
