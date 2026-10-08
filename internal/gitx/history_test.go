package gitx_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// repoWithFeature: main has base, then upstream (o.txt); feat branches at
// base with three commits: a (a.txt), merge-free b (b.txt), c (a.txt again).
func repoWithFeature(t *testing.T) (g *gitx.Git, base, main string, feat []string) {
	t.Helper()
	ctx := context.Background()
	g = testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	base = testutil.Commit(t, g, "main", "base", map[string]string{"f.txt": "f\n", "o.txt": "o\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", base)
	feat = append(feat,
		testutil.Commit(t, g, "feat", "add a", map[string]string{"a.txt": "a1\n"}),
		testutil.Commit(t, g, "feat", "add b", map[string]string{"b.txt": "b\n"}),
		testutil.Commit(t, g, "feat", "edit a", map[string]string{"a.txt": "a2\n"}))
	main = testutil.Commit(t, g, "main", "upstream", map[string]string{"o.txt": "upstream\n"})
	return g, base, main, feat
}

func TestSourceCommitsOldestFirstSkippingUpstreamPatches(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _, main, feat := repoWithFeature(t)
	got, err := g.SourceCommits(ctx, main, feat[2])
	if err != nil || !reflect.DeepEqual(got, feat) {
		t.Fatalf("SourceCommits = %v %v, want %v", got, err, feat)
	}
	// The same patch as "add b" lands on main: replay leaves b out, as rebase does.
	testutil.Commit(t, g, "main", "add b upstream", map[string]string{"b.txt": "b\n"})
	main2, _ := g.MustResolve(ctx, "refs/heads/main")
	got, _ = g.SourceCommits(ctx, main2, feat[2])
	if !reflect.DeepEqual(got, []string{feat[0], feat[2]}) {
		t.Fatalf("after upstream b: %v", got)
	}
	if got, _ := g.SourceCommits(ctx, main, main); len(got) != 0 {
		t.Fatalf("no commits: %v", got)
	}
}

// A side branch merged back into the feature: like rebase, the replay
// linearizes it to the non-merge commits, oldest first, each after the
// commits it builds on, and the result holds every side.
func TestSourceCommitsAndCherryPickLinearizeAMerge(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, base, main, _ := repoWithFeature(t)
	_ = g.UpdateRef(ctx, "refs/heads/feat2", base)
	a := testutil.Commit(t, g, "feat2", "add x", map[string]string{"x.txt": "x\n"})
	_ = g.UpdateRef(ctx, "refs/heads/side", a)
	s := testutil.Commit(t, g, "side", "side s", map[string]string{"s.txt": "s\n"})
	b := testutil.Commit(t, g, "feat2", "add y", map[string]string{"y.txt": "y\n"})
	// The merge's tree: y on top of the side (the files don't overlap).
	_ = g.UpdateRef(ctx, "refs/heads/tmp", s)
	merged, _ := g.TreeOf(ctx, testutil.Commit(t, g, "tmp", "tmp", map[string]string{"y.txt": "y\n"}))
	m, err := g.CommitTree(ctx, merged, []string{b, s}, "merge side", testutil.Author(5), testutil.Author(5))
	if err != nil {
		t.Fatal(err)
	}
	_ = g.UpdateRef(ctx, "refs/heads/feat2", m)
	c := testutil.Commit(t, g, "feat2", "add z", map[string]string{"z.txt": "z\n"})

	got, err := g.SourceCommits(ctx, main, c)
	if err != nil || len(got) != 4 || got[0] != a || got[3] != c {
		t.Fatalf("SourceCommits = %v %v; want %s, then %s and %s in some order, then %s", got, err, a, b, s, c)
	}
	if mid := []string{got[1], got[2]}; !reflect.DeepEqual(mid, []string{b, s}) && !reflect.DeepEqual(mid, []string{s, b}) {
		t.Fatalf("SourceCommits = %v: the merge %s must be left out", got, m)
	}
	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	out, err := wt.CherryPick(ctx, got, bot())
	if err != nil || len(out) != 4 {
		t.Fatalf("CherryPick = %v %v", out, err)
	}
	parent := main
	for i, r := range out {
		if p, _ := g.MustResolve(ctx, r+"^"); p != parent {
			t.Fatalf("replayed commit %d's parent is %s, want %s", i, p, parent)
		}
		if _, two, _ := g.Resolve(ctx, r+"^2"); two {
			t.Fatalf("replayed commit %d is a merge", i)
		}
		parent = r
	}
	for _, f := range []string{"x.txt", "s.txt", "y.txt", "z.txt"} {
		if _, ok, _ := g.ShowFile(ctx, out[3], f); !ok {
			t.Fatalf("the linear replay lost %s", f)
		}
	}
}

func TestCherryPickReplaysInOrderAndKeepsAuthors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _, main, feat := repoWithFeature(t)
	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	got, err := wt.CherryPick(ctx, feat, bot())
	if err != nil || len(got) != 3 {
		t.Fatalf("CherryPick = %v %v", got, err)
	}
	if p, _ := g.MustResolve(ctx, got[0]+"^"); p != main {
		t.Fatalf("first parent %s, want %s", p, main)
	}
	for i := 1; i < 3; i++ {
		if p, _ := g.MustResolve(ctx, got[i]+"^"); p != got[i-1] {
			t.Fatalf("commit %d parent %s", i, p)
		}
	}
	a, _, _ := g.ShowFile(ctx, got[2], "a.txt")
	o, _, _ := g.ShowFile(ctx, got[2], "o.txt")
	if string(a) != "a2\n" || string(o) != "upstream\n" {
		t.Fatalf("tip content a=%q o=%q", a, o)
	}
	src, _ := g.CommitAuthor(ctx, feat[1])
	dst, _ := g.CommitAuthor(ctx, got[1])
	if src != dst {
		t.Fatalf("author changed: %+v -> %+v", src, dst)
	}
	_, committer, _ := g.CommitIdentities(ctx, got[1])
	if committer != "ghostbranch <ghostbranch@invalid>" {
		t.Fatalf("committer = %s", committer)
	}
}

