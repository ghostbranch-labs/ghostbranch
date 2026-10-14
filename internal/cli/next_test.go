package cli_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
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
	// One commit, published as itself under the default preserve mode: no
	// input needed.
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "preserve", "--plan", "--repo", "acme/widget"}) ||
		n[0]["needs_input"] != nil && len(n[0]["needs_input"].([]any)) != 0 || n[0]["needs_human"] != false {
		t.Fatalf("after gate: %v", n)
	}
	_, env = te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	n = nextOf(t, env)
	if len(n) != 1 || argv(n[0])[1] != "apply" || n[0]["needs_human"] != true {
		t.Fatalf("after promote --plan: %v", n)
	}
	grantAgents(te)
	_, env = te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	n = nextOf(t, env)
	if a := argv(n[0]); a[len(a)-1] != "--yes" || n[0]["needs_human"] != false {
		t.Fatalf("under a grant: %v", n)
	}
	// grant_needs_yes leaves a saved plan: publish that, rather than run the
	// promote again.
	_, env = te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature")
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
		{"agents", "allow", "--json=true", "--non-interactive", "--for", "1d"},
	} {
		_, out, errOut := te.run(args...)
		var env map[string]any
		if err := json.Unmarshal([]byte(out), &env); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, errOut)
		}
		n := nextOf(t, env)
		want := []string{"ghostbranch", "agents", "allow"}
		if len(args) > 4 {
			want = append(want, "--for", "1d")
		}
		want = append(want, "--repo", "acme/widget")
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
	if n := nextOf(t, env); len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "agents", "allow", "--repo", "acme/widget", "--workspace", "other"}) {
		t.Fatalf("agents allow next = %v", n)
	}
}

