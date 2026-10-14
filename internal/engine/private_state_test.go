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
// writes with the umask whatever core.sharedRepository says (R2). FETCH_HEAD,
// worktree metadata and checkout files all follow the umask, under 0700
// parents; no worktree is left by the time this walks.
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

// A home the user chose (GHOSTBRANCH_HOME set) keeps its mode: it may be a
// shared or parent folder. Ghostbranch's own default home is tightened
// like the rest of the state (owner ruling on I2, 2026-10-13).
func TestHomeIsNotChmodded(t *testing.T) {
	withUmask022(t)
	for _, c := range []struct {
		name    string
		ownHome bool
		want    os.FileMode
	}{{"a custom home", false, 0o755}, {"the default home", true, 0o700}} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t)
			h.eng.WS.OwnHome = c.ownHome
			home := h.eng.WS.Home()
			if err := os.MkdirAll(home, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(home, 0o755); err != nil {
				t.Fatal(err)
			}
			h.init()
			rec := h.record()
			h.syncOK(engine.SyncOptions{})
			if m := permOf(t, home); m != c.want {
				t.Fatalf("the home is %o, want %o", m, c.want)
			}
			if m := permOf(t, filepath.Join(home, "workspaces")); m != 0o700 {
				t.Fatalf("workspaces is %o", m)
			}
			// init already tightened the default home; sync says nothing more.
			for _, n := range rec.Notes() {
				if strings.HasPrefix(n, "Made "+home+" ") {
					t.Fatalf("sync tightened the home again: %q", n)
				}
			}
		})
	}
}

// R1: a command that records without opening a pair for a write (agents
// revoke) tightens through the ledger append.
func TestRecordTightensWithoutOpeningAPair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(time.Hour)
	if err := os.Chmod(h.eng.WS.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rec := h.record()
	if _, _, err := h.eng.AgentsRevoke(h.ctx, &h.pair, false); err != nil {
		t.Fatal(err)
	}
	if m := permOf(t, h.eng.WS.Dir); m != 0o700 {
		t.Fatalf("%s is %o", h.eng.WS.Dir, m)
	}
	if want := "Made " + h.eng.WS.Dir + " private (0700)"; !slices.Contains(rec.Notes(), want) {
		t.Fatalf("notes lack %q:\n%s", want, rec.String())
	}
}

// R1: opening a pair for a write tightens before the command works, not
// only at its ledger append: the state is private by its first push.
func TestOpenTightensBeforeTheWork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	if err := os.Chmod(h.eng.WS.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var atPush []os.FileMode
	next := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		atPush = append(atPush, permOf(t, h.eng.WS.Dir))
		if next != nil {
			next(op, remote, specs)
		}
	}
	h.gateFeature("feat-x")
	if len(atPush) == 0 {
		t.Fatal("the gate pushed nothing")
	}
	if atPush[0] != 0o700 {
		t.Fatalf("%s was %o at the gate's first push", h.eng.WS.Dir, atPush[0])
	}
}
