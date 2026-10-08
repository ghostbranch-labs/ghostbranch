package gitx_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func bot() gitx.Identity {
	return gitx.Identity{Name: "ghostbranch", Email: "ghostbranch@invalid", When: testutil.Base}
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
	// cherry-pick runs prepare-commit-msg and post-commit; post-commit's
	// exit status is ignored, so each hook also leaves a marker.
	ran := filepath.Join(tmp, "ran")
	for _, h := range []string{"pre-rebase", "post-checkout", "post-rewrite", "prepare-commit-msg", "post-commit"} {
		body := "#!/bin/sh\necho " + h + " >> '" + filepath.ToSlash(ran) + "'\nexit 1\n"
		if err := os.WriteFile(filepath.Join(hooks, h), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The hostile config goes in the system scope, which git still reads:
	// gitx ignores the global scope wholesale, so only the overrides stand
	// between system config and the content git writes.
	system := filepath.Join(tmp, "gitconfig")
	cfg := "[core]\n\tautocrlf = true\n\thooksPath = " + filepath.ToSlash(hooks) + "\n[commit]\n\tgpgSign = true\n"
	if err := os.WriteFile(system, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")

	g := testutil.BareRepo(t, filepath.Join(tmp, "r.git"))
	base := testutil.Commit(t, g, "main", "base", map[string]string{"lf.txt": "a\nb\nc\nd\ne\n"})
	_ = g.UpdateRef(ctx, "refs/heads/feat", base)
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"lf.txt": "A\nb\nc\nd\ne\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"lf.txt": "a\nb\nc\nd\nE\n"})

	wtDir := filepath.Join(tmp, "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	if _, err := wt.CherryPick(ctx, []string{feat}, bot()); err != nil {
		t.Fatalf("replay under hostile user config: %v", err)
	}
	if b, err := os.ReadFile(ran); err == nil {
		t.Fatalf("a hook ran during the replay: %s", b)
	}
	r, _ := wt.MustResolve(ctx, "HEAD")
	// Both sides edited lf.txt, so the replay had to merge through the
	// worktree. autocrlf=true would leak CRs into the result.
	got, _, err := g.ShowFile(ctx, r, "lf.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A\nb\nc\nd\nE\n" {
		t.Fatalf("user config changed merged content: %q", got)
	}
	// The merged file was also written into the worktree: autocrlf=true
	// would have smudged it to CRLF there.
	onDisk, err := os.ReadFile(filepath.Join(wtDir, "lf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("\r")) {
		t.Fatalf("worktree file has CR bytes: %q", onDisk)
	}
}

func TestWorktreeRemoveFallbackRemovesUnregisteredDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	dir := filepath.Join(t.TempDir(), "plain")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := g.WorktreeRemove(ctx, dir); err != nil {
		t.Fatalf("fallback removal should succeed: %v", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("directory should be gone")
	}
	if err := g.WorktreeRemove(ctx, dir); err != nil {
		t.Fatalf("already-deleted dir should not error: %v", err)
	}
}

// Checked-out content is attacker-controlled: feature content, or public
// main, which outside contributors write to. No file in a checkout may
// become git's global config, hooks or attributes, whatever it is named.
func TestCheckoutContentCannotBecomeGitConfigHooksOrAttributes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmp := t.TempDir()
	markers := filepath.Join(tmp, "markers")
	if err := os.MkdirAll(markers, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hooks and core.fsmonitor run through git's own sh (bundled with Git for
	// Windows), so these payloads run on every platform.
	mark := func(name string) string {
		return "echo " + name + " >> '" + filepath.ToSlash(filepath.Join(markers, name)) + "'"
	}
	hook := func(name string) string { return "#!/bin/sh\n" + mark(name) + "\n" }
	hostile := map[string]struct{ mode, content string }{
		// The names gitx once used for its isolation paths under g.Dir.
		"ghostbranch-no-global-config":       {"100644", "[core]\n\tfsmonitor = " + mark("fsmonitor") + "\n"},
		"ghostbranch-no-hooks/post-checkout": {"100755", hook("post-checkout")},
		"ghostbranch-no-hooks/post-rewrite":  {"100755", hook("post-rewrite")},
		// cherry-pick runs these two on every replayed commit.
		"ghostbranch-no-hooks/prepare-commit-msg": {"100755", hook("prepare-commit-msg")},
		"ghostbranch-no-hooks/post-commit":        {"100755", hook("post-commit")},
		"ghostbranch-no-attributes":               {"100644", "*.txt text eol=crlf\n"},
		"lf.txt":                                  {"100644", "a\nb\nc\n"},
	}
	g := testutil.BareRepo(t, filepath.Join(tmp, "r.git"))
	var entries []gitx.TreeEntry
	for path, f := range hostile {
		sha, err := g.HashObject(ctx, []byte(f.content))
		if err != nil {
			t.Fatal(err)
		}
		entries = append(entries, gitx.TreeEntry{Mode: f.mode, Type: "blob", SHA: sha, Path: path})
	}
	tree, err := g.MakeTree(ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	base, err := g.CommitTree(ctx, tree, nil, "hostile base", testutil.Author(0), testutil.Author(0))
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"refs/heads/main", "refs/heads/feat"} {
		if err := g.UpdateRef(ctx, ref, base); err != nil {
			t.Fatal(err)
		}
	}
	feat := testutil.Commit(t, g, "feat", "feature", map[string]string{"feature.txt": "f\n"})
	main := testutil.Commit(t, g, "main", "upstream", map[string]string{"lf.txt": "a\nb\nC\n"})

	wtDir := filepath.Join(tmp, "wt")
	wt, err := g.WorktreeAdd(ctx, wtDir, main)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = g.WorktreeRemove(ctx, wtDir) }()
	if _, err := wt.CherryPick(ctx, []string{feat}, bot()); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if _, err := wt.Run(ctx, "status", "--porcelain"); err != nil { // reads core.fsmonitor
		t.Fatalf("status: %v", err)
	}

	ran, err := os.ReadDir(markers)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ran {
		t.Errorf("checked-out content ran as git config or a hook: %s", m.Name())
	}
	onDisk, err := os.ReadFile(filepath.Join(wtDir, "lf.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(onDisk, []byte("\r")) {
		t.Errorf("checked-out content was applied as attributes: lf.txt = %q", onDisk)
	}
}

// hostileSystemConfig points git's system config at a filter driver and a
// merge driver that append to a sentinel file, and turns on rerere and
// fsmonitor. It returns the sentinel's path.
func hostileSystemConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "ran")
	script := filepath.ToSlash(filepath.Join(dir, "evil.sh"))
	body := "echo ran >> '" + filepath.ToSlash(sentinel) + "'\n" +
		"if [ -n \"$2\" ]; then cp \"$2\" \"$1\"; else cat; fi\n"
	if err := os.WriteFile(filepath.Join(dir, "evil.sh"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "[filter \"evil\"]\n\tclean = sh '" + script + "'\n\tsmudge = sh '" + script + "'\n" +
		"[merge \"evil\"]\n\tdriver = sh '" + script + "' %A %B\n" +
		"[rerere]\n\tenabled = true\n[core]\n\tfsmonitor = true\n"
	sys := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(sys, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", sys)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")
	return sentinel
}

// replayThroughAttributes builds a feature whose .gitattributes names the
// evil drivers, needing a real 3-way merge of a.txt, and replays it.
func replayThroughAttributes(t *testing.T, protect bool) (*gitx.Git, string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	g := testutil.BareRepo(t, filepath.Join(root, "r.git"))
	if protect {
		if err := g.ProtectAttributes(ctx); err != nil {
			t.Fatal(err)
		}
	}
	base := testutil.Commit(t, g, "main", "base", map[string]string{
		".gitattributes": "* filter=evil merge=evil\n", "a.txt": "1\n2\n3\n4\n5\n"})
	if err := g.UpdateRef(ctx, "refs/heads/feat", base); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, g, "main", "upstream", map[string]string{"a.txt": "1 upstream\n2\n3\n4\n5\n"})
	work := testutil.Commit(t, g, "feat", "work", map[string]string{"a.txt": "1\n2\n3\n4\n5 feature\n"})
	wt, err := g.WorktreeAdd(ctx, filepath.Join(root, "wt"), "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.CherryPick(ctx, []string{work}, testutil.Author(9)); err != nil {
		t.Fatal(err)
	}
	head, err := wt.MustResolve(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	return g, head
}

func TestProtectedAttributesStopFiltersAndMergeDrivers(t *testing.T) {
	t.Run("control: the drivers really run without the override", func(t *testing.T) {
		sentinel := hostileSystemConfig(t)
		replayThroughAttributes(t, false)
		if _, err := os.Stat(sentinel); err != nil {
			t.Fatalf("the hostile drivers never ran, so this test proves nothing: %v", err)
		}
	})
	t.Run("protected", func(t *testing.T) {
		sentinel := hostileSystemConfig(t)
		g, head := replayThroughAttributes(t, true)
		if _, err := os.Stat(sentinel); !os.IsNotExist(err) {
			t.Fatalf("a driver ran despite the attribute override (%v)", err)
		}
		got, ok, err := g.ShowFile(context.Background(), head, "a.txt")
		if err != nil || !ok || string(got) != "1 upstream\n2\n3\n4\n5 feature\n" {
			t.Fatalf("a.txt = %q %v: want git's own 3-way merge, byte for byte", got, err)
		}
	})
}

func TestOverridesBeatSystemRerereAndFsmonitor(t *testing.T) {
	hostileSystemConfig(t)
	g := testutil.BareRepo(t, filepath.Join(t.TempDir(), "r.git"))
	for key, want := range map[string]string{"rerere.enabled": "false", "core.fsmonitor": "false"} {
		got, err := g.Run(context.Background(), "config", "--get", key)
		if err != nil || got != want {
			t.Errorf("%s = %q (%v), want %q", key, got, err, want)
		}
	}
}

func TestProtectAttributesWritesTheMirrorFile(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "m.git")
	g := testutil.BareRepo(t, dir)
	if err := g.ProtectAttributes(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "info", "attributes"))
	if err != nil || string(got) != gitx.AttributesOverride {
		t.Fatalf("info/attributes = %q %v", got, err)
	}
}
