package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// argAfter is the value after flag in args, or "".
func argAfter(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return ""
}

func needsInput(n map[string]any) []string {
	var out []string
	for _, v := range n["needs_input"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

// applyAfterRevoking revokes the plan's waiver and applies it: LEAK_FOUND.
func applyAfterRevoking(te *testEnv) []map[string]any {
	te.t.Helper()
	id := te.lastPlanID()
	te.mustRun("waivers", "revoke", te.finding)
	code, env := te.runJSON("apply", id, "--yes")
	if e, _ := env["error"].(map[string]any); code != 3 || e["code"] != "LEAK_FOUND" {
		te.t.Fatalf("apply: %d %v", code, env)
	}
	next := nextOf(te.t, env)
	last := next[len(next)-1]
	if a := argv(last); len(a) < 4 || a[1] != "waivers" || a[2] != "add" || !slices.Contains(a, te.finding) || last["needs_human"] != true {
		te.t.Fatalf("the last next isn't a person's waivers add: %v", last)
	}
	return next
}

// A squash finding has no commits: next makes a new plan in the plan's
// mode, on the plan's branch, asking for a new message.
func TestApplyLeakNextReplansInThePlansMode(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	waiveLeak(te)
	te.mustRun("gate", "feat-x")
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add config", "--branch", "add-config", "--plan")
	next := applyAfterRevoking(te)
	a := argv(next[0])
	if len(a) < 6 || !slices.Equal(a[:6], []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan"}) ||
		argAfter(a, "--repo") != "acme/widget" || argAfter(a, "--branch") != "add-config" ||
		!slices.Contains(needsInput(next[0]), "--message") || next[0]["needs_human"] == true {
		t.Fatalf("next[0] = %v", next[0])
	}
}

// A finding in an older preserve commit (added, then deleted by the tip):
// next offers one squash commit, curating it away, or rewriting it.
func TestApplyLeakNextForAnOlderCommit(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withLeakyFeature(te)
	te.shadowCommit("feat-x", map[string]string{"config.env": testutil.Delete, "other.txt": "other\n"})
	te.finding = te.firstWaivable()
	saved := te.env
	atConsole(te, typeTheCode)
	te.mustRun("waivers", "add", "feat-x", te.finding, "--reason", "test fixture")
	te.env = saved
	te.mustRun("gate", "feat-x")
	te.mustRun("promote", "feat-x", "--mode", "preserve", "--title", "Add other", "--plan")
	next := applyAfterRevoking(te)
	want := [][]string{
		{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan"},
		{"ghostbranch", "promote", "feat-x", "--mode", "curated", "--plan"},
		{"ghostbranch", "gate", "feat-x"},
	}
	if len(next) < len(want)+1 {
		t.Fatalf("next = %v", next)
	}
	for i, w := range want {
		if a := argv(next[i]); len(a) < len(w) || !slices.Equal(a[:len(w)], w) || argAfter(a, "--repo") != "acme/widget" {
			t.Errorf("next[%d] = %v, want it to start %v, with --repo acme/widget", i, a, w)
		}
	}
	if !slices.Contains(needsInput(next[0]), "--message") || !slices.Contains(needsInput(next[1]), "--curate") {
		t.Fatalf("needs_input = %v, %v", next[0]["needs_input"], next[1]["needs_input"])
	}
}

// F-18: next after PLAN_NOT_FOUND re-plans the feature whose gate matches,
// on this pair.
func TestNextAfterPlanNotFound(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	id := te.lastPlanID()
	if err := os.Remove(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "plans", id+".json")); err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("apply", id)
	n := nextOf(t, env)
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "PLAN_NOT_FOUND" || len(n) != 1 ||
		!slices.Equal(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--plan", "--repo", "acme/widget"}) {
		t.Fatalf("exit %d, env %v", code, env)
	}
}

// Review M2: with several pairs and no --repo, PLAN_NOT_FOUND's status step
// asks for --repo, so it runs once given; with it, apply looks the plan up.
func TestNextAfterPlanNotFoundWithoutAPair(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	twoPairs(te)
	code, env := te.runJSON("apply", "20261005T120000Z-0a0b0c")
	n := nextOf(t, env)
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "PLAN_NOT_FOUND" || len(n) != 1 ||
		!slices.Equal(argv(n[0]), []string{"ghostbranch", "status"}) || !reflect.DeepEqual(n[0]["needs_input"], []any{"--repo"}) {
		t.Fatalf("exit %d, env %v", code, env)
	}
	if code, out, errOut := te.run("status", "--repo", "acme/widget"); code != 0 {
		t.Fatalf("status --repo: exit %d\n%s\n%s", code, out, errOut)
	}
}

// Review M3: an apply --repo that names a repo that isn't set up says so
// (PAIR_NOT_FOUND, with the init steps), not "pass --repo".
func TestApplyWithAnUnregisteredRepoIsPairNotFound(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	code, env := te.runJSON("apply", "20261005T120000Z-0a0b0c", "--repo", "acme/other")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 2 || e["code"] != "PAIR_NOT_FOUND" || d["public"] != "acme/other" || len(nextOf(t, env)) != 2 {
		t.Fatalf("exit %d, env %v", code, env)
	}
}