// A stale plan for another public branch is re-planned for that branch.
func TestNextReplansForThePlansBranch(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--branch", "public-name", "--plan")
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, env := te.runJSON("apply", te.lastPlanID())
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--repo", "acme/widget", "--branch", "public-name"}) ||
		!reflect.DeepEqual(n[0]["needs_input"], []any{"--message"}) {
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
// the feature branch: the step drops only the texts that leaked, keeps the
// rest, and asks for each one dropped (a body taken from the message leaks
// with it).
func TestNextAsksForNewTextWhenTheMessageLeaks(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	leaky := "Add feature\n\nTOKEN=" + testutil.FakeGitHubToken()
	for _, c := range []struct {
		args []string
		want []string
		need []any
	}{
		{[]string{"promote", "feat-x", "--mode", "squash", "-m", leaky, "--plan"},
			[]string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--json"}, []any{"--message", "--body"}},
		{[]string{"promote", "feat-x", "--mode", "squash", "--message=" + leaky, "--title", "t", "--body=b", "--plan"},
			[]string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--title", "t", "--body=b", "--plan", "--json"}, []any{"--message"}},
	} {
		args := c.args
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		if code == 0 || e["code"] != "LEAK_FOUND" {
			t.Fatalf("%v: %d %v", args, code, env)
		}
		n := nextOf(t, env)
		if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), c.want) ||
			!reflect.DeepEqual(n[0]["needs_input"], c.need) || !strings.Contains(n[0]["why"].(string), "run it again with new text") {
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
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "preserve", "--plan", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "upgrade", "--repo", "acme/widget"}) ||
		!strings.Contains(n[1]["why"].(string), afterPlan) {
		t.Fatalf("exit %d, next = %v", code, n)
	}
	// A new commit makes promote gate again, which reports the overlay.
	te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
	code, env = te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
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

// data.checkout is the argvs that get the branch, checkout_hint the same as
// one line; next's why leads with checking it out, names the remote that
// reaches the shadow repo, and doesn't say "never origin" when that remote
// is origin. The command is still gate.
func TestFeatureStartPushHintNamesTheRemote(t *testing.T) {
	t.Parallel()
	const url = "https://github.com/acme/widget-shadow.git"
	for _, tc := range []struct {
		remote, free string
		in           bool
		checkout     [][]string
		hint, why    string
	}{
		{"shadow", "ghostbranch-shadow", true, [][]string{{"git", "fetch", "shadow"}, {"git", "switch", "feat-x"}},
			"git fetch shadow && git switch feat-x",
			"check out with `git fetch shadow && git switch feat-x`, commit on feat-x, push it with `git push shadow feat-x` (the shadow repo, never origin), then gate it"},
		{"origin", "shadow", true, [][]string{{"git", "fetch", "origin"}, {"git", "switch", "feat-x"}},
			"git fetch origin && git switch feat-x",
			"check out with `git fetch origin && git switch feat-x`, commit on feat-x, push it with `git push origin feat-x` (origin is the shadow repo here), then gate it"},
		{"", "shadow", true, [][]string{{"git", "remote", "add", "shadow", url}, {"git", "fetch", "shadow"}, {"git", "switch", "feat-x"}},
			"git remote add shadow " + url + " && git fetch shadow && git switch feat-x",
			"check out with `git remote add shadow " + url + " && git fetch shadow && git switch feat-x`, commit on feat-x, push it with `git push shadow feat-x` (the shadow repo, never origin), then gate it"},
		{"", "shadow", false, [][]string{{"git", "clone", "--origin", "shadow", "--branch", "feat-x", url}},
			"git clone --origin shadow --branch feat-x " + url,
			"check out with `git clone --origin shadow --branch feat-x " + url + "`, commit on feat-x, push it with `git push shadow feat-x` (the shadow repo, never origin), then gate it"},
	} {
		te := newTestEnv(t)
		te.env.ShadowRemote = func(string) (string, string, bool) { return tc.remote, tc.free, tc.in }
		initOnly(te)
		_, env := te.runJSON("feature", "start", "feat-x")
		data := env["data"].(map[string]any)
		raw, _ := json.Marshal(data["checkout"])
		want, _ := json.Marshal(tc.checkout)
		if string(raw) != string(want) || data["checkout_hint"] != tc.hint {
			t.Errorf("%q: checkout = %s, hint = %q", tc.remote, raw, data["checkout_hint"])
		}
		if n := nextOf(t, env); len(n) != 1 || n[0]["why"] != tc.why ||
			!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}) {
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

// After a green gate, next plans the publish in the configured mode and asks
// for what that mode needs.
func TestNextAfterGateFollowsTheDefaultMode(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"a.txt": "a\n"})
	te.shadowCommit("feat-x", map[string]string{"b.txt": "b\n"})
	_, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "preserve", "--plan", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(n[0]["needs_input"], []any{"--title"}) {
		t.Fatalf("preserve, two commits: %v", n)
	}
	editConfig(te, "default_mode: preserve", "default_mode: curated")
	_, env = te.runJSON("gate", "feat-x")
	if n := nextOf(t, env); !reflect.DeepEqual(n[0]["needs_input"], []any{"--curate", "--title"}) {
		t.Fatalf("curated: %v", n)
	}
	editConfig(te, "default_mode: curated", "default_mode: squash")
	_, env = te.runJSON("gate", "feat-x")
	if n := nextOf(t, env); !reflect.DeepEqual(n[0]["needs_input"], []any{"--message"}) {
		t.Fatalf("squash: %v", n)
	}
}

// A stale plan is re-planned in its own mode, with its title and body.
func TestNextReplanKeepsModeTitleAndBody(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	te.mustRun("promote", "feat-x", "--title", "My title", "--body", "My body", "--plan")
	testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, env := te.runJSON("apply", te.lastPlanID(), "--yes")
	n := nextOf(t, env)
	want := []string{"ghostbranch", "promote", "feat-x", "--mode", "preserve", "--plan", "--repo", "acme/widget", "--title", "My title", "--body", "My body"}
	if need, _ := n[0]["needs_input"].([]any); !reflect.DeepEqual(argv(n[0]), want) || len(need) != 0 {
		t.Fatalf("re-plan: %v", n)
	}
}

// A leak in a feature commit's message is fixed in history, not by typed
// text.
func TestNextForAMessageLeakInHistory(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	editConfig(te, "block_patterns: []", `block_patterns: ["JIRA-[0-9]+"]`)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommitMsg("feat-x", "fix JIRA-1", map[string]string{"a.txt": "a\n"})
	_, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if len(n) != 3 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "--plan", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(n[0]["needs_input"], []any{"--message"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "promote", "feat-x", "--mode", "curated", "--plan", "--repo", "acme/widget"}) ||
		!strings.Contains(n[1]["why"].(string), "new messages") ||
		!reflect.DeepEqual(argv(n[2]), []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}) ||
		!strings.Contains(n[2]["why"].(string), "reword") || !strings.Contains(n[2]["why"].(string), "git push --force-with-lease shadow feat-x") {
		t.Fatalf("history message leak: %v", n)
	}
}

// A secret only in an older commit: the agent may publish so that commit
// never ships, or rewrite it; it is never told to wait for a person to
// change the default mode.
func TestNextForALeakOnlyInAnOlderCommit(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	te.shadowCommit("feat-x", map[string]string{"config.env": testutil.Delete, "a.txt": "a\n"})
	_, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if len(n) < 3 || !reflect.DeepEqual(argv(n[0])[:5], []string{"ghostbranch", "promote", "feat-x", "--mode", "squash"}) ||
		!strings.Contains(n[0]["why"].(string), "only in older commits") ||
		!reflect.DeepEqual(n[1]["needs_input"], []any{"--curate", "--title"}) ||
		!reflect.DeepEqual(argv(n[2])[:3], []string{"ghostbranch", "gate", "feat-x"}) ||
		!strings.Contains(n[2]["why"].(string), "git push --force-with-lease shadow feat-x") {
		t.Fatalf("older-commit leak: %v", n)
	}
	for _, x := range n {
		if strings.Contains(x["why"].(string), "default_mode") {
			t.Fatalf("next must not wait on a person switching the default mode: %v", x)
		}
	}
	// The squash promote it suggests passes the gate and plans.
	code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add a", "--plan")
	if code != 0 {
		t.Fatalf("squash promote: exit %d %v", code, env)
	}
}

// A broken marker only in an older commit gets the same steps, naming
// error.details.commit.
func TestNextForAMarkerOnlyInAnOlderCommit(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"app.go": "ok\n// BEGIN-INTERNAL\nx\n"})
	te.shadowCommit("feat-x", map[string]string{"app.go": "ok\n"})
	_, env := te.runJSON("gate", "feat-x")
	if e, _ := env["error"].(map[string]any); e["code"] != "MARKER_INVALID" {
		t.Fatalf("gate = %v", env)
	}
	n := nextOf(t, env)
	if len(n) != 3 || argv(n[0])[4] != "squash" || argv(n[1])[4] != "curated" || !strings.Contains(n[2]["why"].(string), "error.details.commit") {
		t.Fatalf("older-commit marker: %v", n)
	}
}

