package cli_test

import (
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// withStack is feat-x with one commit, and feat-y started on it (feature
// start --from) with one commit of its own.
func withStack(te *testEnv) {
	withFeature(te)
	te.mustRun("feature", "start", "feat-y", "--from", "feat-x")
	te.shadowCommit("feat-y", map[string]string{"stacked.txt": "y\n"})
}

// withConflictingStack is withStack where feat-y changes feature.txt and
// feat-x then changes the same line, so restacking feat-y conflicts.
func withConflictingStack(te *testEnv) {
	withStack(te)
	te.shadowCommit("feat-y", map[string]string{"feature.txt": "from y\n"})
	te.shadowCommit("feat-x", map[string]string{"feature.txt": "from x\n"})
}

func shadowMeta(te *testEnv, name string) feature.Meta {
	te.t.Helper()
	raw, ok, err := gitx.New(te.forge.RepoDir("acme/widget-shadow")).ShowFile(te.t.Context(), "refs/ghostbranch/features/"+name, feature.MetaFile)
	if err != nil || !ok {
		te.t.Fatalf("read %s: %v", name, err)
	}
	m, err := feature.Decode(raw)
	if err != nil {
		te.t.Fatal(err)
	}
	return m
}

func argvs(n []map[string]any) [][]string {
	out := make([][]string, len(n))
	for i := range n {
		out[i] = argv(n[i])
	}
	return out
}

func TestFoldNeedsRebaseNextGivesTheGitSteps(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withStack(te)
	te.shadowCommit("feat-x", map[string]string{"later.txt": "later\n"})
	fork := shadowMeta(te, "feat-y").ForkedAt
	code, env := te.runJSON("feature", "fold", "feat-y")
	if code != contract.FoldNeedsRebase.Exit() {
		t.Fatalf("exit %d: %v", code, env)
	}
	want := [][]string{
		{"git", "fetch", "shadow"},
		{"git", "switch", "feat-y"},
		{"git", "rebase", "--onto", "shadow/feat-x", fork, "feat-y"},
		{"git", "push", "--force-with-lease", "shadow", "feat-y"},
		{"ghostbranch", "feature", "fold", "feat-y", "--repo", "acme/widget"},
	}
	if got := argvs(nextOf(t, env)); !reflect.DeepEqual(got, want) {
		t.Fatalf("next = %v", got)
	}
}

func TestRestackConflictNextGivesTheGitStepsThenRestack(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withConflictingStack(te)
	fork := shadowMeta(te, "feat-y").ForkedAt
	code, env := te.runJSON("feature", "restack", "feat-x")
	if code != contract.RebaseConflict.Exit() {
		t.Fatalf("exit %d: %v", code, env)
	}
	got := argvs(nextOf(t, env))
	if len(got) != 5 || !reflect.DeepEqual(got[2], []string{"git", "rebase", "--onto", "shadow/feat-x", fork, "feat-y"}) ||
		!reflect.DeepEqual(got[4], []string{"ghostbranch", "feature", "restack", "feat-x", "--repo", "acme/widget"}) {
		t.Fatalf("next = %v", got)
	}
}

func TestHasChildrenNextOffersTheThreeWaysOut(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withStack(te)
	for _, op := range []string{"abandon", "close"} {
		code, env := te.runJSON("feature", op, "feat-x")
		if code != contract.HasChildren.Exit() {
			t.Fatalf("%s: exit %d: %v", op, code, env)
		}
		n := nextOf(t, env)
		want := [][]string{
			{"ghostbranch", "feature", "fold", "feat-y", "--repo", "acme/widget"},
			{"ghostbranch", "feature", op, "feat-y", "--repo", "acme/widget"},
			{"ghostbranch", "feature", "restack", "feat-y", "--onto", "main", "--repo", "acme/widget"},
		}
		if got := argvs(n); !reflect.DeepEqual(got, want) || n[1]["needs_human"] != (op == "close") {
			t.Fatalf("%s: next = %v", op, n)
		}
	}
}
