package cli_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func publishedWithForeign(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
	testutil.Commit(te.t, te.pubGit, "feat-x", "their fix", map[string]string{"fix.txt": "fix\n"})
}

func publishedWithConflict(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
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

// M6: a pull that rewrote the feature branch tells the checkout to follow
// it (fetch, reset --keep) and the branches above to restack, before the
// gate, in the JSON next as in the human output.
func TestNextAfterPullFollowsTheRewrittenBranch(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishedWithForeign(te)
	te.mustRun("feature", "start", "feat-y", "--from", "feat-x")
	_, env := te.runJSON("feature", "pull", "feat-x")
	cmds := nextCommands(env)
	want := []string{"git fetch ", "git reset --keep ", "ghostbranch feature restack feat-x", "ghostbranch gate feat-x"}
	if len(cmds) != len(want) {
		t.Fatalf("next = %v", cmds)
	}
	for i, w := range want {
		if !strings.HasPrefix(cmds[i], w) {
			t.Fatalf("next[%d] = %q, want %q...; all %v", i, cmds[i], w, cmds)
		}
	}
	if !strings.HasSuffix(cmds[1], "/feat-x") {
		t.Fatalf("reset target = %q", cmds[1])
	}
}

// I1: an update whose push landed but whose record didn't: promote and
// feature pull point at apply of its plan, never at feature pull's git
// steps or at adopting Ghostbranch's own commits.
func TestNextAfterAnUnrecordedUpdateIsApply(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	te.mustRun("gate", "feat-x")
	_, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	data, _ := env["data"].(map[string]any)
	pl, _ := data["plan"].(map[string]any)
	id, _ := pl["id"].(string)
	if id == "" {
		t.Fatalf("promote --plan = %v", env)
	}
	// apply pushed, then stopped before writing the record.
	if _, err := te.pubGit.Run(t.Context(), "fetch", te.forge.RepoDir("acme/widget-shadow"), "+refs/ghostbranch/plans/"+id+":refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"}, {"feature", "pull", "feat-x"}} {
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		d, _ := e["details"].(map[string]any)
		cmds := nextCommands(env)
		if code == 0 || e["code"] != "PUBLIC_BRANCH_CHANGED" || d["reason"] != "unrecorded_update" || d["plan"] != id ||
			len(cmds) == 0 || !strings.HasPrefix(cmds[0], "ghostbranch apply "+id) || hasNext(env, "git merge") || hasNext(env, "feature pull") {
			t.Fatalf("%v: exit %d, error %v, next %v", args, code, e, cmds)
		}
	}
}

// I3: a preserve update that would drop a commit feature pull brought in
// is refused; next gives the git steps that bring it back, then the gate.
func TestNextAfterAdoptedMissingCherryPicks(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "preserve", "--yes", "--keep")
	sg := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	published, err := sg.MustResolve(t.Context(), "refs/heads/feat-x")
	if err != nil {
		t.Fatal(err)
	}
	theirs := testutil.Commit(t, te.pubGit, "feat-x", "their fix", map[string]string{"fix.txt": "fix\n"})
	te.mustRun("feature", "pull", "feat-x")
	if err := sg.UpdateRef(t.Context(), "refs/heads/feat-x", published); err != nil {
		t.Fatal(err)
	}
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	te.mustRun("gate", "feat-x")
	code, env := te.runJSON("promote", "feat-x", "--mode", "preserve", "--title", "Add feature", "--yes")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	cmds := nextCommands(env)
	if code == 0 || d["reason"] != "adopted_missing" || len(cmds) != 5 || cmds[2] != "git cherry-pick "+theirs ||
		!strings.HasPrefix(cmds[4], "ghostbranch gate feat-x") {
		t.Fatalf("exit %d, error %v, next %v", code, e, cmds)
	}
}

// F-14: next after NO_OPEN_PR depends on why: never published is a promote.
func TestNextAfterNoOpenPR(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, env := te.runJSON("feature", "pull", "feat-x")
	e, _ := env["error"].(map[string]any)
	if code != 2 || e["code"] != "NO_OPEN_PR" || e["retryable"] != false {
		t.Fatalf("exit %d, env %v", code, env)
	}
	if !hasNext(env, "ghostbranch promote feat-x --plan --repo acme/widget") {
		t.Fatalf("next = %v", nextCommands(env))
	}
}
