package overlay_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
)

func TestFixedSetMatches(t *testing.T) {
	s := overlay.NewSet()
	cases := map[string]bool{
		".ghostbranch.yml":                       true,
		".ghostbranch/README.md":                 true,
		".ghostbranch/notes/deep/x.md":           true,
		".github/workflows/ghostbranch-sync.yml": true,
		".github/workflows/ghostbranch-a/b.yml":  false, // * stops at /
		".github/workflows/ci.yml":               false,
		"src/.ghostbranch.yml":                   false, // anchored at repo root
		".ghostbranch.yml.bak":                   false,
		"ghostbranch.yml":                        false,
		"docs/.ghostbranch/README.md":            false,
	}
	for path, want := range cases {
		if got := s.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestExtraPatterns(t *testing.T) {
	s := overlay.NewSet("internal-docs/**", "secret?.txt")
	for path, want := range map[string]bool{
		"internal-docs/a/b.md": true,
		"secret1.txt":          true,
		"secret12.txt":         false,
		"internal-docs.md":     false,
	} {
		if got := s.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
	if len(s.Patterns()) != len(overlay.Fixed)+2 {
		t.Fatalf("Patterns = %v", s.Patterns())
	}
}

func TestNonASCIIPatterns(t *testing.T) {
	s := overlay.NewSet("docs/café/**")
	cases := map[string]bool{
		"docs/café/notes.md": true,
		"docs/cafe/notes.md": false,
	}
	for path, want := range cases {
		if got := s.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDoubleStarMatchesZeroOrMoreDirectories(t *testing.T) {
	s := overlay.NewSet("**/NOTES.md", "docs/**/private.md", "**/*.key")
	cases := map[string]bool{
		"NOTES.md":                true,
		"a/NOTES.md":              true,
		"a/b/NOTES.md":            true,
		"NOTES.md.bak":            false,
		"a/NOTES.md.bak":          false,
		"xNOTES.md":               false,
		"docs/private.md":         true,
		"docs/x/private.md":       true,
		"docs/x/y/private.md":     true,
		"docsprivate.md":          false,
		"docs/xprivate.md":        false,
		"other/docs/private.md":   false,
		"id.key":                  true,
		"deep/dir/id.key":         true,
		".ghostbranch.yml":        true,
		".ghostbranch.yml.backup": false,
	}
	for path, want := range cases {
		if got := s.Match(path); got != want {
			t.Errorf("Match(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestDoubleStarEdgeForms(t *testing.T) {
	if !overlay.NewSet("**").Match("any/path/at/all.txt") {
		t.Error("bare ** should match everything")
	}
	s := overlay.NewSet()
	if s.Match(".ghostbranch.yml.x") || !s.Match(".ghostbranch/a/b") {
		t.Error(".ghostbranch/** semantics changed")
	}
	if overlay.NewSet(".ghostbranch/**").Match(".ghostbranchx/a") {
		t.Error("trailing /** must stay under its directory")
	}
	if !overlay.NewSet("a/**/b/**/c.txt").Match("a/b/c.txt") {
		t.Error("two inner /**/ should each match zero directories")
	}
}