// An author leak needs a person to change how authors are published;
// squash, which the publisher authors, is the agent's own way out.
func TestNextForAnAuthorLeak(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	editConfig(te, "block_patterns: []", `block_patterns: ["author@example"]`)
	editConfig(te, "author_email: noreply", "author_email: keep")
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"a.txt": "a\n"})
	_, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	last := n[len(n)-1]
	if argv(n[0])[4] != "squash" || last["needs_human"] != true || !strings.Contains(last["why"].(string), "promote.author_email: noreply") {
		t.Fatalf("author leak: %v", n)
	}
	for _, x := range n {
		if slices.Contains(argv(x), "curated") {
			t.Fatalf("curated keeps each group's first author, so it is no fix: %v", x)
		}
	}
}

// A curated promote's messages come from the --curate file: a leak in them
// asks for a new file (and a title, which came from that message), not a
// change on the feature branch.
func TestNextForALeakInCuratedMessages(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	editConfig(te, "block_patterns: []", `block_patterns: ["JIRA-[0-9]+"]`)
	sha, err := gitx.New(te.forge.RepoDir("acme/widget-shadow")).MustResolve(t.Context(), "refs/heads/feat-x")
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(te.root, "c.json")
	_ = os.WriteFile(file, []byte(`{"commits": [{"from": ["`+sha+`"], "message": "fix JIRA-1"}]}`), 0o644)
	code, env := te.runJSON("promote", "feat-x", "--mode", "curated", "--curate", file, "--plan")
	if e, _ := env["error"].(map[string]any); code != 3 || e["code"] != "LEAK_FOUND" {
		t.Fatalf("exit %d: %v", code, env)
	}
	n := nextOf(t, env)
	if !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "promote", "feat-x", "--mode", "curated", "--plan", "--json"}) ||
		!reflect.DeepEqual(n[0]["needs_input"], []any{"--curate", "--title"}) {
		t.Fatalf("curated message leak: %v", n)
	}
}

// A squash promote whose typed title alone leaks keeps its message: the
// step plus a new --title runs.
func TestNextForASquashTitleLeakIsRunnable(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	editConfig(te, "block_patterns: []", `block_patterns: ["JIRA-[0-9]+"]`)
	_, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--title", "JIRA-1 work", "--plan")
	n := nextOf(t, env)
	want := []string{"ghostbranch", "promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan", "--json"}
	if !reflect.DeepEqual(argv(n[0]), want) || !reflect.DeepEqual(n[0]["needs_input"], []any{"--title"}) {
		t.Fatalf("title leak: %v", n)
	}
	if code, env := te.runJSON(append(argv(n[0])[1:], "--title", "Add feature")...); code != 0 {
		t.Fatalf("the step with a new title: exit %d %v", code, env)
	}
}

// A bad --curate file is replaced, not repeated.
func TestNextForCurateProblems(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	bad := filepath.Join(te.root, "c.json")
	_ = os.WriteFile(bad, []byte(`{"commits": [{"from": ["1234567"], "message": "m"}]}`), 0o644)
	_, env := te.runJSON("promote", "feat-x", "--mode", "curated", "--curate", bad, "--plan")
	n := nextOf(t, env)
	if a := argv(n[0]); slices.Contains(a, "--curate") || !reflect.DeepEqual(n[0]["needs_input"], []any{"--curate"}) {
		t.Fatalf("curate invalid: %v", n)
	}
}

// A pair that isn't registered here (fresh GHOSTBRANCH_HOME) offers adopt
// first, for a shadow repo that already exists, then a fresh init.
func TestNextOffersAdoptWhenThePairIsNotRegistered(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, env := te.runJSON("gate", "feat-x", "--repo", "acme/widget")
	n := nextOf(t, env)
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "PAIR_NOT_FOUND" {
		t.Fatalf("exit %d, env = %v", code, env)
	}
	if len(n) != 2 ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "init", "acme/widget", "--adopt", "--plan", "--json"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "init", "acme/widget", "--plan", "--json"}) ||
		n[0]["needs_human"] != false || n[1]["needs_human"] != false {
		t.Fatalf("next = %v", n)
	}
}

func TestNextAdoptStepsKeepTheWorkspace(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, env := te.runJSON("gate", "feat-x", "--repo", "acme/widget", "--workspace", "other")
	n := nextOf(t, env)
	if code != 2 || len(n) != 2 ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "init", "acme/widget", "--adopt", "--plan", "--json", "--workspace", "other"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "init", "acme/widget", "--plan", "--json", "--workspace", "other"}) {
		t.Fatalf("exit %d, next = %v", code, n)
	}
}

