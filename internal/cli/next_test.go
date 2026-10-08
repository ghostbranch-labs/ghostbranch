package cli_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// An outdated managed overlay asks for upgrade, on success only.
func TestNextAsksForUpgradeWhenTheOverlayIsOutdated(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	cfg := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), `schedule: "off"`, `schedule: "23 */6 * * *"`, 1)
	te.shadowCommit(engine.OverlayBranch, map[string]string{".ghostbranch.yml": cfg})
	_, env := te.runJSON("audit")
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "upgrade", "--repo", "acme/widget"}) || n[0]["needs_human"] != false {
		t.Fatalf("next = %v", n)
	}
}

// Upgrading moves the overlay branch, which the gate candidate and the plan
// fingerprint include, so a passed gate asks to plan the publish first and
// to upgrade after.
func TestNextPutsUpgradeAfterTheCommandsOwnSteps(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	cfg := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), `schedule: "off"`, `schedule: "23 */6 * * *"`, 1)
	te.shadowCommit(engine.OverlayBranch, map[string]string{".ghostbranch.yml": cfg})
	const afterPlan = "after the plan is applied (upgrade moves the overlay and would make the plan stale)"
	code, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if code != 0 || len(n) != 2 ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "upgrade", "--repo", "acme/widget"}) ||
		!strings.Contains(n[1]["why"].(string), afterPlan) {
		t.Fatalf("exit %d, next = %v", code, n)
	}
	// A new commit makes promote gate again, which reports the overlay.
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	code, env = te.runJSON("promote", "feat-x", "-m", "Add feature", "--plan")
	n = nextOf(t, env)
	if code != 0 || len(n) != 2 || argv(n[0])[1] != "apply" ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "upgrade", "--repo", "acme/widget"}) ||
		!strings.Contains(n[1]["why"].(string), afterPlan) {
		t.Fatalf("promote --plan: exit %d, next = %v", code, n)
	}
}

// upgrade points at the audit only for a finding worth reading, not for the
// billed-minutes note every private shadow repo gets. A push-triggered
// workflow is one: every sync starts it in the shadow repo.
func TestNextAfterUpgradeAsksForTheAuditOnlyOnFindings(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, env := te.runJSON("upgrade")
	if n := nextOf(t, env); len(n) != 0 {
		t.Fatalf("billed minutes only: next = %v", n)
	}
	testutil.Commit(t, te.pubGit, "main", "push ci", map[string]string{".github/workflows/push.yml": "on:\n  push:\n    branches: [main]\njobs:\n  t:\n    runs-on: x\n    steps:\n      - run: echo\n"})
	te.mustRun("sync")
	_, env = te.runJSON("upgrade") // PUSH_TRIGGERS_ON_SYNC, scheduled sync off
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "audit", "--repo", "acme/widget"}) {
		t.Fatalf("next = %v", n)
	}
}

// The audit's text names what happens to a rewritable guard in each mode.
func TestAuditTextFollowsTheGuardMode(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	testutil.Commit(t, te.pubGit, "main", "guarded", map[string]string{".github/workflows/rel.yml": `on: push
jobs:
  r:
    if: github.repository == 'acme/widget'
    runs-on: x
    steps:
      - run: echo hi
`})
	initOnly(te)
	for mode, want := range map[string]string{
		"rewrite": "(rewritten by the gate)",
		"warn":    "(skips in the shadow repo (ci.rewrite_repo_guards: warn))",
		"off":     "(skips in the shadow repo (ci.rewrite_repo_guards: off))",
	} {
		cfg := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), "rewrite_repo_guards: rewrite", "rewrite_repo_guards: "+mode, 1)
		te.shadowCommit(engine.OverlayBranch, map[string]string{".ghostbranch.yml": cfg})
		code, out, errOut := te.run("audit")
		if code != 0 || !strings.Contains(out, "repo guard: .github/workflows/rel.yml job r "+want+"\n") {
			t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", mode, code, out, errOut)
		}
	}
}

