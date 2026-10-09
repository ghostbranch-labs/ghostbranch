package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// lockMirrorOnPush makes the mirror update after a push fail, as a crashed
// git's leftover .lock file would. Just before a push to a ref starting
// with dst, it locks the mirror's copy: mirror plus the rest of the ref
// name. The push itself still goes through.
func (h *harness) lockMirrorOnPush(dst, mirror string) {
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if _, ref, ok := strings.Cut(s, ":"); ok {
				if rest, ok := strings.CutPrefix(ref, dst); ok {
					h.lockMirrorRef(mirror + rest)
				}
			}
		}
	}
}

// wantMirrorWarning fails unless warns has exactly one MIRROR_NOT_UPDATED,
// naming ref.
func wantMirrorWarning(t *testing.T, warns []contract.Warning, ref string) {
	t.Helper()
	ws := warningsFor(warns, contract.MirrorNotUpdated)
	if len(ws) != 1 || ws[0].Details["ref"] != ref || !strings.Contains(ws[0].Message, ref) {
		t.Fatalf("want one MIRROR_NOT_UPDATED for %s, got %+v", ref, warns)
	}
}

func TestGateWarnsWhenTheMirrorCantBeUpdated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.lockMirrorOnPush("refs/heads/ghostbranch-gate/", "refs/remotes/shadow/ghostbranch-gate/")
	res, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPassed {
		t.Fatalf("gate = %+v %v", res, err)
	}
	wantMirrorWarning(t, warns, "refs/remotes/shadow/ghostbranch-gate/feat-x")
	if h.shadowRefs()["refs/heads/ghostbranch-gate/feat-x"] != res.Candidate {
		t.Fatal("the shadow repo lacks the gate candidate")
	}
}

func TestPromoteWarnsWhenTheMirrorCantRecordThePlan(t *testing.T) {
	t.Parallel()
	h := gated(t)
	h.lockMirrorOnPush("refs/ghostbranch/plans/", "refs/shadow-meta/plans/")
	res, warns, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add feature", PlanOnly: true})
	if err != nil || res.Plan == nil || res.PlanPath == "" {
		t.Fatalf("promote = %+v %v", res, err)
	}
	wantMirrorWarning(t, warns, "refs/shadow-meta/plans/"+res.Plan.ID)
	if _, ok := h.shadowRefs()["refs/ghostbranch/plans/"+res.Plan.ID]; !ok {
		t.Fatal("the shadow repo lacks the plan ref")
	}
}

func TestSyncWarnsWhenTheMirrorCantBeUpdated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pm := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	h.lockMirrorOnPush("refs/heads/main", "refs/remotes/shadow/main")
	res, warns := h.syncOK(engine.SyncOptions{NoCancel: true})
	if res.Action != "fast_forward" || res.ShadowMainAfter != pm {
		t.Fatalf("sync = %+v", res)
	}
	wantMirrorWarning(t, warns, "refs/remotes/shadow/main")
	if h.shadowRefs()["refs/heads/main"] != pm {
		t.Fatal("shadow main wasn't fast-forwarded")
	}
}

func TestUpgradeWarnsWhenTheMirrorCantBeUpdated(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	h.lockMirrorOnPush("refs/heads/"+engine.OverlayBranch, "refs/remotes/shadow/"+engine.OverlayBranch)
	res, warns, err := h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || res.Commit == "" {
		t.Fatalf("upgrade = %+v %v", res, err)
	}
	wantMirrorWarning(t, warns, "refs/remotes/shadow/"+engine.OverlayBranch)
	if h.shadowRefs()["refs/heads/"+engine.OverlayBranch] != res.Commit {
		t.Fatal("the overlay wasn't upgraded in the shadow repo")
	}
}
