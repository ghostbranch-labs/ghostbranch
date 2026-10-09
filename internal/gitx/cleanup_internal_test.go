package gitx

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
)

func isolationDir() string {
	iso.mu.Lock()
	defer iso.mu.Unlock()
	return iso.dir
}

// Not parallel: it removes the process-wide isolation directory, which
// parallel tests would be using. Go runs it before any parallel test resumes.
func TestCleanupRemovesTheIsolationDirectory(t *testing.T) {
	ctx := context.Background()
	if _, err := New(t.TempDir()).Run(ctx, "version"); err != nil {
		t.Fatal(err)
	}
	dir := isolationDir()
	if dir == "" {
		t.Fatal("no isolation directory after a git call")
	}
	if err := Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s is still there: %v", dir, err)
	}
	if err := Cleanup(); err != nil {
		t.Fatalf("a second Cleanup: %v", err)
	}
	// A git call after Cleanup makes a fresh directory.
	if _, err := New(t.TempDir()).Run(ctx, "version"); err != nil {
		t.Fatal(err)
	}
	if again := isolationDir(); again == "" || again == dir {
		t.Fatalf("after Cleanup the directory is %q (was %q)", again, dir)
	}
}
