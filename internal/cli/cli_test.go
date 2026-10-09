package cli_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestFullLoopThroughTheCLI(t *testing.T) {
	t.Parallel()
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
	code, env = te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	data, _ := env["data"].(map[string]any)
	applied, _ := data["applied"].(map[string]any)
	if code != 0 || applied["public_pr"] != float64(1) {
		t.Fatalf("promote: %d %v", code, env)
	}
}

func TestUsageErrorsAreBadFlagWithExit2(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 1 || e["code"] != "AUTH_MISSING" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestActorMustBeWellFormed(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.vars["GHOSTBRANCH_ACTOR"] = "robot"
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 2 || e["field"] != "GHOSTBRANCH_ACTOR" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestHumanOutputAndHelp(t *testing.T) {
	t.Parallel()
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

// Human output says how the history was scanned and will publish, and which
// commits a finding is in.
func TestHumanOutputForHistoryModes(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, out, errOut := te.run("gate", "feat-x")
	if code != 0 || !strings.Contains(out, "  1 commit(s), scanned as preserve\n") {
		t.Fatalf("gate: %d %q %q", code, out, errOut)
	}
	code, out, errOut = te.run("promote", "feat-x", "--plan")
	if code != 0 || !strings.Contains(out, "  preserve: 1 public commit(s)\nPublish it with: ghostbranch apply ") {
		t.Fatalf("promote --plan: %d %q %q", code, out, errOut)
	}
	editConfig(te, "block_patterns: []", `block_patterns: ["JIRA-[0-9]+"]`)
	te.shadowCommitMsg("feat-x", "fix JIRA-1", map[string]string{"b.txt": "b\n"})
	code, out, errOut = te.run("gate", "feat-x")
	if code != 3 || !regexp.MustCompile(`leak f-[0-9a-f]+: .* \(commit\(s\) [0-9a-f]{12}\)\n`).MatchString(out) {
		t.Fatalf("gate with a message leak: %d %q %q", code, out, errOut)
	}
}

func TestPromoteWithoutYesAndNoTTYNeedsApproval(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	code, _, errOut := te.run("promote", "feat-x", "--mode", "squash", "-m", "Add feature")
	if code != 5 || !strings.Contains(errOut, "APPROVAL_REQUIRED") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestPlumbingFailureIsInternalNotUsage(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// DEFERRED (sp2): a malformed actor is reported before auth or network.
func TestActorIsCheckedFirst(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.vars["GHOSTBRANCH_ACTOR"] = "robot"
	code, env := te.runJSON("sync")
	if e, _ := env["error"].(map[string]any); code != 2 || e["field"] != "GHOSTBRANCH_ACTOR" {
		t.Fatalf("%d %v", code, env)
	}
}

// DEFERRED (sp2): --json=true is honoured even when flag parsing fails.
func TestJSONEqualsTrue(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, out, _ := te.run("gate", "--json=true")
	if code != 2 || !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestGuideNeedsNoTokenAndMatchesItsSchema(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	code, out, _ := te.run("guide", "--json")
	if code != 0 {
		t.Fatalf("guide must work without a token: %d", code)
	}
	var env struct{ Data json.RawMessage }
	_ = json.Unmarshal([]byte(out), &env)
	validate(t, "guide.schema.json", env.Data)
	code, human, _ := te.run("guide")
	if code != 0 || !strings.HasPrefix(human, "# Ghostbranch: guide for coding agents") {
		t.Fatalf("%d %q", code, human[:60])
	}
	te.vars["CLAUDE_CONFIG_DIR"] = filepath.Join(te.root, "claude")
	if code, env := te.runJSON("guide", "--install", "claude"); code != 0 {
		t.Fatalf("install: %d %v", code, env)
	}
	if _, err := os.Stat(filepath.Join(te.root, "claude", "skills", "ghostbranch", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := te.run("explain", "LEAK_FOUND"); code != 0 || !strings.Contains(out, "LEAK_FOUND (error, exit 3)") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestNewCommandsNeverPush(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	pairCheckout(te)
	te.vars["CLAUDE_CONFIG_DIR"] = filepath.Join(te.root, "claude")
	var pushes []string
	te.env.OnPush = func(op, remote string, _ []string) { pushes = append(pushes, op+" "+remote) }
	before, _ := te.pubGit.Refs(context.Background(), "refs")
	for _, args := range [][]string{{"guide"}, {"guide", "--install", "codex"}, {"explain", "--all"},
		{"agents", "status"}, {"agents", "allow"}, {"agents", "revoke"}, {"guard", "install"}, {"guard", "status"}, {"guard", "uninstall"}} {
		te.run(args...)
	}
	after, _ := te.pubGit.Refs(context.Background(), "refs")
	if len(pushes) != 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("pushes: %v", pushes)
	}
}

// --all revokes every waiver in the workspace, whatever the repo, so an
// explicit --repo beside it is refused rather than silently ignored.
func TestWaiversRevokeAllRefusesRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	waiveLeak(te)
	code, env := te.runJSON("waivers", "revoke", "--all", "--repo", "acme/widget")
	e, _ := env["error"].(map[string]any)
	if code != 2 || e["code"] != "BAD_FLAG" || e["field"] != "repo" {
		t.Fatalf("%d %v", code, env)
	}
	if b, err := os.ReadFile(te.waiversFile()); err != nil || !strings.Contains(string(b), te.finding) {
		t.Fatalf("the waiver must survive a refused revoke: %v %s", err, b)
	}
}

// sync cancels the workflow runs its push starts in the shadow repo and
// says so; --no-cancel leaves them running.
func TestSyncCancelsPushRunsAndNoCancelSkips(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	up := testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n",
		".github/workflows/push.yml": "on: push\njobs:\n  t:\n    runs-on: x\n    steps:\n      - run: echo\n"})
	te.forge.Runs = map[string][]fake.Run{"acme/widget-shadow": {
		{SHA: up, Run: forge.Run{ID: 7, Workflow: "ci", Status: "queued", URL: "https://github.example/runs/7"}}}}
	code, out, _ := te.run("sync")
	if code != 0 || !strings.Contains(out, "  cancelled 1 push-triggered workflow run(s) in the shadow repo\n") {
		t.Fatalf("%d %q", code, out)
	}
	if got := te.forge.Cancelled["acme/widget-shadow"]; len(got) != 1 || got[0] != 7 {
		t.Fatalf("cancelled = %v", got)
	}

	next := testutil.Commit(t, te.pubGit, "main", "upstream 2", map[string]string{"up.txt": "up 2\n"})
	te.forge.Runs["acme/widget-shadow"] = append(te.forge.Runs["acme/widget-shadow"],
		fake.Run{SHA: next, Run: forge.Run{ID: 8, Workflow: "ci", Status: "queued"}})
	calls := te.forge.PushRunsCalls
	code, env := te.runJSON("sync", "--no-cancel")
	data, _ := env["data"].(map[string]any)
	if runs, ok := data["cancelled_runs"].([]any); code != 0 || !ok || len(runs) != 0 || te.forge.PushRunsCalls != calls {
		t.Fatalf("--no-cancel: %d %v, PushRuns calls %d -> %d", code, env, calls, te.forge.PushRunsCalls)
	}
}

// Without --json, wait reports progress on stderr and the result on stdout.
func TestWaitHumanOutput(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.mustRun("gate", "feat-x", "--detach")
	code, out, errOut := te.run("wait", te.job("feat-x"))
	if code != 0 || !strings.Contains(errOut, "waiting for CI on ") || !strings.Contains(out, "Gate for feat-x: checks passed") {
		t.Fatalf("%d\nstdout: %q\nstderr: %q", code, out, errOut)
	}
}

func TestGateDetachNextIsWait(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, env := te.runJSON("gate", "feat-x", "--detach")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["checks"] != "pending" {
		t.Fatalf("%d %v", code, env)
	}
	want := []string{"ghostbranch", "wait", te.job("feat-x"), "--repo", "acme/widget"}
	if next := nextOf(t, env); !reflect.DeepEqual(argv(next[0]), want) {
		t.Fatalf("next[0] = %v, want %v", argv(next[0]), want)
	}
	code, out, errOut := te.run("gate", "feat-x", "--detach")
	if wantLine := "Gate for feat-x: CI running on candidate "; code != 0 || !strings.Contains(out, wantLine) || !strings.Contains(out, "; job "+te.job("feat-x")+".\n") {
		t.Fatalf("%d\nstdout: %q\nstderr: %q", code, out, errOut)
	}
}

// status and feature show name a pending gate's job, and status offers wait.
func TestPendingGateShowsItsJob(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.mustRun("gate", "feat-x", "--detach")
	job := te.job("feat-x")
	if code, out, _ := te.run("status"); code != 0 || !strings.Contains(out, "pending (job "+job+")") {
		t.Fatalf("status: %d %q", code, out)
	}
	code, env := te.runJSON("status")
	found := false
	for _, n := range nextOf(t, env) {
		found = found || slices.Equal(argv(n), []string{"ghostbranch", "wait", job, "--repo", "acme/widget"})
	}
	if code != 0 || !found {
		t.Fatalf("status --json next has no wait %s: %d %v", job, code, env)
	}
	if code, out, _ := te.run("feature", "show", "feat-x"); code != 0 || !strings.Contains(out, "  gate: pending (job "+job+")\n") {
		t.Fatalf("feature show: %d %q", code, out)
	}
}

// A dry-run wait would wait on CI and then refuse to write: wait refuses it.
func TestWaitRefusesDryRun(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.mustRun("gate", "feat-x", "--detach")
	code, out, _ := te.run("wait", te.job("feat-x"), "--dry-run", "--json")
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("%v: %q", err, out)
	}
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "BAD_FLAG" || e["field"] != "dry-run" {
		t.Fatalf("%d %v", code, env)
	}
}

// wait --json is NDJSON even when its arguments are wrong.
func TestWaitUsageErrorIsOneLine(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, out, _ := te.run("wait", "--json")
	if code != 2 || strings.Count(out, "\n") != 1 || !json.Valid([]byte(out)) {
		t.Fatalf("%d %q", code, out)
	}
}
