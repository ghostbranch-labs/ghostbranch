package guard_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const zero = "0000000000000000000000000000000000000000"

// brokenObjects is a checkout whose git fails (say, a safe.directory refusal).
type brokenObjects struct{}

func (brokenObjects) Resolve(context.Context, string) (string, bool, error) {
	return "", false, errors.New("git: exit status 128")
}

func (brokenObjects) IsAncestor(context.Context, string, string) (bool, error) {
	return false, errors.New("git: exit status 128")
}

func (brokenObjects) RevList(context.Context, ...string) ([]string, error) {
	return nil, errors.New("git: exit status 128")
}

// brokenRevList is a checkout that resolves nothing and whose rev-list fails.
type brokenRevList struct{ brokenObjects }

func (brokenRevList) Resolve(context.Context, string) (string, bool, error) { return "", false, nil }

// A git error while reading the checkout must fail the check, never let the
// push through as if the feature's commits were absent.
func TestCheckFailsClosedOnGitErrors(t *testing.T) {
	f := guard.Facts{Names: map[string]bool{"feat-x": true}, Heads: map[string]string{"feat-x": strings.Repeat("a", 40)}}
	line := guard.Line{LocalRef: "HEAD", LocalSHA: strings.Repeat("b", 40), RemoteRef: "refs/heads/main", RemoteSHA: zero}
	blocks, err := guard.Check(context.Background(), brokenObjects{}, "origin", []guard.Line{line}, f)
	if err == nil {
		t.Fatalf("a git error must be an error, got blocks %v", blocks)
	}
	f = guard.Facts{Names: map[string]bool{}, Heads: map[string]string{}, Commits: map[string]string{strings.Repeat("a", 40): "feat-x"}}
	blocks, err = guard.Check(context.Background(), brokenRevList{}, "origin", []guard.Line{line}, f)
	if err == nil {
		t.Fatalf("a rev-list error must be an error, got blocks %v", blocks)
	}
}

// objects is the mirror read as a checkout, as guard.Objects.
type objects struct{ *gitx.Git }

func (o objects) RevList(ctx context.Context, args ...string) ([]string, error) {
	out, err := o.Run(ctx, append([]string{"rev-list"}, args...)...)
	return strings.Fields(out), err
}

// mirror builds a repo laid out like the engine's mirror; it also serves as
// the checkout, so every object exists in both.
func mirror(t *testing.T) (g *gitx.Git, main, feat string) {
	t.Helper()
	ctx := context.Background()
	g = testutil.BareRepo(t, t.TempDir())
	main = testutil.Commit(t, g, "main", "m", map[string]string{"a.txt": "a\n"})
	for _, r := range []string{"refs/heads/feat-x", "refs/remotes/public/main", "refs/remotes/shadow/done",
		"refs/shadow-meta/features/feat-x", "refs/shadow-meta/features/done"} {
		if err := g.UpdateRef(ctx, r, main); err != nil {
			t.Fatal(err)
		}
	}
	feat = testutil.Commit(t, g, "feat-x", "f", map[string]string{"f.txt": "secret\n"})
	if err := g.UpdateRef(ctx, "refs/remotes/shadow/feat-x", feat); err != nil {
		t.Fatal(err)
	}
	return g, main, feat
}

func TestLoadFacts(t *testing.T) {
	g, main, feat := mirror(t)
	f, err := guard.LoadFacts(context.Background(), g)
	if err != nil || !f.Names["feat-x"] || !f.Names["done"] || f.Heads["feat-x"] != feat {
		t.Fatalf("%+v %v", f, err)
	}
	if _, ok := f.Heads["done"]; ok {
		t.Fatal("a feature already in public main has no private commits to look for")
	}
	if f.PublicMain != main || len(f.Commits) != 1 || f.Commits[feat] != "feat-x" {
		t.Fatalf("public main %q, commits %v", f.PublicMain, f.Commits)
	}
}

