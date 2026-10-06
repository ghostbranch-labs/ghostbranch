package engine_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/strip"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const internalSrc = "package app\n\n// BEGIN-INTERNAL\nconst internal = 1\n// END-INTERNAL\nconst public = 2\n"
const publicSrc = "package app\n\nconst public = 2\n"

func TestGateStripsInternalSectionsBeforeCI(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"app.go": internalSrc})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	sh := h.shadowGit()
	if ci, _, _ := sh.ShowFile(h.ctx, res.Candidate, "app.go"); string(ci) != publicSrc {
		t.Fatalf("CI ran on %q, want the stripped file", ci)
	}
	if r, _, _ := sh.ShowFile(h.ctx, res.Candidate+"^", "app.go"); string(r) != internalSrc {
		t.Fatalf("R must keep the internal lines, got %q", r)
	}
	pr, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "Add app", Yes: true})
	if err != nil || pr.Applied == nil {
		t.Fatalf("promote = %+v %v", pr, err)
	}
	if pub, _, _ := h.pubGit.ShowFile(h.ctx, "refs/heads/feat-x", "app.go"); string(pub) != publicSrc {
		t.Fatalf("published %q", pub)
	}
}

func TestGateStopsOnInvalidMarkersBeforeAnyPush(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"app.go": "ok\n// BEGIN-INTERNAL\nx\n"})
	n := len(h.pushes)
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.MarkerInvalid)
	probs, _ := ce.Details["problems"].([]strip.Problem)
	if ce.Details["feature"] != "feat-x" || len(probs) != 1 || probs[0] != (strip.Problem{Path: "app.go", Line: 2, Problem: "unmatched_begin"}) {
		t.Fatalf("details = %+v", ce.Details)
	}
	if len(h.pushes) != n || len(h.forge.PRs(shadowName)) != 0 {
		t.Fatal("the gate pushed or opened a PR before failing on markers")
	}
}

// forgeRecordedTree rewrites a feature's recorded gate in the shadow repo,
// as a binary that stripped differently would have left it.
func forgeRecordedTree(t *testing.T, h *harness, name, tree string) {
	t.Helper()
	sh := h.shadowGit()
	ref := "refs/ghostbranch/features/" + name
	raw, ok, err := sh.ShowFile(h.ctx, ref, feature.MetaFile)
	if err != nil || !ok {
		t.Fatalf("read %s: %v", ref, err)
	}
	m, err := feature.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	m.Gate.PublishedTree = tree
	blob, err := sh.HashObject(h.ctx, feature.Encode(m))
	if err != nil {
		t.Fatal(err)
	}
	old, err := sh.MustResolve(h.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := sh.ListTree(h.ctx, old)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].Path == feature.MetaFile {
			entries[i].SHA = blob
		}
	}
	tr, err := sh.MakeTree(h.ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	c, err := sh.CommitTree(h.ctx, tr, []string{old}, "forged", testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.UpdateRef(h.ctx, ref, c); err != nil {
		t.Fatal(err)
	}
}

// Review Focus 1: a gate recorded by the SP2 binary kept the internal lines
// in its published tree. Promote must re-gate, never publish that tree.
func TestPromoteRegatesWhenTheRecordedTreeDiffers(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"app.go": internalSrc})
	gr, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	unstripped, err := h.shadowGit().TreeOf(h.ctx, gr.Candidate+"^")
	if err != nil {
		t.Fatal(err)
	}
	forgeRecordedTree(t, h, "feat-x", unstripped)
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused || res.Plan.Publish.Tree != gr.PublishedTree {
		t.Fatalf("promote = %+v %v; want a fresh gate and the stripped tree %s", res.Gate, err, gr.PublishedTree)
	}
}

func TestTheMirrorProtectsItsAttributes(t *testing.T) {
	h := newHarness(t)
	h.init()
	got, err := os.ReadFile(filepath.Join(h.eng.WS.MirrorDir(h.pair), "info", "attributes"))
	if err != nil || string(got) != gitx.AttributesOverride {
		t.Fatalf("info/attributes = %q %v", got, err)
	}
}