// publishFeature starts, commits, gates and publishes name under an agent
// grant, and returns its public PR number. extra goes on the promote:
// "--keep" for a setup that later works on the shadow branch (Archive is
// the CLI's default, 6.5).
func publishFeature(te *testEnv, name string, extra ...string) int {
	te.t.Helper()
	te.mustRun("feature", "start", name)
	te.shadowCommit(name, map[string]string{name + ".txt": name + "\n"})
	te.mustRun("gate", name)
	args := append([]string{"promote", name, "--mode", "squash", "-m", "Add " + name, "--yes"}, extra...)
	code, env := te.runJSON(args...)
	data, _ := env["data"].(map[string]any)
	applied, _ := data["applied"].(map[string]any)
	pr, ok := applied["public_pr"].(float64)
	if code != 0 || !ok {
		te.t.Fatalf("publish %s: exit %d, %v", name, code, env)
	}
	return int(pr)
}

// status leads with sync when shadow main is behind public main, from its
// SHADOW_BEHIND warning, as every pair command does.
func TestNextForStatusLeadsWithSync(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	code, env := te.runJSON("status")
	n := nextOf(t, env)
	if code != 0 || len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "sync", "--repo", "acme/widget"}) ||
		n[0]["needs_human"] != false {
		t.Fatalf("exit %d, next = %v", code, n)
	}
}

// A merged feature is closed by sync; one merged with newer commits, or
// whose PR was closed, needs a person to close it.
func TestNextForLifecycleStates(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a"))
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-b", "--keep"))
	te.shadowCommit("feat-b", map[string]string{"later.txt": "later\n"})
	te.forge.ClosePR("acme/widget", publishFeature(te, "feat-c"))
	te.mustRun("feature", "start", "feat-d") // draft, clean: nothing to do
	want := []struct {
		cmd   []string
		human bool
		why   string
	}{
		{[]string{"ghostbranch", "sync", "--repo", "acme/widget"}, false, "sync closes merged features; or run `ghostbranch feature close feat-a --repo acme/widget`"},
		{[]string{"ghostbranch", "feature", "close", "feat-b", "--repo", "acme/widget"}, true, "its PR merged but there are newer commits; promote them or a person closes it"},
		{[]string{"ghostbranch", "feature", "close", "feat-c", "--repo", "acme/widget"}, true, "its PR was closed without merging"},
	}
	for _, args := range [][]string{{"status"}, {"feature", "list"}} {
		code, env := te.runJSON(args...)
		n := nextOf(t, env)
		if code != 0 || len(n) != len(want) {
			t.Fatalf("%v: exit %d, next = %v", args, code, n)
		}
		for i, w := range want {
			if !reflect.DeepEqual(argv(n[i]), w.cmd) || n[i]["needs_human"] != w.human || n[i]["why"] != w.why {
				t.Errorf("%v: next[%d] = %v, want %+v", args, i, n[i], w)
			}
		}
	}
	// Behind public main too: one sync does both.
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, env := te.runJSON("status")
	n := nextOf(t, env)
	if len(n) != 3 || !reflect.DeepEqual(argv(n[0]), want[0].cmd) ||
		n[0]["why"] != "shadow main is behind public main; sync it (fast-forward only); "+want[0].why || argv(n[1])[2] != "close" {
		t.Fatalf("behind: next = %v", n)
	}
}

// sync closes the merged feature itself and says so; its next names only
// the features a person must close. --no-close closes nothing.
func TestNextForSyncAttention(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	prA := publishFeature(te, "feat-a")
	te.forge.SetPRMerged("acme/widget", prA)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-b", "--keep"))
	te.shadowCommit("feat-b", map[string]string{"later.txt": "later\n"})
	te.forge.ClosePR("acme/widget", publishFeature(te, "feat-c"))
	want := []struct {
		cmd []string
		why string
	}{
		{[]string{"ghostbranch", "feature", "close", "feat-b", "--repo", "acme/widget"}, "its PR merged but there are newer commits; promote them or a person closes it"},
		{[]string{"ghostbranch", "feature", "close", "feat-c", "--repo", "acme/widget"}, "its PR was closed without merging"},
	}
	check := func(env map[string]any, closed []string, active int, want []struct {
		cmd []string
		why string
	}) {
		t.Helper()
		data, _ := env["data"].(map[string]any)
		var names []string
		cl, ok := data["closed"].([]any)
		if !ok {
			t.Fatalf("data.closed = %v", data["closed"])
		}
		for _, c := range cl {
			names = append(names, c.(map[string]any)["name"].(string))
		}
		if !slices.Equal(names, closed) || data["active_features"] != float64(active) {
			t.Fatalf("closed = %v, active = %v; want %v, %d", names, data["active_features"], closed, active)
		}
		n := nextOf(t, env)
		if len(n) != len(want) {
			t.Fatalf("next = %v", n)
		}
		for i, w := range want {
			if !reflect.DeepEqual(argv(n[i]), w.cmd) || n[i]["needs_human"] != true || n[i]["why"] != w.why {
				t.Errorf("next[%d] = %v, want %+v", i, n[i], w)
			}
		}
	}
	code, env := te.runJSON("sync", "--no-close")
	if code != 0 {
		t.Fatalf("sync --no-close: exit %d %v", code, env)
	}
	check(env, nil, 0, nil) // --no-close doesn't survey
	code, env = te.runJSON("sync")
	if code != 0 {
		t.Fatalf("sync: exit %d %v", code, env)
	}
	check(env, []string{"feat-a"}, 2, want)
	if _, env := te.runJSON("feature", "show", "feat-a"); env["ok"] != false {
		t.Fatalf("feat-a is still there: %v", env)
	}
	// The person sees what sync closed.
	prD := publishFeature(te, "feat-d")
	te.forge.SetPRMerged("acme/widget", prD)
	code, out, _ := te.run("sync")
	if line := fmt.Sprintf("closed merged feature feat-d (PR #%d)", prD); code != 0 || !strings.Contains(out, line) {
		t.Fatalf("exit %d, output lacks %q:\n%s", code, line, out)
	}
}

