package cli_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/console"
)

func afterPublish(env map[string]any) map[string]any {
	data, _ := env["data"].(map[string]any)
	if applied, ok := data["applied"].(map[string]any); ok {
		data = applied
	}
	m, _ := data["after_publish"].(map[string]any)
	return m
}

// The CLI hands off by default (R3); --keep keeps.
func TestApplyArchivesByDefaultAndKeepKeeps(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--plan")
	code, env := te.runJSON("apply", te.lastPlanID(), "--yes")
	if code != 0 || afterPublish(env)["feat-x"] != "archived" {
		t.Fatalf("apply --yes: exit %d, %v", code, env)
	}
	te2 := newTestEnv(t)
	publishable(te2)
	code, env = te2.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
	if code != 0 || afterPublish(env)["feat-x"] != "kept" {
		t.Fatalf("promote --keep: exit %d, %v", code, env)
	}
}

// At a console the person sees the choice above the code; typing k keeps.
func TestConsolePublishAsksArchiveOrKeep(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	ty := atConsoleChoosing(te, "k\n", typeTheCode)
	code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature")
	seen := ty.seen.String()
	if code != 0 || afterPublish(env)["feat-x"] != "kept" || !strings.Contains(seen, "After publishing, this feature:") ||
		!strings.Contains(seen, "❯ Archive") || strings.Index(seen, "After publishing") > strings.Index(seen, "type this code") {
		t.Fatalf("exit %d, %v\nconsole:\n%s", code, env, seen)
	}
	// The code is shown only after the question line (review M3).
	q, c := strings.Index(seen, console.ChoicePrompt), strings.Index(seen, "type this code")
	if q < 0 || c < q {
		t.Fatalf("the code isn't after the question (question at %d, code at %d):\n%s", q, c, seen)
	}
}

// At a console Enter takes the default, Archive.
func TestConsolePublishEnterArchives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	atConsoleChoosing(te, "\n", typeTheCode)
	if code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature"); code != 0 || afterPublish(env)["feat-x"] != "archived" {
		t.Fatalf("exit %d, %v", code, env)
	}
}

func TestFeatureArchiveCommand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	publishable(te)
	te.mustRun("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes", "--keep")
	code, env := te.runJSON("feature", "archive", "feat-x", "--plan")
	if data, _ := env["data"].(map[string]any); code != 0 || data["preview"] == nil {
		t.Fatalf("--plan: exit %d, %v", code, env)
	}
	code, env = te.runJSON("feature", "archive", "feat-x")
	if data, _ := env["data"].(map[string]any); code != 0 || data["changed"] != true {
		t.Fatalf("archive: exit %d, %v", code, env)
	}
	code, env = te.runJSON("feature", "archive", "feat-x")
	if data, _ := env["data"].(map[string]any); code != 0 || data["changed"] != false {
		t.Fatalf("again: exit %d, %v", code, env)
	}
}
