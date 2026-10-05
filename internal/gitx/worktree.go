package gitx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// WorktreeAdd checks out rev, detached, into a new worktree at dir and
// returns a Git for it.
func (g *Git) WorktreeAdd(ctx context.Context, dir, rev string) (*Git, error) {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
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

// ConflictError means a rebase stopped on conflicts. The rebase has been
// aborted; Files lists the conflicting paths.
type ConflictError struct {
	Files []string
}

func (e *ConflictError) Error() string {
	return "rebase conflict in " + strings.Join(e.Files, ", ")
}

// Rebase rebases this worktree's detached HEAD onto onto, committing as
// committer. On conflict it aborts and returns *ConflictError.
func (g *Git) Rebase(ctx context.Context, onto string, committer Identity) error {
	rg := g.With(g.Dir)
	rg.Env = append(rg.Env, committer.env("GIT_COMMITTER")...)
	_, err := rg.Run(ctx, "rebase", "--quiet", onto)
	if err == nil {
		return nil
	}
	out, derr := rg.runRaw(ctx, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if _, aerr := rg.Run(ctx, "rebase", "--abort"); aerr != nil {
		return fmt.Errorf("rebase failed and abort failed, worktree may be mid-rebase: %w", errors.Join(err, aerr))
	}
	files := splitNUL(out)
	if derr == nil && len(files) > 0 {
		sort.Strings(files)
		return &ConflictError{Files: files}
	}
	return err
}
