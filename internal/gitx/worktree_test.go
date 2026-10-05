package gitx_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func bot() gitx.Identity {
	return gitx.Identity{Name: "ghostbranch", Email: "ghostbranch@invalid", When: testutil.Base}
}

func TestRebaseClean(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"f.txt": "f\n", "o.txt": "o\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", base)
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"f.txt": "feature\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"o.txt": "upstream\n"})

	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, feat)
	if err != nil {
		t.Fatal(err)
	}
	if err := wt.Rebase(ctx, main, bot()); err != nil {
		t.Fatal(err)
	}
	r, _ := wt.MustResolve(ctx, "HEAD")
	if parent, _ := g.MustResolve(ctx, r+"^"); parent != main {
		t.Fatalf("rebased parent = %s, want %s", parent, main)
	}
	b, _, _ := g.ShowFile(ctx, r, "o.txt")
	f, _, _ := g.ShowFile(ctx, r, "f.txt")
	if string(b) != "upstream\n" || string(f) != "feature\n" {
		t.Fatalf("rebased content wrong: o=%q f=%q", b, f)
	}
	if err := g.WorktreeRemove(ctx, wtDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatal("worktree dir should be gone")
	}
}

func TestRebaseConflictIsReportedAndAborted(t *testing.T) {
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"f.txt": "f\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", base)
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"f.txt": "feature\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"f.txt": "upstream\n"})

	wtDir := filepath.Join(t.TempDir(), "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, feat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	var ce *gitx.ConflictError
	if err := wt.Rebase(ctx, main, bot()); !errors.As(err, &ce) || !reflect.DeepEqual(ce.Files, []string{"f.txt"}) {
		t.Fatalf("want conflict on f.txt, got %v", err)
	}
	if head, _ := wt.MustResolve(ctx, "HEAD"); head != feat {
		t.Fatalf("after abort HEAD = %s, want %s", head, feat)
	}
}

// Review Focus 2: the user's own git config must not change content,
// require signing, or run hooks.
func TestUserGitConfigCannotChangeContentOrRunHooks(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	hooks := filepath.Join(tmp, "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"pre-rebase", "post-checkout", "post-rewrite"} {
		if err := os.WriteFile(filepath.Join(hooks, h), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	global := filepath.Join(tmp, "gitconfig")
	cfg := "[core]\n\tautocrlf = true\n\thooksPath = " + filepath.ToSlash(hooks) + "\n[commit]\n\tgpgSign = true\n"
	if err := os.WriteFile(global, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)

	g := testutil.BareRepo(t, filepath.Join(tmp, "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"lf.txt": "one\ntwo\n", "o.txt": "o\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", base)
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"lf.txt": "one\ntwo\nthree\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"o.txt": "upstream\n"})
	wantBlob, _ := g.MustResolve(ctx, feat+":lf.txt")

	wtDir := filepath.Join(tmp, "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, feat)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	if err := wt.Rebase(ctx, main, bot()); err != nil {
		t.Fatalf("rebase under hostile user config: %v", err)
	}
	r, _ := wt.MustResolve(ctx, "HEAD")
	if got, _ := g.MustResolve(ctx, r+":lf.txt"); got != wantBlob {
		t.Fatalf("user config changed blob: %s != %s", got, wantBlob)
	}
}
