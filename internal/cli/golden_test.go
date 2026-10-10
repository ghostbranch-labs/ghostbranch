package cli_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

var update = flag.Bool("update", false, "rewrite golden files")

var dataSchemas = map[string]string{
	"init": "init.schema.json", "sync": "sync.schema.json", "feature start": "feature-start.schema.json",
	"gate": "gate.schema.json", "promote": "promote.schema.json", "apply": "apply.schema.json",
	"agents allow": "agents-allow.schema.json", "agents revoke": "agents-revoke.schema.json", "agents status": "agents-status.schema.json",
	"waivers add": "waivers-add.schema.json", "waivers list": "waivers-list.schema.json", "waivers revoke": "waivers-revoke.schema.json",
	"explain": "explain.schema.json", "guide": "guide.schema.json", "audit": "audit.schema.json", "upgrade": "upgrade.schema.json",
	"guard install": "guard.schema.json", "guard uninstall": "guard.schema.json", "guard status": "guard.schema.json", "guard check": "guard.schema.json",
	"remove": "remove.schema.json", "log": "log.schema.json",
	"status": "status.schema.json", "feature list": "feature-list.schema.json", "feature show": "feature-show.schema.json",
	"feature abandon": "feature-abandon.schema.json", "feature close": "feature-close.schema.json",
	"wait": "gate.schema.json", "login": "login.schema.json", "logout": "logout.schema.json", "auth status": "auth-status.schema.json",
	"doctor": "doctor.schema.json", "feature fold": "feature-fold.schema.json", "feature restack": "feature-restack.schema.json",
}

var rootPath = regexp.MustCompile(`<ROOT>[^"]*`)

// normalize replaces the temp dir with <ROOT> and uses / in those paths, so
// goldens match on every OS.
func normalize(out, root string) string {
	esc, _ := json.Marshal(root)
	out = strings.ReplaceAll(out, strings.Trim(string(esc), `"`), "<ROOT>")
	out = strings.ReplaceAll(out, filepath.ToSlash(root), "<ROOT>")
	return rootPath.ReplaceAllStringFunc(out, func(s string) string { return strings.ReplaceAll(s, `\\`, "/") })
}

func failing(string, string) []forge.Check {
	return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}
}

