package cli_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func nextOf(t *testing.T, env map[string]any) []map[string]any {
	t.Helper()
	raw, ok := env["next"].([]any)
	if !ok {
		t.Fatalf("no next in %v", env)
	}
	var out []map[string]any
	for _, n := range raw {
		out = append(out, n.(map[string]any))
	}
	return out
}

func argv(n map[string]any) []string {
	var out []string
	for _, a := range n["command"].([]any) {
		out = append(out, a.(string))
	}
	return out
}

func TestNextWalksTheLoop(t *testing.T) {
	te := newTestEnv(t)
	initOnly(te)
	_, env := te.runJSON("feature", "start", "feat-x")
	if n := nextOf(t, env); len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}) {
		t.Fatalf("after feature start: %v", n)
	}
	te.shadowCommit("feat-x", map[string]string{"feature.txt": "new\n"})
	_, env = te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(n[0]["needs_input"], []any{"--message"}) || n[0]["needs_human"] != false {
		t.Fatalf("after gate: %v", n)
	}
	_, env = te.runJSON("promote", "feat-x", "-m", "Add feature", "--plan")
	n = nextOf(t, env)
	if len(n) != 1 || argv(n[0])[1] != "apply" || n[0]["needs_human"] != true {
		t.Fatalf("after promote --plan: %v", n)
	}
	grantAgents(te)
	_, env = te.runJSON("promote", "feat-x", "-m", "Add feature", "--plan")
	n = nextOf(t, env)
	if a := argv(n[0]); a[len(a)-1] != "--yes" || n[0]["needs_human"] != false {
		t.Fatalf("under a grant: %v", n)
	}
	// grant_needs_yes leaves a saved plan: publish that, rather than run the
	// promote again.
	_, env = te.runJSON("promote", "feat-x", "-m", "Add feature")
	n = nextOf(t, env)
	if a := argv(n[0]); len(n) != 1 || !reflect.DeepEqual(a, []string{"ghostbranch", "apply", te.lastPlanID(), "--yes"}) || n[0]["needs_human"] != false {
		t.Fatalf("grant_needs_yes: %v", n)
	}
}

// A command a person must run goes to their own terminal: without
// --non-interactive (which hides the console) and without --json.
func TestNextForAPersonDropsAgentFlags(t *testing.T) {
	te := newTestEnv(t)
	initOnly(te)
	for _, args := range [][]string{
		{"agents", "allow", "--non-interactive", "--json"},
		{"agents", "allow", "--json=true", "--non-interactive", "--for", "24h"},
	} {
		_, out, errOut := te.run(args...)
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, errOut)
		}
		n := nextOf(t, env)
		want := []string{"ghostbranch", "agents", "allow"}
		if len(args) > 4 {
			want = append(want, "--for", "24h")
		}
		if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), want) || n[0]["needs_human"] != true {
			t.Fatalf("%v: next = %v", args, n)
		}
	}
}

// Every suggestion names the workspace this run used, wherever it came from.
func TestNextCarriesTheWorkspace(t *testing.T) {
	te := newTestEnv(t)
	te.vars["GHOSTBRANCH_WORKSPACE"] = "other"
	te.env.GuardState = func(public string) (bool, bool) { return public == "acme/widget", false }
	initOnly(te)
	_, env := te.runJSON("feature", "start", "feat-x")
	n := nextOf(t, env)
	if len(n) != 2 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget", "--workspace", "other"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "guard", "install", "--workspace", "other"}) {
		t.Fatalf("next = %v", n)
	}
	_, env = te.runJSON("agents", "allow")
	if n := nextOf(t, env); len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "agents", "allow", "--workspace", "other"}) {
		t.Fatalf("agents allow next = %v", n)
	}
}

// A stale plan for another public branch is re-planned for that branch.
func TestNextReplansForThePlansBranch(t *testing.T) {
	te := newTestEnv(t)
	gatedFeature(te)
	te.mustRun("promote", "feat-x", "-m", "Add feature", "--branch", "public-name", "--plan")
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, env := te.runJSON("apply", te.lastPlanID())
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--repo", "acme/widget", "--branch", "public-name"}) {
		t.Fatalf("next = %v", n)
	}
}

func TestNextSuggestsTheGuardInAnUnguardedCheckout(t *testing.T) {
	te := newTestEnv(t)
	te.env.GuardState = func(public string) (bool, bool) { return public == "acme/widget", false }
	_, env := te.runJSON("init", "acme/widget")
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "guard", "install"}) || n[0]["needs_human"] != true {
		t.Fatalf("%v", n)
	}
	te.env.GuardState = func(string) (bool, bool) { return true, true }
	if _, env := te.runJSON("feature", "start", "feat-x"); len(nextOf(t, env)) != 1 {
		t.Fatal("no guard suggestion once it is installed")
	}
}

func TestNextHandsWaivableLeaksToAPerson(t *testing.T) {
	te := newTestEnv(t)
	leakyWithID(te)
	_, m := te.runJSON("gate", "feat-x")
	next, _ := m["next"].([]any)
	if len(next) != 2 {
		t.Fatalf("next = %v", next)
	}
	fix, waive := next[0].(map[string]any), next[1].(map[string]any)
	if !reflect.DeepEqual(fix["command"], []any{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}) || fix["needs_human"] != false {
		t.Fatalf("fix = %v", fix)
	}
	if !reflect.DeepEqual(waive["command"], []any{"ghostbranch", "waivers", "add", "feat-x", te.finding, "--repo", "acme/widget"}) ||
		waive["needs_human"] != true || !reflect.DeepEqual(waive["needs_input"], []any{"--reason"}) {
		t.Fatalf("waive = %v", waive)
	}
}

func TestNextForBadMarkersOnlyFixes(t *testing.T) {
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"app.go": "// END-INTERNAL\n"})
	_, m := te.runJSON("gate", "feat-x")
	next, _ := m["next"].([]any)
	if len(next) != 1 || next[0].(map[string]any)["needs_human"] != false {
		t.Fatalf("next = %v", next)
	}
}

// A leak in text the caller typed is fixed by new text, not by a change on
// the feature branch: the step drops the old text and asks for a message.
func TestNextAsksForNewTextWhenTheMessageLeaks(t *testing.T) {
	te := newTestEnv(t)
	gatedFeature(te)
	leaky := "Add feature\n\nTOKEN=" + testutil.FakeGitHubToken()
	for _, args := range [][]string{
		{"promote", "feat-x", "-m", leaky, "--plan"},
		{"promote", "feat-x", "--message=" + leaky, "--title", "t", "--body=b", "--plan"},
	} {
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		if code == 0 || e["code"] != "LEAK_FOUND" {
			t.Fatalf("%v: %d %v", args, code, env)
		}
		n := nextOf(t, env)
		if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--plan", "--json"}) ||
			!reflect.DeepEqual(n[0]["needs_input"], []any{"--message"}) || !strings.Contains(n[0]["why"].(string), "commit message, title or body") {
			t.Fatalf("%v: next = %v", args, n)
		}
	}
}
