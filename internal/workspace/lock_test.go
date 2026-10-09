package workspace_test

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// The helper process: with GB_LOCK_HELPER set, take the lock named there,
// print "held", and wait for stdin to close (or exit at once with
// GB_LOCK_EXIT).
func TestMain(m *testing.M) {
	if p := os.Getenv("GB_LOCK_HELPER"); p != "" {
		if _, err := workspace.Lock(p, time.Now(), "helper"); err != nil {
			fmt.Println("error:", err)
			os.Exit(2)
		}
		fmt.Println("held")
		if os.Getenv("GB_LOCK_EXIT") == "" {
			_, _ = io.ReadAll(os.Stdin)
		}
		os.Exit(0) // never unlocks: the OS must
	}
	os.Exit(m.Run())
}

func holdInChild(t *testing.T, path string, exit bool) (*exec.Cmd, io.WriteCloser) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "GB_LOCK_HELPER="+path)
	if exit {
		cmd.Env = append(cmd.Env, "GB_LOCK_EXIT=1")
	}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	if strings.TrimSpace(line) != "held" {
		t.Fatalf("child: %q", line)
	}
	return cmd, in
}

func TestALockHeldByAnotherProcessIsLocked(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "locks", "p.lock")
	cmd, in := holdInChild(t, path, false)
	_, err := workspace.Lock(path, time.Now(), "gate")
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.Locked {
		t.Fatalf("want LOCKED, got %v", err)
	}
	held, _ := ce.Details["held_by"].(map[string]any)
	if held["op"] != "helper" || held["pid"] != cmd.Process.Pid {
		t.Fatalf("held_by = %+v", ce.Details)
	}
	_ = in.Close()
	_ = cmd.Wait()
}

// Review Focus 1.
func TestALockDiesWithItsProcess(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.lock")
	cmd, _ := holdInChild(t, path, true)
	_ = cmd.Wait() // exited without unlocking
	drop, err := workspace.Lock(path, time.Now(), "gate")
	if err != nil {
		t.Fatalf("the lock outlived its process: %v", err)
	}
	drop()
}

func TestTwoHandlesInOneProcessConflict(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.lock")
	drop, err := workspace.Lock(path, time.Now(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Lock(path, time.Now(), "b"); contract.From(err) == nil || contract.From(err).Code != contract.Locked {
		t.Fatalf("second lock: %v", err)
	}
	drop()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("drop deleted the lock file: %v", err)
	}
	drop2, err := workspace.Lock(path, time.Now(), "b")
	if err != nil {
		t.Fatalf("after drop: %v", err)
	}
	drop2()
}

func TestAnOldLockIsStillHeld(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.lock")
	drop, _ := workspace.Lock(path, time.Now().Add(-5*time.Hour), "promote")
	defer drop()
	if _, err := workspace.Lock(path, time.Now(), "gate"); contract.From(err) == nil || contract.From(err).Code != contract.Locked {
		t.Fatalf("a 5 h old held lock was taken over: %v", err)
	}
}

func TestAnOldFormatLockFileIsNotAnError(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.lock")
	_ = os.WriteFile(path, []byte("1234 2026-10-08T10:00:00Z\n"), 0o644) // a 5c lock file, not held
	drop, err := workspace.Lock(path, time.Now(), "gate")
	if err != nil {
		t.Fatalf("an unheld old-format file blocks the lock: %v", err)
	}
	drop()
}

func TestUpdateKeepsConcurrentRegistrations(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	a, _ := workspace.Open(home, "default")
	b, _ := workspace.Open(home, "default") // both loaded before either saved
	if err := a.Update(time.Now(), func(w *workspace.Workspace) bool {
		return w.AddPair(workspace.Pair{Public: "o/a", Shadow: "o/a-shadow"})
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.Update(time.Now(), func(w *workspace.Workspace) bool {
		return w.AddPair(workspace.Pair{Public: "o/b", Shadow: "o/b-shadow"})
	}); err != nil {
		t.Fatal(err)
	}
	fresh, _ := workspace.Open(home, "default")
	if len(fresh.Pairs) != 2 || len(b.Pairs) != 2 {
		t.Fatalf("pairs = %+v (b sees %+v)", fresh.Pairs, b.Pairs)
	}
}
