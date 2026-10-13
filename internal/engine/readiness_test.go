package engine_test

import (
	"slices"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// gateOK gates name, fails the test unless it passed, and returns the
// result with its readiness.
func gateOK(h *harness, name string) *engine.GateResult {
	h.t.Helper()
	res, _, err := h.eng.Gate(h.ctx, h.pair, name)
	if err != nil || res.Checks != feature.ChecksPassed {
		h.t.Fatalf("gate %s = %+v %v", name, res, err)
	}
	return res
}

// F-12 (field trial K-02): gating the branch above first leads with the
// bottom; once the bottom is gated, the stack is ready, and stays ready when
// the bottom is gated again with new SHAs on the same trees (trap 51).
func TestGateResultReadiness(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})

	if rd := gateOK(h, "feat-b").Ready; rd.GateFirst != "feat-a" || rd.Levels != nil {
		t.Fatalf("feat-b first: %+v", rd)
	}
	if rd := gateOK(h, "feat-a").Ready; rd.GateFirst != "" || len(rd.StaleAbove) != 0 ||
		!slices.Equal(rd.Levels, []string{"feat-a", "feat-b"}) || len(rd.TitleFor) != 0 {
		t.Fatalf("then feat-a: %+v", rd)
	}
	h.clock.Sleep(time.Minute) // new committer times: new SHAs, same trees
	if rd := gateOK(h, "feat-a").Ready; !slices.Equal(rd.Levels, []string{"feat-a", "feat-b"}) {
		t.Fatalf("feat-a gated again: %+v", rd)
	}
	// A second commit on the bottom: feat-b's gate goes stale, and feat-a's
	// PR needs a title.
	testutil.Commit(t, h.shadowGit(), "feat-a", "more a", map[string]string{"a2.txt": "a2\n"})
	if rd := gateOK(h, "feat-a").Ready; !slices.Equal(rd.StaleAbove, []string{"feat-b"}) ||
		!slices.Equal(rd.Levels, []string{"feat-a"}) || !slices.Equal(rd.TitleFor, []string{"feat-a"}) {
		t.Fatalf("feat-a moved: %+v", rd)
	}
}

// Review Focus 4: at a fork with two fresh children, readiness stops at the
// fork: one plan publishes one line.
func TestGateResultReadinessStopsAtAFork(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	h.startOn("feat-c", "feat-a", map[string]string{"c.txt": "c\n"})
	gateOK(h, "feat-a")
	gateOK(h, "feat-b")
	gateOK(h, "feat-c")
	if rd := gateOK(h, "feat-a").Ready; rd.GateFirst != "" || len(rd.StaleAbove) != 0 || !slices.Equal(rd.Levels, []string{"feat-a"}) {
		t.Fatalf("fork: %+v", rd)
	}
}

// A gate finished by wait is read back from its record, which has no stack
// built in this run: it links to the level below by the record's BaseTree.
func TestWaitResultReadiness(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	gateOK(h, "feat-a")
	jres, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-b")
	if err != nil || jres.Job == nil {
		t.Fatalf("gate --detach = %+v %v", jres, err)
	}
	res, _, err := h.eng.Wait(h.ctx, h.pair, *jres.Job, engine.WaitOptions{})
	if err != nil || res.Checks != feature.ChecksPassed {
		t.Fatalf("wait = %+v %v", res, err)
	}
	if rd := res.Ready; rd.GateFirst != "" || !slices.Equal(rd.Levels, []string{"feat-a", "feat-b"}) {
		t.Fatalf("wait: %+v", rd)
	}
}
