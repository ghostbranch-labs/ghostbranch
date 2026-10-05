package engine_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestSyncNoopFastForwardAndDiverged(t *testing.T) {
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.Sync(h.ctx, h.pair)
	if err != nil || res.Action != "noop" {
		t.Fatalf("noop: %+v %v", res, err)
	}

	newMain := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "1\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair)
	if err != nil || res.Action != "fast_forward" || res.ShadowMainAfter != newMain {
		t.Fatalf("ff: %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != newMain {
		t.Fatalf("shadow main = %s, want %s", got, newMain)
	}

	h.eng.DryRun = true
	testutil.Commit(t, h.pubGit, "main", "upstream 2", map[string]string{"up.txt": "2\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair)
	if err != nil || res.Action != "fast_forward" || !res.DryRun {
		t.Fatalf("dry ff: %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != newMain {
		t.Fatal("dry run moved shadow main")
	}
	h.eng.DryRun = false

	rogue := testutil.Commit(t, h.shadowGit(), "main", "rogue", map[string]string{"rogue.txt": "x\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair)
	ce := wantCode(t, err, contract.SyncDiverged)
	if res.Action != "diverged" || ce.Details["shadow_main"] != rogue {
		t.Fatalf("diverged: %+v %+v", res, ce)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != rogue {
		t.Fatal("sync must never force-push a diverged shadow main")
	}
}
