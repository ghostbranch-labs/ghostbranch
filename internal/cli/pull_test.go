package cli_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func publishedWithForeign(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	testutil.Commit(te.t, te.pubGit, "feat-x", "their fix", map[string]string{"fix.txt": "fix\n"})
}

func publishedWithConflict(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	testutil.Commit(te.t, te.pubGit, "feat-x", "their change", map[string]string{"feature.txt": "theirs\n"})
	te.shadowCommit("feat-x", map[string]string{"feature.txt": "mine\n"})
}

// The refusal points at feature pull; feature pull then points at gate.
// A pull that can't apply gives the person's git steps, then pull again.
func TestNextAfterPublicBranchChanged(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishedWithForeign(te)
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	te.run("gate", "feat-x")
	_, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	if !hasNext(env, "ghostbranch feature pull feat-x") {
		t.Fatalf("after PUBLIC_BRANCH_CHANGED, next = %v", nextCommands(env))
	}
	if _, env = te.runJSON("feature", "pull", "feat-x"); !hasNext(env, "ghostbranch gate feat-x") {
		t.Fatalf("after feature pull, next = %v", nextCommands(env))
	}

	te2 := newTestEnv(t)
	publishedWithConflict(te2)
	_, env = te2.runJSON("feature", "pull", "feat-x")
	cmds := nextCommands(env)
	if len(cmds) != 5 || cmds[2] != "git merge origin/feat-x" || !strings.HasPrefix(cmds[4], "ghostbranch feature pull feat-x") {
		t.Fatalf("by-hand next = %v", cmds)
	}
}
