package guard_test

import (
	"context"
	"errors"
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

// A git error while reading the checkout must fail the check, never let the
// push through as if the feature's commits were absent.
func TestCheckFailsClosedOnGitErrors(t *testing.T) {
	f := guard.Facts{Names: map[string]bool{"feat-x": true}, Heads: map[string]string{"feat-x": strings.Repeat("a", 40)}}
	line := guard.Line{LocalRef: "HEAD", LocalSHA: strings.Repeat("b", 40), RemoteRef: "refs/heads/main", RemoteSHA: zero}
	blocks, err := guard.Check(context.Background(), brokenObjects{}, []guard.Line{line}, f)
	if err == nil {
		t.Fatalf("a git error must be an error, got blocks %v", blocks)
	}
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
	g, _, feat := mirror(t)
	f, err := guard.LoadFacts(context.Background(), g)
	if err != nil || !f.Names["feat-x"] || !f.Names["done"] || f.Heads["feat-x"] != feat {
		t.Fatalf("%+v %v", f, err)
	}
	if _, ok := f.Heads["done"]; ok {
		t.Fatal("a feature already in public main has no private commits to look for")
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
		blocks, err := guard.Check(ctx, g, c.lines, f)
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

func TestParseInput(t *testing.T) {
	ls, err := guard.ParseInput(strings.NewReader("refs/heads/main abc refs/heads/main def\n\n"))
	if err != nil || len(ls) != 1 || ls[0].RemoteSHA != "def" {
		t.Fatalf("%+v %v", ls, err)
	}
	if _, err := guard.ParseInput(strings.NewReader("garbage\n")); err == nil {
		t.Fatal("a malformed line must be an error")
	}
}
