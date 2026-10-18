package cli_test

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
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
	"doctor": "doctor.schema.json", "feature fold": "feature-fold.schema.json", "feature restack": "feature-restack.schema.json", "feature pull": "feature-pull.schema.json",
	"feature archive": "feature-archive.schema.json", "version": "version.schema.json", "reference": "reference.schema.json",
}

var rootPath = regexp.MustCompile(`<ROOT>[^"]*`)

// pidNumber and pidField are a process id in a LOCKED holder's message and
// details; it differs per run. The field becomes the string "<PID>" so the
// golden stays valid JSON.
var (
	pidNumber = regexp.MustCompile(`pid [0-9]+`)
	pidField  = regexp.MustCompile(`"pid": [0-9]+`)
)

// normalize replaces the temp dir with <ROOT> and uses / in those paths, so
// goldens match on every OS.
// loopbackPort is a fake server's random port, as in the device_code line.
var loopbackPort = regexp.MustCompile(`127\.0\.0\.1:\d+`)

func normalize(out, root string) string {
	esc, _ := json.Marshal(root)
	out = strings.ReplaceAll(out, strings.Trim(string(esc), `"`), "<ROOT>")
	out = strings.ReplaceAll(out, filepath.ToSlash(root), "<ROOT>")
	out = rootPath.ReplaceAllStringFunc(out, func(s string) string { return strings.ReplaceAll(s, `\\`, "/") })
	out = loopbackPort.ReplaceAllString(out, "127.0.0.1:<PORT>")
	out = pidNumber.ReplaceAllString(out, "pid <PID>")
	out = pidField.ReplaceAllString(out, `"pid": "<PID>"`)
	return normalizeSHAs(out)
}

// shaOrKeep matches a git hash (a word-bounded run of 7 to 40 lowercase hex
// digits) or one of the things that look like one but are not, which are
// kept as they are: paths under <ROOT>, and finding ids (f- and 12 hex of a
// content SHA-256, stable across commits). Longer runs such as a 64-hex
// match_sha256 have no word boundary inside them, so they never match.
var shaOrKeep = regexp.MustCompile(`<ROOT>[^"\s]*|\bf-[0-9a-f]{12}\b|\b[0-9a-f]{7,40}\b`)

func isSHA(m string) bool { return !strings.HasPrefix(m, "<ROOT>") && !strings.HasPrefix(m, "f-") }

