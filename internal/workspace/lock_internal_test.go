package workspace

import (
	"strings"
	"testing"
)

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

// Review M8: the holder's text comes from a file, so it is shown only when
// every part looks as Ghostbranch writes it.
func TestHolderTextChecksEachPart(t *testing.T) {
	t.Parallel()
	good := map[string]any{"op": "feature pull", "pid": 42, "since": "2026-10-13T07:58:14Z"}
	if got := HolderText(good); got != "feature pull, pid 42, since 2026-10-13T07:58:14Z" {
		t.Fatalf("good holder: %q", got)
	}
	for name, h := range map[string]map[string]any{
		"escape in op":     {"op": "gate\x1b[2J", "pid": 42, "since": "2026-10-13T07:58:14Z"},
		"newline in op":    {"op": "gate\nrm -rf", "pid": 42, "since": "2026-10-13T07:58:14Z"},
		"long op":          {"op": strings.Repeat("a", 42), "pid": 42, "since": "2026-10-13T07:58:14Z"},
		"upper-case op":    {"op": "Gate", "pid": 42, "since": "2026-10-13T07:58:14Z"},
		"negative pid":     {"op": "gate", "pid": -1, "since": "2026-10-13T07:58:14Z"},
		"since not a time": {"op": "gate", "pid": 42, "since": "yesterday\x07"},
		"pid not a number": {"op": "gate", "pid": "42", "since": "2026-10-13T07:58:14Z"},
	} {
		if got := HolderText(h); got != "" {
			t.Errorf("%s: %q", name, got)
		}
		if got := lockedMessage("/s/p.lock", h); got != "another ghostbranch operation holds the lock /s/p.lock" {
			t.Errorf("%s: message %q", name, got)
		}
	}
}
