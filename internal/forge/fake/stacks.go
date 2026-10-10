package fake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// stack is a native stack in the fake: its number and PRs, bottom first.
type stack struct {
	number int
	prs    []int
}

func (s *stack) view() forge.Stack { return forge.Stack{Number: s.number, PRs: slices.Clone(s.prs)} }

// mergedAt is when the fake's merges and GitHub-side rebases happen, so the
// commits they make get the same SHAs on every run.
var mergedAt = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func ints(ns []int) string {
	s := make([]string, len(ns))
	for i, n := range ns {
		s[i] = strconv.Itoa(n)
	}
	return strings.Join(s, ",")
}

// stackCall records a Stacker call and answers the scripted refusals: a
// named missing permission, an unknown repo, no native stacks, and (for
// calls that change a stack) a 409. Callers hold f.mu.
func (f *Forge) stackCall(method, full, call string, changes bool) error {
	f.StackCalls = append(f.StackCalls, call)
	if err := f.missing(method, full); err != nil {
		return err
	}
	if _, ok := f.repos[full]; !ok {
		return fmt.Errorf("repo %s: %w", full, forge.ErrNotFound)
	}
	if changes && f.StacksUnavailable {
		return fmt.Errorf("%s in %s: %w", call, full, forge.ErrStacksUnavailable)
	}
	if changes && f.StackConflicts > 0 {
		f.StackConflicts--
		return fmt.Errorf("%s in %s: %w", call, full, forge.ErrConflict)
	}
	return nil
}

// pr is PR n of full, or nil. Callers hold f.mu.
func (f *Forge) pr(full string, n int) *forge.PR {
	for i := range f.prs[full] {
		if f.prs[full][i].Number == n {
			return &f.prs[full][i]
		}
	}
	return nil
}

// stackOf is the stack holding PR n and n's index in it, or nil and -1.
// Callers hold f.mu.
func (f *Forge) stackOf(full string, n int) (*stack, int) {
	for _, s := range f.stacks[full] {
		if i := slices.Index(s.prs, n); i >= 0 {
			return s, i
		}
	}
	return nil, -1
}

// chain checks prs can stack on the PR whose head is below ("" for the
// bottom of a new stack), as GitHub's 422 does: each is an open PR, listed
// once, in no stack yet, and based on the head of the PR below it.
// Callers hold f.mu.
func (f *Forge) chain(full, below string, prs []int) error {
	refuse := func(why string) error {
		return fmt.Errorf("stack PRs %s in %s: %s: %w", ints(prs), full, why, forge.ErrStacksUnavailable)
	}
	if len(prs) == 0 {
		return refuse("no pull requests")
	}
	for i, n := range prs {
		p := f.pr(full, n)
		switch {
		case p == nil:
			return refuse(fmt.Sprintf("no PR #%d", n))
		case p.State != "open":
			return refuse(fmt.Sprintf("PR #%d is not open", n))
		case slices.Index(prs, n) != i:
			return refuse(fmt.Sprintf("PR #%d is listed twice", n))
		}
		if s, _ := f.stackOf(full, n); s != nil {
			return refuse(fmt.Sprintf("PR #%d is already in stack %d", n, s.number))
		}
		if (i > 0 || below != "") && p.Base != below {
			return refuse("pull requests must form a stack, where each PR's base ref is the previous PR's head ref")
		}
		below = p.Head
	}
	return nil
}

func (f *Forge) CreateStack(_ context.Context, full string, prs []int) (forge.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stackCall("CreateStack", full, "create "+ints(prs), true); err != nil {
		return forge.Stack{}, err
	}
	if err := f.chain(full, "", prs); err != nil {
		return forge.Stack{}, err
	}
	f.next[full]++
	s := &stack{number: f.next[full], prs: slices.Clone(prs)}
	f.stacks[full] = append(f.stacks[full], s)
	return s.view(), nil
}

func (f *Forge) AddToStack(_ context.Context, full string, number int, prs []int) (forge.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stackCall("AddToStack", full, fmt.Sprintf("add %d: %s", number, ints(prs)), true); err != nil {
		return forge.Stack{}, err
	}
	var s *stack
	for _, x := range f.stacks[full] {
		if x.number == number {
			s = x
		}
	}
	if s == nil {
		return forge.Stack{}, fmt.Errorf("stack %d in %s: %w", number, full, forge.ErrStacksUnavailable)
	}
	top := f.pr(full, s.prs[len(s.prs)-1])
	if err := f.chain(full, top.Head, prs); err != nil {
		return forge.Stack{}, err
	}
	s.prs = append(s.prs, prs...)
	return s.view(), nil
}

func (f *Forge) StackOf(_ context.Context, full string, pr int) (*forge.Stack, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.stackCall("StackOf", full, fmt.Sprintf("of %d", pr), false); err != nil {
		return nil, err
	}
	if f.pr(full, pr) == nil {
		return nil, fmt.Errorf("PR %s#%d: %w", full, pr, forge.ErrNotFound)
	}
	s, _ := f.stackOf(full, pr)
	if s == nil || f.StacksUnavailable {
		return nil, nil
	}
	v := s.view()
	return &v, nil
}

