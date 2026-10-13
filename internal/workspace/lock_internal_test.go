package workspace

import "testing"

// A holder that can't be read in full is left out of the message.
func TestLockedMessageWithoutAHolder(t *testing.T) {
	t.Parallel()
	if got := lockedMessage("/s/p.lock", map[string]any{}); got != "another ghostbranch operation holds the lock /s/p.lock" {
		t.Fatalf("%q", got)
	}
	if got := lockedMessage("/s/p.lock", map[string]any{"pid": 7, "since": "2026-10-08T10:00:00Z"}); got != "another ghostbranch operation holds the lock /s/p.lock" {
		t.Fatalf("an old-format holder with no op: %q", got)
	}
}
