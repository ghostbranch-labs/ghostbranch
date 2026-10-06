// Package guide is the agent guide: embedded in the binary so it always
// matches it, with its code tables rendered from the contract registry, plus
// the small user-level skill that points agents at it.
package guide

import (
	_ "embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

//go:embed guide.md
var source string

//go:embed skill.md
var skill string

// Marker identifies a SKILL.md that Ghostbranch wrote and may replace or remove.
const Marker = "Installed by `ghostbranch guide --install`."

// Printed is `ghostbranch guide` output.
type Printed struct {
	Markdown string `json:"markdown"`
}

// InstallResult is `ghostbranch guide --install/--uninstall` output.
type InstallResult struct {
	Target  string           `json:"target"`
	Path    string           `json:"path"`
	Action  string           `json:"action"`
	Preview *preview.Preview `json:"preview"`
}

func Skill() string { return skill }

func cell(s string) string { return strings.ReplaceAll(s, "|", `\|`) }

// Render returns the guide with its exit and error tables filled from entries.
func Render(entries []contract.Entry) string {
	var exits, errs strings.Builder
	exits.WriteString("| Exit | Meaning | What to do |\n|---|---|---|\n")
	errs.WriteString("| Code | Exit | Meaning | Needs a person |\n|---|---|---|---|\n")
	for _, e := range entries {
		switch e.Kind {
		case contract.KindExit:
			fmt.Fprintf(&exits, "| %s | %s | %s |\n", e.Code, cell(e.Summary), cell(e.WhatToDo))
		case contract.KindError:
			who := "no"
			if e.NeedsHuman {
				who = "**yes**"
			}
			fmt.Fprintf(&errs, "| `%s` | %d | %s | %s |\n", e.Code, *e.ExitCode, cell(e.Summary), who)
		}
	}
	return strings.NewReplacer(
		"{{EXIT_CODES}}", strings.TrimRight(exits.String(), "\n"),
		"{{ERROR_CODES}}", strings.TrimRight(errs.String(), "\n"),
	).Replace(source)
}

// SkillPath is where the skill for target goes. It is always a user-level
// path, never inside a repo checkout: a committed skill file would reveal
// that a shadow repo exists.
func SkillPath(target string, getenv func(string) string, home func() (string, error)) (string, error) {
	switch target {
	case "claude":
		dir := strings.TrimSpace(getenv("CLAUDE_CONFIG_DIR"))
		if dir == "" {
			h, err := home()
			if err != nil {
				return "", contract.Wrap(contract.Internal, err, "find the home directory")
			}
			dir = filepath.Join(h, ".claude")
		}
		return filepath.Join(dir, "skills", "ghostbranch", "SKILL.md"), nil
	case "codex":
		h, err := home()
		if err != nil {
			return "", contract.Wrap(contract.Internal, err, "find the home directory")
		}
		return filepath.Join(h, ".agents", "skills", "ghostbranch", "SKILL.md"), nil
	}
	return "", contract.New(contract.BadFlag, "unknown agent %q: use claude or codex", target).WithField("install")
}

func foreign(path string) error {
	return contract.New(contract.BadFlag, "%s was not written by ghostbranch; remove or rename it yourself", path).WithField("install")
}

// Install writes the skill to path and says what it did (or, with dryRun,
// what it would do): installed, updated or unchanged.
func Install(path string, dryRun bool) (string, error) {
	old, err := os.ReadFile(path)
	action := "installed"
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return "", contract.Wrap(contract.Internal, err, "read %s", path)
	case string(old) == skill:
		return "unchanged", nil
	case !strings.Contains(string(old), Marker):
		return "", foreign(path)
	default:
		action = "updated"
	}
	if dryRun {
		return action, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", contract.Wrap(contract.Internal, err, "create %s", filepath.Dir(path))
	}
	if err := os.WriteFile(path, []byte(skill), 0o644); err != nil {
		return "", contract.Wrap(contract.Internal, err, "write %s", path)
	}
	return action, nil
}

// Uninstall removes a skill Ghostbranch wrote, and its folder when empty.
func Uninstall(path string, dryRun bool) (string, error) {
	old, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", contract.Wrap(contract.Internal, err, "read %s", path)
	case !strings.Contains(string(old), Marker):
		return "", foreign(path)
	}
	if dryRun {
		return "removed", nil
	}
	if err := os.Remove(path); err != nil {
		return "", contract.Wrap(contract.Internal, err, "remove %s", path)
	}
	_ = os.Remove(filepath.Dir(path)) // only succeeds when empty
	return "removed", nil
}
