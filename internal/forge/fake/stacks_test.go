package fake_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

var _ forge.Stacker = (*fake.Forge)(nil)

const widget = "acme/widget"

// stackRepo makes acme/widget with main and the branches a, b and c, each
// one commit on the one below, and opens PRs 1 (a→main), 2 (b→a) and
// 3 (c→b): the shape of the sandbox check in the findings.
func stackRepo(t *testing.T) (*fake.Forge, *gitx.Git) {
	t.Helper()
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	dir, err := f.AddRepo(widget, false)
	if err != nil {
		t.Fatal(err)
	}
	g := gitx.New(dir)
	testutil.Commit(t, g, "main", "initial", map[string]string{"README.md": "# widget\n"})
	below := "main"
	for _, b := range []string{"a", "b", "c"} {
		tip, err := g.MustResolve(ctx, "refs/heads/"+below)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.UpdateRef(ctx, "refs/heads/"+b, tip); err != nil {
			t.Fatal(err)
		}
		testutil.Commit(t, g, b, "add "+b, map[string]string{b + ".txt": b + "\n"})
		if _, err := f.CreatePR(ctx, widget, forge.NewPR{Title: "add " + b, Head: b, Base: below}); err != nil {
			t.Fatal(err)
		}
		below = b
	}
	return f, g
}

func heads(t *testing.T, g *gitx.Git) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, b := range []string{"main", "a", "b", "c"} {
		sha, err := g.MustResolve(context.Background(), "refs/heads/"+b)
		if err != nil {
			t.Fatal(err)
		}
		out[b] = sha
	}
	return out
}

// patch is a commit's diff from its first parent, as git prints it.
func patch(t *testing.T, g *gitx.Git, sha string) string {
	t.Helper()
	out, err := g.Run(context.Background(), "diff", sha+"^", sha)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFakeStacksChainAndShareTheNumberSequence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, g := stackRepo(t)
	if _, err := f.CreateStack(ctx, widget, []int{2, 1}); !errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("top first: %v", err)
	}
	s, err := f.CreateStack(ctx, widget, []int{1, 2})
	if err != nil || s.Number != 4 || !slices.Equal(s.PRs, []int{1, 2}) {
		t.Fatalf("CreateStack = %+v %v", s, err)
	}
	if _, err := f.AddToStack(ctx, widget, 4, []int{1}); !errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("a PR already stacked: %v", err)
	}
	if s, err = f.AddToStack(ctx, widget, 4, []int{3}); err != nil || !slices.Equal(s.PRs, []int{1, 2, 3}) {
		t.Fatalf("AddToStack = %+v %v", s, err)
	}
	if got, err := f.StackOf(ctx, widget, 2); err != nil || got == nil || got.Number != 4 {
		t.Fatalf("StackOf(2) = %+v %v", got, err)
	}
	// The stack took number 4, so the next PR is 5.
	testutil.Commit(t, g, "d", "add d", map[string]string{"d.txt": "d\n"})
	pr, err := f.CreatePR(ctx, widget, forge.NewPR{Head: "d", Base: "main"})
	if err != nil || pr.Number != 5 {
		t.Fatalf("CreatePR after a stack = %+v %v", pr, err)
	}
	if got, err := f.StackOf(ctx, widget, 5); err != nil || got != nil {
		t.Fatalf("StackOf(5) = %+v %v, want nil", got, err)
	}
	if _, err := f.AddToStack(ctx, widget, 9, []int{5}); !errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("no stack 9: %v", err)
	}
	want := []string{"create 2,1", "create 1,2", "add 4: 1", "add 4: 3", "of 2", "of 5", "add 9: 5"}
	if !slices.Equal(f.StackCalls, want) {
		t.Fatalf("StackCalls = %q, want %q", f.StackCalls, want)
	}
	if all := f.Stacks(widget); len(all) != 1 || all[0].Number != 4 {
		t.Fatalf("Stacks = %+v", all)
	}
}

func TestFakeStacksUnavailableAndConflicts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, _ := stackRepo(t)
	f.StacksUnavailable = true
	if _, err := f.CreateStack(ctx, widget, []int{1, 2}); !errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("CreateStack: %v", err)
	}
	if _, err := f.AddToStack(ctx, widget, 4, []int{3}); !errors.Is(err, forge.ErrStacksUnavailable) {
		t.Fatalf("AddToStack: %v", err)
	}
	if s, err := f.StackOf(ctx, widget, 1); err != nil || s != nil {
		t.Fatalf("StackOf on a host without stacks = %+v %v, want nil", s, err)
	}
	f.StacksUnavailable = false
	f.StackConflicts = 1
	if _, err := f.CreateStack(ctx, widget, []int{1, 2}); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("scripted 409: %v", err)
	}
	// The refused call took no number.
	if s, err := f.CreateStack(ctx, widget, []int{1, 2}); err != nil || s.Number != 4 {
		t.Fatalf("retry = %+v %v", s, err)
	}
}

