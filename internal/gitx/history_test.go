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
