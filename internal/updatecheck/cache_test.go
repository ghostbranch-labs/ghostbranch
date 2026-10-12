package updatecheck_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func TestCacheRoundTripsAtomicallyAndPrivately(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "home", updatecheck.CacheFile)
	var c updatecheck.Cache
	rel := updatecheck.Release{Version: mustParse(t, "v0.13.1"), Tag: "v0.13.1", URL: updatecheck.ReleaseURL("v0.13.1")}
	c.Merge(updatecheck.Result{At: clock(), Latest: &rel, Advisories: []updatecheck.Advisory{{ID: "GHSA-2222-3333-4444", Range: "< 0.13.1", Patched: "0.13.1"}}}, mustParse(t, "v0.13.0"))
	c.Reminded(updatecheck.Person, clock())
	if err := updatecheck.Save(path, c); err != nil {
		t.Fatal(err)
	}
	got := updatecheck.Load(path)
	if !got.CheckedAt.Equal(clock()) || got.Latest == nil || got.Latest.Version != "0.13.1" || len(got.Advisories) != 1 ||
		got.BinaryVersion != "0.13.0" || !got.RemindedPersonAt.Equal(clock()) || !got.RemindedAgentAt.IsZero() || got.Format != 1 {
		t.Fatalf("loaded %+v", got)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Fatalf("mode %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// A cache, not a record: missing, damaged or newer-format files read as
// empty and are rewritten; nothing ever fails.
func TestCacheIgnoresWhatItCantUse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for name, body := range map[string]string{
		"damaged.json": "{not json",
		"newer.json":   `{"format": 2, "checked_at": "2026-10-11T12:00:00Z"}`,
		"none.json":    `{"checked_at": "2026-10-11T12:00:00Z"}`,
	} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := updatecheck.Load(p); !got.CheckedAt.IsZero() {
			t.Errorf("%s loaded %+v", name, got)
		}
	}
	if got := updatecheck.Load(filepath.Join(dir, "missing.json")); !got.CheckedAt.IsZero() {
		t.Fatalf("missing loaded %+v", got)
	}
}

// Review Focus 3: a path the cache can't be written to fails cleanly.
func TestSaveFailsCleanlyWhenThePathIsADirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), updatecheck.CacheFile)
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := updatecheck.Save(path, updatecheck.Cache{}); err == nil {
		t.Fatal("saving over a directory must fail")
	}
	if left, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "*.tmp")); len(left) != 0 {
		t.Fatalf("temp files left: %v", left)
	}
}

// R7: a failed look keeps what was known and records why; 404 forgets the
// release; a different binary's advisories are dropped.
func TestMerge(t *testing.T) {
	t.Parallel()
	cur := mustParse(t, "v0.13.0")
	rel := updatecheck.Release{Version: mustParse(t, "v0.13.1"), Tag: "v0.13.1"}
	var c updatecheck.Cache
	c.Merge(updatecheck.Result{At: clock(), Latest: &rel, Advisories: []updatecheck.Advisory{{ID: "GHSA-2222-3333-4444"}}}, cur)
	later := clock().Add(5 * time.Hour)
	c.Merge(updatecheck.Result{At: later, Err: errors.New("dial tcp: timeout")}, cur)
	if !c.CheckedAt.Equal(later) || c.LastError != "dial tcp: timeout" || c.Latest == nil || len(c.Advisories) != 1 {
		t.Fatalf("after a failure: %+v", c)
	}
	c.Merge(updatecheck.Result{At: later, Err: errors.New("x")}, mustParse(t, "v0.12.0"))
	if len(c.Advisories) != 0 || c.BinaryVersion != "0.12.0" {
		t.Fatalf("another binary kept advisories: %+v", c)
	}
	c.Merge(updatecheck.Result{At: later, Err: updatecheck.ErrNoRelease}, cur)
	if c.Latest != nil || c.LastError != "" {
		t.Fatalf("after 404: %+v", c)
	}
}
