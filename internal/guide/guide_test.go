package guide_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/guide"
)

func TestRenderListsEveryCodeFromTheRegistry(t *testing.T) {
	out := guide.Render(contract.Registry())
	if strings.Contains(out, "{{") {
		t.Fatal("a placeholder was left in the guide")
	}
	for _, c := range contract.AllCodes() {
		if !strings.Contains(out, "| `"+string(c)+"` |") {
			t.Errorf("%s is missing from the error table", c)
		}
	}
	for x := 0; x <= 6; x++ {
		if !strings.Contains(out, "\n| "+string(rune('0'+x))+" |") {
			t.Errorf("exit %d is missing from the exit table", x)
		}
	}
	// 270: the history-modes section (sub-project 5a) added about 18 lines;
	// 272: the adopt sentence in *Set up a repo*, with the SHADOW_CREATE_DENIED hand-off kept whole.
	// 274: two sub-project 5b error codes (UPGRADE_NEEDED, FEATURE_MOVED).
	// 293: the lifecycle section (with --repo on each example and the grant note), --repo in the loop, and the stdout/stderr note (sub-project 5b).
	// 296: the SHADOW_BEHIND error row, data.checkout as argvs, and feature start refusing a stale shadow main (sub-project 5b).
	// 314: the jobs section (+15), and the sub-project 5c error rows (JOB_STALE and the other new codes).
	// 318: the jobs section's recovery lines (status names a pending job, an empty current_job, promote --detach's next step).
	// 332: 5d-1 (locks, guard, records, interrupted publish, config keys, cancels).
	// 333: the guard bullet says a branch named after a known feature is refused whatever it carries (5d-1 final review).
	// 341: 5d-2 (newer merged features, every branch_gone a person closes, the ledger's name filter, file modes, the new warnings and the unreadable state).
	// 343: the final review's fixes (an unreadable record may be a passing read error; where a newer merged feature shows).
	// 347: the four sub-project 6.1 error rows (LOGIN_EXPIRED, LOGIN_UNAVAILABLE, TOKEN_PERMISSION_MISSING, DOCTOR_FAILED).
	// 376: 6.1 (doctor, guided init, TOKEN_PERMISSION_MISSING, the agent rule, the Tokens section, the isolation check), plus 6.1's error rows
	// 377: the 6.1 final review's fixes (doctor's fix command only when there is one; pick the public and shadow repos when installing the app).
	// 378: guided init asks where an org's shadow repo lives, and --shadow sets it (6.1 Task 10).
	// 379: one line on terminal colour, --no-color and NO_COLOR.
	// 383: the four sub-project 6.1a error rows (FOLD_NEEDS_REBASE, HAS_CHILDREN, NOT_A_STACK, PARENT_REWRITTEN).
	// 406: the PUBLIC_BRANCH_CHANGED error row (sub-project 6.1a-2).
	// 426: the Updating an open PR section and feature pull (sub-project 6.1a-2, 20 lines, with the stack refusals).
	// 428: PUBLIC_BRANCH_CHANGED's reasons unrecorded_update and adopted_missing (6.1a-2 final review).
	// 429: commits pulled in from the PR keep their authors (6.1a-2 final review, owner's ruling).
	// 432: a clean rebase replaces, and stale_gate's update_mode (6.1a-2 acceptance fixes).
	// 443: the Updates section and the stderr line (6.4a).
	// 446: the three 6.3b error rows (PAIR_NOT_FOUND, NO_OPEN_PR, PLAN_NOT_FOUND).
	// 452: how a native stack merges and what a squash loses (6.3b, F-16/F-31).
	// 453: the FEATURE_ARCHIVED error row (6.5).
	// 472: the After publishing section, the archived state, the Never exception and the guard's archived-branch sentence (6.5).
	// 473: the hand-off follow-ups: how to reach the PR branch, merged_moved, the three archive-blocked cases (6.5 Task 11 review).
	// 480: all 14 states in the Lifecycle list, and in After publishing waivers add, the two stacking refusals and the long lines rewrapped (6.6).
	// 497: the When the person is stuck section (14 lines), login's install wording, and Strict
	// isolation's pre-filled token link with the repository choice it can't make (Tier 1 setup guidance).
	if n := strings.Count(out, "\n"); n > 497 {
		t.Errorf("the guide is %d lines; keep it short (spec: about 150 plus the tables)", n)
	}
}