// Stacks returns a copy of every stack in full.
func (f *Forge) Stacks(full string) []forge.Stack {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.Stack
	for _, s := range f.stacks[full] {
		out = append(out, s.view())
	}
	return out
}

// MergeStack merges PR pr of a stack in full as GitHub's merge-async does:
// pr and every unmerged PR below it merge, bottom first.
//   - "squash": the stack's base branch gains one commit per merged PR,
//     holding that PR's own changes.
//   - "merge": it gains one merge commit for the group, whose second parent
//     is pr's head, so the original commits keep their SHAs.
//
// Then, as GitHub does, each PR left above is rebased: its own commits are
// replayed onto the branch below (same patches and authors, new SHAs,
// committed by the fake's user) and its branch is force-updated. The PR
// directly above the merged ones is retargeted to the stack's base; the
// others keep their bases. Merged PRs stay in the stack.
func (f *Forge) MergeStack(full string, pr int, method string) error {
	ctx := context.Background()
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[full]
	if !ok {
		return fmt.Errorf("repo %s: %w", full, forge.ErrNotFound)
	}
	if method != "squash" && method != "merge" {
		return fmt.Errorf("merge method %q: the fake merges a stack with squash or merge", method)
	}
	s, at := f.stackOf(full, pr)
	if s == nil {
		return fmt.Errorf("PR %s#%d is in no stack", full, pr)
	}
	var merging, above []*forge.PR
	for i, n := range s.prs {
		p := f.pr(full, n)
		switch {
		case p.Merged:
		case i <= at:
			merging = append(merging, p)
		default:
			above = append(above, p)
		}
	}
	if len(merging) == 0 {
		return fmt.Errorf("PR %s#%d is already merged", full, pr)
	}
	g := gitx.New(r.CloneURL)
	base := merging[0].Base
	tip, err := g.MustResolve(ctx, "refs/heads/"+base)
	if err != nil {
		return err
	}
	old := map[int]string{}
	for _, p := range append(slices.Clone(merging), above...) {
		if old[p.Number], err = g.MustResolve(ctx, "refs/heads/"+p.Head); err != nil {
			return err
		}
	}
	who := gitx.Identity{Name: f.User.Login, Email: forge.NoreplyEmail(f.User), When: mergedAt}
	top := merging[len(merging)-1]
	switch method {
	case "squash":
		below := tip
		for _, p := range merging {
			replayed, err := f.replay(ctx, g, tip, below, old[p.Number], who)
			if err != nil {
				return err
			}
			tree, err := g.TreeOf(ctx, replayed)
			if err != nil {
				return err
			}
			if tip, err = g.CommitTree(ctx, tree, []string{tip}, fmt.Sprintf("%s (#%d)", p.Head, p.Number), who, who); err != nil {
				return err
			}
			below = old[p.Number]
		}
	case "merge":
		out, err := g.Run(ctx, "merge-tree", "--write-tree", tip, old[top.Number])
		if err != nil {
			return fmt.Errorf("merge PR %s#%d: %w", full, top.Number, err)
		}
		tree, _, _ := strings.Cut(out, "\n")
		if tip, err = g.CommitTree(ctx, tree, []string{tip, old[top.Number]},
			fmt.Sprintf("Merge pull request #%d from %s", top.Number, top.Head), who, who); err != nil {
			return err
		}
	}
	if err := g.UpdateRef(ctx, "refs/heads/"+base, tip); err != nil {
		return err
	}
	for _, p := range merging {
		p.State, p.Merged = "closed", true
	}
	onto, below := tip, old[top.Number]
	for i, p := range above {
		moved, err := f.replay(ctx, g, onto, below, old[p.Number], who)
		if err != nil {
			return err
		}
		if err := g.UpdateRef(ctx, "refs/heads/"+p.Head, moved); err != nil {
			return err
		}
		if i == 0 {
			p.Base = base
		}
		onto, below = moved, old[p.Number]
	}
	return nil
}

// replay puts head's own commits (those not in below) onto onto, in a
// scratch worktree, committing as who, and returns the new head (onto when
// there is nothing to replay). Callers hold f.mu.
func (f *Forge) replay(ctx context.Context, g *gitx.Git, onto, below, head string, who gitx.Identity) (string, error) {
	commits, err := g.SourceCommits(ctx, below, head)
	if err != nil {
		return "", err
	}
	if len(commits) == 0 {
		return onto, nil
	}
	if err := os.MkdirAll(f.root, 0o755); err != nil {
		return "", err
	}
	scratch, err := os.MkdirTemp(f.root, "replay-")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	dir := filepath.Join(scratch, "wt")
	wt, err := g.WorktreeAdd(ctx, dir, onto)
	if err != nil {
		return "", err
	}
	defer func() { _ = g.WorktreeRemove(ctx, dir) }()
	out, err := wt.CherryPick(ctx, commits, who)
	if err != nil {
		return "", fmt.Errorf("replay %s onto %s: %w", head, onto, err)
	}
	return out[len(out)-1], nil
}
