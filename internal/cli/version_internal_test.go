package cli

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

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

// 6.4b-1: GoReleaser's snapshot stamp ("{{ incpatch .Version }}-devel+{{
// .ShortCommit }}") reaches version unchanged and never looks for updates.
// Not parallel: it sets a package variable.
func TestSnapshotStampIsADevelopmentBuild(t *testing.T) {
	old := stampedVersion
	t.Cleanup(func() { stampedVersion = old })
	stampedVersion = "0.0.1-devel+abc1234"
	v, _ := buildInfo()
	if v != "0.0.1-devel+abc1234" {
		t.Fatalf("version %q", v)
	}
	if got := updatecheck.OffReason(func(string) string { return "" }, v); got != "development build" {
		t.Fatalf("OffReason %q, want development build", got)
	}
}
