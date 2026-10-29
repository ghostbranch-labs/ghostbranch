package guard_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
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

// trunkConfig is the overlay config of a pair whose base branch is trunk.
const trunkConfig = "public: acme/widget\nshadow: acme/widget-shadow\npublic_branch: trunk\n"

// trunkMirror builds a mirror for a pair on trunk, with a stale public main
// at m. Feature feat-x has commits p and u; public trunk is at p, so p is
// published and u isn't. The overlay branch holds overlay, or there is none
// when overlay is nil. The mirror is also the checkout.
func trunkMirror(t *testing.T, overlay map[string]string) (g *gitx.Git, p, u string) {
	t.Helper()
	ctx := context.Background()
	g = testutil.BareRepo(t, t.TempDir())
	m := testutil.Commit(t, g, "trunk", "m", map[string]string{"a.txt": "a\n"})
	for _, r := range []string{"refs/heads/feat-x", "refs/remotes/public/main", "refs/shadow-meta/features/feat-x"} {
		if err := g.UpdateRef(ctx, r, m); err != nil {
			t.Fatal(err)
		}
	}
	p = testutil.Commit(t, g, "feat-x", "p", map[string]string{"f.txt": "published\n"})
	u = testutil.Commit(t, g, "feat-x", "u", map[string]string{"f.txt": "secret\n"})
	for r, sha := range map[string]string{"refs/remotes/public/trunk": p, "refs/remotes/shadow/feat-x": u} {
		if err := g.UpdateRef(ctx, r, sha); err != nil {
			t.Fatal(err)
		}
	}
	if overlay != nil {
		o := testutil.Commit(t, g, "ghostbranch", "overlay", overlay)
		if err := g.UpdateRef(ctx, "refs/remotes/shadow/ghostbranch", o); err != nil {
			t.Fatal(err)
		}
	}
	return g, p, u
}

// pushedTo checks one push of sha to refs/heads/<branch>, a new branch, and
// returns the refused refs.
func pushedTo(t *testing.T, g *gitx.Git, branch, sha string, f guard.Facts) []string {
	t.Helper()
	line := guard.Line{LocalRef: "refs/heads/" + branch, LocalSHA: sha, RemoteRef: "refs/heads/" + branch, RemoteSHA: zero}
	blocks, err := guard.Check(context.Background(), objects{g}, []guard.Line{line}, f)
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, b := range blocks {
		refs = append(refs, b.Ref)
	}
	return refs
}

