package cli_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// What a person reads after abandon, its undo, and a close.
func TestAbandonAndCloseHumanOutput(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.mustRun("feature", "start", "feat-x")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"feature", "abandon", "feat-x"}, "Abandoned feat-x (its branch is kept; undo with --undo).\n"},
		{[]string{"feature", "abandon", "feat-x", "--undo"}, "Restored feat-x; it is an active feature again.\n"},
	} {
		code, out, errOut := te.run(c.args...)
		if code != 0 || out != c.want {
			t.Fatalf("%v: exit %d\nstdout: %q\nstderr: %s", c.args, code, out, errOut)
		}
	}

	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a"))
	code, out, errOut := te.run("feature", "close", "feat-a")
	if code != 0 || !strings.HasPrefix(out, "Closed feat-a (merged): deleted 4 ref(s) from the shadow repo; closed gate PR #") {
		t.Fatalf("close: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	for _, ref := range []string{"refs/heads/feat-a  ", "refs/heads/ghostbranch-gate/feat-a  ", "refs/ghostbranch/features/feat-a  ", "refs/ghostbranch/plans/"} {
		if !strings.Contains(out, "\n  "+ref) {
			t.Errorf("close output lacks %q:\n%s", ref, out)
		}
	}
	if !strings.Contains(out, "ghostbranch log") {
		t.Errorf("close output doesn't say where the SHAs are kept:\n%s", out)
	}
}

// Refs deleted but the gate PR still open: neither close nor sync says "closed".
func TestAPartialCloseDoesNotReadAsClosed(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	grantAgents(te)
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-a"))
	te.forge.SetPRMerged("acme/widget", publishFeature(te, "feat-b"))
	te.forge.ClosePRErr = errors.New("boom")
	code, out, errOut := te.run("feature", "close", "feat-a")
	if code == 0 || !strings.HasPrefix(out, "Deleted 4 ref(s) of feat-a (merged) from the shadow repo, but its gate PR #") || strings.Contains(out, "Closed") {
		t.Fatalf("close: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
	code, out, errOut = te.run("sync")
	if code != 0 || !strings.Contains(out, "deleted merged feature feat-b (PR #") || strings.Contains(out, "closed merged feature feat-b") ||
		strings.Contains(errOut, "closed merged feature feat-b") {
		t.Fatalf("sync: exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}

// corruptFeature points name's metadata ref in the shadow repo at a commit
// whose feature.json doesn't parse.
func corruptFeature(te *testEnv, name string) {
	te.t.Helper()
	ctx := context.Background()
	sh := gitx.New(te.forge.RepoDir("acme/widget-shadow"))
	sha := testutil.Commit(te.t, sh, "tmp-corrupt", "corrupt", map[string]string{feature.MetaFile: "not json\n"})
	if _, err := sh.Run(ctx, "update-ref", "refs/ghostbranch/features/"+name, sha); err != nil {
		te.t.Fatal(err)
	}
	if err := sh.DeleteRef(ctx, "refs/heads/tmp-corrupt"); err != nil {
		te.t.Fatal(err)
	}
}

func TestStatusShowsAnUnreadableFeature(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	corruptFeature(te, "feat-bad")
	code, out, errOut := te.run("status", "--json")
	var env struct{ Data json.RawMessage }
	if err := json.Unmarshal([]byte(out), &env); err != nil || code != 0 {
		t.Fatalf("status: %d %v\n%s\n%s", code, err, out, errOut)
	}
	testutil.ValidateSchema(t, "status.schema.json", env.Data)
	var d struct {
		Features []struct{ Name, State, Error string }
	}
	_ = json.Unmarshal(env.Data, &d)
	if len(d.Features) != 2 || d.Features[0].Name != "feat-bad" || d.Features[0].State != "unreadable" || d.Features[0].Error == "" {
		t.Fatalf("features = %+v", d.Features)
	}
	if code, out, _ := te.run("status"); code != 0 || !strings.Contains(out, "feat-bad couldn't be read: ") {
		t.Fatalf("human status: %d\n%s", code, out)
	}
	code, m := te.runJSON("feature", "show", "feat-bad")
	if e, _ := m["error"].(map[string]any); code == 0 || e["code"] != "INTERNAL" {
		t.Fatalf("feature show: %d %v", code, m)
	}
}

func TestALedgerFailureIsAWarningInTheEnvelope(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	ledgerPath := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "ledger.jsonl")
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledgerPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("feature", "start", "feat-x")
	ws, _ := env["warnings"].([]any)
	found := false
	for _, w := range ws {
		m := w.(map[string]any)
		d, _ := m["details"].(map[string]any)
		found = found || (m["code"] == "LEDGER_NOT_WRITTEN" && d["op"] == "feature start")
	}
	if code != 0 || !found {
		t.Fatalf("feature start: %d %v", code, env)
	}
	_, _, errOut := te.run("feature", "start", "feat-y")
	if !strings.Contains(errOut, "warning: the ledger entry for feature start couldn't be written") || strings.Contains(errOut, "could not write the ledger") {
		t.Fatalf("stderr:\n%s", errOut)
	}
}

// abandon --plan shows the metadata push it would make and changes nothing.
func TestAbandonPlanChangesNothing(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	sg, meta := gitx.New(te.forge.RepoDir("acme/widget-shadow")), "refs/ghostbranch/features/feat-x"
	before, err := sg.MustResolve(te.t.Context(), meta)
	if err != nil {
		t.Fatal(err)
	}
	code, env := te.runJSON("feature", "abandon", "feat-x", "--plan")
	data, _ := env["data"].(map[string]any)
	pv, _ := data["preview"].(map[string]any)
	ops, _ := pv["ops"].([]any)
	if code != 0 || len(ops) != 1 {
		t.Fatalf("%d %v", code, env)
	}
	if after, _ := sg.MustResolve(te.t.Context(), meta); after != before {
		t.Fatal("--plan moved the metadata")
	}
	if code, out, _ := te.run("feature", "abandon", "feat-x", "--plan"); code != 0 || !strings.HasPrefix(out, "Dry run; nothing was changed. This would:\n") {
		t.Fatalf("human: %d %q", code, out)
	}
	if code, out, _ := te.run("feature", "abandon", "feat-x", "--undo"); code != 0 || out != "feat-x was not abandoned; nothing changed.\n" {
		t.Fatalf("the plan abandoned it: %d %q", code, out)
	}
}
