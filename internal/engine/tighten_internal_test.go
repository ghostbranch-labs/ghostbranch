package engine

import (
	"errors"
	"slices"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// A chmod that fails is a warning note, once per run, never an error.
func TestTightenFailureIsANote(t *testing.T) {
	t.Parallel()
	rec := &progress.Recorder{}
	e := &Engine{Progress: rec}
	ts := []workspace.Tightened{{Dir: "/state/workspaces"}, {Dir: "/state/workspaces/default", Err: errors.New("operation not permitted")}}
	e.noteTightened(ts)
	e.noteTightened(ts[1:]) // a second write in the same run
	want := []string{
		"Made /state/workspaces private (0700)",
		"couldn't make /state/workspaces/default private (operation not permitted); other users on this machine may be able to read it: run chmod 700 /state/workspaces/default",
	}
	if got := rec.Notes(); !slices.Equal(got, want) {
		t.Fatalf("notes = %q, want %q", got, want)
	}
}
