package engine

import (
	"context"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// guardedEntries rewrites a copy: the entries it was given are T, which the
// leak scan reads and promote publishes (directive 10).
func TestGuardedEntriesNeverChangesItsInput(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	const wf = ".github/workflows/up.yml"
	T := testutil.Commit(t, g, "t", "t", map[string]string{
		wf:          "on: [pull_request]\njobs:\n  a:\n    if: github.repository == 'acme/widget'\n    runs-on: x\n",
		"README.md": "r\n",
	})
	tEntries, err := g.ListTree(ctx, T)
	if err != nil {
		t.Fatal(err)
	}
	before := append([]gitx.TreeEntry(nil), tEntries...)
	names := workflow.Names{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	out, rep, _, err := (&Engine{}).guardedEntries(ctx, g, tEntries, names, "rewrite")
	if err != nil || len(rep.Rewritten) != 1 {
		t.Fatalf("rewrite: %+v %v", rep, err)
	}
	if !reflect.DeepEqual(tEntries, before) {
		t.Fatalf("the input entries changed:\n%+v\nwant\n%+v", tEntries, before)
	}
	for i, en := range out {
		if en.Path == wf && en.SHA == before[i].SHA {
			t.Fatal("the guarded workflow was not rewritten in the copy")
		}
	}
}

// The rewrite uses GitHub's canonical names, so a pair typed in another
// case on another machine makes the same candidate and the same plan.
func TestGuardNamesAreGitHubsCanonicalNames(t *testing.T) {
	t.Parallel()
	pc := &pairCtx{
		pair:   workspace.Pair{Public: "ACME/Widget", Shadow: "acme/WIDGET-shadow"},
		public: forge.Repo{FullName: "acme/widget"},
		shadow: forge.Repo{FullName: "acme/widget-shadow"},
	}
	if got := guardNames(pc); got != (workflow.Names{Public: "acme/widget", Shadow: "acme/widget-shadow"}) {
		t.Fatalf("guardNames = %+v", got)
	}
}
