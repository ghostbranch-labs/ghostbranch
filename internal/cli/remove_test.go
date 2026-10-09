package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
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
	for _, bad := range [][]string{{"--since", "yesterday"}, {"--limit", "-1"}, {"--limit", "0"}, {"--repo", "widget"}} {
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

// log --repo shows one pair's entries: a registered public or shadow repo,
// any other owner/name (a removed pair's history), or . for the pair this
// checkout's remotes name. It still needs no token.
func TestLogRepoFiltersByPair(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.env.NewForge = func(string) (forge.Forge, error) {
		t.Error("log built a GitHub client")
		return te.forge, nil
	}
	ws := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default")
	if err := ledger.Append(filepath.Join(ws, "ledger.jsonl"), ledger.Entry{TS: time.Date(2026, 10, 5, 13, 0, 0, 0, time.UTC),
		Actor: "human:octo", Op: "remove", Pair: "acme/other", Summary: "removed acme/other"}); err != nil {
		t.Fatal(err)
	}
	pairs := func(args ...string) string {
		t.Helper()
		code, env := te.runJSON(append([]string{"log"}, args...)...)
		if code != 0 {
			t.Fatalf("log %v: exit %d %v", args, code, env)
		}
		data, _ := env["data"].(map[string]any)
		es, _ := data["entries"].([]any)
		var out []string
		for _, e := range es {
			out = append(out, e.(map[string]any)["pair"].(string)+" "+e.(map[string]any)["op"].(string))
		}
		return strings.Join(out, ",")
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{nil, "acme/other remove,acme/widget init"},
		{[]string{"--repo", "acme/widget"}, "acme/widget init"},
		{[]string{"--repo", "ACME/Widget"}, "acme/widget init"},
		{[]string{"--repo", "acme/widget-shadow"}, "acme/widget init"},
		{[]string{"--repo", "acme/other"}, "acme/other remove"},
		{[]string{"--repo", "acme/none"}, ""},
	} {
		if got := pairs(c.args...); got != c.want {
			t.Errorf("log %v: got %q, want %q", c.args, got, c.want)
		}
	}
	// --repo . names the pair from this checkout's remotes, or fails.
	code, env := te.runJSON("log", "--repo", ".")
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "PAIR_AMBIGUOUS" {
		t.Fatalf("--repo . outside a checkout: %d %v", code, env)
	}
	te.env.CwdRemotes = func() []workspace.Remote {
		return []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget.git"}}
	}
	if got := pairs("--repo", "."); got != "acme/widget init" {
		t.Errorf("--repo . in a widget checkout: got %q", got)
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

// remove asks GitHub nothing: no token, no client, and no login.
func TestRemoveNeedsNoToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.env.NewForge = func(string) (forge.Forge, error) {
		t.Error("remove built a GitHub client")
		return te.forge, nil
	}
	if code, env := te.runJSON("remove", "--plan"); code != 0 || len(env["warnings"].([]any)) != 0 {
		t.Fatalf("remove --plan: %d %v", code, env)
	}
	atConsole(te, typeTheCode)
	code, env := te.runJSON("remove")
	ws, _ := env["warnings"].([]any)
	if code != 0 || len(ws) != 1 || ws[0].(map[string]any)["code"] != "ACTOR_UNKNOWN" {
		t.Fatalf("remove: %d %v", code, env)
	}
	es, err := ledger.Read(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "ledger.jsonl"))
	if err != nil || es[len(es)-1].Op != "remove" || es[len(es)-1].Actor != "human:unknown" {
		t.Fatalf("ledger: %+v %v", es, err)
	}
}
