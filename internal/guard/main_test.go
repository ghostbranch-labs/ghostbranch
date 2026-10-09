package guard_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// fixtureRoot holds fixtures that tests share, such as the stale-clone mirror.
var fixtureRoot string

// TestMain keeps the developer's git config out of every test in the
// package, once, so tests that read a checkout with plain git can run in
// parallel (t.Setenv forbids that).
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "ghostbranch-guard-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := filepath.Join(root, "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = os.Setenv("GIT_CONFIG_GLOBAL", cfg)
	_ = os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	fixtureRoot = root
	testutil.Main(m, func() { _ = os.RemoveAll(root) })
}
