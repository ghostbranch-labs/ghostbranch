package engine

import (
	"context"
	"runtime"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// tightenState makes the state directories private on a writing command,
// for installs made before 6.3b (F-28, decision 4). It never fails the
// command: a dim note for each directory it changed, a warning note for a
// chmod that failed, and doctor keeps warning about what stays open. Dry
// runs, and so doctor, never tighten.
func (e *Engine) tightenState() {
	if e.DryRun || e.WS == nil {
		return
	}
	e.noteTightened(e.WS.Tighten())
}

// noteTightened prints Tighten's findings; a failed chmod is noted once per
// run, however many writes the command makes.
func (e *Engine) noteTightened(ts []workspace.Tightened) {
	for _, t := range ts {
		if t.Err == nil {
			e.notef(progress.Info, "Made %s private (0700)", t.Dir)
			continue
		}
		if e.tightenFailed[t.Dir] {
			continue
		}
		if e.tightenFailed == nil {
			e.tightenFailed = map[string]bool{}
		}
		e.tightenFailed[t.Dir] = true
		e.notef(progress.Warn, "couldn't make %s private (%v); other users on this machine may be able to read it: run chmod 700 %s", t.Dir, t.Err, t.Dir)
	}
}

// privateMirror makes a mirror made before 6.3b write its next files 0600
// (core.sharedRepository), as gitx.Init sets for a new one. Windows has no
// mode bits, so it skips the extra git calls there.
func (e *Engine) privateMirror(ctx context.Context, g *gitx.Git) {
	if runtime.GOOS == "windows" {
		return
	}
	if v, err := g.Run(ctx, "config", "--get", "core.sharedRepository"); err == nil && v == "0600" {
		return
	}
	if _, err := g.Run(ctx, "config", "core.sharedRepository", "0600"); err != nil {
		e.notef(progress.Warn, "couldn't make the local mirror %s write private files: %v", g.Dir, err)
		return
	}
	e.notef(progress.Info, "Made the local mirror %s write private files (core.sharedRepository 0600)", g.Dir)
}
