package cli_test

import (
	"strings"
	"testing"
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
