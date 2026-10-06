package cli_test

import (
	"reflect"
	"testing"
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
	_, env = te.runJSON("promote", "feat-x", "-m", "Add feature")
	n = nextOf(t, env)
	if a := argv(n[0]); !reflect.DeepEqual(a, []string{"ghostbranch", "promote", "feat-x", "-m", "Add feature", "--json", "--yes"}) || n[0]["needs_human"] != false {
		t.Fatalf("grant_needs_yes: %v", n)
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