// normalizeSHAs replaces each git hash in out with <SHA1>, <SHA2>, ...,
// numbered by first appearance in out. <SHAn> is always a full 40-hex hash;
// anything shorter carries its length, as <SHAn:12> or <SHAn:7>. A hash
// that is a prefix of a longer one in out (a short hash of a full one)
// shares the longer one's number; a lone short hash gets its own. One
// mapping covers all of out, so a hash repeated on other NDJSON lines keeps
// its number. Goldens then pin which hashes are equal, which are short
// forms of which, and every hash's length, but not their values, so a
// change to commit identity or content does not churn them.
func normalizeSHAs(out string) string {
	var seen []string
	for _, m := range shaOrKeep.FindAllString(out, -1) {
		if isSHA(m) && !slices.Contains(seen, m) {
			seen = append(seen, m)
		}
	}
	// canon is the hash m stands for: the first-seen longest hash that m is
	// a prefix of (m itself if none is longer).
	canon := func(m string) string {
		best := m
		for _, h := range seen {
			if len(h) > len(best) && strings.HasPrefix(h, m) {
				best = h
			}
		}
		return best
	}
	num := map[string]int{}
	for _, m := range seen {
		if c := canon(m); num[c] == 0 {
			num[c] = len(num) + 1
		}
	}
	return shaOrKeep.ReplaceAllStringFunc(out, func(m string) string {
		if !isSHA(m) {
			return m
		}
		n := num[canon(m)]
		if len(m) == 40 {
			return fmt.Sprintf("<SHA%d>", n)
		}
		return fmt.Sprintf("<SHA%d:%d>", n, len(m))
	})
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
		{"version", nil, []string{"version"}, 0},
		{"reference", nil, []string{"reference"}, 0},
		{"version_flag", nil, []string{"--version"}, 0},
		{"update_available", func(te *testEnv) {
			_, url := newFakeReleases(te.t, "v0.13.0", "[]")
			te.releaseBuild("v0.12.0", url)
		}, []string{"version"}, 0},
		{"update_available_security", func(te *testEnv) {
			_, url := newFakeReleases(te.t, "v0.13.1", securityAdvisory)
			te.releaseBuild("v0.13.0", url)
		}, []string{"version"}, 0},
		{"update_available_on_error", func(te *testEnv) {
			initOnly(te)
			_, url := newFakeReleases(te.t, "v0.13.0", "[]")
			te.releaseBuild("v0.12.0", url)
		}, []string{"gate", "no-such-feature"}, 2},
		{"init_dry_run", nil, []string{"init", "acme/widget", "--dry-run"}, 0},
		{"init_create_denied", func(te *testEnv) { te.forge.CreateForbidden = true }, []string{"init", "acme/widget"}, 1},
		{"init_plan_setup", func(te *testEnv) { appLogin(te); personal(te); installApp(te) }, []string{"init", "acme/widget", "--plan"}, 0},
		{"init_plan_shadow_exists", func(te *testEnv) {
			if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
				te.t.Fatal(err)
			}
		}, []string{"init", "acme/widget", "--plan"}, 2},
		{"init_app_not_installed", func(te *testEnv) { appLogin(te); personal(te) }, []string{"init", "acme/widget"}, contract.AppNotInstalled.Exit()},
		{"init_token_wrong_account", func(te *testEnv) { te.forge.User.Login, te.forge.Orgs = "octo", nil }, []string{"init", "acme/widget"}, contract.TokenWrongAccount.Exit()},
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
		{"gate_no_commits", func(te *testEnv) { initOnly(te); te.mustRun("feature", "start", "feat-x") }, []string{"gate", "feat-x"}, contract.NothingToPublish.Exit()},
		{"promote_no_commits", func(te *testEnv) { initOnly(te); te.mustRun("feature", "start", "feat-x") }, []string{"promote", "feat-x", "--plan"}, contract.NothingToPublish.Exit()},
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
		{"gate_stacked", withStack, []string{"gate", "feat-y"}, 0},
		{"gate_update_append", func(te *testEnv) {
			publishable(te)
			te.mustRun("promote", "feat-x", "--yes", "--keep")
			te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
		}, []string{"gate", "feat-x"}, 0},
		{"gate_parent_rewritten", func(te *testEnv) {
			withStack(te)
			sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
			main, err := sh.MustResolve(te.t.Context(), "refs/heads/main")
			if err != nil {
				te.t.Fatal(err)
			}
			if err := sh.UpdateRef(te.t.Context(), "refs/heads/feat-x", main); err != nil {
				te.t.Fatal(err)
			}
			te.shadowCommit("feat-x", map[string]string{"feature.txt": "rewritten\n"})
		}, []string{"gate", "feat-y"}, contract.ParentRewritten.Exit()},
		{"promote_plan", gatedFeature, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan"}, 0},
		{"promote_stack_plan", withGatedStack, []string{"promote", "feat-a", "feat-b", "--mode", "squash", "--plan"}, 0},
		{"promote_not_a_stack", withGatedStack, []string{"promote", "feat-b", "--plan"}, contract.NotAStack.Exit()},
		{"gate_stack_bottom", func(te *testEnv) {
			initOnly(te)
			te.mustRun("feature", "start", "feat-a")
			te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
			te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
			te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
		}, []string{"gate", "feat-a"}, 0},
		{"gate_stack_top", func(te *testEnv) {
			initOnly(te)
			te.mustRun("feature", "start", "feat-a")
			te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
			te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
			te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
			te.mustRun("gate", "feat-a")
		}, []string{"gate", "feat-b"}, 0},
		{"status_stack", withGatedStack, []string{"status"}, 0},
		{"promote_needs_human", func(te *testEnv) { gatedFeature(te); te.vars["GHOSTBRANCH_ACTOR"] = "agent:claude-code" },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"}, 5},
		{"promote_plan_under_grant", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan"}, 0},
		{"promote_update_plan", func(te *testEnv) {
			publishable(te)
			te.mustRun("promote", "feat-x", "--yes", "--keep")
			te.shadowCommit("feat-x", map[string]string{"more.txt": "more\n"})
		}, []string{"promote", "feat-x", "--plan"}, 0},
		{"promote_grant_needs_yes", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_not_confirmed", func(te *testEnv) { gatedFeature(te); atConsole(te, func(string) string { return "n\n" }) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_too_many_attempts", func(te *testEnv) { gatedFeature(te); atConsole(te, func(string) string { return "X\nY\nZ\n" }) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 5},
		{"promote_console_publish", func(te *testEnv) { gatedFeature(te); atConsole(te, typeTheCode) },
			[]string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature"}, 0},
		{"promote_publish", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"}, 0},
		{"promote_publish_keep", publishable, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep"}, 0},
		{"apply_archived", func(te *testEnv) {
			publishable(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
		}, []string{"apply", "<PLAN>", "--yes"}, 0},
		{"apply_keep", func(te *testEnv) {
			publishable(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
		}, []string{"apply", "<PLAN>", "--yes", "--keep"}, 0},
		{"apply_archive_unavailable", func(te *testEnv) {
			publishable(te)
			te.mustRun("feature", "start", "feat-y", "--from", "feat-x")
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
		}, []string{"apply", "<PLAN>", "--yes"}, 0},
		{"apply_archive_incomplete", func(te *testEnv) {
			publishable(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
			te.forge.ClosePRErr = errors.New("connection reset")
		}, []string{"apply", "<PLAN>", "--yes"}, 0},
		{"apply_stack_archive_stopped", func(te *testEnv) {
			// A three-level stack whose hand-off stops at the middle level:
			// the top is archived, the middle incomplete, the bottom kept.
			withGatedStack(te)
			te.mustRun("feature", "start", "feat-c", "--from", "feat-b")
			te.shadowCommit("feat-c", map[string]string{"c.txt": "c\n"})
			te.mustRun("gate", "feat-c")
			grantAgents(te)
			te.mustRun("promote", "feat-a", "feat-b", "feat-c", "--mode", "squash", "--plan")
			gate := 0
			for _, pr := range te.forge.PRs("acme/widget-shadow") {
				if pr.Head == "ghostbranch-gate/feat-b" && pr.State == "open" {
					gate = pr.Number
				}
			}
			if gate == 0 {
				te.t.Fatal("no gate PR for feat-b")
			}
			te.forge.ClosePRFail = func(full string, n int) error {
				if full == "acme/widget-shadow" && n == gate {
					return errors.New("connection reset")
				}
				return nil
			}
		}, []string{"apply", "<PLAN>", "--yes"}, 0},
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
		{"agents_allow_plan", initOnly, []string{"agents", "allow", "--for", "1d", "--plan"}, 0},
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
		{"feature_archive", keptFeature, []string{"feature", "archive", "feat-x"}, 0},
		{"feature_archive_plan", keptFeature, []string{"feature", "archive", "feat-x", "--plan"}, 0},
		{"feature_archive_already", archivedFeature, []string{"feature", "archive", "feat-x"}, 0},
		{"feature_archive_has_children", func(te *testEnv) {
			keptFeature(te)
			te.mustRun("feature", "start", "feat-y", "--from", "feat-x")
		}, []string{"feature", "archive", "feat-x"}, contract.HasChildren.Exit()},
		{"feature_archive_not_published", withFeature, []string{"feature", "archive", "feat-x"}, contract.NoOpenPR.Exit()},
		{"gate_archived", archivedFeature, []string{"gate", "feat-x"}, contract.FeatureArchived.Exit()},
		{"promote_archived", archivedFeature, []string{"promote", "feat-x", "--plan"}, contract.FeatureArchived.Exit()},
		{"feature_pull_archived", archivedFeature, []string{"feature", "pull", "feat-x"}, contract.FeatureArchived.Exit()},
		{"status_archived", archivedFeature, []string{"status"}, 0},
		{"feature_list_archived", archivedFeature, []string{"feature", "list"}, 0},
		{"feature_show_archived", archivedFeature, []string{"feature", "show", "feat-x"}, 0},
		{"sync_closes_archived", func(te *testEnv) {
			initOnly(te)
			grantAgents(te)
			te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-x"))
		}, []string{"sync"}, 0},
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
		{"feature_pull", publishedWithForeign, []string{"feature", "pull", "feat-x"}, 0},
		{"feature_pull_by_hand", publishedWithConflict, []string{"feature", "pull", "feat-x"}, contract.PRChangedOnGitHub.Exit()},
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
		{"wait_stack_child_first", func(te *testEnv) {
			initOnly(te)
			te.mustRun("feature", "start", "feat-a")
			te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
			te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
			te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
			te.mustRun("gate", "feat-b", "--detach")
		}, []string{"wait", "<JOB:feat-b>"}, 0},
		{"wait_bad_job", initOnly, []string{"wait", "nope"}, 2},
		{"promote_detach", withFeature, []string{"promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan", "--detach"}, 0},
		{"login_with_token", func(te *testEnv) {
			te.forge.User = forge.User{Login: "octo", ID: 1}
			te.env.Stdin = strings.NewReader("ghp_test_golden\n")
		}, []string{"login", "--with-token"}, 0},
		{"login_device_no_console", func(te *testEnv) {
			deviceLogin(te, authtest.Options{Pending: 1})
			te.env.ConsoleUsable = false
		}, []string{"login"}, 0},
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
		{"doctor_unregistered", initOnly, []string{"doctor", "--repo", "acme/other"}, 1},
		{"feature_pull_no_pr", withFeature, []string{"feature", "pull", "feat-x"}, contract.NoOpenPR.Exit()},
		{"apply_plan_not_found", initOnly, []string{"apply", "20261005T120000Z-0a0b0c"}, contract.PlanNotFound.Exit()},
		{"apply_plan_other_machine", func(te *testEnv) {
			gatedFeature(te)
			te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
			te.gonePlan = te.lastPlanID()
			if err := os.Remove(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "plans", te.gonePlan+".json")); err != nil {
				te.t.Fatal(err)
			}
			// No mirror either: the plan's ref can only come from the
			// shadow repo (review M6a).
			mirror := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "pairs", "acme__widget", "mirror.git")
			if _, err := os.Stat(mirror); err != nil {
				te.t.Fatal(err)
			}
			if err := os.RemoveAll(mirror); err != nil {
				te.t.Fatal(err)
			}
		}, []string{"apply", "<GONE_PLAN>"}, contract.PlanNotFound.Exit()},
		{"locked", func(te *testEnv) { initOnly(te); holdPairLock(te) }, []string{"sync"}, contract.Locked.Exit()},
		{"pair_not_found", initOnly, []string{"agents", "status", "--repo", "acme/other"}, contract.PairNotFound.Exit()},
		{"pair_ambiguous", twoPairs, []string{"status"}, contract.PairAmbiguous.Exit()},
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
				if name, ok := strings.CutPrefix(a, "<JOB:"); ok {
					args[i] = te.job(strings.TrimSuffix(name, ">"))
				}
				if a == "<GONE_PLAN>" {
					args[i] = te.gonePlan
				}
			}
			code, out, errOut := te.run(append(args, "--json")...)
			if code != s.exit {
				t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, s.exit, out, errOut)
			}
			last := []byte(out)
			if args[0] == "wait" {
				last = waitStream(t, out, "wait-event.schema.json")
			}
			if args[0] == "login" && s.name == "login_device_no_console" {
				last = waitStream(t, out, "login-event.schema.json")
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
func waitStream(t *testing.T, out, eventSchema string) []byte {
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
		testutil.ValidateSchema(t, eventSchema, v)
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

// holdPairLock takes acme/widget's pair lock as a gate started at the test
// clock's time would, until the test ends.
func holdPairLock(te *testEnv) {
	te.t.Helper()
	path := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "locks", "acme__widget.lock")
	drop, err := workspace.Lock(path, time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC), "gate")
	if err != nil {
		te.t.Fatal(err)
	}
	te.t.Cleanup(drop)
}

// twoPairs registers acme/widget and acme/other, so a command with no
// --repo outside a checkout can't tell which one.
func twoPairs(te *testEnv) {
	te.t.Helper()
	initOnly(te)
	dir, err := te.forge.AddRepo("acme/other", false)
	if err != nil {
		te.t.Fatal(err)
	}
	testutil.Commit(te.t, gitx.New(dir), "main", "initial", map[string]string{"README.md": "# other\n"})
	te.mustRun("init", "acme/other")
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

// withGatedStack is feat-a on main and feat-b on feat-a, one commit each,
// both gated.
func withGatedStack(te *testEnv) {
	initOnly(te)
	te.mustRun("feature", "start", "feat-a")
	te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
	te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
	te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
	te.mustRun("gate", "feat-a")
	te.mustRun("gate", "feat-b")
}

// keptFeature publishes feat-x under the grant and keeps its shadow branch.
func keptFeature(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
}

// archivedFeature publishes feat-x under the grant; Archive is the
// default, so it is handed off.
func archivedFeature(te *testEnv) {
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
}

// Run pre-scans argv for --json and --no-color; the reference must still
// report their real defaults.
func TestReferenceDefaultsIgnoreArgv(t *testing.T) {
	te := newTestEnv(t)
	code, env := te.runJSON("reference", "--no-color")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	data, _ := env["data"].(map[string]any)
	flags, _ := data["global_flags"].([]any)
	seen := map[string]string{}
	for _, f := range flags {
		m, _ := f.(map[string]any)
		seen[m["name"].(string)], _ = m["default"].(string)
	}
	for _, n := range []string{"json", "no-color"} {
		d, ok := seen[n]
		if !ok || d != "false" {
			t.Errorf("%s default %q (present %v), want false", n, d, ok)
		}
	}
}

func TestNormalizeSHAs(t *testing.T) {
	t.Parallel()
	full1 := "fdbdd961a40b1633b04895ab8e5e0a6a7e0c3f11"
	full2 := "3f55f01aba5c834d4915c93f0a1b2c3d4e5f6a7b"
	cases := []struct{ name, in, want string }{
		{"same hash, same placeholder",
			`{"a":"` + full1 + `","b":"` + full1 + `"}`,
			`{"a":"<SHA1>","b":"<SHA1>"}`},
		{"numbered by first appearance",
			`{"a":"` + full2 + `","b":"` + full1 + `","c":"` + full2 + `"}`,
			`{"a":"<SHA1>","b":"<SHA2>","c":"<SHA1>"}`},
		{"short prefix after the full",
			`{"a":"` + full1 + `","job":"gate/feat-x/` + full1[:12] + `"}`,
			`{"a":"<SHA1>","job":"gate/feat-x/<SHA1:12>"}`},
		{"short prefix before the full takes the full's number",
			`{"m":"candidate ` + full2[:12] + `","x":"` + full1 + `","a":"` + full2 + `"}`,
			`{"m":"candidate <SHA1:12>","x":"<SHA2>","a":"<SHA1>"}`},
		{"7-hex prefix",
			`{"a":"` + full1 + `","m":"at ` + full1[:7] + `"}`,
			`{"a":"<SHA1>","m":"at <SHA1:7>"}`},
		{"lone short hash",
			`{"commit":"0123456789ab"}`,
			`{"commit":"<SHA1:12>"}`},
		{"lengths pinned; a lone short hash gets its own number",
			`{"j":"gate/feat-x/` + full2[:12] + `","a":"` + full1 + `","m":"at ` + full1[:7] + ` and ` + full1[:12] + `"}`,
			`{"j":"gate/feat-x/<SHA1:12>","a":"<SHA2>","m":"at <SHA2:7> and <SHA2:12>"}`},
		{"6-hex ids untouched",
			`{"id":"20261005T120005Z-0c0d0e","g":"g-030405"}`,
			`{"id":"20261005T120005Z-0c0d0e","g":"g-030405"}`},
		{"41-hex untouched",
			`{"a":"` + full1 + `f"}`,
			`{"a":"` + full1 + `f"}`},
		{"64-hex untouched",
			`{"match_sha256":"76375e21aaeadbb33067f84087af1bb4f81a039c0f0e3ab2b684e20858af07ad"}`,
			`{"match_sha256":"76375e21aaeadbb33067f84087af1bb4f81a039c0f0e3ab2b684e20858af07ad"}`},
		{"not word-bounded untouched",
			`{"a":"x` + full1[:12] + `","b":"` + full1[:12] + `g","t":"20261005T120005Z"}`,
			`{"a":"x` + full1[:12] + `","b":"` + full1[:12] + `g","t":"20261005T120005Z"}`},
		{"finding ids untouched",
			`{"id":"f-7b1b6723317f","m":"f-000000000000: not a waivable finding"}`,
			`{"id":"f-7b1b6723317f","m":"f-000000000000: not a waivable finding"}`},
		{"root paths untouched",
			`{"p":"<ROOT>/home/` + full1[:12] + `/x.json","c":"` + full1 + `"}`,
			`{"p":"<ROOT>/home/` + full1[:12] + `/x.json","c":"<SHA1>"}`},
		{"one mapping across NDJSON lines",
			`{"job":"gate/feat-x/` + full2[:12] + `"}` + "\n" + `{"data":{"candidate":"` + full2 + `"}}` + "\n",
			`{"job":"gate/feat-x/<SHA1:12>"}` + "\n" + `{"data":{"candidate":"<SHA1>"}}` + "\n"},
		{"version strings untouched",
			`{"schema_version":"0.13","version":"v0.13.0"}`,
			`{"schema_version":"0.13","version":"v0.13.0"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSHAs(c.in)
			if got != c.want {
				t.Fatalf("normalizeSHAs:\n got  %s\n want %s", got, c.want)
			}
			if again := normalizeSHAs(c.in); again != got {
				t.Fatalf("not deterministic: %s then %s", got, again)
			}
		})
	}
}
