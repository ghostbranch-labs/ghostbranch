package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// A stacked feature starts at its parent's shadow tip and records the parent
// and that tip as forked_at; the parent's record is never written. Stacks
// go to any depth.
func TestFeatureStartFromStacksOnTheParentsTip(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	aHead := h.startFeature("a", map[string]string{"a.txt": "a\n"})
	sh := h.shadowGit()
	aRecord, err := sh.MustResolve(h.ctx, "refs/ghostbranch/features/a")
	if err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.FeatureStart(h.ctx, h.pair, "b", engine.FeatureStartOptions{From: "a"})
	if err != nil || res.Branch != "b" || res.Parent != "a" || res.Base != aHead {
		t.Fatalf("start b --from a: %+v %v", res, err)
	}
	if head, _ := sh.MustResolve(h.ctx, "refs/heads/b"); head != aHead {
		t.Fatalf("b starts at %s, want a's tip %s", head, aHead)
	}
	if m := h.meta("b"); m.Parent != "a" || m.ForkedAt != aHead || m.Base != aHead || !m.Stacked() {
		t.Fatalf("b's record = %+v", m)
	}
	if now, _ := sh.MustResolve(h.ctx, "refs/ghostbranch/features/a"); now != aRecord {
		t.Fatal("starting b wrote a's record")
	}
	bHead := testutil.Commit(t, sh, "b", "work on b", map[string]string{"b.txt": "b\n"})
	if _, _, err := h.eng.FeatureStart(h.ctx, h.pair, "c", engine.FeatureStartOptions{From: "b"}); err != nil {
		t.Fatal(err)
	}
	if m := h.meta("c"); m.Parent != "b" || m.ForkedAt != bHead || m.Base != bHead {
		t.Fatalf("c's record = %+v", m)
	}
}

// No --from, --from main and an empty From all start at shadow main, and
// the record has no parent or forked_at keys: single features don't change.
func TestFeatureStartFromMainIsTheDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	main, err := h.shadowGit().MustResolve(h.ctx, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []struct {
		name string
		opts []engine.FeatureStartOptions
	}{
		{"plain", nil},
		{"from-main", []engine.FeatureStartOptions{{From: "main"}}},
		{"from-empty", []engine.FeatureStartOptions{{}}},
	} {
		res, _, err := h.eng.FeatureStart(h.ctx, h.pair, x.name, x.opts...)
		if err != nil || res.Parent != feature.MainParent || res.Base != main {
			t.Fatalf("%s: %+v %v", x.name, res, err)
		}
		raw, _, err := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/"+x.name, feature.MetaFile)
		if err != nil || strings.Contains(string(raw), "parent") || strings.Contains(string(raw), "forked_at") {
			t.Fatalf("%s's record names a parent (%v):\n%s", x.name, err, raw)
		}
	}
}

// --from refuses a parent that isn't an open feature, names the from field,
// and creates nothing.
func TestFeatureStartFromRefusesWhatItCantStackOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("dropped", map[string]string{"d.txt": "d\n"})
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "dropped", false); err != nil {
		t.Fatal(err)
	}
	h.startFeature("newer", map[string]string{"n.txt": "n\n"})
	h.editRemoteMeta("newer", func(m *feature.Meta) { m.SchemaVersion = "0.99" })
	h.startFeature("bad", map[string]string{"x.txt": "x\n"})
	h.corruptMeta("bad")
	h.startFeature("gone", map[string]string{"g.txt": "g\n"})
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/gone"); err != nil {
		t.Fatal(err)
	}
	for from, want := range map[string]contract.Code{
		"nope": contract.FeatureNotFound, "dropped": contract.BadFlag, "gone": contract.BadFlag,
		"newer": contract.UpgradeNeeded, "bad": contract.Internal, "bad name": contract.NameInvalid,
	} {
		_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "child", engine.FeatureStartOptions{From: from})
		if ce := wantCode(t, err, want); ce.Field != "from" {
			t.Errorf("--from %q: field %q, want from", from, ce.Field)
		}
		refs := h.shadowRefs()
		if _, ok := refs["refs/heads/child"]; ok {
			t.Fatalf("--from %q created the branch", from)
		}
		if _, ok := refs["refs/ghostbranch/features/child"]; ok {
			t.Fatalf("--from %q created the record", from)
		}
	}
}
