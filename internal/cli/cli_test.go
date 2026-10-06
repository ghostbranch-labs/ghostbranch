package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestFullLoopThroughTheCLI(t *testing.T) {
	te := newTestEnv(t)
	for _, args := range [][]string{{"init", "acme/widget"}, {"sync"}, {"feature", "start", "feat-x"}} {
		if code, env := te.runJSON(args...); code != 0 || env["ok"] != true {
			t.Fatalf("%v: %d %v", args, code, env)
		}
	}
	te.shadowCommit("feat-x", map[string]string{"feature.txt": "new\n"})
	code, env := te.runJSON("gate", "feat-x")
	if code != 0 || env["command"] != "gate" {
		t.Fatalf("gate: %d %v", code, env)
	}
	grantAgents(te)
	code, env = te.runJSON("promote", "feat-x", "-m", "Add feature", "--yes")
	data, _ := env["data"].(map[string]any)
	applied, _ := data["applied"].(map[string]any)
	if code != 0 || applied["public_pr"] != float64(1) {
		t.Fatalf("promote: %d %v", code, env)
	}
}

func TestUsageErrorsAreBadFlagWithExit2(t *testing.T) {
	te := newTestEnv(t)
	for _, args := range [][]string{{"gate"}, {"--bogus"}, {"frobnicate"}, {"promote", "x", "--mode"}} {
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		if code != 2 || e["code"] != "BAD_FLAG" {
			t.Errorf("%v: %d %v", args, code, env)
		}
	}
}

func TestAuthMissing(t *testing.T) {
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 1 || e["code"] != "AUTH_MISSING" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestActorMustBeWellFormed(t *testing.T) {
	te := newTestEnv(t)
	te.vars["GHOSTBRANCH_ACTOR"] = "robot"
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 2 || e["field"] != "GHOSTBRANCH_ACTOR" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestHumanOutputAndHelp(t *testing.T) {
	te := newTestEnv(t)
	code, out, _ := te.run("init", "acme/widget")
	if code != 0 || !strings.Contains(out, "acme/widget-shadow") || strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("human init output: %d %q", code, out)
	}
	code, out, _ = te.run("--help")
	if code != 0 || !strings.Contains(out, "promote") {
		t.Fatalf("help: %d %q", code, out)
	}
	code, _, errOut := te.run("gate", "no-such-feature")
	if code != 2 || !strings.Contains(errOut, "FEATURE_NOT_FOUND") {
		t.Fatalf("errors go to stderr with their code: %d %q", code, errOut)
	}
}

func TestPromoteWithoutYesAndNoTTYNeedsApproval(t *testing.T) {
	te := newTestEnv(t)
	gatedFeature(te)
	code, _, errOut := te.run("promote", "feat-x", "-m", "Add feature")
	if code != 5 || !strings.Contains(errOut, "APPROVAL_REQUIRED") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestPlumbingFailureIsInternalNotUsage(t *testing.T) {
	te := newTestEnv(t)
	dir := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Corrupt workspace.json makes workspace.Open fail with a plain error.
	if err := os.WriteFile(filepath.Join(dir, "workspace.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 1 || e["code"] != "INTERNAL" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestPlanIsAPreview(t *testing.T) {
	te := newTestEnv(t)
	initOnly(te)
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	code, out, _ := te.run("sync", "--plan")
	if code != 0 || !strings.Contains(out, "Dry run; nothing was changed") || !strings.Contains(out, "refs/heads/main") {
		t.Fatalf("%d %q", code, out)
	}
	code, env := te.runJSON("sync")
	if data, _ := env["data"].(map[string]any); code != 0 || data["action"] != "fast_forward" || data["preview"] != nil {
		t.Fatalf("the preview must not have synced: %d %v", code, env)
	}
}