func TestSkillIsAValidAgentSkill(t *testing.T) {
	s := guide.Skill()
	if !strings.HasPrefix(s, "---\nname: ghostbranch\ndescription: ") || !strings.Contains(s, guide.Marker) {
		t.Fatalf("SKILL.md must start with name/description frontmatter and carry the marker:\n%s", s)
	}
}

func TestSkillPath(t *testing.T) {
	home := func() (string, error) { return filepath.FromSlash("/home/u"), nil }
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		target string
		vars   map[string]string
		want   string
	}{
		{"claude", nil, "/home/u/.claude/skills/ghostbranch/SKILL.md"},
		{"claude", map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}, "/cfg/skills/ghostbranch/SKILL.md"},
		{"codex", map[string]string{"CLAUDE_CONFIG_DIR": "/cfg"}, "/home/u/.agents/skills/ghostbranch/SKILL.md"},
	}
	for _, c := range cases {
		got, err := guide.SkillPath(c.target, env(c.vars), home)
		if err != nil || got != filepath.FromSlash(c.want) {
			t.Errorf("%s %v: %q %v", c.target, c.vars, got, err)
		}
	}
	_, err := guide.SkillPath("cursor", env(nil), home)
	if ce := contract.From(err); ce == nil || ce.Code != contract.BadFlag || ce.Field != "install" {
		t.Fatal(err)
	}
	_, err = guide.SkillPath("codex", env(nil), func() (string, error) { return "", errors.New("no home") })
	if err == nil {
		t.Fatal("no home directory must be an error")
	}
}

