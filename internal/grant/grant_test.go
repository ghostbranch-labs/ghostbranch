package grant_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

var t0 = time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

func store(t *testing.T) (*grant.Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "grants.json")
	return grant.NewStore(path, filepath.Join(dir, "grants.lock")), path
}

func mk(id, public, shadow string, d time.Duration) grant.Grant {
	exp := t0.Add(d)
	return grant.Grant{ID: id, Public: public, Shadow: shadow, CreatedAt: t0, ExpiresAt: &exp, CreatedBy: "human:octo"}
}

func TestParseFor(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 6 * time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "30d": 720 * time.Hour} {
		d, err := grant.ParseFor(in, false)
		if err != nil || *d != want {
			t.Errorf("%q: %v %v", in, d, err)
		}
	}
	if d, err := grant.ParseFor("", true); err != nil || d != nil {
		t.Errorf("until revoked: %v %v", d, err)
	}
	for _, c := range []struct {
		f string
		u bool
	}{{"7h", false}, {"1h", false}, {"forever", false}, {"6h", true}} {
		_, err := grant.ParseFor(c.f, c.u)
		if ce := contract.From(err); ce == nil || ce.Code != contract.BadFlag || ce.Field != "for" {
			t.Errorf("%+v: want BAD_FLAG on for, got %v", c, err)
		}
	}
}

