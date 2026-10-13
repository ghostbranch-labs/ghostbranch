//go:build !windows

package gitx_test

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// F-28: the mirror holds unreleased shadow commits. git writes a repo's
// files with the umask unless the repo says otherwise;
// core.sharedRepository=0600 (set by Init) makes git keep them to the owner
// whatever the umask. Not parallel: the umask is per process.
func TestInitSharedPrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	ctx := context.Background()
	root := t.TempDir()
	dir := filepath.Join(root, "pairs", "acme__widget", "mirror.git")
	g, err := gitx.Init(ctx, dir, true)
	if err != nil {
		t.Fatal(err)
	}
	if v, err := g.Run(ctx, "config", "--get", "core.sharedRepository"); err != nil || v != "0600" {
		t.Fatalf("core.sharedRepository = %q %v", v, err)
	}
	if err := g.ProtectAttributes(ctx); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, g, "main", "initial", map[string]string{"a.txt": "a\n"})
	work := filepath.Join(root, "pairs", "acme__widget", "work", "gate-1")
	if _, err := g.WorktreeAdd(ctx, work, "main"); err != nil {
		t.Fatal(err)
	}
	// git ignores core.sharedRepository for FETCH_HEAD, a worktree's
	// metadata (worktrees/<id>/, its gitdir and commondir) and the files it
	// checks out: they follow the umask, and are private only because their
	// parents (mirror.git, work) are 0700. So the walk below runs after the
	// worktree is gone, and this pins the parent while it exists.
	if fi, err := os.Stat(filepath.Dir(work)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("the worktree's parent: %v %v", fi, err)
	}
	if err := g.WorktreeRemove(ctx, work); err != nil {
		t.Fatal(err)
	}
	var loose []string
	check := func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if perm := fi.Mode().Perm(); (d.IsDir() && perm != 0o700) || (!d.IsDir() && perm&0o077 != 0) {
			loose = append(loose, fmt.Sprintf("%s %o", p, perm))
		}
		return nil
	}
	for _, top := range []string{filepath.Join(root, "pairs"), filepath.Join(root, "pairs", "acme__widget", "work")} {
		if err := filepath.WalkDir(top, check); err != nil {
			t.Fatal(err)
		}
	}
	if len(loose) > 0 {
		t.Fatalf("open to other users:\n%v", loose)
	}
}
