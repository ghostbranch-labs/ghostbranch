package engine_test

import (
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func syncFile(h *harness) ([]byte, bool) {
	h.t.Helper()
	b, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/heads/"+engine.OverlayBranch, managed.SyncPath)
	if err != nil {
		h.t.Fatal(err)
	}
	return b, ok
}

func TestUpgradeAddsUpdatesAndRemovesTheSyncWorkflow(t *testing.T) {
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || res.Changed || res.Commit != "" || res.Files[0].Action != "unchanged" {
		t.Fatalf("nothing to do: %+v %v", res, err)
	}
	h.eng.DryRun = true
	res, _, err = h.eng.Upgrade(h.ctx, h.pair)
	h.eng.DryRun = false
	if err != nil || res.Changed || res.Preview == nil || len(res.Preview.Ops) != 0 || res.Preview.Fingerprint["overlay"] == "" {
		t.Fatalf("nothing to plan: %+v %v", res, err)
	}

	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	h.eng.DryRun = true
	res, _, err = h.eng.Upgrade(h.ctx, h.pair)
	h.eng.DryRun = false
	if err != nil || !res.Changed || res.Preview == nil || res.Files[0].Action != "add" {
		t.Fatalf("plan: %+v %v", res, err)
	}
	if _, ok := syncFile(h); ok {
		t.Fatal("--plan wrote the file")
	}

	res, _, err = h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || !res.Changed || res.Commit == "" || res.Audit == nil {
		t.Fatalf("add: %+v %v", res, err)
	}
	body, ok := syncFile(h)
	if !ok || managed.StatusOf(managed.SyncPath, body, true, managed.Input{Public: publicName, Schedule: "23 */6 * * *", Branches: []string{"main"}}) != managed.Current {
		t.Fatal("the sync workflow is not current after upgrade")
	}

	testutil.Commit(t, h.shadowGit(), engine.OverlayBranch, "hand edit", map[string]string{managed.SyncPath: string(body) + "# mine\n"})
	h.setConfig(`schedule: "23 */6 * * *"`, `schedule: "off"`)
	res, _, err = h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || res.Files[0].Action != "remove" || !res.Files[0].ReplacesLocalEdits {
		t.Fatalf("remove: %+v %v", res, err)
	}
	if _, ok := syncFile(h); ok {
		t.Fatal("the sync workflow is still there")
	}
	last := h.ledger()[len(h.ledger())-1]
	if last.Op != "upgrade" || last.ExitCode != 0 {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestUpgradeRefusesWhenTheOverlayMoves(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	// Someone moves the overlay after Upgrade has fetched it, at the moment it pushes.
	raced := false
	h.eng.OnPush = func(op, remote string, specs []string) {
		if op == "upgrade" && !raced {
			raced = true
			testutil.Commit(t, h.shadowGit(), engine.OverlayBranch, "race", map[string]string{".ghostbranch/notes.md": "x\n"})
		}
	}
	_, _, err := h.eng.Upgrade(h.ctx, h.pair)
	_ = wantCode(t, err, contract.RefMoved)
	if !raced {
		t.Fatal("upgrade never pushed")
	}
	if _, ok := syncFile(h); ok {
		t.Fatal("the refused upgrade wrote the sync workflow")
	}
}

// The audit after upgrade names push-triggered public workflows, which a
// sync starts in the shadow repo whether or not scheduled sync is installed.
func TestUpgradeAuditWarnsAboutPushTriggers(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "push ci", map[string]string{".github/workflows/push.yml": "on:\n  push:\n    branches: [main]\njobs:\n  t:\n    runs-on: x\n    steps:\n      - run: echo\n"})
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	_, warns, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil {
		t.Fatal(err)
	}
	if c := codes(warns); c[contract.PushTriggersOnSync] != 1 || c[contract.OverlayOutdated] != 1 {
		t.Fatalf("before upgrade, warnings = %+v", warns)
	}
	res, warns, err := h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || res.Audit == nil {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	if c := codes(warns); c[contract.PushTriggersOnSync] != 1 || c[contract.OverlayOutdated] != 0 {
		t.Fatalf("after upgrade, warnings = %+v", warns)
	}
	if len(res.Audit.PushTriggers) != 1 || res.Audit.PushTriggers[0] != ".github/workflows/push.yml" {
		t.Fatalf("push triggers = %v", res.Audit.PushTriggers)
	}
}

// Upgrade rewrites only the managed path: every other overlay entry keeps
// its mode, type and blob, an executable private workflow included.
func TestUpgradeKeepsTheRestOfTheOverlay(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	g := h.shadowGit()
	testutil.Commit(t, g, engine.OverlayBranch, "notes", map[string]string{".ghostbranch/notes.md": "private notes\n"})
	parent, err := g.MustResolve(h.ctx, "refs/heads/"+engine.OverlayBranch)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := g.ListTree(h.ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := g.HashObject(h.ctx, []byte("on: [workflow_dispatch]\njobs:\n  p:\n    runs-on: x\n    steps:\n      - run: echo\n"))
	if err != nil {
		t.Fatal(err)
	}
	entries = append(entries, gitx.TreeEntry{Mode: "100755", Type: "blob", SHA: blob, Path: ".github/workflows/ghostbranch-private.yml"})
	tree, err := g.MakeTree(h.ctx, entries)
	if err != nil {
		t.Fatal(err)
	}
	before, err := g.CommitTree(h.ctx, tree, []string{parent}, "private workflow", testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.UpdateRef(h.ctx, "refs/heads/"+engine.OverlayBranch, before); err != nil {
		t.Fatal(err)
	}

	res, _, err := h.eng.Upgrade(h.ctx, h.pair)
	if err != nil || res.Commit == "" {
		t.Fatalf("upgrade: %+v %v", res, err)
	}
	others := func(rev string) []gitx.TreeEntry {
		t.Helper()
		es, err := g.ListTree(h.ctx, rev)
		if err != nil {
			t.Fatal(err)
		}
		var out []gitx.TreeEntry
		for _, e := range es {
			if e.Path != managed.SyncPath {
				out = append(out, e)
			}
		}
		return out
	}
	old, upgraded := others(before), others(res.Commit)
	if len(old) < 3 || !reflect.DeepEqual(old, upgraded) {
		t.Fatalf("overlay entries changed:\nbefore %+v\nafter  %+v", old, upgraded)
	}
	found := false
	for _, e := range upgraded {
		if e.Path == ".github/workflows/ghostbranch-private.yml" && e.Mode == "100755" {
			found = true
		}
	}
	if !found {
		t.Fatal("the executable private workflow is missing")
	}
}
