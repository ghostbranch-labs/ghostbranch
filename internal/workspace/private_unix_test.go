//go:build !windows

package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"syscall"
	"testing"
	"time"
)

func modeOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

// New state is private whatever the umask (F-28). Not parallel: the umask
// is per process.
func TestNewStateIsPrivate(t *testing.T) {
	old := syscall.Umask(0o022)
	defer syscall.Umask(old)
	home := filepath.Join(t.TempDir(), "home")
	w, err := Open(home, "default")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Save(); err != nil {
		t.Fatal(err)
	}
	p := Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}
	drop, err := Lock(w.LockPath(p), time.Now(), "gate")
	if err != nil {
		t.Fatal(err)
	}
	drop()
	for _, d := range []string{home, filepath.Join(home, "workspaces"), w.Dir, filepath.Join(w.Dir, "locks")} {
		if m := modeOf(t, d); m != 0o700 {
			t.Errorf("%s is %o, want 700", d, m)
		}
	}
	for _, f := range []string{filepath.Join(w.Dir, "workspace.json"), w.LockPath(p)} {
		if m := modeOf(t, f); m != 0o600 {
			t.Errorf("%s is %o, want 600", f, m)
		}
	}
}

// An install made before 6.3b: Tighten closes the state directories under
// the home, never the home itself, and leaves missing ones alone.
func TestTightenLeavesTheHomeAndMissingDirsAlone(t *testing.T) {
	home := t.TempDir()
	w, _ := Open(home, "default")
	loose := []string{filepath.Join(home, "workspaces"), w.Dir, filepath.Join(w.Dir, "pairs")}
	for _, d := range append([]string{home}, loose...) {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for _, tt := range w.Tighten() {
		if tt.Err != nil {
			t.Fatalf("tighten %s: %v", tt.Dir, tt.Err)
		}
		got = append(got, tt.Dir)
	}
	if !slices.Equal(got, loose) {
		t.Fatalf("tightened %v, want %v", got, loose)
	}
	for _, d := range loose {
		if m := modeOf(t, d); m != 0o700 {
			t.Errorf("%s is %o", d, m)
		}
	}
	if m := modeOf(t, home); m != 0o755 {
		t.Errorf("the home was chmodded to %o", m)
	}
	for _, d := range []string{filepath.Join(w.Dir, "locks"), filepath.Join(home, "locks")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s was created: %v", d, err)
		}
	}
	if again := w.Tighten(); len(again) != 0 {
		t.Fatalf("a second Tighten changed %v", again)
	}
}

// A chmod that fails is reported, not hidden (the engine turns it into a
// note). Swaps the package's chmod: not parallel.
func TestTightenReportsAFailedChmod(t *testing.T) {
	home := t.TempDir()
	w, _ := Open(home, "default")
	if err := os.MkdirAll(w.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(home, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := chmod
	chmod = func(string, os.FileMode) error { return errors.New("operation not permitted") }
	defer func() { chmod = old }()
	got := w.Tighten()
	if len(got) != 1 || got[0].Dir != filepath.Join(home, "workspaces") || got[0].Err == nil {
		t.Fatalf("Tighten = %+v", got)
	}
}
