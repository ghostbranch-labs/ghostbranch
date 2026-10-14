package cli_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// Usage problems with a stack promote fail before any network work.
func TestPromoteStackFlags(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	for _, c := range []struct {
		args  []string
		field string
	}{
		{[]string{"promote", "feat-a", "feat-b", "--title", "t"}, "title"},
		{[]string{"promote", "feat-a", "feat-b", "--title-for", "feat-c=t"}, "title-for"},
		{[]string{"promote", "feat-a", "feat-b", "--title-for", "feat-a"}, "title-for"},
		{[]string{"promote", "feat-a", "feat-b", "--mode-for", "feat-b=curated"}, "mode-for"},
		{[]string{"promote", "feat-a", "feat-b", "--branch", "x"}, "branch"},
	} {
		code, env := te.runJSON(c.args...)
		e, _ := env["error"].(map[string]any)
		if code != 2 || e["code"] != "BAD_FLAG" || e["field"] != c.field {
			t.Errorf("%v: exit %d, error %v; want BAD_FLAG on %s", c.args, code, e, c.field)
		}
	}
}

// A child of a parent published before public main moved can't be gated
// onto it: next first promotes the parent (which updates its PR onto the
// new main), as the error says, with sync (which moves the child once the
// parent merges) as the alternative; never another gate or promote of the
// child that would refuse the same way.
func TestPromoteOnAParentPublishedBeforeMainMovedNextPromotesTheParent(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-a")
	te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
	te.mustRun("gate", "feat-a")
	grantAgents(te)
	te.mustRun("promote", "feat-a", "--mode", "squash", "-m", "Add a", "--yes", "--keep")
	te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
	te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})
	te.mustRun("gate", "feat-b")
	testutil.Commit(t, te.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	code, env := te.runJSON("promote", "feat-b", "--mode", "squash", "-m", "Add b", "--plan")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	next, _ := env["next"].([]any)
	if code != 2 || d["reason"] != "parent_moved" || len(next) != 2 {
		t.Fatalf("exit %d, error %v, next %v", code, e, next)
	}
	first, _ := next[0].(map[string]any)["command"].([]any)
	second, _ := next[1].(map[string]any)["command"].([]any)
	if len(first) < 4 || first[1] != "promote" || first[2] != "feat-a" || first[3] != "--plan" || len(second) < 2 || second[1] != "sync" {
		t.Fatalf("next = %v, want promote feat-a --plan, then sync", next)
	}
}
