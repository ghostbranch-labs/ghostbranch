package guide_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
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
	if n := strings.Count(out, "\n"); n > 250 {
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
		"Any token can read public repos, so for a public repo you only grant permissions on the shadow repo",
		"never give it to an agent", "Actions write (optional: lets `sync` cancel the workflow runs its pushes start)",
		"add `--shadow <owner/name>` to both", "(`--no-cancel` skips that)",
		"Write the promote `--message` yourself from the change", "show it to the person; use theirs if they give one",
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