func TestCheck(t *testing.T) {
	g, main, feat := mirror(t)
	ctx := context.Background()
	f, _ := guard.LoadFacts(ctx, g)
	push := func(local, sha, remote string) guard.Line {
		return guard.Line{LocalRef: local, LocalSHA: sha, RemoteRef: remote, RemoteSHA: zero}
	}
	cases := []struct {
		name    string
		lines   []guard.Line
		blocked []string
	}{
		{"public main", []guard.Line{push("refs/heads/main", main, "refs/heads/main")}, nil},
		{"a tag", []guard.Line{push("refs/tags/v1", main, "refs/tags/v1")}, nil},
		{"a deletion", []guard.Line{{LocalRef: "(delete)", LocalSHA: zero, RemoteRef: "refs/heads/feat-x", RemoteSHA: feat}}, nil},
		{"a release branch", []guard.Line{push("refs/heads/release", main, "refs/heads/release")}, nil},
		{"a feature by name", []guard.Line{push("refs/heads/feat-x", feat, "refs/heads/feat-x")}, []string{"refs/heads/feat-x"}},
		{"a feature under another name", []guard.Line{push(feat, feat, "refs/heads/renamed")}, []string{"refs/heads/renamed"}},
		{"HEAD:main from a feature", []guard.Line{push("HEAD", feat, "refs/heads/main")}, []string{"refs/heads/main"}},
		{"the overlay branch", []guard.Line{push("refs/heads/ghostbranch", main, "refs/heads/ghostbranch")}, []string{"refs/heads/ghostbranch"}},
		{"a gate branch", []guard.Line{push("refs/heads/x", main, "refs/heads/ghostbranch-gate/feat-x")}, []string{"refs/heads/ghostbranch-gate/feat-x"}},
		{"a feature-named local branch", []guard.Line{push("refs/heads/done", main, "refs/heads/other")}, []string{"refs/heads/other"}},
		// Review Focus 3: one push, several refs; only the feature is named.
		{"a mixed push", []guard.Line{push("refs/heads/main", main, "refs/heads/main"), push("refs/heads/feat-x", feat, "refs/heads/feat-x")}, []string{"refs/heads/feat-x"}},
	}
	for _, c := range cases {
		blocks, err := guard.Check(ctx, objects{g}, "origin", c.lines, f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		var got []string
		for _, b := range blocks {
			got = append(got, b.Ref)
			if b.Reason == "" {
				t.Errorf("%s: a block must say why", c.name)
			}
		}
		if !reflect.DeepEqual(got, c.blocked) {
			t.Errorf("%s: blocked %v, want %v", c.name, got, c.blocked)
		}
	}
}

// staleClone builds a mirror whose feature feat-y has commits a, b and c
// (head c), and a checkout that fetched only a and b, on branch spike, with
// origin/main at public main. The checkout is read with plain git, so the
// test keeps the developer's git config out.
func staleClone(t *testing.T) (dir string, co *guard.Checkout, f guard.Facts, main, b string) {
	t.Helper()
	hermetic(t)
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	main = testutil.Commit(t, g, "main", "m", map[string]string{"a.txt": "a\n"})
	for _, r := range []string{"refs/heads/feat-y", "refs/remotes/public/main", "refs/shadow-meta/features/feat-y"} {
		if err := g.UpdateRef(ctx, r, main); err != nil {
			t.Fatal(err)
		}
	}
	testutil.Commit(t, g, "feat-y", "a", map[string]string{"y.txt": "1\n"})
	b = testutil.Commit(t, g, "feat-y", "b", map[string]string{"y.txt": "2\n"})
	if err := g.UpdateRef(ctx, "refs/heads/stale", b); err != nil {
		t.Fatal(err)
	}
	c := testutil.Commit(t, g, "feat-y", "c", map[string]string{"y.txt": "3\n"})
	if err := g.UpdateRef(ctx, "refs/remotes/shadow/feat-y", c); err != nil {
		t.Fatal(err)
	}
	f, err := guard.LoadFacts(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	gitIn(t, dir, "fetch", "-q", filepath.ToSlash(g.Dir), "refs/heads/main:refs/remotes/origin/main", "refs/heads/stale:refs/heads/spike")
	gitIn(t, dir, "switch", "-q", "spike")
	if co, err = guard.Open(ctx, dir); err != nil {
		t.Fatal(err)
	}
	return dir, co, f, main, b
}

// checkPush runs Check on one push of sha to refs/heads/<branch> at remote.
func checkPush(t *testing.T, co *guard.Checkout, remote, branch, sha string, f guard.Facts) []guard.Block {
	t.Helper()
	line := guard.Line{LocalRef: "refs/heads/" + branch, LocalSHA: sha, RemoteRef: "refs/heads/" + branch, RemoteSHA: zero}
	blocks, err := guard.Check(context.Background(), co, remote, []guard.Line{line}, f)
	if err != nil {
		t.Fatal(err)
	}
	return blocks
}

func wantBlockedFor(t *testing.T, what string, blocks []guard.Block, b string) {
	t.Helper()
	if len(blocks) != 1 || !strings.Contains(blocks[0].Reason, `"feat-y"`) || !strings.Contains(blocks[0].Reason, b[:12]) {
		t.Fatalf("%s: want one block naming feat-y and %s, got %+v", what, b[:12], blocks)
	}
}

// A stale clone holds an older feature commit than the mirror's head; the
// head check cannot see it, but the commit itself is still unpublished.
func TestCheckRefusesAnOlderFeatureCommitUnderAnotherName(t *testing.T) {
	_, co, f, _, b := staleClone(t)
	wantBlockedFor(t, "spike at b", checkPush(t, co, "origin", "spike", b, f), b)
}

func TestCheckRefusesABranchOffAMidFeatureCommit(t *testing.T) {
	dir, co, f, _, b := staleClone(t)
	gitIn(t, dir, "switch", "-q", "-c", "fix", b)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "d")
	d := gitIn(t, dir, "rev-parse", "HEAD")
	wantBlockedFor(t, "fix at d", checkPush(t, co, "origin", "fix", d, f), b)
}

func TestCheckPassesPublishedAndUnrelatedCommits(t *testing.T) {
	dir, co, f, main, _ := staleClone(t)
	if blocks := checkPush(t, co, "origin", "release", main, f); len(blocks) != 0 {
		t.Fatalf("a branch at public main: %+v", blocks)
	}
	gitIn(t, dir, "switch", "-q", "-c", "other", main)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "unrelated")
	other := gitIn(t, dir, "rev-parse", "HEAD")
	if blocks := checkPush(t, co, "origin", "other", other, f); len(blocks) != 0 {
		t.Fatalf("new commits on public main: %+v", blocks)
	}
}

// Review Focus 2: a push to a bare URL has no tracking refs to exclude, so
// the feature's commits are still found, and unrelated work still passes.
func TestCheckRefusesAFeatureCommitPushedToAURL(t *testing.T) {
	dir, co, f, main, b := staleClone(t)
	const url = "https://github.com/o/r"
	wantBlockedFor(t, "spike at b to a URL", checkPush(t, co, url, "spike", b, f), b)
	gitIn(t, dir, "switch", "-q", "-c", "other", main)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", "unrelated")
	other := gitIn(t, dir, "rev-parse", "HEAD")
	if blocks := checkPush(t, co, url, "other", other, f); len(blocks) != 0 {
		t.Fatalf("unrelated work to a URL: %+v", blocks)
	}
}

func TestParseInput(t *testing.T) {
	ls, err := guard.ParseInput(strings.NewReader("refs/heads/main abc refs/heads/main def\n\n"))
	if err != nil || len(ls) != 1 || ls[0].RemoteSHA != "def" {
		t.Fatalf("%+v %v", ls, err)
	}
	if _, err := guard.ParseInput(strings.NewReader("garbage\n")); err == nil {
		t.Fatal("a malformed line must be an error")
	}
}
