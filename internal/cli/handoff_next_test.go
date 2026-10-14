package cli_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

var errClosePR = errors.New("connection reset")

// After an archive, next says how to fix the PR: fetch its branch, work on
// it as GitHub has it, push to it; sync closes it after the merge.
func TestNextAfterAnArchive(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	_, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	for _, want := range []string{"git fetch origin feat-x", "git switch --detach origin/feat-x", "git push origin HEAD:feat-x"} {
		if !hasNext(env, want) {
			t.Fatalf("next lacks %q: %v", want, nextCommands(env))
		}
	}
	if _, env = te.runJSON("gate", "feat-x"); !hasNext(env, "git push origin HEAD:feat-x") {
		t.Fatalf("FEATURE_ARCHIVED next = %v", nextCommands(env))
	}
	if _, env = te.runJSON("status"); !hasNext(env, "ghostbranch sync") {
		t.Fatalf("status next = %v", nextCommands(env))
	}
}

// ARCHIVE_INCOMPLETE leads with feature archive. (apply resolves its pair
// without a.pair, so its next steps carry no --repo.)
func TestNextAfterAnIncompleteArchive(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	te.forge.ClosePRErr = errClosePR
	_, env := te.runJSON("apply", te.lastPlanID(), "--yes")
	if cmds := nextCommands(env); len(cmds) == 0 || cmds[0] != "ghostbranch feature archive feat-x" {
		t.Fatalf("next = %v", cmds)
	}
}

// An archived feature merged with commits it never published (R9) is a
// person's close: promote refuses it, so next never offers it (P17).
func TestNextForAnArchivedMergedMovedFeature(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	pr := publishFeature(te, "feat-x", "--keep")
	te.shadowCommit("feat-x", map[string]string{"later.txt": "later\n"})
	te.mustRun("feature", "archive", "feat-x")
	te.forge.SetPRMerged("acme/widget", pr)
	_, env := te.runJSON("status")
	if !hasNext(env, "ghostbranch feature close feat-x") || hasNext(env, "ghostbranch promote") {
		t.Fatalf("next = %v", nextCommands(env))
	}
	for _, n := range nextOf(t, env) {
		if reflect.DeepEqual(argv(n), []string{"ghostbranch", "feature", "close", "feat-x", "--repo", "acme/widget"}) &&
			(n["needs_human"] != true || n["why"] != "its PR merged, but its archive holds commits never published; a person closes it at the console") {
			t.Fatalf("close next = %v", n)
		}
	}
}

// apply <plan> --keep publishes and keeps: after_publish reads kept, the
// shadow branch is still there, and there is no archive ref.
func TestApplyKeepKeepsTheShadowBranch(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	code, env := te.runJSON("apply", te.lastPlanID(), "--yes", "--keep")
	if code != 0 || afterPublish(env)["feat-x"] != "kept" {
		t.Fatalf("apply --keep: exit %d, %v", code, env)
	}
	g := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	if _, ok, err := g.Resolve(t.Context(), "refs/heads/feat-x"); err != nil || !ok {
		t.Fatalf("shadow branch feat-x: present %v, %v", ok, err)
	}
	if _, ok, err := g.Resolve(t.Context(), "refs/ghostbranch/archive/feat-x"); err != nil || ok {
		t.Fatalf("archive ref: present %v, %v", ok, err)
	}
}