// A diverged shadow main fails sync with exit 4, but sync still closes the
// merged feature and next still names the features a person must close.
func TestNextForSyncDivergedKeepsAttention(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a"))
	te.forge.ClosePR("acme/widget", publishFeature(te, "feat-c"))
	te.shadowCommit("main", map[string]string{"rogue.txt": "x\n"})
	code, env := te.runJSON("sync")
	e, _ := env["error"].(map[string]any)
	if code != 4 || e["code"] != "SYNC_DIVERGED" {
		t.Fatalf("exit %d %v", code, env)
	}
	data, _ := env["data"].(map[string]any)
	if cl, _ := data["closed"].([]any); len(cl) != 1 || cl[0].(map[string]any)["name"] != "feat-a" {
		t.Fatalf("closed = %v", data["closed"])
	}
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "close", "feat-c", "--repo", "acme/widget"}) ||
		n[0]["needs_human"] != true {
		t.Fatalf("next = %v", n)
	}
	code, out, _ := te.run("sync")
	if code != 4 || !strings.Contains(out, "Shadow main has diverged from public main; nothing was pushed to it.\n") {
		t.Fatalf("exit %d:\n%s", code, out)
	}
}

// A feature whose branch was deleted is closed by sync if its PR merged,
// and otherwise by a person.
func TestNextForLifecycleBranchGone(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a", "--keep"))
	publishFeature(te, "feat-b", "--keep")
	sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	for _, b := range []string{"feat-a", "feat-b"} {
		if err := sh.DeleteRef(t.Context(), "refs/heads/"+b); err != nil {
			t.Fatal(err)
		}
	}
	code, env := te.runJSON("status")
	n := nextOf(t, env)
	if code != 0 || len(n) != 2 ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "sync", "--repo", "acme/widget"}) || n[0]["needs_human"] != false ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "feature", "close", "feat-b", "--repo", "acme/widget"}) || n[1]["needs_human"] != true ||
		n[1]["why"] != "its branch was deleted; a person closes it at the console" {
		t.Fatalf("exit %d, next = %v", code, n)
	}
}

// A feature that no longer merges cleanly onto public main is gated again,
// after the sync that brings public main in.
func TestNextForLifecycleConflicts(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"feature.txt": "other\n"})
	code, env := te.runJSON("status")
	n := nextOf(t, env)
	if code != 0 || len(n) != 2 ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "sync", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}) ||
		n[1]["why"] != "no longer merges cleanly onto public main" {
		t.Fatalf("exit %d, next = %v", code, n)
	}
}

// An outdated managed overlay trails status's next with upgrade, after the
// sync that leads it.
func TestNextForStatusTrailsWithUpgrade(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	scheduled(te)
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, env := te.runJSON("status")
	n := nextOf(t, env)
	if len(n) != 2 || argv(n[0])[1] != "sync" ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "upgrade", "--repo", "acme/widget"}) {
		t.Fatalf("next = %v", n)
	}
}

// status, feature list and feature show print for people, escaping what a
// terminal would act on in commit subjects.
func TestLifecycleHumanOutput(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.shadowCommitMsg("feat-x", "evil \x1b[2J subject", map[string]string{"two.txt": "2\n"})
	code, out, errOut := te.run("status")
	if code != 0 || !strings.Contains(out, "acme/widget -> acme/widget-shadow: shadow main up to date, overlay current, scheduled sync off\n") ||
		!strings.Contains(out, "NAME    STATE  DRIFT  GATE  PR\nfeat-x  draft  clean  -     -\n") {
		t.Fatalf("status: %d\n%s%s", code, out, errOut)
	}
	if code, out, errOut = te.run("feature", "list"); code != 0 || !strings.HasPrefix(out, "NAME    STATE  DRIFT  GATE  PR\nfeat-x  draft") {
		t.Fatalf("feature list: %d\n%s%s", code, out, errOut)
	}
	code, out, errOut = te.run("feature", "show", "feat-x")
	if code != 0 || !strings.HasPrefix(out, "Feature feat-x: draft\n") || !strings.Contains(out, "  gate: never run\n") ||
		!strings.Contains(out, "Commits since public main (2):\n") || !strings.Contains(out, `evil \x1b[2J subject`) ||
		strings.Contains(out, "\x1b") || !strings.Contains(out, "Ledger (1):\n") {
		t.Fatalf("feature show: %d\n%s%s", code, out, errOut)
	}
	te.mustRun("gate", "feat-x")
	if _, out, _ = te.run("feature", "list"); !strings.Contains(out, "feat-x  gate_passed  clean  passed  -\n") {
		t.Fatalf("feature list after gate:\n%s", out)
	}
}

