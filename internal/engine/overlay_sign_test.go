package engine_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

// The overlay commits made by init and upgrade keep the person as author and
// are committed by the Ghostbranch app's bot.
func TestOverlayCommitsAreAuthoredByThePersonAndCommittedByTheBot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	wantAuthor := "octo <42+octo@users.noreply.github.com>"
	wantCommitter := auth.BotName + " <" + auth.BotEmail + ">"
	check := func(step string) {
		t.Helper()
		sha, err := h.shadowGit().MustResolve(h.ctx, "refs/heads/"+engine.OverlayBranch)
		if err != nil {
			t.Fatal(err)
		}
		a, c, err := h.shadowGit().CommitIdentities(h.ctx, sha)
		if err != nil {
			t.Fatal(err)
		}
		if a != wantAuthor || c != wantCommitter {
			t.Fatalf("%s: author %q committer %q", step, a, c)
		}
	}
	h.init()
	check("init")
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	if res, _, err := h.eng.Upgrade(h.ctx, h.pair); err != nil || res.Commit == "" {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	check("upgrade")
}
