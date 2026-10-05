// Package testutil holds helpers for tests only. Production code must not
// import it.
package testutil

import (
	"context"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// Base is the time every test commit is anchored to, so SHAs are stable.
var Base = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// Author is the fixed test identity for the n-th commit on a branch.
func Author(n int) gitx.Identity {
	return gitx.Identity{Name: "Test Author", Email: "author@example.com", When: Base.Add(time.Duration(n) * time.Minute)}
}

// Delete, as a file's content in Commit, removes that path.
const Delete = "\x00delete"

// BareRepo creates a bare repository at dir.
func BareRepo(t testing.TB, dir string) *gitx.Git {
	t.Helper()
	g, err := gitx.Init(context.Background(), dir, true)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Commit adds a commit on refs/heads/<branch> that applies files on top of
// the branch's current tree, using plumbing only (no worktree). It returns
// the new commit's SHA.
func Commit(t testing.TB, g *gitx.Git, branch, message string, files map[string]string) string {
	t.Helper()
	ctx := context.Background()
	ref := "refs/heads/" + branch
	parent, ok, err := g.Resolve(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	byPath := map[string]gitx.TreeEntry{}
	n := 0
	if ok {
		es, err := g.ListTree(ctx, parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			byPath[e.Path] = e
		}
		count, err := g.Run(ctx, "rev-list", "--count", parent)
		if err != nil {
			t.Fatal(err)
		}
		n, _ = strconv.Atoi(count)
	}
	for p, c := range files {
		if c == Delete {
			delete(byPath, p)
			continue
		}
		sha, err := g.HashObject(ctx, []byte(c))
		if err != nil {
			t.Fatal(err)
		}
		byPath[p] = gitx.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Path: p}
	}
	entries := make([]gitx.TreeEntry, 0, len(byPath))
	for _, e := range byPath {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	tree, err := g.MakeTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	var parents []string
	if ok {
		parents = []string{parent}
	}
	who := Author(n)
	sha, err := g.CommitTree(ctx, tree, parents, message, who, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, ref, sha); err != nil {
		t.Fatal(err)
	}
	return sha
}

// Clock is a fake clock; Sleep advances it instantly.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

func NewClock(t time.Time) *Clock { return &Clock{now: t} }

func (c *Clock) Now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.now }

func (c *Clock) Sleep(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

// SeqReader yields 0, 1, 2, … so generated IDs are deterministic.
type SeqReader struct {
	mu sync.Mutex
	n  byte
}

func (r *SeqReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range p {
		p[i] = r.n
		r.n++
	}
	return len(p), nil
}

// FakeGitHubToken returns a string shaped like a GitHub PAT, so Gitleaks
// flags it. It is assembled at run time so this source file never contains
// a token-shaped literal (our own repo is scanned too).
func FakeGitHubToken() string {
	return "ghp_" + "Zx9Qw3Er7Ty1Ui5Op2" + "As8Df4Gh6Jk0LmNbVc"
}
