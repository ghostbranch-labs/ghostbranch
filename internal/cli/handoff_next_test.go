package cli_test

import (
	"errors"
	"reflect"
	"strings"
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

// An archive that stopped partway (its branch in the archive ref, its
// record unmarked) reads branch_gone; while its PR is open, status offers
// to finish it, not a person's close (review M4).
func TestNextForAPartlyArchivedFeature(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	te.forge.ClosePRErr = errClosePR
	te.mustRun("apply", te.lastPlanID(), "--yes")
	te.forge.ClosePRErr = nil
	_, env := te.runJSON("status")
	if !hasNext(env, "ghostbranch feature archive feat-x") || hasNext(env, "feature close feat-x") {
		t.Fatalf("status next = %v", nextCommands(env))
	}
}

// A kept parent's gate with only an archived child above it (R12) offers
// the parent's own promote, and never names the handed-off child as a line
// to promote (review M6).
func TestNextAfterTheGateOfAParentWithAnArchivedChild(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withGatedStack(te)
	grantAgents(te)
	te.mustRun("promote", "feat-a", "feat-b", "--mode", "squash", "--yes", "--keep")
	te.mustRun("feature", "archive", "feat-b")
	te.shadowCommit("feat-a", map[string]string{"a2.txt": "a2\n"})
	_, env := te.runJSON("gate", "feat-a")
	ns, _ := env["next"].([]any)
	if !hasNext(env, "promote feat-a") || len(ns) != 1 {
		t.Fatalf("next = %v", nextCommands(env))
	}
	if why, _ := ns[0].(map[string]any)["why"].(string); strings.Contains(why, "feat-b") {
		t.Fatalf("the gate's next names the handed-off child: %s", why)
	}
}

// Once an archived feature's PR merged or was closed, FEATURE_ARCHIVED and
// feature archive no longer send an agent to push to it: a merged one is
// sync's, a closed one a person's close (review M9, Task 10 minor 1).
func TestNextForAnArchivedFeatureWhosePRIsDone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ state, want string }{
		{"merged", "ghostbranch sync"},
		{"closed", "ghostbranch feature close feat-x"},
	} {
		te := newTestEnv(t)
		archivedFeature(te)
		pr := te.forge.PRs("acme/widget")[0].Number
		if tc.state == "merged" {
			te.forge.SetPRMerged("acme/widget", pr)
		} else {
			te.forge.ClosePR("acme/widget", pr)
		}
		for _, args := range [][]string{{"gate", "feat-x"}, {"feature", "archive", "feat-x"}} {
			_, env := te.runJSON(args...)
			if cmds := nextCommands(env); len(cmds) != 1 || !strings.HasPrefix(cmds[0], tc.want) || hasNext(env, "git push origin") {
				t.Errorf("%s, %v: next = %v", tc.state, args, cmds)
			}
		}
	}
}

// FEATURE_ARCHIVED from feature start --from or feature restack --onto
// sends the run to main, never to the archived feature's PR: start from
// main, or restack onto main, with sync first once that PR merged (final
// review I1). Exit 2 and details stay archivedErr's.
func TestNextForStackingOnAnArchivedFeature(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"open", "merged", "closed"} {
		te := newTestEnv(t)
		archivedFeature(te)
		te.mustRun("feature", "start", "feat-a")
		te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
		pr := te.forge.PRs("acme/widget")[0].Number
		switch state {
		case "merged":
			te.forge.SetPRMerged("acme/widget", pr)
		case "closed":
			te.forge.ClosePR("acme/widget", pr)
		}
		for _, tc := range []struct {
			args []string
			want string
		}{
			{[]string{"feature", "start", "feat-y", "--from", "feat-x"}, "ghostbranch feature start feat-y --json --repo acme/widget"},
			{[]string{"feature", "restack", "feat-a", "--onto", "feat-x"}, "ghostbranch feature restack feat-a --json --onto main --repo acme/widget"},
		} {
			code, env := te.runJSON(tc.args...)
			want := []string{tc.want}
			if state == "merged" {
				want = []string{"ghostbranch sync --repo acme/widget", tc.want}
			}
			errObj, _ := env["error"].(map[string]any)
			details, _ := errObj["details"].(map[string]any)
			if cmds := nextCommands(env); code != 2 || errObj["code"] != "FEATURE_ARCHIVED" ||
				details["feature"] != "feat-x" || details["pr_state"] != state || !reflect.DeepEqual(cmds, want) {
				t.Errorf("%s, %v: exit %d, error %v, next = %v, want %v", state, tc.args, code, errObj, cmds, want)
			}
		}
	}
}
