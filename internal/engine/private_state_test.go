//go:build !windows

package engine_test

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// withUmask022 runs the rest of the test under a 022 umask, as most
// systems have. Not parallel: the umask is per process.
func withUmask022(t *testing.T) {
	old := syscall.Umask(0o022)
	t.Cleanup(func() { syscall.Umask(old) })
}

func permOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// looseUnder lists every directory under root that isn't 0700 and every
// file with group or other bits, except mirror.git/FETCH_HEAD, which git
// writes with the umask whatever core.sharedRepository says (R2).
func looseUnder(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		perm := fi.Mode().Perm()
		switch {
		case d.IsDir() && perm != 0o700:
			out = append(out, fmt.Sprintf("%s %o", p, perm))
		case !d.IsDir() && perm&0o077 != 0 && d.Name() != "FETCH_HEAD":
			out = append(out, fmt.Sprintf("%s %o", p, perm))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// F-28: init, a gate and a saved plan leave nothing other users can read.
func TestStateDirsArePrivate(t *testing.T) {
	withUmask022(t)
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.gateFeature("feat-x")
	h.grant(time.Hour)
	if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add x", PlanOnly: true}); err != nil {
		t.Fatal(err)
	}
	if loose := looseUnder(t, h.eng.WS.Home()); len(loose) > 0 {
		t.Fatalf("open to other users:\n%s", strings.Join(loose, "\n"))
	}
	mirror := gitx.New(h.eng.WS.MirrorDir(h.pair))
	if v, err := mirror.Run(h.ctx, "config", "--get", "core.sharedRepository"); err != nil || v != "0600" {
		t.Fatalf("core.sharedRepository = %q %v", v, err)
	}
}

// Decision 4: an install made before 6.3b is made private by the next
// writing command, which says so once.
func TestExistingLooseStateIsTightened(t *testing.T) {
	withUmask022(t)
	h := newHarness(t)
	h.init()
	ws := h.eng.WS
	loose := []string{filepath.Join(ws.Home(), "workspaces"), ws.Dir, filepath.Join(ws.Dir, "pairs"), filepath.Join(ws.Dir, "locks")}
	for _, d := range loose {
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mirror := gitx.New(ws.MirrorDir(h.pair))
	if _, err := mirror.Run(h.ctx, "config", "--unset", "core.sharedRepository"); err != nil {
		t.Fatal(err) // a mirror made before 6.3b
	}
	rec := h.record()
	h.syncOK(engine.SyncOptions{})
	for _, d := range loose {
		if m := permOf(t, d); m != 0o700 {
			t.Errorf("%s is %o", d, m)
		}
		if want := "Made " + d + " private (0700)"; !slices.Contains(rec.Notes(), want) {
			t.Errorf("notes lack %q:\n%s", want, rec.String())
		}
	}
	if v, err := mirror.Run(h.ctx, "config", "--get", "core.sharedRepository"); err != nil || v != "0600" {
		t.Fatalf("core.sharedRepository = %q %v", v, err)
	}
	rec = h.record()
	h.syncOK(engine.SyncOptions{})
	for _, n := range rec.Notes() {
		if strings.HasPrefix(n, "Made ") {
			t.Fatalf("the next command tightened again: %q", n)
		}
	}
}

// A home Ghostbranch didn't create keeps its mode: it may be a shared or
// parent folder.
func TestHomeIsNotChmodded(t *testing.T) {
	withUmask022(t)
	h := newHarness(t)
	home := h.eng.WS.Home()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}
	h.init()
	h.syncOK(engine.SyncOptions{})
	if m := permOf(t, home); m != 0o755 {
		t.Fatalf("the home was chmodded to %o", m)
	}
	if m := permOf(t, filepath.Join(home, "workspaces")); m != 0o700 {
		t.Fatalf("workspaces is %o", m)
	}
}
