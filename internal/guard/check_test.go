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

// brokenBases is a checkout where the pushed commit peels, but resolving the
// bases to exclude fails.
type brokenBases struct{ brokenObjects }

func (brokenBases) Resolve(_ context.Context, rev string) (string, bool, error) {
	if strings.HasSuffix(rev, "^{commit}") {
		return strings.TrimSuffix(rev, "^{commit}"), true, nil
	}
	return "", false, errors.New("git: exit status 128")
}

// brokenRevList is a checkout that resolves everything and whose rev-list fails.
type brokenRevList struct{ brokenObjects }

func (brokenRevList) Resolve(_ context.Context, rev string) (string, bool, error) {
	return rev, true, nil
}

// A git error while reading the checkout must fail the check, never let the
// push through as if the feature's commits were absent.
func TestCheckFailsClosedOnGitErrors(t *testing.T) {
	t.Parallel()
	heads := guard.Facts{Names: map[string]bool{"feat-x": true}, Heads: map[string]string{"feat-x": strings.Repeat("a", 40)}}
	commits := guard.Facts{Names: map[string]bool{}, Heads: map[string]string{}, Commits: map[string]string{strings.Repeat("a", 40): "feat-x"},
		PublicMain: strings.Repeat("c", 40)}
	line := guard.Line{LocalRef: "HEAD", LocalSHA: strings.Repeat("b", 40), RemoteRef: "refs/heads/main", RemoteSHA: zero}
	for _, c := range []struct {
		what string
		objs guard.Objects
		f    guard.Facts
	}{
		{"every git call fails", brokenObjects{}, heads},
		{"peeling the pushed commit fails", brokenObjects{}, commits},
		{"resolving a feature head fails", brokenBases{}, heads},
		{"resolving a base fails", brokenBases{}, commits},
		{"rev-list fails", brokenRevList{}, commits},
	} {
		blocks, err := guard.Check(context.Background(), c.objs, []guard.Line{line}, c.f)
		if err == nil {
			t.Errorf("%s: a git error must be an error, got blocks %v", c.what, blocks)
		}
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

// A commit two features share always names the same one: the first by name.
func TestLoadFactsNamesASharedCommitStably(t *testing.T) {
	t.Parallel()
	g, _, feat := mirror(t)
	ctx := context.Background()
	for _, r := range []string{"refs/shadow-meta/features/aaa", "refs/remotes/shadow/aaa", "refs/shadow-meta/features/zzz", "refs/remotes/shadow/zzz"} {
		if err := g.UpdateRef(ctx, r, feat); err != nil {
			t.Fatal(err)
		}
	}
	for range 10 {
		f, err := guard.LoadFacts(ctx, g)
		if err != nil || f.Commits[feat] != "aaa" {
			t.Fatalf("shared commit names %q (%v), want aaa", f.Commits[feat], err)
		}
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
		blocks, err := guard.Check(ctx, objects{g}, c.lines, f)
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

// checkPush runs Check on one push of sha to refs/heads/<branch>, whose
// remote branch is at remoteSHA (zero for a new branch).
func checkPush(t *testing.T, co *guard.Checkout, branch, sha, remoteSHA string, f guard.Facts) []guard.Block {
	t.Helper()
	line := guard.Line{LocalRef: "refs/heads/" + branch, LocalSHA: sha, RemoteRef: "refs/heads/" + branch, RemoteSHA: remoteSHA}
	blocks, err := guard.Check(context.Background(), co, []guard.Line{line}, f)
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

// commitOn makes a new empty commit on a new branch at start, in the checkout.
func commitOn(t *testing.T, dir, branch, start string) string {
	t.Helper()
	gitIn(t, dir, "switch", "-q", "-c", branch, start)
	gitIn(t, dir, "commit", "-q", "--allow-empty", "-m", branch)
	return gitIn(t, dir, "rev-parse", "HEAD")
}

// A stale clone holds an older feature commit than the mirror's head; the
// head check cannot see it, but the commit itself is still unpublished.
func TestCheckRefusesAnOlderFeatureCommitUnderAnotherName(t *testing.T) {
	_, co, f, _, b := staleClone(t)
	wantBlockedFor(t, "spike at b", checkPush(t, co, "spike", b, zero, f), b)
}

func TestCheckRefusesABranchOffAMidFeatureCommit(t *testing.T) {
	dir, co, f, _, b := staleClone(t)
	d := commitOn(t, dir, "fix", b)
	wantBlockedFor(t, "fix at d", checkPush(t, co, "fix", d, zero, f), b)
}

func TestCheckPassesPublishedAndUnrelatedCommits(t *testing.T) {
	dir, co, f, main, _ := staleClone(t)
	if blocks := checkPush(t, co, "release", main, zero, f); len(blocks) != 0 {
		t.Fatalf("a branch at public main: %+v", blocks)
	}
	other := commitOn(t, dir, "other", main)
	if blocks := checkPush(t, co, "other", other, zero, f); len(blocks) != 0 {
		t.Fatalf("new commits on public main: %+v", blocks)
	}
}

// Tracking refs are not trusted as public: a remote that fetches from the
// shadow, or any fetch into refs/remotes, can hold feature commits there.
func TestCheckRefusesAFeatureCommitHeldByATrackingRef(t *testing.T) {
	dir, co, f, _, b := staleClone(t)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/feat-y", b)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/shadow/feat-y", b)
	wantBlockedFor(t, "spike at b, with origin/feat-y at b", checkPush(t, co, "spike", b, zero, f), b)
}

// An update to an existing remote branch only lists what is new on top of
// it: feature work on top is refused, unrelated work passes.
func TestCheckUpdatesOfAnExistingRemoteBranch(t *testing.T) {
	dir, co, f, main, b := staleClone(t)
	topic := commitOn(t, dir, "topic", main)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/topic", topic)
	gitIn(t, dir, "merge", "-q", "--no-edit", b)
	merged := gitIn(t, dir, "rev-parse", "HEAD")
	wantBlockedFor(t, "feature work on topic", checkPush(t, co, "topic", merged, topic, f), b)
	next := commitOn(t, dir, "next", topic)
	if blocks := checkPush(t, co, "topic", next, topic, f); len(blocks) != 0 {
		t.Fatalf("unrelated work on topic: %+v", blocks)
	}
}

// A force push over a remote commit this clone never fetched is still
// checked against public main.
func TestCheckRefusesAForcePushOverAnUnknownCommit(t *testing.T) {
	_, co, f, _, b := staleClone(t)
	wantBlockedFor(t, "spike at b over an unknown commit", checkPush(t, co, "spike", b, "1234567890123456789012345678901234567890", f), b)
}

// A tree or blob lists no commits, so it can't be content-checked; it is
// refused rather than waved through.
func TestCheckRefusesARefThatIsNotACommit(t *testing.T) {
	dir, co, f, _, b := staleClone(t)
	tree := gitIn(t, dir, "rev-parse", b+"^{tree}")
	blocks := checkPush(t, co, "files", tree, zero, f)
	if len(blocks) != 1 || !strings.Contains(blocks[0].Reason, "does not point at a commit") {
		t.Fatalf("a tree: %+v", blocks)
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
