package cli_test

import (
	"strings"
	"testing"
)

func TestFeatureStartFromSaysWhatItStacksOn(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	code, out, errOut := te.run("feature", "start", "feat-y", "--from", "feat-x")
	if code != 0 || !strings.Contains(out, "Started feature feat-y on feat-x, at its tip ") {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
	}
}
