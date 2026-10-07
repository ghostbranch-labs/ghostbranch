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
	if err != nil || res.Action != "fast_forward" || res.Preview == nil {
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

func TestSyncFollowsEveryConfiguredBranch(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{"r": "1\n"})
	h.init() // mirrors release too
	h.setConfig("branches: [main]", "branches: [main, release, dev]")
	testutil.Commit(t, h.pubGit, "dev", "dev", map[string]string{"d": "1\n"})
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n": "1\n"})
	res, _, err := h.eng.Sync(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range res.Branches {
		got[b.Name] = b.Status
	}
	if got["main"] != "fast_forwarded" || got["release"] != "up_to_date" || got["dev"] != "created" || res.ShadowMainAfter != next {
		t.Fatalf("branches = %+v", res.Branches)
	}
}

func TestSyncRefusesABranchPublicLacks(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.setConfig("branches: [main]", "branches: [main, ghost]")
	n := len(h.pushes)
	_, _, err := h.eng.Sync(h.ctx, h.pair)
	if ce := wantCode(t, err, contract.ConfigInvalid); ce.Field != "sync.branches" {
		t.Fatalf("field = %q", ce.Field)
	}
	if len(h.pushes) != n {
		t.Fatal("sync pushed before refusing")
	}
}

func TestSyncReportsEachDivergedBranchAndPushesTheRest(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{"r": "1\n"})
	h.init()
	h.setConfig("branches: [main]", "branches: [main, release]")
	shadowOnly := testutil.Commit(t, h.shadowGit(), "release", "shadow-only", map[string]string{"x": "1\n"})
	testutil.Commit(t, h.pubGit, "release", "rel2", map[string]string{"r": "2\n"})
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n": "1\n"})
	res, _, err := h.eng.Sync(h.ctx, h.pair)
	ce := wantCode(t, err, contract.SyncDiverged)
	if b, _ := ce.Details["branches"].([]string); len(b) != 1 || b[0] != "release" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if res.ShadowMainAfter != next {
		t.Fatal("main was not fast-forwarded alongside the diverged branch")
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/release"); got != shadowOnly {
		t.Fatalf("shadow release = %s, want the shadow-only commit %s left alone", got, shadowOnly)
	}
}
