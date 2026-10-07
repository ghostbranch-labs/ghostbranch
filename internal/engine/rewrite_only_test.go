package engine

import (
	"context"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

func TestRewriteOnly(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	names := workflow.Names{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	const wf = ".github/workflows/up.yml"
	src := "on: [pull_request]\njobs:\n  a:\n    if: github.repository == 'acme/widget'\n    runs-on: x\n"
	rw, _, err := workflow.Rewrite(wf, []byte(src), names)
	if err != nil || string(rw) == src {
		t.Fatalf("rewrite: %v", err)
	}
	P := testutil.Commit(t, g, "p", "p", map[string]string{wf: src, "README.md": "r\n"})
	C := testutil.Commit(t, g, "c", "c", map[string]string{wf: string(rw), "README.md": "r\n"})
	F := testutil.Commit(t, g, "f", "f", map[string]string{wf: "on: push\njobs: {}\n", "README.md": "r\n"})
	X := testutil.Commit(t, g, "x", "x", map[string]string{wf: src, "README.md": "changed\n"})
	if !rewriteOnly(ctx, g, C, P, wf, names) {
		t.Fatal("a rewrite-only difference was refused")
	}
	if rewriteOnly(ctx, g, F, P, wf, names) {
		t.Fatal("a forged workflow was accepted")
	}
	if rewriteOnly(ctx, g, X, P, "README.md", names) {
		t.Fatal("a non-workflow path was accepted")
	}
	if rewriteOnly(ctx, g, C, P, ".github/workflows/missing.yml", names) {
		t.Fatal("a path absent from both was accepted")
	}
	// The rewrite under other names is not this pair's rewrite.
	if rewriteOnly(ctx, g, C, P, wf, workflow.Names{Public: "acme/widget", Shadow: "acme/other"}) {
		t.Fatal("a rewrite for another shadow repo was accepted")
	}
	// A guard-free workflow rewrites to itself, so a difference in its mode
	// alone is not a rewrite.
	plain := "on: [pull_request]\njobs:\n  a:\n    runs-on: x\n"
	Q := testutil.Commit(t, g, "q", "q", map[string]string{wf: plain})
	entries, err := g.ListTree(ctx, Q)
	if err != nil {
		t.Fatal(err)
	}
	entries[0].Mode = "100755"
	tree, err := g.MakeTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	M, err := g.CommitTree(ctx, tree, nil, "m", testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if diff, _ := g.DiffNames(ctx, M, Q); len(diff) != 1 {
		t.Fatalf("the mode change is not a difference: %v", diff)
	}
	if rewriteOnly(ctx, g, M, Q, wf, names) {
		t.Fatal("a mode-only difference was accepted")
	}
}