func TestInstallAndUninstall(t *testing.T) {
	skills := t.TempDir()
	path := filepath.Join(skills, "ghostbranch", "SKILL.md")
	other := filepath.Join(skills, "someone-else", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(other), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("theirs"), 0o644); err != nil {
		t.Fatal(err)
	}
	step := func(name string, fn func(string, bool) (string, error), dry bool, want string) {
		t.Helper()
		if got, err := fn(path, dry); err != nil || got != want {
			t.Fatalf("%s: %q %v, want %q", name, got, err, want)
		}
	}
	step("dry install", guide.Install, true, "installed")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("--dry-run wrote the skill")
	}
	step("install", guide.Install, false, "installed")
	step("install again", guide.Install, false, "unchanged")
	if err := os.WriteFile(path, []byte(guide.Marker+"\nold version"), 0o644); err != nil {
		t.Fatal(err)
	}
	step("upgrade", guide.Install, false, "updated")
	step("uninstall", guide.Uninstall, false, "removed")
	step("uninstall again", guide.Uninstall, false, "absent")
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatal("uninstall must remove the empty skill folder")
	}
	if b, _ := os.ReadFile(other); string(b) != "theirs" {
		t.Fatal("other skills must be left alone")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("a hand-written skill"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, fn := range map[string]func(string, bool) (string, error){"install": guide.Install, "uninstall": guide.Uninstall} {
		if _, err := fn(path, false); contract.From(err) == nil || contract.From(err).Code != contract.BadFlag {
			t.Errorf("%s over a foreign SKILL.md: %v", name, err)
		}
	}
}

func TestGuideTellsAgentsToHandWaiversToAPerson(t *testing.T) {
	g := guide.Render(contract.Registry())
	for _, want := range []string{"## When the gate finds a leak", "waivers add", "waivers.json", "MARKER_INVALID", "## Internal sections (for people)",
		"give the person the `waivers add` command", "Never change `leak_scan`"} {
		if !strings.Contains(g, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

func TestGuideCoversSetupAndCIParity(t *testing.T) {
	out := guide.Render(contract.Registry())
	for _, want := range []string{
		"## Set up a repo", "SHADOW_CREATE_DENIED", "ghostbranch init <public> --adopt",
		"Never ask the person for a token with wider access",
		"## Keeping up with public", "ghostbranch sync", "SHADOW_BEHIND", "sync.schedule", "ghostbranch upgrade",
		"GHOSTBRANCH_SYNC_TOKEN", "never public write",
		"## Token permissions (for people)", "Administration", "Secrets: read",
		"REPO_GUARD_UNREWRITABLE",
		"of `error.details.steps`", "Shadow: Contents, Pull requests and Workflows write",
		"other than through `ghostbranch init`", "`SYNC_DIVERGED`, which needs a",
		"Shadow: Administration, Contents and Workflows read and write |",
		"afterwards narrow it to the shadow repo or delete it, and give an agent only its own shadow-only token (see *Strict isolation*)",
		"Any token can read public repos, so for a public repo you only grant permissions on the shadow repo",
		"never give it to an agent", "Actions write (optional: lets `sync` cancel the workflow runs its pushes start)",
		"add `--shadow <owner/name>` to both", "(`--no-cancel` skips that)",
		"Write the PR `--title` (and, for squash, `-m`;", "use theirs if they give them",
		"you may rewrite the fixture to build the value at", "Never waive it yourself",
		"tell the person in your handover which finding you", "Anything that might be real",
		"applies its permissions to every repo it", "it could write that repo too: scheduled sync isn't supported",
		"Actions write is optional: it lets",
		"`<existing> && (github.event_name != 'push' || github.repository == '<public>')`",
		"Once the person confirms both are done", "deploy or publish credentials", "`ci.rewrite_repo_guards: warn`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
}

// Only promote and apply write to the public repo: the table is where a
// person is sent to widen an agent's token, so any other row granting
// public write would hand the agent the publish.
func TestTokenTableGrantsPublicWriteOnlyToPromote(t *testing.T) {
	out := guide.Render(contract.Registry())
	_, table, _ := strings.Cut(out, "## Token permissions (for people)")
	table, _, _ = strings.Cut(table, "\n## ")
	rows := 0
	for _, line := range strings.Split(table, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		rows++
		cells := strings.Split(line, "|")
		cmd, perms := cells[1], cells[2]
		i := strings.Index(perms, "Public:")
		public := i >= 0 && strings.Contains(perms[i:], "write") ||
			strings.Contains(perms, "two repos") && strings.Contains(perms, "write")
		if public != strings.Contains(cmd, "promote") {
			t.Errorf("row %q: public write = %v", strings.TrimSpace(cmd), public)
		}
	}
	if rows != 6 {
		t.Fatalf("found %d token rows, want 6", rows)
	}
}

// An agent syncs before every feature, not only after a SHADOW_BEHIND warning,
// which feature start reports only once the branch already exists.
func TestGuideSaysToSyncBeforeEveryFeature(t *testing.T) {
	out := guide.Render(contract.Registry())
	for _, want := range []string{"**Always run `ghostbranch sync --repo <public> --json` first**", "before `feature start` or resuming"} {
		if !strings.Contains(out, want) {
			t.Fatalf("guide is missing %q", want)
		}
	}
}

func TestGuideTeachesHistoryModes(t *testing.T) {
	out := guide.Render(contract.Registry())
	for _, want := range []string{"(`preserve` unless set); pass `--mode` to choose", "--curate", `"commits"`, "author_email", "--title", "--mode squash",
		"`ghostbranch promote <name> --repo <public> --plan --json` publishes in the repo's `promote.default_mode`",
		"Never waive a finding yourself; a person may waive a false positive at their console.",
		"`git push --force-with-lease <shadow remote> <name>`", "introduce or change it"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide is missing %q", want)
		}
	}
	if strings.Contains(out, `--mode squash -m "<message>" --plan --json`) {
		t.Error("squash must not be the step-4 command; preserve is the default")
	}
}

func TestGuideTellsAgentsToAdoptAnExistingShadowRepo(t *testing.T) {
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"Shadow repo already exists (another machine, or a fresh `GHOSTBRANCH_HOME`)?",
		"`ghostbranch init <public> --adopt --plan --json`",
		"it only registers the pair here and checks the shadow repo",
		// The SHADOW_CREATE_DENIED hand-off (whitespace-normalised here, as the guide wraps lines).
		"show the person `error.details.steps`",
		"(the command in `next`; its `needs_human` stands for their steps 1 and 2)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

func TestGuideTeachesTheLifecycle(t *testing.T) {
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## Lifecycle",
		"`ghostbranch status --repo <public> --json` shows the pair's health and every feature's state",
		"`ghostbranch sync --repo <public>` closes features whose public PR merged and whose branch hasn't moved",
		"`ghostbranch feature close <name> --repo <public>` with `needs_human`",
		"needs a person at the console unless the feature is merged and unmoved",
		"`merged_moved`, `pr_closed` and any `branch_gone` that `sync` leaves",
		"an agent grant does not cover it, so don't ask for `agents allow` to unblock a close",
		"`ghostbranch feature abandon <name> --repo <public>` parks a feature and deletes nothing",
		"`ghostbranch log --repo <public>` shows the history",
		"Never close or abandon a feature a person is still working on.",
		"JSON is on stdout and progress on stderr: never merge the two streams",
		"`ghostbranch feature start <name> --repo <public> --json`",
		"`ghostbranch gate <name> --repo <public> --json`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// An agent that reads the first line of `wait --json` gets a progress event
// instead of the result, and one that re-gates after an interrupt starts the
// CI it was already waiting on.
func TestGuideTeachesJobsAndWait(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## Long CI: jobs",
		"`gate` waits for CI by default; that is the normal path",
		"`ghostbranch gate <name> --detach --repo <public> --json` returns `data.job` once CI has started",
		"`ghostbranch wait <job> --repo <public> --json`",
		// The last-line rule.
		"`wait --json` prints one JSON object per line. **The last line is the result**, the same as `gate --json`",
		"the lines before it are progress events",
		"A finished job answers at once.",
		"`wait --timeout 10m` returns `checks: pending` (exit 0) if CI is still running",
		// The interrupt rule.
		"After Ctrl-C you get `INTERRUPTED`.",
		"With `error.details.job`, CI is still running: run `ghostbranch wait <job> --repo <public> --json`, not `gate`.",
		"Without `job` or `plan_id` nothing is pending: run the command again.",
		"If the session dropped, `ghostbranch status --repo <public> --json` names a pending job (`gate.job`): wait on it.",
		"`JOB_STALE`", "`error.details.current_job`", "if `current_job` is empty, gate again",
		"`promote --detach` returns the job in `data.gate.job` and no plan; once `wait` passes, run the same promote without `--detach` (it reuses the green gate).",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// An agent that deletes a lock file, promotes again after an interrupt, or
// writes over a record from a newer Ghostbranch does damage the CLI can't undo.
func TestGuideTeachesHardening(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		// Locks.
		"`LOCKED`: another ghostbranch command is running on this machine (`error.details.held_by`",
		"never delete lock files",
		// The pre-push guard.
		"refuses any push that carries an unpublished feature commit the mirror knows of, under any branch name",
		"A branch named after a feature the mirror knows is refused whatever it carries, rewritten commits included.",
		"Commits rewritten out of the shadow repo can't be recognized",
		// Config keys.
		"An unknown key in `.ghostbranch.yml`, including under `sync:` or `ci:`",
		"`error.details.unknown_key`",
		// Newer records.
		"`UPGRADE_NEEDED` with `error.details.record`",
		"`RECORD_NEWER`",
		"such a feature is never closed unattended",
		// An interrupted publish.
		"`INTERRUPTED` with `error.details.plan_id` after `promote`",
		"Resume with `ghostbranch apply <plan_id> --repo <public>` (as `next` says; with `needs_human` the person runs it), never `promote` again",
		"`BRANCH_EXISTS` with `error.details.plan_id`",
		// Verified cancels.
		"`cancelled_runs` lists only runs confirmed cancelled",
		"`CI_NOT_CANCELLED` means runs may still be going: the token couldn't cancel them (it needs Actions write), they kept going, or sync couldn't check; it names the runs it knows of, or counts them",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// What 5d-2 taught: newer merged features, every branch_gone a person
// closes, the ledger's name filter, file modes, and the new warnings and state.
func TestGuideTeachesPolish(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"A merged feature whose record is newer still counts as active: it is in `sync`'s `data.attention`, and `status` and `next` give `feature close` with `needs_human`",
		"once this machine is upgraded, `sync` closes it",
		"gated again after the publish, with its gate branch moved, or with a record too old to say what was published",
		"`--feature <name>` also lists the entries of an earlier feature with the same name",
		"Plan files, grants, waivers and the ledger are mode 0600",
		"`LEDGER_NOT_WRITTEN`: keep `details.refs`",
		"`PLAN_FILE_UNREADABLE`: fix or delete the file at `details.path`",
		"and `unreadable` (a record that can't be read: `feature show <name>` gives the error; run the command again if it was a passing failure, as a corrupt one only a person at the console closes",
		"`remove` needs no GitHub token",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// What 6.1 taught: login and the resolution order, doctor, guided init,
// and the agent-token warning.
func TestGuideTeachesSetup(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"**`ghostbranch doctor --json` checks the setup and changes nothing in your repos or records.**",
		"`DOCTOR_FAILED` lists the failing checks in `error.details.failed`",
		"with a `fix` command when there is one.",
		"Then install the app on the account that owns your public repo and pick that repo; `ghostbranch init` creates the shadow repo.",
		"`init` asks questions only when a person runs it at a terminal; with `--json` it never asks.",
		"`--guard`/`--no-guard`, `--sync`/`--no-sync`",
		"For an org's public repo it asks where the shadow repo lives; `--shadow <owner/name>` sets it.",
		"`TOKEN_PERMISSION_MISSING` names the permission and the repo in `error.details.permission` and `error.details.repo`",
		"Never run `ghostbranch login` or `ghostbranch logout`",
		"`GHOSTBRANCH_TOKEN`, then your stored login, then `GH_TOKEN`, `GITHUB_TOKEN`, then `gh auth token`",
		"With `GHOSTBRANCH_TOKEN` set, the stored login is never read.",
		"`ghostbranch login --with-token` stores a token you pipe in on stdin",
		"It doesn't revoke the app at GitHub",
		"`AGENT_ON_PERSON_LOGIN`",
		"`\"token_kind\"`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// An agent that promotes a stack's branches one by one, gates out of
// order, or restacks by hand publishes the wrong thing or nothing.
func TestGuideTeachesStacks(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## Stacks",
		"`ghostbranch feature start <b> --from <a> --repo <public> --json`",
		"Gate from the bottom up",
		"scans only its own commits",
		"`next` walks you up the stack",
		"`parent_stale`",
		"`ghostbranch feature restack <parent> --repo <public> --json`",
		"`ghostbranch promote a b c --repo <public> --plan --json` plans the whole stack, bottom first",
		"One approval publishes every PR",
		"`--title-for b=\"<title>\"`",
		"`NOT_A_STACK`", "`error.details.reason`",
		"`ghostbranch feature fold <c> --repo <public> --json`",
		"`HAS_CHILDREN`", "`feature restack <child> --onto main`",
		"`public_pr_needs_update`",
		// F-16/F-31: how a stack merges, and what a squash loses.
		"Merging is a maintainer's call, on GitHub.",
		"on its top PR's page, the merge button merges that PR and every PR below it",
		"Merging a lower PR lands only part of the stack.",
		"`gh pr merge` refuses a stacked PR; from a terminal use `PUT repos/<public>/pulls/<top>/merge-async` with `merge_method=merge`",
		"\"Squash and merge\" makes one commit per PR and loses a `preserve` or `curated` history",
		"Never merge a PR yourself unless the person told you to.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// An agent that opens a second PR after a review, or force-pushes over a
// maintainer's commit, loses the review or their work.
func TestGuideTeachesUpdatingAnOpenPR(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## Updating an open PR",
		"promote updates the open PR",
		"`pr_behind`", "`#14 (behind)`",
		"`append`", "`replace`", "`replace_reason`", "`expected_head`",
		"`promote a` updates the open PRs above a too",
		"from the bottom up", "`open_above`", "a person closes or merges every line's PRs but one, then `promote a` again",
		"is always replaced (a leased force-push), never appended", "`published`, `pr_behind`,",
		"`PUBLIC_BRANCH_CHANGED`", "`ghostbranch feature pull <name> --repo <public> --json`",
		"the update keeps their commits",
		"`PLAN_STALE` with `public_branch` in `error.details.moved`",
		"promote it: the update replaces its PR's branch",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
	if strings.Contains(out, "a person updates the public PR by hand") {
		t.Error("the guide still says a person updates the public PR by hand")
	}
}

// An agent that runs the upgrade itself, or buries an advisory, changes the
// person's install without asking.
func TestGuideTeachesUpdates(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## Updates",
		"`UPDATE_AVAILABLE`",
		"never run the upgrade yourself",
		"`GHOSTBRANCH_NO_UPDATE_CHECK=1`",
		"Progress is on stderr only, never in `--json` stdout; with `--json` the update reminder comes only as the `UPDATE_AVAILABLE` warning",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// What 6.5 taught: Archive is the default, a handed-off PR is fixed by
// pushing to it, and what refuses it.
func TestGuideTeachesHandOff(t *testing.T) {
	t.Parallel()
	out := strings.Join(strings.Fields(guide.Render(contract.Registry())), " ")
	for _, want := range []string{
		"## After publishing",
		"**Archive** (the default) or **Keep**",
		"Under `--yes` or `--non-interactive` it archives unless you pass `--keep`",
		"`data.after_publish` says, per feature: `archived`, `kept` or `incomplete`",
		"the shadow branch moves to `refs/ghostbranch/archive/<name>` (nothing is lost)",
		"`git push origin HEAD:<branch>`. No gate, no plan, no apply.",
		"never on the old shadow branch: the guard refuses its private commits",
		"`promote`, `gate`, `feature pull` and `waivers add` refuse an archived feature (`FEATURE_ARCHIVED`)",
		"nothing can be started from it (`feature start --from`) or restacked onto it (`feature restack --onto`)",
		"`ghostbranch feature archive <name> --repo <public> --json`",
		"feature's own PR branch (see *After publishing*)",              // the Never bullet
		"deletes their shadow branches (and an archived one's archive)", // the sync bullet
		"`git fetch origin && git switch --detach origin/<branch>`",
		"(unless it reads `merged_moved`: a person closes it)",
		"(a handed-off PR: see *After publishing*)",
		"`ARCHIVE_INCOMPLETE`: the publish stands",
		"An older Ghostbranch shows an archived feature as `branch_gone`: update it.",
		"`branch_gone`, `archived`, `abandoned`",
		"The one exception is an `archived` feature's own PR branch",
		"a push to it is allowed unless it carries the archive's private commits", // P22, the guard section
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
}

// F-17: the Lifecycle list names every state status can show, so an agent
// that meets one has a definition to act on.
func TestGuideListsEveryState(t *testing.T) {
	t.Parallel()
	out := guide.Render(contract.Registry())
	_, sec, ok := strings.Cut(out, "## Lifecycle")
	if !ok {
		t.Fatal("the guide has no ## Lifecycle section")
	}
	list, _, ok := strings.Cut(sec, "\n- `ghostbranch sync")
	if !ok {
		t.Fatal("the Lifecycle section has no sync bullet to end the state list")
	}
	if len(feature.States) != 14 {
		t.Fatalf("feature.States has %d states, want 14", len(feature.States))
	}
	for _, s := range feature.States {
		if !strings.Contains(list, "`"+string(s)+"`") {
			t.Errorf("the Lifecycle list lacks `%s`", s)
		}
	}
}

// feature.States is exactly the feature report's state enum, in order, so
// the guide test above covers every state the contract can carry.
func TestStatesMatchTheFeatureReportSchema(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("..", "..", "schemas", "v0", "feature-report.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties struct {
			State struct {
				Enum []string `json:"enum"`
			} `json:"state"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(feature.States))
	for i, st := range feature.States {
		got[i] = string(st)
	}
	if !slices.Equal(got, s.Properties.State.Enum) {
		t.Fatalf("feature.States = %v\nschema enum   = %v", got, s.Properties.State.Enum)
	}
}

// Strict isolation gives the agent's token as a pre-filled link, from the
// same list forge keeps, and says the repository choice the link can't make.
func TestGuideLinksTheAgentsToken(t *testing.T) {
	out := guide.Render(contract.Registry())
	link := forge.TokenLink("ghostbranch-agent-<repo>", "Ghostbranch agent: the shadow repo only", "<shadow-owner>", 30, forge.AgentTokenPermissions)
	sec := section(t, out, "## Strict isolation (for people)")
	if !strings.Contains(sec, link) {
		t.Fatalf("no agent token link %s in:\n%s", link, sec)
	}
	if !strings.Contains(strings.Join(strings.Fields(sec), " "), `choose "Only select repositories", then pick only the shadow repo`) {
		t.Fatalf("no repository choice:\n%s", sec)
	}
}

// When the person is stuck: the agent diagnoses, hands over one person-only
// step at a time, and never touches a token.
func TestGuideSaysWhatToDoWhenThePersonIsStuck(t *testing.T) {
	sec := strings.Join(strings.Fields(section(t, guide.Render(contract.Registry()), "## When the person is stuck")), " ")
	for _, want := range []string{"ghostbranch doctor --json", "ghostbranch explain <CODE> --json", "one step at a time",
		"`AUTH_MISSING`", "`LOGIN_EXPIRED`", "ghostbranch login", "in their own terminal", "install link", "Strict isolation",
		"Never ask for, read or paste a token", "never ask for wider access"} {
		if !strings.Contains(sec, want) {
			t.Errorf("the section lacks %q:\n%s", want, sec)
		}
	}
	if n := strings.Count(section(t, guide.Render(contract.Registry()), "## When the person is stuck"), "\n"); n > 16 {
		t.Errorf("the section is %d lines; keep it about 8-12", n)
	}
}

// Tokens (for people) gives login's install wording: the public repo's
// account and that repo; init creates the shadow repo.
func TestGuideTokensSaysWhatToInstall(t *testing.T) {
	sec := strings.Join(strings.Fields(section(t, guide.Render(contract.Registry()), "## Tokens (for people)")), " ")
	if !strings.Contains(sec, "install the app on the account that owns your public repo and pick that repo; `ghostbranch init` creates the shadow repo") {
		t.Fatalf("install wording:\n%s", sec)
	}
	if strings.Contains(sec, "pick both the public repo and its shadow repo") {
		t.Fatalf("old wording:\n%s", sec)
	}
}

// section is the guide from heading to the next "## " heading.
func section(t *testing.T, out, heading string) string {
	t.Helper()
	i := strings.Index(out, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("no %q section", heading)
	}
	rest := out[i+1+len(heading):]
	if j := strings.Index(rest, "\n## "); j >= 0 {
		rest = rest[:j]
	}
	return rest
}