// feature close: a console refusal goes to a person, a moved feature is
// looked at again, and a gate PR left open after the deletes is closed by
// a person on GitHub.
func TestNextForFeatureClose(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.mustRun("feature", "start", "feat-d")
	code, env := te.runJSON("feature", "close", "feat-d", "--non-interactive")
	n := nextOf(t, env)
	if code != 5 || len(n) != 1 || n[0]["needs_human"] != true ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "close", "feat-d", "--repo", "acme/widget"}) {
		t.Fatalf("no console: exit %d, next = %v", code, n)
	}

	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a", "--keep"))
	raced := false
	te.env.OnPush = func(op, _ string, _ []string) {
		if op == "feature close" && !raced {
			raced = true
			te.shadowCommit("feat-a", map[string]string{"race.txt": "r\n"})
		}
	}
	code, env = te.runJSON("feature", "close", "feat-a")
	te.env.OnPush = nil
	n = nextOf(t, env)
	if code != 4 || len(n) != 1 || n[0]["needs_human"] != false ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "show", "feat-a", "--repo", "acme/widget"}) {
		t.Fatalf("moved: exit %d, next = %v", code, n)
	}

	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-c", "--keep"))
	te.forge.ClosePRErr = errors.New("boom")
	code, env = te.runJSON("feature", "close", "feat-c")
	te.forge.ClosePRErr = nil
	n = nextOf(t, env)
	data, _ := env["data"].(map[string]any)
	deleted, _ := data["deleted"].(map[string]any)
	if code != 1 || len(deleted) != 4 || len(n) != 1 || n[0]["needs_human"] != true ||
		!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "log", "--feature", "feat-c", "--op", "feature close", "--repo", "acme/widget"}) {
		t.Fatalf("gate PR left open: exit %d, deleted %v, next = %v", code, deleted, n)
	}
}

// feature close --plan offers the close itself, for a person when the
// feature needs a console and for the agent when it is merged and unmoved.
func TestNextForFeatureClosePlan(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.mustRun("feature", "start", "feat-d")
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a"))
	for _, c := range []struct {
		name  string
		human bool
	}{{"feat-d", true}, {"feat-a", false}} {
		code, env := te.runJSON("feature", "close", c.name, "--plan")
		n := nextOf(t, env)
		data, _ := env["data"].(map[string]any)
		if code != 0 || data["needs_console"] != c.human || len(n) != 1 || n[0]["needs_human"] != c.human ||
			!reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "close", c.name, "--repo", "acme/widget"}) {
			t.Fatalf("%s --plan: exit %d, needs_console %v, next = %v", c.name, code, data["needs_console"], n)
		}
	}
}

// feature start on a stale shadow main fails with exit 4; next syncs, then
// starts the feature again.
func TestFeatureStartBehindNextSyncsThenStartsAgain(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	code, env := te.runJSON("feature", "start", "feat-x", "--repo", "acme/widget")
	e, _ := env["error"].(map[string]any)
	if code != 4 || e["code"] != "SHADOW_BEHIND" {
		t.Fatalf("exit %d %v", code, env)
	}
	n := nextOf(t, env)
	if len(n) != 2 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "sync", "--repo", "acme/widget"}) ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "feature", "start", "feat-x", "--repo", "acme/widget"}) {
		t.Fatalf("next = %v", n)
	}
	// Without --repo, and with --plan: the retry is scoped and keeps --plan.
	_, env = te.runJSON("feature", "start", "feat-x", "--plan")
	if n := nextOf(t, env); len(n) != 2 ||
		!reflect.DeepEqual(argv(n[1]), []string{"ghostbranch", "feature", "start", "feat-x", "--plan", "--repo", "acme/widget"}) {
		t.Fatalf("--plan next = %v", n)
	}
	te.mustRun("sync")
	if code, env := te.runJSON("feature", "start", "feat-x"); code != 0 {
		t.Fatalf("start after sync: exit %d %v", code, env)
	}
}

// A bare sync with no feature in progress offers feature start, with the
// name as needs_input; otherwise it doesn't.
func TestSyncNextOffersFeatureStartWhenIdle(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	start := []string{"ghostbranch", "feature", "start", "--repo", "acme/widget"}
	offers := func(args ...string) bool {
		t.Helper()
		code, env := te.runJSON(args...)
		if code != 0 {
			t.Fatalf("%v: exit %d %v", args, code, env)
		}
		for _, n := range nextOf(t, env) {
			if reflect.DeepEqual(argv(n), start) {
				need, _ := n["needs_input"].([]any)
				why, _ := n["why"].(string)
				if len(need) != 1 || need[0] != "<name>" || !strings.Contains(why, "<name>") || n["needs_human"] != false {
					t.Fatalf("feature start next = %v", n)
				}
				return true
			}
		}
		return false
	}
	if !offers("sync") {
		t.Fatal("an idle bare sync must offer feature start")
	}
	if offers("sync", "--plan") || offers("sync", "--no-close") || offers("sync", "--no-close=true") {
		t.Fatal("only a sync that surveyed the features offers feature start")
	}
	if !offers("sync", "--no-close=false") {
		t.Fatal("--no-close=false is a bare sync")
	}
	te.mustRun("feature", "start", "feat-x")
	if offers("sync") || offers("sync", "--no-close=true") {
		t.Fatal("a feature is in progress: no feature start")
	}
}

