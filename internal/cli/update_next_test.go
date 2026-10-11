package cli_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func published(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
}

// status shows a PR behind its feature, and one changed on GitHub; next
// gives gate (then promote), or feature pull.
func TestStatusShowsBehindAndChangedPRs(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	published(te)
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	_, env := te.runJSON("status")
	if !hasNext(env, "ghostbranch gate feat-x") {
		t.Fatalf("pr_behind next = %v", nextCommands(env))
	}
	if _, out, _ := te.run("status"); !strings.Contains(out, "(behind)") {
		t.Fatalf("status:\n%s", out)
	}
	te.mustRun("gate", "feat-x")
	if _, env = te.runJSON("status"); !hasNext(env, "ghostbranch promote feat-x") {
		t.Fatalf("gated pr_behind next = %v", nextCommands(env))
	}
	testutil.Commit(te.t, te.pubGit, "feat-x", "their fix", map[string]string{"fix.txt": "fix\n"})
	if _, env = te.runJSON("status"); !hasNext(env, "ghostbranch feature pull feat-x") {
		t.Fatalf("changed on GitHub next = %v", nextCommands(env))
	}
	if _, out, _ := te.run("status"); !strings.Contains(out, "(changed on GitHub)") {
		t.Fatalf("status:\n%s", out)
	}
}

// After a publish, next says to merge on GitHub, then sync.
func TestNextAfterPublishSaysMergeThenSync(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	_, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	if !hasNext(env, "ghostbranch sync") {
		t.Fatalf("after a publish, next = %v", nextCommands(env))
	}
}

// A stack promote without titles: needs_input names every --title-for.
func TestNextNamesEveryMissingTitleFor(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-a")
	te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
	te.shadowCommit("feat-a", map[string]string{"a2.txt": "a2\n"})
	te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
	te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
	te.shadowCommit("feat-b", map[string]string{"b2.txt": "b2\n"})
	te.mustRun("gate", "feat-a")
	te.mustRun("gate", "feat-b")
	_, env := te.runJSON("promote", "feat-a", "feat-b", "--plan")
	ns, _ := env["next"].([]any)
	if len(ns) != 1 {
		t.Fatalf("next = %v", env["next"])
	}
	need, _ := ns[0].(map[string]any)["needs_input"].([]any)
	if len(need) != 2 || need[0] != "--title-for feat-a=<title>" || need[1] != "--title-for feat-b=<title>" {
		t.Fatalf("needs_input = %v", need)
	}
	// The human next prints each placeholder once, as given.
	if _, _, errOut := te.run("promote", "feat-a", "feat-b", "--plan"); !strings.Contains(errOut, "--title-for feat-a=<title> --title-for feat-b=<title>") ||
		strings.Contains(errOut, "<title-for") {
		t.Fatalf("human next:\n%s", errOut)
	}
}

// start --from an ungated parent gates the parent first.
func TestNextAfterStartFromGatesTheParentFirst(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	_, env := te.runJSON("feature", "start", "feat-y", "--from", "feat-x")
	cmds := nextCommands(env)
	if len(cmds) < 2 || !strings.Contains(cmds[0], "gate feat-x") || !strings.Contains(cmds[1], "gate feat-y") {
		t.Fatalf("next = %v", cmds)
	}
}

func TestVersion(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	for _, args := range [][]string{{"version"}, {"--version"}} {
		code, env := te.runJSON(args...)
		data, _ := env["data"].(map[string]any)
		if code != 0 || env["command"] != "version" || data["version"] != "(unknown)" || data["schema_version"] == "" {
			t.Fatalf("%v: exit %d, %v", args, code, env)
		}
	}
	if code, out, _ := te.run("--version"); code != 0 || !strings.HasPrefix(out, "ghostbranch (unknown)") {
		t.Fatalf("--version: %d %q", code, out)
	}
}
