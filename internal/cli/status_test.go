package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// A drift check that can't run warns DRIFT_UNKNOWN and reports the drift as
// unknown; status still succeeds.
func TestStatusWarnsDriftUnknown(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.shadowCommit("feat-x", map[string]string{"README.md": "# widget, the feature's take\n"})
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"README.md": "# widget, upstream's take\n"})
	te.mustRun("status") // fetches both into the mirror
	ws, err := workspace.Open(te.vars["GHOSTBRANCH_HOME"], "default")
	if err != nil {
		t.Fatal(err)
	}
	dir := ws.MirrorDir(workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"})
	blob, err := gitx.New(dir).Run(t.Context(), "rev-parse", "refs/remotes/shadow/feat-x:README.md")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "objects", blob[:2], blob[2:])); err != nil {
		t.Fatalf("the feature's README blob isn't a loose object in the mirror: %v", err)
	}
	code, env := te.runJSON("status")
	if code != 0 {
		t.Fatalf("status: %d %v", code, env)
	}
	found := false
	ws2, _ := env["warnings"].([]any)
	for _, w := range ws2 {
		m, _ := w.(map[string]any)
		d, _ := m["details"].(map[string]any)
		found = found || (m["code"] == "DRIFT_UNKNOWN" && d["feature"] == "feat-x")
	}
	data, _ := env["data"].(map[string]any)
	fs, _ := data["features"].([]any)
	if !found || len(fs) != 1 {
		t.Fatalf("no DRIFT_UNKNOWN for feat-x: %v", env)
	}
	f, _ := fs[0].(map[string]any)
	if drift, _ := f["drift"].(map[string]any); drift["status"] != "unknown" {
		t.Fatalf("drift = %v", f["drift"])
	}
}