func TestGoldenContract(t *testing.T) {
	t.Parallel()
	scenarios := []struct {
		name string
		prep func(*testEnv)
		args []string
		exit int
	}{
		{"init", nil, []string{"init", "acme/widget"}, 0},
		{"init_dry_run", nil, []string{"init", "acme/widget", "--dry-run"}, 0},
		{"init_create_denied", func(te *testEnv) { te.forge.CreateForbidden = true }, []string{"init", "acme/widget"}, 1},
		{"sync_noop", initOnly, []string{"sync"}, 0},
		{"sync_fast_forward", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"sync"}, 0},
		{"sync_branches", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "release", "release", map[string]string{"rel.txt": "rel\n"})
			editConfig(te, "branches: [main]", "branches: [main, release]")
		}, []string{"sync"}, 0},
		{"feature_start", initOnly, []string{"feature", "start", "feat-x"}, 0},
		{"feature_start_behind", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"feature", "start", "feat-x"}, 4},
		{"feature_start_exists", withFeature, []string{"feature", "start", "feat-x"}, 4},
		{"feature_start_from", withFeature, []string{"feature", "start", "feat-y", "--from", "feat-x"}, 0},
		{"feature_start_from_plan", withFeature, []string{"feature", "start", "feat-y", "--from", "feat-x", "--plan"}, 0},
		{"feature_start_from_unknown", initOnly, []string{"feature", "start", "feat-y", "--from", "nope"}, 2},
		{"feature_start_from_behind", func(te *testEnv) {
			withFeature(te)
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"feature", "start", "feat-y", "--from", "feat-x"}, 4},
		{"gate", withFeature, []string{"gate", "feat-x"}, 0},
		{"gate_checks_failed", func(te *testEnv) { withFeature(te); te.forge.Checks = failing }, []string{"gate", "feat-x"}, 3},
		{"gate_leak", withLeakyFeature, []string{"gate", "feat-x"}, 3},
		{"gate_permission_missing", func(te *testEnv) {
			withFeature(te)
			te.forge.MissingPermission = map[string]string{"CreatePR": "Pull requests: write"}
		}, []string{"gate", "feat-x"}, 1},
		{"promote_plan", gatedFeature, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan"}, 0},
		{"promote_needs_human", func(te *testEnv) { gatedFeature(te); te.vars["GHOSTBRANCH_ACTOR"] = "agent:claude-code" },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"}, 5},
		{"promote_plan_under_grant", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan"}, 0},
		{"promote_grant_needs_yes", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_not_confirmed", func(te *testEnv) { gatedFeature(te); atConsole(te, func(string) string { return "n\n" }) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_too_many_attempts", func(te *testEnv) { gatedFeature(te); atConsole(te, func(string) string { return "X\nY\nZ\n" }) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_console_publish", func(te *testEnv) { gatedFeature(te); atConsole(te, typeTheCode) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 0},
		{"promote_publish", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"}, 0},
		{"apply_stale", func(te *testEnv) {
			gatedFeature(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"apply", "<PLAN>", "--yes"}, 6},
		{"sync_plan", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"sync", "--plan"}, 0},
		{"feature_start_plan", initOnly, []string{"feature", "start", "feat-x", "--plan"}, 0},
		{"gate_plan", withFeature, []string{"gate", "feat-x", "--plan"}, 0},
		{"promote_preserve_plan", gatedFeature, []string{"promote", "feat-x", "--plan"}, 0},
		{"promote_message_not_squash", gatedFeature, []string{"promote", "feat-x", "-m", "Add feature", "--plan"}, 2},
		{"promote_title_required", func(te *testEnv) { gatedFeature(te); te.shadowCommit("feat-x", map[string]string{"two.txt": "2\n"}) },
			[]string{"promote", "feat-x", "--plan"}, 2},
		{"promote_curated_plan", gatedFeature, []string{"promote", "feat-x", "--mode", "curated", "--curate", "<CURATE>", "--plan"}, 0},
		{"promote_curate_invalid", gatedFeature, []string{"promote", "feat-x", "--mode", "curated", "--curate", "<CURATE_BAD>", "--plan"}, 2},
		{"promote_dry_run_stale_gate", withFeature, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--dry-run"}, 0},
		{"apply_dry_run", func(te *testEnv) {
			gatedFeature(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
		}, []string{"apply", "<PLAN>", "--dry-run"}, 0},
		{"bad_flag", nil, []string{"gate"}, 2},
		{"shadow_not_private", func(te *testEnv) { initOnly(te); te.forge.SetPrivate("acme/widget-shadow", false) }, []string{"sync"}, 1},
		{"agents_allow_no_console", initOnly, []string{"agents", "allow"}, 5},
		{"agents_allow_plan", initOnly, []string{"agents", "allow", "--for", "24h", "--plan"}, 0},
		{"agents_allow_bad_for", initOnly, []string{"agents", "allow", "--for", "7h"}, 2},
		{"agents_status", func(te *testEnv) { initOnly(te); grantAgents(te) }, []string{"agents", "status"}, 0},
		{"explain_code", nil, []string{"explain", "approval_required"}, 0},
		{"explain_exit", nil, []string{"explain", "3"}, 0},
		{"explain_unknown", nil, []string{"explain", "NOPE"}, 2},
		{"guide_install_plan", func(te *testEnv) { te.vars["CLAUDE_CONFIG_DIR"] = filepath.Join(te.root, "claude") },
			[]string{"guide", "--install", "claude", "--plan"}, 0},
		{"agents_revoke", func(te *testEnv) { initOnly(te); grantAgents(te) }, []string{"agents", "revoke"}, 0},
		{"actor_unknown", func(te *testEnv) { initOnly(te); te.forge.UserErr = errors.New("rate limited") }, []string{"sync"}, 0},
		{"guard_not_a_checkout", initOnly, []string{"guard", "status"}, 2},
		{"gate_marker_invalid", func(te *testEnv) {
			initOnly(te)
			te.mustRun("feature", "start", "feat-x")
			te.shadowCommit("feat-x", map[string]string{"app.go": "ok\n// BEGIN-INTERNAL\nx\n"})
		}, []string{"gate", "feat-x"}, 3},
		{"gate_waived", waiveLeak, []string{"gate", "feat-x"}, 0},
		{"waivers_add_no_console", leakyWithID, []string{"waivers", "add", "feat-x", "<FINDING>", "--reason", "test fixture"}, 5},
		{"waivers_add_plan", leakyWithID, []string{"waivers", "add", "feat-x", "<FINDING>", "--reason", "test fixture", "--plan"}, 0},
		{"waivers_add_unknown", leakyWithID, []string{"waivers", "add", "feat-x", "f-000000000000", "--reason", "x", "--plan"}, 2},
		{"waivers_add_missing_reason", initOnly, []string{"waivers", "add", "feat-x", "f-000000000000"}, 2},
		{"waivers_list", waiveLeak, []string{"waivers", "list"}, 0},
		{"waivers_revoke", waiveLeak, []string{"waivers", "revoke", "<FINDING>"}, 0},
		{"audit", initOnly, []string{"audit"}, 0},
		{"audit_secrets_unlisted", func(te *testEnv) {
			// committed before init, so init mirrors it into shadow main
			testutil.Commit(te.t, te.pubGit, "main", "uses a secret", map[string]string{".github/workflows/rel.yml": `on: push
jobs:
  r:
    runs-on: x
    steps:
      - run: echo ${{ secrets.NPM_TOKEN }}
`})
			initOnly(te)
			te.forge.SecretsForbidden = map[string]bool{"acme/widget-shadow": true}
		}, []string{"audit"}, 0},
		{"upgrade_noop", initOnly, []string{"upgrade"}, 0},
		{"upgrade_plan", scheduled, []string{"upgrade", "--plan"}, 0},
		{"upgrade_plan_noop", initOnly, []string{"upgrade", "--plan"}, 0},
		{"upgrade", scheduled, []string{"upgrade"}, 0},
		{"remove_plan", func(te *testEnv) {
			gatedFeature(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
			grantAgents(te)
		}, []string{"remove", "--plan"}, 0},
		{"remove_no_console", initOnly, []string{"remove"}, 5},
		{"log", withFeature, []string{"log"}, 0},
		{"status", withFeature, []string{"status"}, 0},
		{"status_behind", func(te *testEnv) {
			initOnly(te)
			testutil.Commit(te.t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, []string{"status"}, 0},
		{"feature_list", withFeature, []string{"feature", "list"}, 0},
		{"feature_show", gatedFeature, []string{"feature", "show", "feat-x"}, 0},
		{"feature_show_unknown", initOnly, []string{"feature", "show", "nope"}, 2},
		{"feature_abandon", withFeature, []string{"feature", "abandon", "feat-x"}, 0},
		{"feature_abandon_plan", withFeature, []string{"feature", "abandon", "feat-x", "--plan"}, 0},
		{"feature_close_merged", func(te *testEnv) {
			initOnly(te)
			grantAgents(te)
			te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-x"))
		}, []string{"feature", "close", "feat-x"}, 0},
		{"feature_close_needs_human", withFeature, []string{"feature", "close", "feat-x"}, 5},
		{"feature_close_plan", gatedFeature, []string{"feature", "close", "feat-x", "--plan"}, 0},
		{"feature_fold", withStack, []string{"feature", "fold", "feat-y"}, 0},
		{"feature_fold_plan", withStack, []string{"feature", "fold", "feat-y", "--plan"}, 0},
		{"feature_fold_needs_rebase", func(te *testEnv) {
			withStack(te)
			te.shadowCommit("feat-x", map[string]string{"later.txt": "later\n"})
		}, []string{"feature", "fold", "feat-y"}, contract.FoldNeedsRebase.Exit()},
		{"feature_restack", func(te *testEnv) {
			withStack(te)
			te.shadowCommit("feat-x", map[string]string{"later.txt": "later\n"})
		}, []string{"feature", "restack", "feat-x"}, 0},
		{"feature_restack_onto_main", withStack, []string{"feature", "restack", "feat-y", "--onto", "main"}, 0},
		{"feature_restack_conflict", withConflictingStack, []string{"feature", "restack", "feat-x"}, contract.RebaseConflict.Exit()},
		{"feature_abandon_has_children", withStack, []string{"feature", "abandon", "feat-x"}, contract.HasChildren.Exit()},
		{"feature_close_has_children", withStack, []string{"feature", "close", "feat-x"}, contract.HasChildren.Exit()},
		{"gate_detach", withFeature, []string{"gate", "feat-x", "--detach"}, 0},
		{"wait", func(te *testEnv) { withFeature(te); te.mustRun("gate", "feat-x", "--detach") }, []string{"wait", "<JOB>"}, 0},
		{"wait_finished", func(te *testEnv) { withFeature(te); te.mustRun("gate", "feat-x") }, []string{"wait", "<JOB>"}, 0},
		{"wait_stale", func(te *testEnv) {
			withFeature(te)
			te.mustRun("gate", "feat-x", "--detach")
			te.oldJob = te.job("feat-x")
			te.shadowCommit("feat-x", map[string]string{"two.txt": "2\n"})
			te.mustRun("gate", "feat-x", "--detach")
		}, []string{"wait", "<OLD_JOB>"}, 4},
		{"wait_bad_job", initOnly, []string{"wait", "nope"}, 2},
		{"promote_detach", withFeature, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan", "--detach"}, 0},
		{"login_with_token", func(te *testEnv) {
			te.forge.User = forge.User{Login: "octo", ID: 1}
			te.env.Stdin = strings.NewReader("ghp_test_golden\n")
		}, []string{"login", "--with-token"}, 0},
		{"login_no_console", nil, []string{"login"}, 1},
		{"login_device", func(te *testEnv) { deviceLogin(te, authtest.Options{Pending: 1}) }, []string{"login"}, 0},
		{"logout", func(te *testEnv) { storeLogin(te, "ghu_test_x", "ghr_test_x", 8*time.Hour) }, []string{"logout"}, 0},
		{"auth_status_login", func(te *testEnv) {
			te.forge.User = forge.User{Login: "octo", ID: 1}
			initOnly(te)
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			storeLogin(te, "ghu_test_x", "ghr_test_x", 8*time.Hour)
		}, []string{"auth", "status"}, 0},
		{"auth_status_env", nil, []string{"auth", "status"}, 0},
		{"doctor", initOnly, []string{"doctor", "--repo", "acme/widget"}, 0},
		{"doctor_no_pair", nil, []string{"doctor"}, 0},
		{"doctor_failed", func(te *testEnv) { initOnly(te); delete(te.vars, "GHOSTBRANCH_TOKEN") }, []string{"doctor", "--repo", "acme/widget"}, 1},
	}
	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if s.prep != nil {
				s.prep(te)
			}
			args := append([]string(nil), s.args...)
			for i, a := range args {
				if a == "<PLAN>" {
					args[i] = te.lastPlanID()
				}
				if a == "<FINDING>" {
					args[i] = te.finding
				}
				if a == "<CURATE>" || a == "<CURATE_BAD>" {
					args[i] = te.curateFile(a == "<CURATE_BAD>")
				}
				if a == "<JOB>" {
					args[i] = te.job("feat-x")
				}
				if a == "<OLD_JOB>" {
					args[i] = te.oldJob
				}
			}
			code, out, errOut := te.run(append(args, "--json")...)
			if code != s.exit {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, s.exit, out, errOut)
			}
			last := []byte(out)
			if args[0] == "wait" {
				last = waitStream(t, out)
			}
			testutil.ValidateSchema(t, "envelope.schema.json", last)
			var env struct {
				Command string          `json:"command"`
				Data    json.RawMessage `json:"data"`
			}
			_ = json.Unmarshal(last, &env)
			if string(env.Data) != "{}" {
				testutil.ValidateSchema(t, dataSchemas[env.Command], env.Data)
			}
			got := normalize(out, te.root)
			golden := filepath.Join("testdata", "golden", s.name+".json")
			if *update {
				_ = os.MkdirAll(filepath.Dir(golden), 0o755)
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("missing golden %s; run go test ./internal/cli -run Golden -update", golden)
			}
			if strings.ReplaceAll(string(want), "\r\n", "\n") != got {
				t.Fatalf("output differs from %s\n--- got\n%s\n--- want\n%s", golden, got, want)
			}
		})
	}
}

// waitStream checks wait --json's stdout: NDJSON, one complete JSON value
// per line, every value but the last a job event. It returns the last
// value, the envelope.
func waitStream(t *testing.T, out string) []byte {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(out))
	var values []json.RawMessage
	for {
		var v json.RawMessage
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("stdout is not a JSON stream: %v\n%s", err, out)
		}
		values = append(values, v)
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(values) == 0 || len(lines) != len(values) {
		t.Fatalf("want one JSON value per line, got %d value(s) on %d line(s):\n%s", len(values), len(lines), out)
	}
	for i, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("line %d is not one complete JSON value: %s", i+1, line)
		}
	}
	for _, v := range values[:len(values)-1] {
		testutil.ValidateSchema(t, "wait-event.schema.json", v)
	}
	return values[len(values)-1]
}

// curateFile writes a --curate file under te.root: one group holding
// feat-x's one commit under the message "Add feature", or, if bad, no
// groups at all.
func (te *testEnv) curateFile(bad bool) string {
	te.t.Helper()
	body := `{"commits": []}`
	if !bad {
		sha, err := gitx.New(te.forge.RepoDir("acme/widget-shadow")).MustResolve(te.t.Context(), "refs/heads/feat-x")
		if err != nil {
			te.t.Fatal(err)
		}
		body = `{"commits": [{"from": ["` + sha + `"], "message": "Add feature"}]}`
	}
	path := filepath.Join(te.root, "curate.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		te.t.Fatal(err)
	}
	return path
}

// scheduled is an initialized pair whose config turns scheduled sync on, so
// the sync workflow is wanted but not yet on the overlay branch.
func scheduled(te *testEnv) {
	initOnly(te)
	editConfig(te, `schedule: "off"`, `schedule: "23 */6 * * *"`)
}

// editConfig replaces old with new in the overlay branch's .ghostbranch.yml.
func editConfig(te *testEnv, old, new string) {
	sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	raw, _, err := sh.ShowFile(te.t.Context(), "refs/heads/ghostbranch", ".ghostbranch.yml")
	if err != nil {
		te.t.Fatal(err)
	}
	if !strings.Contains(string(raw), old) {
		te.t.Fatalf("config has no %q", old)
	}
	te.shadowCommit("ghostbranch", map[string]string{".ghostbranch.yml": strings.Replace(string(raw), old, new, 1)})
}

// Plans, feature.json and the ledger are part of the contract too.
func TestArtifactsMatchTheirSchemas(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	grantAgents(te)
	te.mustRun("apply", te.lastPlanID(), "--yes")
	home := te.vars["GHOSTBRANCH_HOME"]

	planRaw, err := os.ReadFile(filepath.Join(home, "workspaces", "default", "plans", te.lastPlanID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "plan.schema.json", planRaw)

	sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	meta, ok, err := sh.ShowFile(te.t.Context(), "refs/ghostbranch/features/feat-x", "feature.json")
	if err != nil || !ok {
		t.Fatalf("feature.json: %v", err)
	}
	testutil.ValidateSchema(t, "feature.schema.json", meta)

	f, err := os.Open(filepath.Join(home, "workspaces", "default", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		testutil.ValidateSchema(t, "ledger-entry.schema.json", sc.Bytes())
		n++
	}
	if n < 5 {
		t.Fatalf("want at least 5 ledger lines (init, feature start, gate, promote, apply), got %d", n)
	}

	grants, err := os.ReadFile(filepath.Join(home, "workspaces", "default", "grants.json"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "grants.schema.json", grants)
}

func TestWaiverArtifactsMatchTheirSchemas(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	waiveLeak(te)
	raw, err := os.ReadFile(te.waiversFile())
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "waivers.schema.json", raw)
	te.mustRun("gate", "feat-x")
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add config", "--plan")
	planRaw, err := os.ReadFile(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "plans", te.lastPlanID()+".json"))
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "plan.schema.json", planRaw)
	if !strings.Contains(string(planRaw), te.finding) {
		t.Fatal("the plan does not record the waived finding")
	}
	f, err := os.Open(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		testutil.ValidateSchema(t, "ledger-entry.schema.json", sc.Bytes())
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
}