func TestStoreLifecycle(t *testing.T) {
	s, _ := store(t)
	if _, ok, err := s.Valid("acme/widget", "acme/widget-shadow", t0); ok || err != nil {
		t.Fatalf("empty store: %v %v", ok, err)
	}
	if r, err := s.Put(mk("g-1", "acme/widget", "acme/widget-shadow", 6*time.Hour), t0); r != nil || err != nil {
		t.Fatalf("put: %v %v", r, err)
	}
	if got, ok, _ := s.Valid("Acme/Widget", "ACME/widget-shadow", t0.Add(time.Hour)); !ok || got.ID != "g-1" {
		t.Fatal("a grant matches its pair case-insensitively")
	}
	if _, ok, _ := s.Valid("acme/widget", "acme/other-shadow", t0); ok {
		t.Fatal("a grant is tied to both repos of the pair")
	}
	if _, ok, _ := s.Valid("acme/widget", "acme/widget-shadow", t0.Add(6*time.Hour)); ok {
		t.Fatal("a grant is invalid from its expiry on")
	}
	r, err := s.Put(mk("g-2", "acme/widget", "acme/widget-shadow", 24*time.Hour), t0)
	if err != nil || r == nil || r.ID != "g-1" {
		t.Fatalf("replace: %v %v", r, err)
	}
	if _, err := s.Put(mk("g-3", "acme/other", "acme/other-shadow", 6*time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if all, _ := s.Load(); len(all) != 2 {
		t.Fatalf("one grant per pair: %+v", all)
	}
	rev, err := s.Revoke("ACME/widget", false, t0)
	if err != nil || len(rev) != 1 || rev[0].ID != "g-2" {
		t.Fatalf("revoke: %+v %v", rev, err)
	}
	rev, err = s.Revoke("", true, t0)
	if err != nil || len(rev) != 1 || rev[0].ID != "g-3" {
		t.Fatalf("revoke --all: %+v %v", rev, err)
	}
}

func TestUntilRevokedAndPruning(t *testing.T) {
	s, _ := store(t)
	forever := grant.Grant{ID: "g-f", Public: "acme/a", Shadow: "acme/a-shadow", CreatedAt: t0, CreatedBy: "human:octo"}
	if _, err := s.Put(forever, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(mk("g-short", "acme/b", "acme/b-shadow", time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(365 * 24 * time.Hour)
	if _, ok, _ := s.Valid("acme/a", "acme/a-shadow", later); !ok {
		t.Fatal("--until-revoked never expires")
	}
	if _, err := s.Put(mk("g-c", "acme/c", "acme/c-shadow", time.Hour), later); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Load()
	for _, x := range all {
		if x.ID == "g-short" {
			t.Fatal("expired grants are pruned on the next write")
		}
	}
}

// Review Focus 2: a corrupt file fails closed, and revoke --all clears it.
func TestCorruptGrantsFile(t *testing.T) {
	s, path := store(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Valid("acme/widget", "acme/widget-shadow", t0); err == nil || ok {
		t.Fatalf("corrupt file must be an error, not 'no grant': %v %v", ok, err)
	}
	if _, err := s.Put(mk("g-1", "acme/widget", "acme/widget-shadow", time.Hour), t0); err == nil {
		t.Fatal("put must not overwrite a corrupt file silently")
	}
	if _, err := s.Revoke("acme/widget", false, t0); err == nil {
		t.Fatal("a single revoke must not overwrite a corrupt file silently")
	}
	if _, err := s.Revoke("", true, t0); err != nil {
		t.Fatalf("revoke --all must clear a corrupt file: %v", err)
	}
	if all, err := s.Load(); err != nil || len(all) != 0 {
		t.Fatalf("after clearing: %v %v", all, err)
	}
}

func TestFileIsPrivateAndAtomic(t *testing.T) {
	s, path := store(t)
	if _, err := s.Put(mk("g-1", "acme/widget", "acme/widget-shadow", time.Hour), t0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temp file must be renamed away")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode = %v", fi.Mode().Perm())
		}
	}
}

func TestLockedStore(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, "grants.lock")
	unlock, err := workspace.Lock(lock, time.Now(), "test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	s := grant.NewStore(filepath.Join(dir, "grants.json"), lock)
	_, err = s.Put(mk("g-1", "acme/widget", "acme/widget-shadow", time.Hour), time.Now())
	if ce := contract.From(err); ce == nil || ce.Code != contract.Locked {
		t.Fatalf("want LOCKED, got %v", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

func TestNewID(t *testing.T) {
	if id, err := grant.NewID(zeroReader{}); err != nil || id != "g-000000" {
		t.Fatalf("%q %v", id, err)
	}
}

// A grants file from a newer Ghostbranch is refused: never "no grant", and
// never overwritten, not even by revoke --all.
func TestNewerGrantsFile(t *testing.T) {
	t.Parallel()
	s, path := store(t)
	raw := []byte(`{"schema_version":"0.99","grants":[]}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := s.Load()
	if ce := contract.From(err); ce == nil || ce.Code != contract.UpgradeNeeded || ce.Details["record"] != "grants" ||
		ce.Details["record_version"] != "0.99" || ce.Details["supported"] != contract.SchemaVersion {
		t.Fatalf("want UPGRADE_NEEDED for grants, got %v", err)
	}
	upgrade := func(err error) bool { ce := contract.From(err); return ce != nil && ce.Code == contract.UpgradeNeeded }
	if _, ok, err := s.Valid("acme/widget", "acme/widget-shadow", t0); !upgrade(err) || ok {
		t.Fatalf("valid = %v %v", ok, err)
	}
	if _, err := s.Put(mk("g-1", "acme/widget", "acme/widget-shadow", time.Hour), t0); !upgrade(err) {
		t.Fatalf("put = %v", err)
	}
	if _, err := s.Revoke("", true, t0); !upgrade(err) {
		t.Fatalf("revoke --all = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != string(raw) {
		t.Fatalf("the newer file was changed: %s", got)
	}
}

// A well-formed file whose values don't decode (a bad time) is as corrupt as
// one that isn't JSON: revoke --all clears it. A file that can't be read, or
// one from a newer Ghostbranch, is never replaced.
func TestRevokeAllClearsAFileWithABadValue(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"bad created_at": `{"schema_version":"0.8","grants":[{"id":"g-1","created_at":"nope"}]}`,
		"bad expires_at": `{"schema_version":"0.8","grants":[{"id":"g-1","expires_at":"nope"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, path := store(t)
			if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Load(); err == nil || strings.Contains(err.Error(), "not valid JSON") || !strings.Contains(err.Error(), "can't read") {
				t.Fatalf("a valid file with a bad value: %v", err)
			}
			if _, err := s.Revoke("", true, t0); err != nil {
				t.Fatalf("revoke --all must clear it: %v", err)
			}
			if all, err := s.Load(); err != nil || len(all) != 0 {
				t.Fatalf("after clearing: %v %v", all, err)
			}
		})
	}
}

func TestRevokeAllKeepsAnUnreadableGrantsFile(t *testing.T) {
	t.Parallel()
	s, path := store(t)
	if err := os.Mkdir(path, 0o755); err != nil { // reading a folder fails, and isn't a decode error
		t.Fatal(err)
	}
	if _, err := s.Revoke("", true, t0); err == nil {
		t.Fatal("revoke --all must not replace a file it couldn't read")
	}
	if fi, err := os.Stat(path); err != nil || !fi.IsDir() {
		t.Fatalf("the unreadable path was replaced: %v %v", fi, err)
	}
}

func TestLoadSaysWhenAFileIsNotJSON(t *testing.T) {
	t.Parallel()
	s, path := store(t)
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load(); err == nil || !strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("Load = %v", err)
	}
}
