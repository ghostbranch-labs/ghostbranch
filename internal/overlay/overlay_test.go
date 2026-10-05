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
