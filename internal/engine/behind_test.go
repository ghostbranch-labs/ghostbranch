package engine_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestShadowBehindWarnsUntilSync(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.pubGit, "main", "up1", map[string]string{"u1": "1\n"})
	testutil.Commit(t, h.pubGit, "main", "up2", map[string]string{"u2": "1\n"})
	// feature start refuses outright (TestFeatureStartRefusesWhenShadowMainIsBehind);
	// other commands warn.
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	var w *contract.Warning
	for i := range warns {
		if warns[i].Code == contract.ShadowBehind {
			w = &warns[i]
		}
	}
	if w == nil || w.Details["behind"] != 2 {
		t.Fatalf("warnings = %+v", warns)
	}
	if _, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{}); err != nil {
		t.Fatal(err)
	}
	_, warns, err = h.eng.Audit(h.ctx, h.pair)
	if err != nil || codes(warns)[contract.ShadowBehind] != 0 {
		t.Fatalf("after sync: %+v %v", warns, err)
	}
}

// A promote that re-runs the gate raises SHADOW_BEHIND once, like the gate.
func TestShadowBehindOncePerCommand(t *testing.T) {
	t.Parallel()
	h := gated(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || codes(warns)[contract.ShadowBehind] != 1 {
		t.Fatalf("gate: %+v %v", warns, err)
	}
	testutil.Commit(t, h.pubGit, "main", "upstream 2", map[string]string{"up.txt": "up 2\n"})
	res, warns, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused || codes(warns)[contract.ShadowBehind] != 1 {
		t.Fatalf("promote: %+v %v", warns, err)
	}
}

// A diverged shadow main is for sync to report, not a reminder to run it.
func TestShadowBehindSilentWhenShadowMainDiverged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.shadowGit(), "main", "shadow-only", map[string]string{"x": "1\n"})
	testutil.Commit(t, h.pubGit, "main", "up", map[string]string{"u": "1\n"})
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil || codes(warns)[contract.ShadowBehind] != 0 {
		t.Fatalf("diverged: %+v %v", warns, err)
	}
}

func TestShadowBehindSilentWhenShadowMainMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if _, err := h.shadowGit().Run(h.ctx, "update-ref", "-d", "refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.pubGit, "main", "up", map[string]string{"u": "1\n"})
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil || codes(warns)[contract.ShadowBehind] != 0 {
		t.Fatalf("missing: %+v %v", warns, err)
	}
}