// --all with --repo revokes only that repo's grants.
func TestAgentsRevokeAllWithRepoKeepsOtherPairs(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	ws := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default")
	s := grant.NewStore(filepath.Join(ws, "grants.json"), filepath.Join(ws, "grants.lock"))
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	exp := now.Add(6 * time.Hour)
	if _, err := s.Put(grant.Grant{ID: "g-0d0e0f", Public: "acme/other", Shadow: "acme/other-shadow",
		CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:octo"}, now); err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("agents", "revoke", "--all", "--repo", "acme/widget")
	data, _ := env["data"].(map[string]any)
	revoked, _ := data["revoked"].([]any)
	if code != 0 || len(revoked) != 1 || revoked[0].(map[string]any)["public"] != "acme/widget" {
		t.Fatalf("exit %d %v", code, env)
	}
	gs, err := s.Load()
	if err != nil || len(gs) != 1 || gs[0].Public != "acme/other" {
		t.Fatalf("grants left = %+v %v", gs, err)
	}
}

// abandon (or --undo) that changes nothing says so, in JSON and to people.
func TestAbandonThatChangesNothingSaysSo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, env := te.runJSON("feature", "abandon", "feat-x", "--undo")
	if data, _ := env["data"].(map[string]any); code != 0 || data["changed"] != false || data["abandoned"] != false {
		t.Fatalf("exit %d %v", code, env)
	}
	if code, out, _ := te.run("feature", "abandon", "feat-x", "--undo"); code != 0 || out != "feat-x was not abandoned; nothing changed.\n" {
		t.Fatalf("exit %d %q", code, out)
	}
	te.mustRun("feature", "abandon", "feat-x")
	code, env = te.runJSON("feature", "abandon", "feat-x")
	if data, _ := env["data"].(map[string]any); code != 0 || data["changed"] != false || data["abandoned"] != true {
		t.Fatalf("exit %d %v", code, env)
	}
	if code, out, _ := te.run("feature", "abandon", "feat-x"); code != 0 || out != "feat-x was already abandoned; nothing changed.\n" {
		t.Fatalf("exit %d %q", code, out)
	}
}

// A detached promote that started a gate offers wait, then the same promote
// without --detach, which reuses the green gate.
func TestNextAfterADetachedPromote(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, env := te.runJSON("promote", "feat-x", "--detach", "--plan")
	next := nextOf(t, env)
	if code != 0 || len(next) < 2 {
		t.Fatalf("%d %v", code, env)
	}
	if want := []string{"ghostbranch", "wait", te.job("feat-x"), "--repo", "acme/widget"}; !slices.Equal(argv(next[0]), want) {
		t.Fatalf("next[0] = %v, want %v", argv(next[0]), want)
	}
	if want := []string{"ghostbranch", "promote", "feat-x", "--plan", "--json"}; !slices.Equal(argv(next[1]), want) {
		t.Fatalf("next[1] = %v, want %v", argv(next[1]), want)
	}
	code, out, _ := te.run("promote", "feat-x", "--detach=true", "--plan")
	if code != 0 || !strings.Contains(out, "No plan yet: wait for the job, then promote again.\n") ||
		!strings.Contains(out, "\n  ghostbranch promote feat-x --plan\n") {
		t.Fatalf("%d %q", code, out)
	}
}

// A stale job's next is the job that replaced it.
func TestNextForAStaleJobIsTheCurrentJob(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.mustRun("gate", "feat-x", "--detach")
	old := te.job("feat-x")
	te.shadowCommit("feat-x", map[string]string{"two.txt": "2\n"})
	te.mustRun("gate", "feat-x", "--detach")
	code, env := te.runJSON("wait", old)
	next := nextOf(t, env)
	if want := []string{"ghostbranch", "wait", te.job("feat-x"), "--repo", "acme/widget"}; code != 4 || len(next) != 1 || !slices.Equal(argv(next[0]), want) {
		t.Fatalf("%d %v", code, env)
	}
}

// Waiting on a feature that was never gated says to gate it.
func TestNextForAMissingJobIsGate(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, env := te.runJSON("wait", "gate/feat-x/0123456789ab")
	next := nextOf(t, env)
	if want := []string{"ghostbranch", "gate", "feat-x", "--repo", "acme/widget"}; code != 2 || len(next) != 1 || !slices.Equal(argv(next[0]), want) {
		t.Fatalf("%d %v", code, env)
	}
}

// A merged feature with newer commits whose gate is pending offers wait
// after the close, and show marks a job for an outdated candidate stale.
func TestNextForAPendingGateOnAMergedMovedFeature(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-b", "--keep"))
	te.shadowCommit("feat-b", map[string]string{"later.txt": "later\n"})
	te.mustRun("gate", "feat-b", "--detach")
	job := te.job("feat-b")
	code, env := te.runJSON("status")
	n := nextOf(t, env)
	if code != 0 || len(n) != 2 || argv(n[0])[2] != "close" ||
		!slices.Equal(argv(n[1]), []string{"ghostbranch", "wait", job, "--repo", "acme/widget"}) {
		t.Fatalf("exit %d, next = %v", code, n)
	}
	te.shadowCommit("feat-b", map[string]string{"later.txt": "later still\n"})
	if code, out, _ := te.run("feature", "show", "feat-b"); code != 0 || !strings.Contains(out, "  gate: pending (job "+job+"), stale\n") {
		t.Fatalf("feature show: %d %q", code, out)
	}
}

// FEATURE_NOT_FOUND offers the list of every feature, abandoned ones too.
func TestNextForAnUnknownFeatureListsThemAll(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, env := te.runJSON("feature", "show", "nope")
	if n := nextOf(t, env); len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "list", "--all", "--repo", "acme/widget"}) {
		t.Fatalf("next = %v", n)
	}
}