func TestCherryPickKeepsCommitsThatBecomeEmpty(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _, main, feat := repoWithFeature(t)
	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, _ := g.WorktreeAdd(ctx, wtDir, main)
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	// Replaying "add a" twice: the second copy changes nothing.
	got, err := wt.CherryPick(ctx, []string{feat[0], feat[0]}, bot())
	if err != nil || len(got) != 2 {
		t.Fatalf("CherryPick = %v %v", got, err)
	}
	t1, _ := g.TreeOf(ctx, got[0])
	t2, _ := g.TreeOf(ctx, got[1])
	if t1 != t2 {
		t.Fatal("the redundant commit must be kept, with its parent's tree")
	}
}

func TestCherryPickConflictNamesTheCommitAndAborts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _, main, feat := repoWithFeature(t)
	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, _ := g.WorktreeAdd(ctx, wtDir, main)
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	// "edit a" before "add a": a.txt does not exist yet.
	_, err := wt.CherryPick(ctx, []string{feat[2], feat[0]}, bot())
	var ce *gitx.ConflictError
	if !errors.As(err, &ce) || ce.Commit != feat[2] || !reflect.DeepEqual(ce.Files, []string{"a.txt"}) {
		t.Fatalf("err = %#v", err)
	}
	if _, err := wt.Run(ctx, "cherry-pick", "--abort"); err == nil {
		t.Fatal("cherry-pick must already be aborted")
	}
}

func TestCommitAuthor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g, _, _, feat := repoWithFeature(t)
	got, err := g.CommitAuthor(ctx, feat[0])
	want := gitx.Identity{Name: "Test Author", Email: "author@example.com", When: testutil.Base.Add(time.Minute)}
	if err != nil || got != want {
		t.Fatalf("CommitAuthor = %+v %v, want %+v", got, err, want)
	}
}

func TestMergeTreeCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"f.txt": "f\n", "o.txt": "o\n"})
	if err := g.UpdateRef(ctx, "refs/heads/feat", base); err != nil {
		t.Fatal(err)
	}
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"f.txt": "feature\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"o.txt": "upstream\n"})
	if clean, files, err := g.MergeTreeCheck(ctx, main, feat); err != nil || !clean || len(files) != 0 {
		t.Fatalf("clean merge = %v %v %v", clean, files, err)
	}
	main2 := testutil.Commit(t, g, "main", "clash", map[string]string{"f.txt": "upstream f\n"})
	clean, files, err := g.MergeTreeCheck(ctx, main2, feat)
	if err != nil || clean || !reflect.DeepEqual(files, []string{"f.txt"}) {
		t.Fatalf("conflict = %v %v %v", clean, files, err)
	}
	if _, _, err := g.MergeTreeCheck(ctx, "nonexistent", feat); err == nil {
		t.Fatal("a bad ref must be an error, not a conflict")
	}
}
