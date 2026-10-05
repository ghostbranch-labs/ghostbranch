package gitx_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestPlumbingRoundTrip(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	c1 := testutil.Commit(t, g, "main", "first", map[string]string{"a.txt": "A\n", "dir/b.txt": "B\n"})

	sha, ok, err := g.Resolve(ctx, "refs/heads/main")
	if err != nil || !ok || sha != c1 {
		t.Fatalf("Resolve main = %q %v %v, want %q", sha, ok, err, c1)
	}
	if _, ok, err := g.Resolve(ctx, "refs/heads/nope"); err != nil || ok {
		t.Fatalf("missing ref: ok=%v err=%v", ok, err)
	}

	es, err := g.ListTree(ctx, c1)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, e := range es {
		paths = append(paths, e.Path)
	}
	if !reflect.DeepEqual(paths, []string{"a.txt", "dir/b.txt"}) {
		t.Fatalf("ListTree paths = %v", paths)
	}

	b, ok, err := g.ShowFile(ctx, c1, "dir/b.txt")
	if err != nil || !ok || string(b) != "B\n" {
		t.Fatalf("ShowFile = %q %v %v", b, ok, err)
	}
	if _, ok, err := g.ShowFile(ctx, c1, "missing.txt"); err != nil || ok {
		t.Fatalf("ShowFile missing: ok=%v err=%v", ok, err)
	}

	c2 := testutil.Commit(t, g, "main", "second", map[string]string{"a.txt": testutil.Delete, "c.txt": "C\n"})
	names, err := g.DiffNames(ctx, c1, c2)
	if err != nil || !reflect.DeepEqual(names, []string{"a.txt", "c.txt"}) {
		t.Fatalf("DiffNames = %v %v", names, err)
	}
	if anc, _ := g.IsAncestor(ctx, c1, c2); !anc {
		t.Fatal("c1 should be an ancestor of c2")
	}
	if anc, _ := g.IsAncestor(ctx, c2, c1); anc {
		t.Fatal("c2 must not be an ancestor of c1")
	}
	refs, err := g.Refs(ctx, "refs/heads")
	if err != nil || refs["refs/heads/main"] != c2 {
		t.Fatalf("Refs = %v %v", refs, err)
	}
}

func TestCommitsAreDeterministic(t *testing.T) {
	files := map[string]string{"x": "1\n"}
	a := testutil.Commit(t, testutil.BareRepo(t, t.TempDir()), "main", "m", files)
	b := testutil.Commit(t, testutil.BareRepo(t, t.TempDir()), "main", "m", files)
	if a != b {
		t.Fatalf("same content and identity must give the same SHA: %s vs %s", a, b)
	}
}

func TestCommitTreeUsesTheGivenIdentityAndMessage(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	tree, err := g.MakeTree(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	who := gitx.Identity{Name: "Ada", Email: "ada@example.com", When: testutil.Base}
	c, err := g.CommitTree(ctx, tree, nil, "subject\n\nbody", who, who)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := g.Run(ctx, "cat-file", "-p", c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "author Ada <ada@example.com> 1767225600 +0000") ||
		!strings.HasSuffix(raw, "subject\n\nbody") {
		t.Fatalf("unexpected commit:\n%s", raw)
	}
}

func TestReadBlobsKeepsBytesExactly(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	content := []byte("crlf\r\nnul\x00end\n")
	sha, err := g.HashObject(ctx, content)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := g.HashObject(ctx, []byte("second"))
	got, err := g.ReadBlobs(ctx, []string{sha, other})
	if err != nil {
		t.Fatal(err)
	}
	if string(got[sha]) != string(content) || string(got[other]) != "second" {
		t.Fatalf("ReadBlobs = %q", got)
	}
}
