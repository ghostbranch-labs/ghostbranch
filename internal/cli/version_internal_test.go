package cli

import "testing"

// R10: a link-time stamp wins over Go's recorded version. Not parallel: it
// sets a package variable.
func TestStampedVersionWins(t *testing.T) {
	old := stampedVersion
	t.Cleanup(func() { stampedVersion = old })
	stampedVersion = "v0.12.0"
	if v, _ := buildInfo(); v != "v0.12.0" {
		t.Fatalf("version %q", v)
	}
}
