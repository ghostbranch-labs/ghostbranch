package engine

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// The waivers add summary is what a person reads before typing the code: a
// file name from a contributor or a reason from an agent must not drive
// the terminal (sub-project 3 leftover). Control characters, bidi marks
// and bytes that aren't UTF-8 show as escapes; the match was already
// %q-quoted.
func TestWaiveSummaryEscapesControlCharacters(t *testing.T) {
	t.Parallel()
	fs := []leak.Finding{
		{ID: "L1", Rule: "block-pattern", Path: "docs/\x1b[2Jnotes.md", Line: 3, Match: "acme-\x1b[31minternal"},
		{ID: "L2", Rule: "github-pat", Path: "a\u202etxt.go", Hint: "ghp_\u2066…\x07abcd"},
		{ID: "L3", Rule: "large-file", Path: "bin/\xffdata.bin"},
	}
	got := waiveSummary(workspace.Pair{Public: "acme/widget"}, "feat-x", fs, []string{"L1", "L2", "L3"},
		"fixture\u202e, not a secret\r\nApproved")
	for _, bad := range []string{"\x1b", "\x07", "\r", "\u202e", "\u2066", "\xff"} {
		if strings.Contains(got, bad) {
			t.Errorf("the summary holds a raw %q:\n%s", bad, got)
		}
	}
	for _, want := range []string{
		`docs/\u001b[2Jnotes.md:3`,
		`"acme-\x1b[31minternal"`,
		`a\u202etxt.go`,
		`ghp_\u2066…\u0007abcd`,
		`bin/\xffdata.bin`,
		`Reason: fixture\u202e, not a secret\u000d\u000aApproved`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the summary lacks %q:\n%s", want, got)
		}
	}
}
