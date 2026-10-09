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
// On conflict it aborts and returns *ConflictError naming that commit; any
// other failure names the commit and aborts nothing that wasn't started.
func (g *Git) CherryPick(ctx context.Context, commits []string, committer Identity) ([]string, error) {
	cg := g.With(g.Dir)
	cg.Env = append(cg.Env, committer.env("GIT_COMMITTER")...)
	out := make([]string, 0, len(commits))
	for _, c := range commits {
		if _, err := cg.Run(ctx, "cherry-pick", "--allow-empty", "--allow-empty-message", "--keep-redundant-commits", c); err != nil {
			// Only a pick that stopped part-way leaves CHERRY_PICK_HEAD; a bad
			// SHA or a refused overwrite leaves nothing to abort.
			_, picking, herr := cg.Resolve(ctx, "CHERRY_PICK_HEAD")
			if herr != nil {
				return nil, fmt.Errorf("cherry-pick %s: %w", c, errors.Join(err, herr))
			}
			if !picking {
				return nil, fmt.Errorf("cherry-pick %s: %w", c, err)
			}
			names, derr := cg.runRaw(ctx, nil, "diff", "--name-only", "--diff-filter=U", "-z")
			if _, aerr := cg.Run(ctx, "cherry-pick", "--abort"); aerr != nil {
				return nil, fmt.Errorf("cherry-pick %s failed and abort failed, worktree may be mid-pick: %w", c, errors.Join(err, aerr))
			}
			if files := splitNUL(names); derr == nil && len(files) > 0 {
				sort.Strings(files)
				return nil, &ConflictError{Files: files, Commit: c}
			}
			return nil, fmt.Errorf("cherry-pick %s: %w", c, err)
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
		// As git's split_ident_line: the email runs from the first '<' to
		// the next '>', and the date follows the last '>'.
		open := strings.IndexByte(rest, '<')
		if open < 0 {
			break
		}
		n := strings.IndexByte(rest[open+1:], '>')
		if n < 0 {
			break
		}
		end := open + 1 + n
		f := strings.Fields(rest[strings.LastIndexByte(rest, '>')+1:])
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

// MergeTreeCheck reports whether head merges cleanly onto base, without a
// worktree: git merge-tree --write-tree exits 0 when clean and 1 when it
// conflicts, listing the conflicted files after the tree line. Any other
// failure (a bad ref, say) is an error.
func (g *Git) MergeTreeCheck(ctx context.Context, base, head string) (bool, []string, error) {
	out, err := g.runRaw(ctx, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", base, head)
	if err == nil {
		return true, nil, nil
	}
	if exitCode(err) != 1 {
		return false, nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	// A bad ref also exits 1, but prints no tree id on stdout.
	if !isObjectID(lines[0]) {
		return false, nil, err
	}
	seen := map[string]bool{}
	files := []string{}
	for _, l := range lines[1:] { // line 1 is the tree id
		if l = strings.TrimSpace(l); l != "" && !seen[l] {
			seen[l] = true
			files = append(files, l)
		}
	}
	sort.Strings(files)
	return false, files, nil
}

// isObjectID reports whether s is a full SHA-1 or SHA-256 object id, as
// git prints it: lowercase hex.
func isObjectID(s string) bool {
	return (len(s) == 40 || len(s) == 64) && strings.Trim(s, "0123456789abcdef") == ""
}
