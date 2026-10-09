package gitx

import (
	"strings"
	"testing"
)

func TestIsObjectID(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		strings.Repeat("a", 40): true, strings.Repeat("0", 64): true,
		strings.Repeat("g", 40): false, strings.Repeat("A", 40): false,
		strings.Repeat("a", 39): false, "": false,
		"fatal: unable to read " + strings.Repeat("a", 18): false, // 40 characters, not hex
	} {
		if got := isObjectID(s); got != want {
			t.Errorf("isObjectID(%q) = %v, want %v", s, got, want)
		}
	}
}