// GitHub's behaviour from the sandbox check: squash-merging the bottom PR
// retargets the PR above to main and rebases every branch above (new SHAs,
// same patches, same authors). The stack stays whole.
func TestFakeSquashMergeOfTheBottomRestacksTheBranchesAbove(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f, g := stackRepo(t)
	if _, err := f.CreateStack(ctx, widget, []int{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	before := heads(t, g)
	if err := f.MergeStack(widget, 1, "squash"); err != nil {
		t.Fatal(err)
	}
	after := heads(t, g)
	if p, _ := g.MustResolve(ctx, after["main"]+"^"); p != before["main"] {
		t.Fatalf("main gained more than one commit: %s^ = %s", after["main"], p)
	}
	if names, _ := g.DiffNames(ctx, before["main"], after["main"]); !slices.Equal(names, []string{"a.txt"}) {
		t.Fatalf("the squash commit changes %v, want [a.txt]", names)
	}
	if after["a"] != before["a"] || after["b"] == before["b"] || after["c"] == before["c"] {
		t.Fatalf("heads before %v, after %v: a stays, b and c are rewritten", before, after)
	}
	for _, x := range []struct{ branch, onto string }{{"b", after["main"]}, {"c", after["b"]}} {
		if p, _ := g.MustResolve(ctx, after[x.branch]+"^"); p != x.onto {
			t.Errorf("%s now sits on %s, want %s", x.branch, p, x.onto)
		}
		if patch(t, g, before[x.branch]) != patch(t, g, after[x.branch]) {
			t.Errorf("%s's patch changed in the rebase", x.branch)
		}
		oldAuthor, _, _ := g.CommitIdentities(ctx, before[x.branch])
		newAuthor, committer, _ := g.CommitIdentities(ctx, after[x.branch])
		if oldAuthor != newAuthor || !strings.Contains(committer, "42+octo@users.noreply.github.com") {
			t.Errorf("%s: author %q → %q, committer %q", x.branch, oldAuthor, newAuthor, committer)
		}
	}
	prs := f.PRs(widget)
	if !prs[0].Merged || prs[1].Merged || prs[1].Base != "main" || prs[2].Merged || prs[2].Base != "b" {
		t.Fatalf("PRs after the merge = %+v", prs)
	}
	if s, _ := f.StackOf(ctx, widget, 1); s == nil || !slices.Equal(s.PRs, []int{1, 2, 3}) {
		t.Fatalf("the stack must stay whole: %+v", s)
	}
}

// Merging the top PR merges the whole stack at once: squash adds one commit
// per PR, merge adds one merge commit for the group and keeps the original
// SHAs.
func TestFakeMergingTheTopMergesTheWholeStack(t *testing.T) {
	t.Parallel()
	for _, method := range []string{"squash", "merge"} {
		t.Run(method, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			f, g := stackRepo(t)
			if _, err := f.CreateStack(ctx, widget, []int{1, 2, 3}); err != nil {
				t.Fatal(err)
			}
			before := heads(t, g)
			if err := f.MergeStack(widget, 3, method); err != nil {
				t.Fatal(err)
			}
			after := heads(t, g)
			for _, pr := range f.PRs(widget) {
				if !pr.Merged {
					t.Errorf("PR #%d not merged", pr.Number)
				}
			}
			if names, _ := g.DiffNames(ctx, before["main"], after["main"]); !slices.Equal(names, []string{"a.txt", "b.txt", "c.txt"}) {
				t.Fatalf("main gained %v", names)
			}
			kept, _ := g.IsAncestor(ctx, before["c"], after["main"])
			switch method {
			case "squash":
				if n, _ := g.Run(ctx, "rev-list", "--count", before["main"]+".."+after["main"]); n != "3" || kept {
					t.Fatalf("squash: %s new commits on main, original commits kept: %v", n, kept)
				}
			case "merge":
				parents, _ := g.Run(ctx, "rev-parse", after["main"]+"^1", after["main"]+"^2")
				if parents != before["main"]+"\n"+before["c"] || !kept {
					t.Fatalf("merge: parents %q, original commits kept: %v", parents, kept)
				}
			}
		})
	}
}

func TestFakeSetPRBase(t *testing.T) {
	t.Parallel()
	f, _ := stackRepo(t)
	f.SetPRBase(widget, 2, "main")
	if prs := f.PRs(widget); prs[1].Base != "main" || prs[2].Base != "b" {
		t.Fatalf("PRs after SetPRBase = %+v", prs)
	}
}

// As on GitHub, a PR that doesn't exist is ErrNotFound, stacks or not.
func TestFakeStackOfAMissingPR(t *testing.T) {
	t.Parallel()
	f, _ := stackRepo(t)
	if _, err := f.StackOf(context.Background(), widget, 9); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("StackOf(9): %v", err)
	}
	f.StacksUnavailable = true
	if _, err := f.StackOf(context.Background(), widget, 9); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("StackOf(9) without stacks: %v", err)
	}
}