// On a trunk pair the guard checks pushes against public trunk, read from
// the overlay, never the stale public main beside it.
func TestGuardUsesBaseBranch(t *testing.T) {
	t.Parallel()
	g, p, u := trunkMirror(t, map[string]string{".ghostbranch.yml": trunkConfig})
	f, err := guard.LoadFacts(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if f.PublicMain != p {
		t.Fatalf("PublicMain %q, want public trunk %q", f.PublicMain, p)
	}
	if got := pushedTo(t, g, "release", p, f); got != nil {
		t.Errorf("a commit in public trunk is published, yet the push was refused: %v", got)
	}
	if got := pushedTo(t, g, "trunk", u, f); !reflect.DeepEqual(got, []string{"refs/heads/trunk"}) {
		t.Errorf("an unpublished feature commit: blocked %v, want refs/heads/trunk", got)
	}
}

// Review Focus 5: an overlay the guard can't read is an error, so the hook
// refuses the push, never a guess at main. An overlay naming a base the
// mirror doesn't have counts every feature commit as unpublished, as an
// older guard does on a master pair: it over-blocks, never allows.
func TestGuardFailsClosedWithoutBase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, c := range []struct{ what, file, body string }{
		{"invalid YAML", ".ghostbranch.yml", "public: [acme/widget\n"},
		{"a key from a newer Ghostbranch", ".ghostbranch.yml", trunkConfig + "from_the_future: 1\n"},
		{"no .ghostbranch.yml", "README.md", "overlay\n"},
	} {
		g, _, _ := trunkMirror(t, map[string]string{c.file: c.body})
		if f, err := guard.LoadFacts(ctx, g); err == nil {
			t.Errorf("%s: LoadFacts must fail, got public main %q", c.what, f.PublicMain)
		}
	}

	g, p, _ := trunkMirror(t, map[string]string{".ghostbranch.yml": trunkConfig})
	if err := g.DeleteRef(ctx, "refs/remotes/public/trunk"); err != nil {
		t.Fatal(err)
	}
	// Public main at p: a guard that fell back to main would let p through.
	if err := g.UpdateRef(ctx, "refs/remotes/public/main", p); err != nil {
		t.Fatal(err)
	}
	f, err := guard.LoadFacts(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if f.PublicMain != "" {
		t.Fatalf("no public trunk in the mirror, yet PublicMain is %q", f.PublicMain)
	}
	if got := pushedTo(t, g, "release", p, f); !reflect.DeepEqual(got, []string{"refs/heads/release"}) {
		t.Errorf("with no public trunk known: blocked %v, want refs/heads/release", got)
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

// staleMirror is the stale-clone tests' mirror: feature feat-y has commits
// a, b and c (head c) on public main, and refs/heads/stale is at b. Tests
// only read it.
type staleMirror struct {
	g       *gitx.Git
	f       guard.Facts
	main, b string
}

var (
	staleOnce sync.Once
	stale     staleMirror
)

// buildStaleMirror builds the stale-clone tests' mirror in dir.
func buildStaleMirror(t testing.TB, dir string) staleMirror {
	t.Helper()
	ctx := context.Background()
	g := testutil.BareRepo(t, dir)
	main := testutil.Commit(t, g, "main", "m", map[string]string{"a.txt": "a\n"})
	for _, r := range []string{"refs/heads/feat-y", "refs/remotes/public/main", "refs/shadow-meta/features/feat-y"} {
		if err := g.UpdateRef(ctx, r, main); err != nil {
			t.Fatal(err)
		}
	}
	testutil.Commit(t, g, "feat-y", "a", map[string]string{"y.txt": "1\n"})
	b := testutil.Commit(t, g, "feat-y", "b", map[string]string{"y.txt": "2\n"})
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
	return staleMirror{g, f, main, b}
}

// sharedStaleMirror builds the mirror once, under fixtureRoot.
func sharedStaleMirror(t *testing.T) staleMirror {
	t.Helper()
	staleOnce.Do(func() { stale = buildStaleMirror(t, filepath.Join(fixtureRoot, "stale-mirror")) })
	if stale.g == nil {
		t.Fatal("the shared stale mirror failed to build; see the test that built it")
	}
	return stale
}

// staleClone gives the test its own checkout that fetched only a and b
// from the shared mirror, on branch spike, with origin/main at public main.
func staleClone(t *testing.T) (dir string, co *guard.Checkout, f guard.Facts, main, b string) {
	t.Helper()
	m := sharedStaleMirror(t)
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	gitIn(t, dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	gitIn(t, dir, "fetch", "-q", filepath.ToSlash(m.g.Dir), "refs/heads/main:refs/remotes/origin/main", "refs/heads/stale:refs/heads/spike")
	gitIn(t, dir, "switch", "-q", "spike")
	co, err := guard.Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return dir, co, m.f, m.main, m.b
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
	t.Parallel()
	_, co, f, _, b := staleClone(t)
	wantBlockedFor(t, "spike at b", checkPush(t, co, "spike", b, zero, f), b)
}

func TestCheckRefusesABranchOffAMidFeatureCommit(t *testing.T) {
	t.Parallel()
	dir, co, f, _, b := staleClone(t)
	d := commitOn(t, dir, "fix", b)
	wantBlockedFor(t, "fix at d", checkPush(t, co, "fix", d, zero, f), b)
}

func TestCheckPassesPublishedAndUnrelatedCommits(t *testing.T) {
	t.Parallel()
	dir, co, f, main, _ := staleClone(t)
	if blocks := checkPush(t, co, "release", main, zero, f); len(blocks) != 0 {
		t.Fatalf("a branch at public main: %+v", blocks)
	}
	other := commitOn(t, dir, "other", main)
	if blocks := checkPush(t, co, "other", other, zero, f); len(blocks) != 0 {
		t.Fatalf("new commits on public main: %+v", blocks)
	}
}

// A remote branch that already holds a feature commit was let through before
// (or the commit was published to it some other way): only what the push adds
// on top is checked, so unrelated work on top passes, while the same commit
// pushed to a new branch is still refused.
func TestCheckPassesUnrelatedWorkOnARemoteAlreadyAtAFeatureCommit(t *testing.T) {
	t.Parallel()
	dir, co, f, _, b := staleClone(t)
	next := commitOn(t, dir, "next", b)
	if blocks := checkPush(t, co, "next", next, b, f); len(blocks) != 0 {
		t.Fatalf("unrelated work on top of a remote already at b: %+v", blocks)
	}
	wantBlockedFor(t, "b itself to a new branch", checkPush(t, co, "next", b, zero, f), b)
}

// Tracking refs are not trusted as public: a remote that fetches from the
// shadow, or any fetch into refs/remotes, can hold feature commits there.
func TestCheckRefusesAFeatureCommitHeldByATrackingRef(t *testing.T) {
	t.Parallel()
	dir, co, f, _, b := staleClone(t)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/feat-y", b)
	gitIn(t, dir, "update-ref", "refs/remotes/origin/shadow/feat-y", b)
	wantBlockedFor(t, "spike at b, with origin/feat-y at b", checkPush(t, co, "spike", b, zero, f), b)
}

// An update to an existing remote branch only lists what is new on top of
// it: feature work on top is refused, unrelated work passes.
func TestCheckUpdatesOfAnExistingRemoteBranch(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	_, co, f, _, b := staleClone(t)
	wantBlockedFor(t, "spike at b over an unknown commit", checkPush(t, co, "spike", b, "1234567890123456789012345678901234567890", f), b)
}

// A tree or blob lists no commits, so it can't be content-checked; it is
// refused rather than waved through.
func TestCheckRefusesARefThatIsNotACommit(t *testing.T) {
	t.Parallel()
	dir, co, f, _, b := staleClone(t)
	tree := gitIn(t, dir, "rev-parse", b+"^{tree}")
	blocks := checkPush(t, co, "files", tree, zero, f)
	if len(blocks) != 1 || !strings.Contains(blocks[0].Reason, "does not point at a commit") {
		t.Fatalf("a tree: %+v", blocks)
	}
}

// countingObjects counts a checkout's rev-lists.
type countingObjects struct {
	guard.Objects
	revLists int
}

func (c *countingObjects) RevList(ctx context.Context, args ...string) ([]string, error) {
	c.revLists++
	return c.Objects.RevList(ctx, args...)
}

// A push of many refs lists its commits once, and still blocks only the
// ref that carries the feature commit.
func TestCheckListsAPushOnce(t *testing.T) {
	t.Parallel()
	dir, co, f, main, b := staleClone(t)
	other := commitOn(t, dir, "other", main)
	objs := &countingObjects{Objects: co}
	lines := []guard.Line{
		{LocalRef: "refs/tags/v1", LocalSHA: main, RemoteRef: "refs/tags/v1", RemoteSHA: zero},
		{LocalRef: "refs/tags/v2", LocalSHA: other, RemoteRef: "refs/tags/v2", RemoteSHA: zero},
		{LocalRef: "refs/heads/spike", LocalSHA: b, RemoteRef: "refs/heads/spike", RemoteSHA: zero},
	}
	blocks, err := guard.Check(context.Background(), objs, lines, f)
	if err != nil || len(blocks) != 1 || blocks[0].Ref != "refs/heads/spike" || !strings.Contains(blocks[0].Reason, b[:12]) {
		t.Fatalf("blocks = %+v %v", blocks, err)
	}
	if objs.revLists != 1 {
		t.Fatalf("%d rev-lists for one push, want 1", objs.revLists)
	}
}

// A commit the remote already holds (here on next, where it was let through
// before) is public whichever ref this push puts it on.
func TestCheckPassesACommitTheRemoteAlreadyHas(t *testing.T) {
	t.Parallel()
	dir, co, f, _, b := staleClone(t)
	next := commitOn(t, dir, "next", b)
	lines := []guard.Line{
		{LocalRef: "refs/heads/next", LocalSHA: next, RemoteRef: "refs/heads/next", RemoteSHA: b},
		{LocalRef: "refs/heads/copy", LocalSHA: b, RemoteRef: "refs/heads/copy", RemoteSHA: zero},
	}
	if blocks, err := guard.Check(context.Background(), co, lines, f); err != nil || len(blocks) != 0 {
		t.Fatalf("blocks = %+v %v", blocks, err)
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

// writeRecord commits m as feature.json on refs/shadow-meta/features/<name>,
// as the engine's mirror holds a record.
func writeRecord(t *testing.T, g *gitx.Git, name string, m feature.Meta) {
	t.Helper()
	writeRaw(t, g, name, feature.Encode(m))
}

// writeRaw commits raw as feature.json on refs/shadow-meta/features/<name>:
// a record that may not parse.
func writeRaw(t *testing.T, g *gitx.Git, name string, raw []byte) {
	t.Helper()
	ctx := context.Background()
	blob, err := g.HashObject(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := g.MakeTree(ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", SHA: blob, Path: feature.MetaFile}})
	if err != nil {
		t.Fatal(err)
	}
	who := gitx.Identity{Name: "bot", Email: "bot@example.com", When: time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)}
	sha, err := g.CommitTree(ctx, tree, nil, "record", who, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/shadow-meta/features/"+name, sha); err != nil {
		t.Fatal(err)
	}
}

// archivedMirror is mirror(t) plus feature arch, handed off: its PR's
// branch arch is at pub (published), its archive ref at priv (a private
// commit never published), and no shadow branch. Feature arch-b is
// archived too, but published under the name of live feature feat-x.
// Feature broken's record is corrupt JSON that claims arch's branch (P12).
// Feature noarch's record is archived, but its archive ref is missing from
// the mirror, so its private commits are unknown and it stays refused.
func archivedMirror(t *testing.T) (g *gitx.Git, main, feat, pub, priv string) {
	t.Helper()
	ctx := context.Background()
	g, main, feat = mirror(t)
	for _, r := range []string{"refs/heads/arch", "refs/heads/arch-private"} {
		if err := g.UpdateRef(ctx, r, main); err != nil {
			t.Fatal(err)
		}
	}
	pub = testutil.Commit(t, g, "arch", "add arch", map[string]string{"arch.txt": "public\n"})
	priv = testutil.Commit(t, g, "arch-private", "add arch, with notes", map[string]string{"arch.txt": "public\n", "notes.md": "internal\n"})
	if err := g.UpdateRef(ctx, "refs/shadow-meta/archive/arch", priv); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, g, "arch", feature.Meta{Name: "arch", Branch: "arch",
		Promoted: &feature.Promoted{PublicPR: 7, Branch: "arch"},
		Archived: &feature.Archived{Ref: "refs/ghostbranch/archive/arch", Head: priv}})
	if err := g.UpdateRef(ctx, "refs/shadow-meta/archive/arch-b", main); err != nil {
		t.Fatal(err)
	}
	writeRecord(t, g, "arch-b", feature.Meta{Name: "arch-b", Branch: "arch-b",
		Promoted: &feature.Promoted{PublicPR: 8, Branch: "feat-x"},
		Archived: &feature.Archived{Ref: "refs/ghostbranch/archive/arch-b", Head: main}})
	writeRecord(t, g, "noarch", feature.Meta{Name: "noarch", Branch: "noarch",
		Promoted: &feature.Promoted{PublicPR: 10, Branch: "noarch"},
		Archived: &feature.Archived{Ref: "refs/ghostbranch/archive/noarch", Head: priv}})
	writeRaw(t, g, "broken", []byte(`{"name":"broken","branch":"broken","promoted":{"public_pr":9,"branch":"arch"},"archived":{"ref":"refs/ghostbranch/archive/broken"`))
	return g, main, feat, pub, priv
}

func TestLoadFactsArchived(t *testing.T) {
	t.Parallel()
	g, _, _, _, priv := archivedMirror(t)
	f, err := guard.LoadFacts(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Archived, map[string]string{"arch": "arch", "feat-x": "arch-b"}) {
		t.Fatalf("archived = %v", f.Archived)
	}
	// R7: the archive's private commit stays refused; it is no head.
	if f.Commits[priv] != "arch" {
		t.Fatalf("the archive's commit %s must stay in Commits: %v", priv[:12], f.Commits)
	}
	if _, ok := f.Heads["arch"]; ok {
		t.Fatal("an archived feature has no head to refuse by ancestry")
	}
	// Review Focus 5: done's record can't be read (its meta ref is a plain
	// commit), so it stays refused by name and is never archived.
	if !f.Names["done"] || f.Archived["done"] != "" {
		t.Fatalf("done: names %v, archived %v", f.Names, f.Archived)
	}
	// P12: broken's record fails to parse, so loadArchive changes nothing:
	// arch's branch still maps to arch (the exact map above), and no
	// branch maps to broken. A fail-open loadArchive fails here.
	for b, n := range f.Archived {
		if n == "broken" {
			t.Fatalf("a record that can't be read archived %s: %v", b, f.Archived)
		}
	}
	if !f.Names["broken"] {
		t.Fatal("broken must stay refused by name")
	}
	// Review fix: noarch's archive ref is missing, so its private commits
	// are unknown; it is never archived and stays refused by name.
	if !f.Names["noarch"] || f.Archived["noarch"] != "" {
		t.Fatalf("noarch: names %v, archived %v", f.Names, f.Archived)
	}
}

func TestCheckArchivedBranch(t *testing.T) {
	t.Parallel()
	g, main, feat, pub, priv := archivedMirror(t)
	ctx := context.Background()
	f, err := guard.LoadFacts(ctx, g)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(ctx, "refs/heads/fix", pub); err != nil {
		t.Fatal(err)
	}
	fix := testutil.Commit(t, g, "fix", "review fix", map[string]string{"fix.txt": "fix\n"})
	if err := g.UpdateRef(ctx, "refs/heads/leak", priv); err != nil {
		t.Fatal(err)
	}
	leak := testutil.Commit(t, g, "leak", "fix on the old shadow branch", map[string]string{"fix.txt": "fix\n"})
	if err := g.UpdateRef(ctx, "refs/heads/carry", feat); err != nil {
		t.Fatal(err)
	}
	carry := testutil.Commit(t, g, "carry", "fix on another feature", map[string]string{"fix.txt": "fix\n"})
	line := func(local, sha, remote, remoteSHA string) guard.Line {
		return guard.Line{LocalRef: local, LocalSHA: sha, RemoteRef: remote, RemoteSHA: remoteSHA}
	}
	for _, c := range []struct {
		name    string
		line    guard.Line
		blocked bool
	}{
		{"a fix on the PR's branch", line("refs/heads/arch", fix, "refs/heads/arch", pub), false},
		{"HEAD:arch", line("HEAD", fix, "refs/heads/arch", pub), false},
		{"the archive's private commit", line("HEAD", leak, "refs/heads/arch", pub), true},
		{"a merge carrying another feature's commit", line("HEAD", carry, "refs/heads/arch", pub), true},
		{"a gate branch", line("HEAD", fix, "refs/heads/ghostbranch-gate/arch", zero), true},
		{"the overlay branch", line("HEAD", main, "refs/heads/ghostbranch", zero), true},
		{"a branch named like a live feature", line("HEAD", fix, "refs/heads/feat-x", zero), true},
		{"another feature's local branch to the archived branch", line("refs/heads/feat-x", fix, "refs/heads/arch", pub), true},
		// P12: features whose records can't be read stay refused by name.
		{"a feature whose record can't be read (done)", line("HEAD", fix, "refs/heads/done", zero), true},
		{"a feature whose record is corrupt (broken)", line("HEAD", fix, "refs/heads/broken", zero), true},
		// Review fix: an archived record whose archive ref is missing.
		{"an archived feature without its archive ref (noarch)", line("refs/heads/noarch", fix, "refs/heads/noarch", zero), true},
	} {
		blocks, err := guard.Check(ctx, objects{g}, []guard.Line{c.line}, f)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if (len(blocks) > 0) != c.blocked {
			t.Errorf("%s: blocks %+v, want blocked %v", c.name, blocks, c.blocked)
		}
	}
}