// A secret the only commit introduced: amend it, don't fix it on top.
func TestNextForATipLeakSaysAmend(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	_, env := te.runJSON("gate", "feat-x")
	n := nextOf(t, env)
	if why := n[0]["why"].(string); !strings.Contains(why, "amend") || !strings.Contains(why, "git push --force-with-lease shadow feat-x") {
		t.Fatalf("next = %v", n)
	}
}

// The human next prints a positional input once: <name>, not <name> <<name>>.
func TestSyncHumanNextNamesThePlaceholderOnce(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, out, _ := te.run("sync")
	if !strings.Contains(out, "ghostbranch feature start --repo acme/widget <name>\n") || strings.Contains(out, "<<name>>") {
		t.Fatalf("stdout:\n%s", out)
	}
}

// A sync whose push failed still surveyed the features: next keeps what
// they need, and the human output says where shadow main was, not where it
// is now.
func TestNextForASyncPushFailureKeepsAttention(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.ClosePR("acme/widget", publishFeature(te, "feat-c"))
	testutil.Commit(t, te.pubGit, "main", "next", map[string]string{"n.txt": "1\n"})
	hooks := filepath.Join(te.forge.RepoDir("acme/widget-shadow"), "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-receive"), []byte("#!/bin/sh\necho 'GH013: repository rule violations found' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("sync")
	e, _ := env["error"].(map[string]any)
	if code == 0 || e["code"] != "PUSH_REFUSED" {
		t.Fatalf("exit %d %v", code, env)
	}
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "feature", "close", "feat-c", "--repo", "acme/widget"}) ||
		n[0]["needs_human"] != true {
		t.Fatalf("next = %v", n)
	}
	_, out, errOut := te.run("sync")
	if !strings.Contains(out, "Shadow main could not be pushed; it was at ") || !strings.Contains(out, " when sync read it.\n") ||
		!strings.Contains(out+errOut, "ghostbranch feature close feat-c --repo acme/widget") {
		t.Fatalf("human output:\nstdout: %s\nstderr: %s", out, errOut)
	}
}

// An agent's run with --repo . hands a person a replay that names the pair.
func TestNextForAPersonNamesThePairForARelativeRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.env.CwdRemotes = func() []workspace.Remote {
		return []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget.git"}}
	}
	_, env := te.runJSON("agents", "allow", "--repo", ".", "--non-interactive")
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "agents", "allow", "--repo", "acme/widget"}) || n[0]["needs_human"] != true {
		t.Fatalf("next = %v", n)
	}
}

// F-01: listings with no --repo still list everything in a workspace with
// no pairs: PAIR_NOT_FOUND there means "no pair", as PAIR_AMBIGUOUS did.
func TestListingsWithoutRepoWorkInAnEmptyWorkspace(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	for _, args := range [][]string{{"agents", "status"}, {"waivers", "list"}} {
		if code, env := te.runJSON(args...); code != 0 {
			t.Fatalf("%v: exit %d %v", args, code, env)
		}
	}
}

// F-01: next after PAIR_NOT_FOUND works taken literally: adopt where the
// shadow repo exists (another GHOSTBRANCH_HOME), create where it doesn't.
func TestNextAfterPairNotFoundRuns(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)                                                   // the pair exists on GitHub ...
	te.vars["GHOSTBRANCH_HOME"] = filepath.Join(te.root, "home-2") // ... but not in this home
	code, env := te.runJSON("gate", "feat-x", "--repo", "acme/widget")
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "PAIR_NOT_FOUND" {
		t.Fatalf("exit %d, env = %v", code, env)
	}
	n := nextOf(t, env)
	if code, out, errOut := te.run(argv(n[0])[1:]...); code != 0 {
		t.Fatalf("next[0] %v: exit %d\n%s\n%s", argv(n[0]), code, out, errOut)
	}
	fresh := newTestEnv(t) // no shadow repo yet
	_, env = fresh.runJSON("gate", "feat-x", "--repo", "acme/widget")
	n = nextOf(t, env)
	if code, out, errOut := fresh.run(argv(n[1])[1:]...); code != 0 {
		t.Fatalf("next[1] %v: exit %d\n%s\n%s", argv(n[1]), code, out, errOut)
	}
}
