package updatecheck_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

func mustParse(t *testing.T, s string) updatecheck.Version {
	t.Helper()
	v, ok := updatecheck.ParseRelease(s)
	if !ok {
		t.Fatalf("%q is not a release", s)
	}
	return v
}

func TestParseRelease(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"v0.12.0": true, "0.12.0": true, "v1.0.0-beta.1": true, "v10.20.30": true, "v1.0.0-rc.1": true,
		"(devel)": false, "(unknown)": false, "": false, "v1.2": false, "v01.2.3": false,
		"v0.0.0-20261011120000-abcdef123456":        false, // pseudo-version
		"v0.12.1-0.20261011120000-abcdef123456":     false, // pseudo-version after a tag
		"v1.2.3-beta.0.20261011120000-abcdef123456": false, // pseudo-version after a pre-release tag
		"v0.12.0+dirty": false, "v0.12.0-beta+meta": false, "v0.12.0\x1b[2J": false, " v0.12.0": false,
	} {
		if _, got := updatecheck.ParseRelease(s); got != want {
			t.Errorf("ParseRelease(%q) = %v, want %v", s, got, want)
		}
	}
	if v := mustParse(t, "v1.2.3-beta.1"); v.String() != "1.2.3-beta.1" || v.Major != 1 || v.Pre != "beta.1" {
		t.Fatalf("parsed %+v", v)
	}
}

func TestCompareFollowsSemverPrecedence(t *testing.T) {
	t.Parallel()
	order := []string{"v0.9.9", "v0.12.0", "v0.12.1", "v0.13.0-alpha", "v0.13.0-alpha.1", "v0.13.0-alpha.beta",
		"v0.13.0-beta", "v0.13.0-beta.2", "v0.13.0-beta.11", "v0.13.0-rc.1", "v0.13.0", "v1.0.0-rc.1", "v1.0.0", "v2.0.0"}
	for i := range order {
		for j := range order {
			got := updatecheck.Compare(mustParse(t, order[i]), mustParse(t, order[j]))
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", order[i], order[j], got, want)
			}
		}
	}
}

// R8: strict until 1.0 includes 1.0's pre-releases.
func TestBelowOne(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{"v0.99.0": true, "v1.0.0-rc.1": true, "v1.0.0": false, "v1.2.0": false} {
		if got := mustParse(t, s).BelowOne(); got != want {
			t.Errorf("%s below 1.0: %v", s, got)
		}
	}
}

func TestAffected(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		rng, v       string
		affected, ok bool
	}{
		{"< 0.13.1", "v0.13.0", true, true},
		{"< 0.13.1", "v0.13.1", false, true},
		{">= 0.10.0, < 0.13.1", "v0.9.0", false, true},
		{">= 0.10.0, < 0.13.1", "v0.12.0", true, true},
		{"<= 0.12.0", "v0.12.0", true, true},
		{"> 0.12.0", "v0.12.0", false, true},
		{"= 0.12.0", "v0.12.0", true, true},
		{"<0.13.1", "v0.13.0", true, true},
		{"< v0.13.1", "v0.13.0", true, true},
		{"", "v0.13.0", false, false},
		{"0.13.1", "v0.13.0", false, false},
		{"< 0.13", "v0.12.0", false, false},
		{"~> 0.13.1", "v0.13.0", false, false},
		{"< 0.13.1,", "v0.13.0", false, false},
	} {
		a, ok := updatecheck.Affected(c.rng, mustParse(t, c.v))
		if a != c.affected || ok != c.ok {
			t.Errorf("Affected(%q, %s) = %v %v, want %v %v", c.rng, c.v, a, ok, c.affected, c.ok)
		}
	}
}
