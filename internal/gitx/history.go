package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceCommits lists the commits replaying head onto base applies, oldest
// first: those in head but not base, without merges, and without commits
// whose patch base already has (as git rebase leaves them out).
func (g *Git) SourceCommits(ctx context.Context, base, head string) ([]string, error) {
	out, err := g.Run(ctx, "rev-list", "--reverse", "--topo-order", "--no-merges", "--cherry-pick", "--right-only", base+"..."+head)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// CherryPick applies commits, in order, onto this worktree's HEAD, keeping
// each commit's author and message and committing as committer. A commit
// that becomes empty is kept. It returns the new SHA for each input commit.
// On conflict it aborts and returns *ConflictError naming that commit.
func (g *Git) CherryPick(ctx context.Context, commits []string, committer Identity) ([]string, error) {
	cg := g.With(g.Dir)
	cg.Env = append(cg.Env, committer.env("GIT_COMMITTER")...)
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		if _, err := cg.Run(ctx, "cherry-pick", "--allow-empty", "--allow-empty-message", "--keep-redundant-commits", c); err != nil {
			names, derr := cg.runRaw(ctx, nil, "diff", "--name-only", "--diff-filter=U", "-z")
			if _, aerr := cg.Run(ctx, "cherry-pick", "--abort"); aerr != nil {
				return nil, fmt.Errorf("cherry-pick failed and abort failed, worktree may be mid-pick: %w", errors.Join(err, aerr))
			}
			files := splitNUL(names)
			if derr == nil && len(files) > 0 {
				sort.Strings(files)
				return nil, &ConflictError{Files: files, Commit: c}
			}
			return nil, err
		}
		head, err := cg.MustResolve(ctx, "HEAD")
		if err != nil {
			return nil, err
		}
		out = append(out, head)
	}
	return out, nil
}

// CommitAuthor reads a commit's author from the raw object, with its time
// in UTC.
func (g *Git) CommitAuthor(ctx context.Context, sha string) (Identity, error) {
	raw, err := g.runRaw(ctx, nil, "cat-file", "commit", sha)
	if err != nil {
		return Identity{}, err
	}
	header, _, _ := bytes.Cut(raw, []byte("\n\n"))
	for _, line := range strings.Split(string(header), "\n") {
		rest, ok := strings.CutPrefix(line, "author ")
		if !ok {
			continue
		}
		open, end := strings.LastIndexByte(rest, '<'), strings.LastIndexByte(rest, '>')
		if open < 0 || end < open {
			break
		}
		f := strings.Fields(rest[end+1:])
		if len(f) < 1 {
			break
		}
		secs, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			break
		}
		return Identity{Name: strings.TrimSpace(rest[:open]), Email: rest[open+1 : end], When: time.Unix(secs, 0).UTC()}, nil
	}
	return Identity{}, fmt.Errorf("commit %s has no readable author", sha)
}
