package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

// P25: nothing is built on a handed-off feature. feature start --from and
// feature restack --onto refuse an archived one with FEATURE_ARCHIVED,
// naming it and the flag, before any branch lookup (never BAD_FLAG "branch
// is gone" or FEATURE_NOT_FOUND), and change nothing.
func TestStackingOnAnArchivedFeatureIsRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr := h.archived("feat-x")
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	check := func(what, field, instead string, err error) {
		t.Helper()
		ce := wantCode(t, err, contract.FeatureArchived)
		if ce.Field != field || ce.Details["feature"] != "feat-x" || ce.Details["public_pr"] != pr ||
			ce.Details["archive_ref"] != archiveRefX || ce.Details["pr_state"] != "open" ||
			!strings.Contains(ce.Message, "nothing can be stacked on a handed-off feature: "+instead+", or wait until PR #") {
			t.Errorf("%s: %+v", what, ce)
		}
	}
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-y", engine.FeatureStartOptions{From: "feat-x"})
	check("feature start --from", "from", "start from main", err)
	if _, ok := h.shadowRefs()["refs/heads/feat-y"]; ok {
		t.Error("feature start --from an archived feature created a branch")
	}
	before := h.shadowRefs()["refs/heads/feat-a"]
	_, _, err = h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a", Onto: "feat-x"})
	check("feature restack --onto", "onto", "restack onto main", err)
	if after := h.shadowRefs()["refs/heads/feat-a"]; after != before {
		t.Errorf("feature restack --onto an archived feature moved feat-a: %s -> %s", before, after)
	}
}

// Once the archived feature's PR merged or was closed, the refusal never
// says to wait for the merge: merged says sync, closed says start from main
// (feature start --from) or restack onto main (feature restack --onto).
func TestStackingOnAnArchivedFeatureAfterItsPR(t *testing.T) {
	t.Parallel()
	for state, want := range map[string][2]string{
		"merged": {"nothing can be stacked on a handed-off feature: run ghostbranch sync, then start from main",
			"nothing can be stacked on a handed-off feature: run ghostbranch sync, then restack onto main"},
		"closed": {"nothing can be stacked on a handed-off feature: start from main",
			"nothing can be stacked on a handed-off feature: restack onto main"},
	} {
		h := newHarness(t)
		h.init()
		pr := h.archived("feat-x")
		h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
		if state == "merged" {
			h.forge.SetPRMerged(publicName, pr)
		} else {
			h.forge.ClosePR(publicName, pr)
		}
		_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-y", engine.FeatureStartOptions{From: "feat-x"})
		ce := wantCode(t, err, contract.FeatureArchived)
		if ce.Details["pr_state"] != state || !strings.HasSuffix(ce.Message, want[0]) || strings.Contains(ce.Message, "wait until") {
			t.Errorf("%s, feature start --from: %+v", state, ce)
		}
		_, _, err = h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "feat-a", Onto: "feat-x"})
		ce = wantCode(t, err, contract.FeatureArchived)
		if ce.Details["pr_state"] != state || !strings.HasSuffix(ce.Message, want[1]) || strings.Contains(ce.Message, "wait until") {
			t.Errorf("%s, feature restack --onto: %+v", state, ce)
		}
	}
}
