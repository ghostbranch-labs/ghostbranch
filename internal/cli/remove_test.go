package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// log reads the local ledger only: no token, no forge, no pair.
func TestLogNeedsNoToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.env.NewForge = func(string) (forge.Forge, error) {
		t.Error("log built a GitHub client")
		return te.forge, nil
	}
	entries := func(args ...string) []any {
		t.Helper()
		code, out, errOut := te.run(append([]string{"log", "--json"}, args...)...)
		if code != 0 {
			t.Fatalf("log %v: exit %d\n%s\n%s", args, code, out, errOut)
		}
		var env struct{ Data json.RawMessage }
		_ = json.Unmarshal([]byte(out), &env)
		validate(t, "log.schema.json", env.Data)
		var d struct{ Entries []any }
		_ = json.Unmarshal(env.Data, &d)
		return d.Entries
	}
	es := entries()
	if len(es) != 2 || es[0].(map[string]any)["op"] != "feature start" || es[1].(map[string]any)["op"] != "init" {
		t.Fatalf("want feature start then init, got %v", es)
	}
	if es := entries("--op", "init"); len(es) != 1 {
		t.Fatalf("--op init: %v", es)
	}
	if es := entries("--feature", "feat-x", "--actor", "human:"); len(es) != 1 {
		t.Fatalf("--feature feat-x --actor human: %v", es)
	}
	if es := entries("--limit", "1"); len(es) != 1 {
		t.Fatalf("--limit 1: %v", es)
	}
	// The test clock starts at 2026-10-05T12:00:00Z.
	if es := entries("--since", "24h"); len(es) != 2 {
		t.Fatalf("--since 24h: %v", es)
	}
	if es := entries("--since", "2026-10-06T00:00:00Z"); len(es) != 0 {
		t.Fatalf("--since tomorrow: %v", es)
	}
	for _, bad := range [][]string{{"--since", "yesterday"}, {"--limit", "-1"}} {
		code, env := te.runJSON(append([]string{"log"}, bad...)...)
		e, _ := env["error"].(map[string]any)
		if code != 2 || e["code"] != "BAD_FLAG" || e["field"] != strings.TrimPrefix(bad[0], "--") {
			t.Fatalf("%v: %d %v", bad, code, env)
		}
	}
	code, out, _ := te.run("log")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.Contains(lines[0], "feature start") || !strings.Contains(lines[0], "feat-x") ||
		!strings.Contains(lines[0], "exit 0") || !strings.HasPrefix(lines[0], "2026-10-05T") {
		t.Fatalf("human log: %d %q", code, out)
	}
}

func TestRemoveThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	atConsole(te, typeTheCode)
	code, out, errOut := te.run("remove")
	if code != 0 {
		t.Fatalf("remove: %d\n%s\n%s", code, out, errOut)
	}
	want := "The shadow repo acme/widget-shadow still exists on GitHub; delete it there if you no longer need it (Settings → Delete this repository).\n"
	if !strings.HasSuffix(out, want) {
		t.Fatalf("human output must end with the shadow repo note:\n%s", out)
	}
	ws := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default")
	if _, err := os.Stat(filepath.Join(ws, "pairs", "acme__widget")); !os.IsNotExist(err) {
		t.Fatalf("the pair directory is still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(ws, "grants.json")); strings.Contains(string(b), "acme/widget") {
		t.Fatalf("the grant survived:\n%s", b)
	}
	if code, env := te.runJSON("sync"); code != 2 || env["error"].(map[string]any)["code"] != "PAIR_AMBIGUOUS" {
		t.Fatalf("the pair is still registered: %d %v", code, env)
	}
}
