package updatecheck_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func TestNoticeTexts(t *testing.T) {
	t.Parallel()
	ordinary := updatecheck.Notice{Current: "0.12.0", Latest: "0.13.0", URL: "u", Upgrade: "brew upgrade ghostbranch"}
	if m, a, b := ordinary.Lines(); m != "!" || a != "Ghostbranch 0.13.0 is out (you have 0.12.0)." || b != "Upgrade: brew upgrade ghostbranch" {
		t.Fatalf("%q %q %q", m, a, b)
	}
	sec := updatecheck.Notice{Current: "0.13.0", Latest: "0.13.1", URL: "u", Security: true, Advisories: []string{"GHSA-2222-3333-4444"}, Upgrade: "scoop update ghostbranch"}
	if m, a, b := sec.Lines(); m != "✗" || a != "Ghostbranch 0.13.1 fixes a security issue (GHSA-2222-3333-4444); you have 0.13.0." || b != "Upgrade now: scoop update ghostbranch" {
		t.Fatalf("%q %q %q", m, a, b)
	}
	sec.Advisories = append(sec.Advisories, "GHSA-5555-6666-7777", "GHSA-8888-9999-cccc")
	if _, a, _ := sec.Lines(); a != "Ghostbranch 0.13.1 fixes security issues (GHSA-2222-3333-4444 and 2 more); you have 0.13.0." {
		t.Fatalf("%q", a)
	}
	w := sec.Warning()
	if w.Code != contract.UpdateAvailable ||
		w.Message != "Ghostbranch 0.13.1 is out (this is 0.13.0); it fixes security issues (GHSA-2222-3333-4444 and 2 more)" {
		t.Fatalf("%+v", w)
	}
	want := map[string]any{"current": "0.13.0", "latest": "0.13.1", "url": "u", "security": true,
		"advisories": []string{"GHSA-2222-3333-4444", "GHSA-5555-6666-7777", "GHSA-8888-9999-cccc"}, "upgrade": "scoop update ghostbranch"}
	if !reflect.DeepEqual(w.Details, want) {
		t.Fatalf("details %v", w.Details)
	}
	if d := ordinary.Warning().Details; !reflect.DeepEqual(d["advisories"], []string{}) || d["security"] != false {
		t.Fatalf("ordinary details %v", d)
	}
}

func TestStatusLines(t *testing.T) {
	t.Parallel()
	now := clock()
	cur := mustParse(t, "v0.12.0")
	for _, c := range []struct {
		name  string
		off   string
		cache updatecheck.Cache
		want  string
	}{
		{"off", "GHOSTBRANCH_NO_UPDATE_CHECK", updatecheck.Cache{}, "update check: off (GHOSTBRANCH_NO_UPDATE_CHECK)"},
		{"dev", "development build", updatecheck.Cache{}, "update check: off (development build)"},
		{"no release", "", cacheWith(t, "0.12.0", "", now.Add(-3*time.Hour)), "update check: on · no release published yet"},
		{"failed", "", updatecheck.Cache{CheckedAt: now, LastError: "GitHub rate limit (HTTP 403)", BinaryVersion: "0.12.0"}, "update check: on · last look failed: GitHub rate limit (HTTP 403)"},
		{"newer", "", cacheWith(t, "0.12.0", "0.13.0", now.Add(-3*time.Hour)), "update check: on · last looked 3h ago · 0.13.0 available (you have 0.12.0)"},
		{"current", "", cacheWith(t, "0.12.0", "0.12.0", now), "update check: on · last looked just now · up to date"},
		{"security", "", cacheWith(t, "0.12.0", "0.13.1", now, "GHSA-2222-3333-4444"), "update check: on · last looked just now · 0.13.1 available (you have 0.12.0) · fixes GHSA-2222-3333-4444"},
	} {
		s := updatecheck.StatusOf(c.off, cur, c.cache, "", "brew upgrade ghostbranch", now)
		if got := s.Line(); got != c.want {
			t.Errorf("%s: %q\nwant %q", c.name, got, c.want)
		}
	}
	s := updatecheck.StatusOf("", cur, cacheWith(t, "0.12.0", "0.13.0", now), "http://127.0.0.1:8765", "u", now)
	if got := s.Line(); got != "update check: on · last looked just now · 0.13.0 available (you have 0.12.0) · GHOSTBRANCH_UPDATE_URL=http://127.0.0.1:8765 (test-only)" ||
		!s.Enabled || s.Override != "http://127.0.0.1:8765" || s.Current != "0.12.0" || s.Latest != "0.13.0" || s.Advisories == nil {
		t.Fatalf("%q %+v", got, s)
	}
}

// F13: doctor prints nothing from the cache file as is: the latest version
// must parse, and the last error is cleaned.
func TestStatusOfReparsesAndCleans(t *testing.T) {
	t.Parallel()
	now := clock()
	cur := mustParse(t, "v0.12.0")
	c := updatecheck.Cache{CheckedAt: now, BinaryVersion: "0.12.0", LastError: "dial\x1b[2J tcp\u202e: timeout\n",
		Latest: &updatecheck.CachedRelease{Version: "0.13.0\x1b[2J"}}
	s := updatecheck.StatusOf("", cur, c, "", "u", now)
	if s.Latest != "" || s.Error != "dial[2J tcp: timeout" {
		t.Fatalf("%+v", s)
	}
	if got := s.Line(); got != "update check: on · last look failed: dial[2J tcp: timeout" {
		t.Fatalf("%q", got)
	}
	c.LastError, c.Latest.Version = "", "v0.13.0"
	if s := updatecheck.StatusOf("", cur, c, "", "u", now); s.Latest != "0.13.0" {
		t.Fatalf("%+v", s)
	}
}
