package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// WorktreeAdd checks out rev, detached, into a new worktree at dir and
// returns a Git for it.
func (g *Git) WorktreeAdd(ctx context.Context, dir, rev string) (*Git, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, err
	}
	if _, err := g.Run(ctx, "worktree", "add", "--detach", "--force", dir, rev); err != nil {
		return nil, err
	}
	return g.With(dir), nil
}

// WorktreeRemove deletes the worktree at dir and prunes its metadata.
func (g *Git) WorktreeRemove(ctx context.Context, dir string) error {
	_, err := g.Run(ctx, "worktree", "remove", "--force", dir)
	var rmErr error
	if err != nil {
		rmErr = os.RemoveAll(dir)
	}
	_, perr := g.Run(ctx, "worktree", "prune")
	if err == nil {
		return nil
	}
	if _, serr := os.Stat(dir); serr == nil {
		if rmErr == nil {
			rmErr = fmt.Errorf("directory %s still exists", dir)
		}
		return fmt.Errorf("remove worktree %s: %w", dir, errors.Join(err, rmErr))
	}
	if perr != nil {
		return perr
	}
	return nil
}

// ConflictError means a rebase or cherry-pick replay stopped on conflicts. It
// has been aborted; Files lists the conflicting paths.
type ConflictError struct {
	Files []string
	// Commit is the commit that stopped a cherry-pick replay; empty for a rebase.
	Commit string
}

func (e *ConflictError) Error() string {
	s := "conflict in " + strings.Join(e.Files, ", ")
	if e.Commit != "" {
		s += " applying " + e.Commit
	}
	return s
}