// The push hint names the remote that points at the shadow repo, and does
// not say "never origin" when that remote is origin.
func TestFeatureStartPushHintNamesTheRemote(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ remote, free, checkout, why string }{
		{"shadow", "ghostbranch-shadow", "git fetch shadow && git switch feat-x",
			"commit on feat-x, push it with `git push shadow feat-x` (the shadow repo, never origin), then gate it"},
		{"origin", "shadow", "git fetch origin && git switch feat-x",
			"commit on feat-x, push it with `git push origin feat-x` (origin is the shadow repo here), then gate it"},
		{"", "shadow", "git remote add shadow https://github.com/acme/widget-shadow.git && git fetch shadow && git switch feat-x",
			"commit on feat-x, push it to the shadow repo with the remote in data.checkout (never origin), then gate it"},
	} {
		te := newTestEnv(t)
		te.env.ShadowRemote = func(string) (string, string, bool) { return tc.remote, tc.free, true }
		initOnly(te)
		_, env := te.runJSON("feature", "start", "feat-x")
		if got := env["data"].(map[string]any)["checkout"]; got != tc.checkout {
			t.Errorf("%q: checkout = %q", tc.remote, got)
		}
		if n := nextOf(t, env); len(n) != 1 || n[0]["why"] != tc.why {
			t.Errorf("%q: next = %v", tc.remote, n)
		}
	}
}

// A denied create waits on a person: they create the repo and add it to the
// token (steps 1 and 2); the adopt runs after that. People see the steps.
func TestCreateDeniedWaitsForAPersonAndShowsTheSteps(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.CreateForbidden = true
	code, env := te.runJSON("init", "acme/widget")
	n := nextOf(t, env)
	if code != 1 || len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "init", "acme/widget", "--adopt"}) ||
		n[0]["needs_human"] != true ||
		n[0]["why"] != "a person first creates the empty private repo and adds it to the token (error.details.steps 1 and 2); then run this to adopt it" {
		t.Fatalf("exit %d, next = %v", code, n)
	}
	if env["data"].(map[string]any)["created_shadow"] != false {
		t.Fatalf("data = %v", env["data"])
	}
	_, _, errOut := te.run("init", "acme/widget")
	for _, want := range []string{
		"error: the token can't create acme/widget-shadow",
		"\n  1. Create an empty private repo acme/widget-shadow",
		"\n  2. Add acme/widget-shadow to the token's repository access",
		"\n  3. Run ghostbranch init acme/widget --adopt.\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Fatalf("stderr lacks %q:\n%s", want, errOut)
		}
	}
}

// A hand-edited managed file asks for a preview first: upgrade would
// replace the edits.
func TestNextPreviewsAnUpgradeThatReplacesHandEdits(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	editConfig(te, `schedule: "off"`, `schedule: "23 */6 * * *"`)
	te.mustRun("upgrade")
	handEditSync(te)
	_, env := te.runJSON("audit")
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "upgrade", "--plan", "--repo", "acme/widget"}) ||
		n[0]["why"] != "a managed file was edited by hand; preview what upgrade would replace" {
		t.Fatalf("next = %v", n)
	}
	code, out, errOut := te.run("upgrade")
	if code != 0 || !strings.Contains(out, "  replaced hand edits in .github/workflows/ghostbranch-sync.yml\n") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// handEditSync appends a line to the installed sync workflow on the overlay.
func handEditSync(te *testEnv) {
	te.t.Helper()
	sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	raw, ok, err := sh.ShowFile(te.t.Context(), "refs/heads/ghostbranch", ".github/workflows/ghostbranch-sync.yml")
	if err != nil || !ok {
		te.t.Fatalf("read the sync workflow: %v", err)
	}
	te.shadowCommit("ghostbranch", map[string]string{".github/workflows/ghostbranch-sync.yml": string(raw) + "# my tweak\n"})
}
