package gitx

import (
	"bytes"
	"context"
	"strings"
)

// PatchIDs returns each commit's stable patch-id against its first parent,
// in order (git patch-id --stable): the same change on another base, or
// with another message, has the same patch-id. A commit that changes
// nothing has "".
func (g *Git) PatchIDs(ctx context.Context, commits []string) ([]string, error) {
	out := make([]string, len(commits))
	for i, c := range commits {
		diff, err := g.runRaw(ctx, nil, "diff-tree", "-p", "--no-color", "--no-renames", "--full-index", c+"^", c)
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(diff)) == 0 {
			continue
		}
		line, err := g.RunInput(ctx, bytes.NewReader(diff), "patch-id", "--stable")
		if err != nil {
			return nil, err
		}
		out[i], _, _ = strings.Cut(line, " ")
	}
	return out, nil
}

// CommitsBetween lists the commits in head but not base along head's
// first-parent line, oldest first; never nil.
func (g *Git) CommitsBetween(ctx context.Context, base, head string) ([]string, error) {
	out, err := g.Run(ctx, "rev-list", "--reverse", "--first-parent", base+".."+head)
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}
