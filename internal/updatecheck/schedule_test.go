package updatecheck_test

import (
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func cacheWith(t *testing.T, binary, latest string, at time.Time, advisories ...string) updatecheck.Cache {
	t.Helper()
	c := updatecheck.Cache{CheckedAt: at, BinaryVersion: binary}
	if latest != "" {
		c.Latest = &updatecheck.CachedRelease{Version: latest, URL: "https://evil.example/"}
	}
	for _, a := range advisories {
		c.Advisories = append(c.Advisories, updatecheck.Advisory{ID: a})
	}
	return c
}

func TestDue(t *testing.T) {
	t.Parallel()
	now := clock()
	below, above := mustParse(t, "v0.13.0"), mustParse(t, "v1.2.0")
	for _, c := range []struct {
		name  string
		cache updatecheck.Cache
		cur   updatecheck.Version
		want  bool
	}{
		{"never looked", updatecheck.Cache{}, below, true},
		{"below 1.0, 3h", cacheWith(t, "0.13.0", "", now.Add(-3*time.Hour)), below, false},
		{"below 1.0, 4h", cacheWith(t, "0.13.0", "", now.Add(-4*time.Hour)), below, true},
		{"1.0+, 23h", cacheWith(t, "1.2.0", "", now.Add(-23*time.Hour)), above, false},
		{"1.0+, 24h", cacheWith(t, "1.2.0", "", now.Add(-24*time.Hour)), above, true},
		{"in the future", cacheWith(t, "0.13.0", "", now.Add(time.Hour)), below, true},
		{"another binary", cacheWith(t, "0.12.0", "", now.Add(-time.Minute)), below, true},
	} {
		if got := updatecheck.Due(c.cache, c.cur, now); got != c.want {
			t.Errorf("%s: Due = %v", c.name, got)
		}
	}
}

func TestNoticeForFollowsTheSchedule(t *testing.T) {
	t.Parallel()
	now := clock()
	for _, c := range []struct {
		name     string
		cur      string
		cache    updatecheck.Cache
		reminded time.Duration // ago; 0 means never
		aud      updatecheck.Audience
		want     bool
	}{
		{"same version", "v0.13.0", cacheWith(t, "0.13.0", "0.13.0", now), 0, updatecheck.Person, false},
		{"no release", "v0.13.0", cacheWith(t, "0.13.0", "", now), 0, updatecheck.Person, false},
		{"below 1.0 reminds every command", "v0.13.0", cacheWith(t, "0.13.0", "0.14.0", now), time.Minute, updatecheck.Person, true},
		{"below 1.0, agents too", "v0.13.0", cacheWith(t, "0.13.0", "0.14.0", now), time.Minute, updatecheck.Agent, true},
		{"1.0+, first time", "v1.2.0", cacheWith(t, "1.2.0", "1.3.0", now), 0, updatecheck.Person, true},
		{"1.0+, reminded 1h ago", "v1.2.0", cacheWith(t, "1.2.0", "1.3.0", now), time.Hour, updatecheck.Person, false},
		{"1.0+, reminded 24h ago", "v1.2.0", cacheWith(t, "1.2.0", "1.3.0", now), 24 * time.Hour, updatecheck.Agent, true},
		{"1.0+, reminded in the future", "v1.2.0", cacheWith(t, "1.2.0", "1.3.0", now), -time.Hour, updatecheck.Person, true},
		{"1.0+, security every command", "v1.2.0", cacheWith(t, "1.2.0", "1.2.1", now, "GHSA-2222-3333-4444"), time.Minute, updatecheck.Agent, true},
		{"another binary's advisories don't count", "v1.2.0", cacheWith(t, "1.1.0", "1.2.1", now, "GHSA-2222-3333-4444"), time.Minute, updatecheck.Person, false},
	} {
		cache := c.cache
		if c.reminded != 0 {
			cache.RemindedPersonAt = now.Add(-c.reminded)
			cache.RemindedAgentAt = now.Add(-c.reminded)
		}
		n := updatecheck.NoticeFor(cache, mustParse(t, c.cur), c.aud, now)
		if (n != nil) != c.want {
			t.Errorf("%s: notice %+v", c.name, n)
		}
	}
}

// Review Focus 1: the notice's URL is built from the version, never taken
// from the cache, and advisory IDs are cleaned.
func TestNoticeRebuildsTheURLAndCleansIDs(t *testing.T) {
	t.Parallel()
	c := cacheWith(t, "0.13.0", "0.13.1", clock(), "GHSA-2222\x1b[2J-3333-4444")
	n := updatecheck.NoticeFor(c, mustParse(t, "v0.13.0"), updatecheck.Person, clock())
	if n == nil || n.URL != "https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.1" || !n.Security ||
		n.Advisories[0] != "GHSA-2222[2J-3333-4444" {
		t.Fatalf("notice %+v", n)
	}
}

func TestOffReason(t *testing.T) {
	t.Parallel()
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		name, version string
		getenv        func(string) string
		want          string
	}{
		{"release", "v0.13.0", env(), ""},
		{"opt out", "v0.13.0", env("GHOSTBRANCH_NO_UPDATE_CHECK", "1"), "GHOSTBRANCH_NO_UPDATE_CHECK"},
		{"devel", "(devel)", env(), "development build"},
		{"unknown", "(unknown)", env(), "development build"},
		{"pseudo", "v0.0.0-20261011120000-abcdef123456", env(), "development build"},
		{"dirty", "v0.13.0+dirty", env(), "development build"},
		{"goreleaser snapshot", "0.0.1-devel+abc1234", env(), "development build"},
		{"release candidate", "v0.1.0-rc.1", env(), ""},
	} {
		if got := updatecheck.OffReason(c.getenv, c.version); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	for _, k := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "CIRCLECI", "JENKINS_URL", "TF_BUILD"} {
		if got := updatecheck.OffReason(env(k, "true"), "v0.13.0"); got != "CI" {
			t.Errorf("%s: %q", k, got)
		}
	}
}

func TestBaseURL(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]string{"": "https://api.github.com", "http://127.0.0.1:8765": "http://127.0.0.1:8765", "http://127.0.0.1:8765/": "http://127.0.0.1:8765"} {
		base, override, err := updatecheck.BaseURL(func(string) string { return v })
		if err != nil || base != want || (override != "") != (v != "") {
			t.Errorf("%q: %q %q %v", v, base, override, err)
		}
	}
	for _, bad := range []string{"ftp://x", "127.0.0.1:8765", "http://", "not a url"} {
		if _, _, err := updatecheck.BaseURL(func(string) string { return bad }); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
