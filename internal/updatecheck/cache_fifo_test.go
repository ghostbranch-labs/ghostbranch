//go:build unix

package updatecheck_test

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
)

// A FIFO in the cache's place reads as empty at once: Load never opens it,
// so no command blocks on it.
func TestLoadNeverBlocksOnAFIFO(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), updatecheck.CacheFile)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan updatecheck.Cache, 1)
	go func() { done <- updatecheck.Load(p) }()
	select {
	case c := <-done:
		if !c.CheckedAt.IsZero() {
			t.Fatalf("loaded %+v", c)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Load blocked on a FIFO")
	}
}
