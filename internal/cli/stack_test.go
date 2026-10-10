package cli_test

import (
	"errors"
	"reflect"
	"strings"
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

// feature fold's next: a moved parent is looked at again (worded for a
// fold), and a gate PR left open after the fold landed is closed by a
// person, with the fold's ledger entry.
func TestNextForFeatureFold(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withStack(te)
	raced := false
	te.env.OnPush = func(op, _ string, _ []string) {
		if op == "feature fold" && !raced {
			raced = true
			te.shadowCommit("feat-x", map[string]string{"race.txt": "r\n"})
		}
	}
	code, env := te.runJSON("feature", "fold", "feat-y")
	te.env.OnPush = nil
	n := nextOf(t, env)
	if code != contract.FeatureMoved.Exit() || len(n) != 1 || n[0]["needs_human"] != false ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "show", "feat-y", "--repo", "acme/widget"}) ||
		!strings.Contains(n[0]["why"].(string), "being folded") {
		t.Fatalf("moved: exit %d, next = %v", code, n)
	}

	te = newTestEnv(t)
	withStack(te)
	te.mustRun("gate", "feat-y")
	te.forge.ClosePRErr = errors.New("boom")
	code, env = te.runJSON("feature", "fold", "feat-y")
	te.forge.ClosePRErr = nil
	n = nextOf(t, env)
	data, _ := env["data"].(map[string]any)
	deleted, _ := data["deleted"].(map[string]any)
	if code != contract.ForgeError.Exit() || len(deleted) == 0 || len(n) != 1 || n[0]["needs_human"] != true ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "log", "--feature", "feat-y", "--op", "feature fold", "--repo", "acme/widget"}) {
		t.Fatalf("gate PR left open: exit %d, deleted %v, next = %v", code, deleted, n)
	}
}

// close and fold refuse beside a record that can't be read; next sends a
// person to close that record at the console, then runs this again.
func TestUnreadableRecordNextIsItsConsoleClose(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withStack(te)
	corruptFeature(te, "feat-bad")
	for _, args := range [][]string{{"feature", "close", "feat-x"}, {"feature", "fold", "feat-y"}} {
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		n := nextOf(t, env)
		if code != contract.Internal.Exit() || e["code"] != "INTERNAL" || len(n) != 2 || n[0]["needs_human"] != true ||
			!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "close", "feat-bad", "--repo", "acme/widget"}) ||
			!reflect.DeepEqual(argv(n[1]), append(append([]string{"ghostbranch"}, args...), "--repo", "acme/widget")) {
			t.Fatalf("%v: exit %d, error %v, next = %v", args, code, e, n)
		}
	}
}
