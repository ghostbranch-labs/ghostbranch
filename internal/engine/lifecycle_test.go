package engine_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// validateResult checks a result's JSON against schemas/v0/<name>.
func validateResult(t *testing.T, name string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	validateSchema(t, name, raw)
}

// status runs Status, checks its result against the schema, and fails
// unless the warnings are exactly want, one each.
func (h *harness) status(want ...contract.WarningCode) (*engine.StatusResult, []contract.Warning) {
	h.t.Helper()
	res, warns, err := h.eng.Status(h.ctx, h.pair)
	if err != nil {
		h.t.Fatalf("status: %v", err)
	}
	wantCodes := map[contract.WarningCode]int{}
	for _, c := range want {
		wantCodes[c]++
	}
	if !reflect.DeepEqual(codes(warns), wantCodes) {
		h.t.Fatalf("status warnings = %+v, want %v", warns, want)
	}
	validateResult(h.t, "status.schema.json", res)
	return res, warns
}

func TestStatusFreshPair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	res, _ := h.status()
	if res.Public != publicName || res.Shadow != shadowName {
		t.Fatalf("pair = %s -> %s", res.Public, res.Shadow)
	}
	if res.Features == nil || len(res.Features) != 0 {
		t.Fatalf("features = %#v, want [] (never null)", res.Features)
	}
	if res.ShadowBehind || res.PublicMain == "" || res.ShadowMain != res.PublicMain {
		t.Fatalf("mains: public %q shadow %q behind %v", res.PublicMain, res.ShadowMain, res.ShadowBehind)
	}
	if res.Overlay == "" || res.OverlayStatus != "current" || res.ScheduledSync {
		t.Fatalf("overlay %q status %q scheduled %v", res.Overlay, res.OverlayStatus, res.ScheduledSync)
	}
}

func TestStatusShadowBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pm := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, warns := h.status(contract.ShadowBehind)
	if !res.ShadowBehind || res.PublicMain != pm || res.ShadowMain == pm {
		t.Fatalf("public %q shadow %q behind %v; want behind at public %s", res.PublicMain, res.ShadowMain, res.ShadowBehind, pm)
	}
	if d := warns[0].Details; d["behind"] != 1 || d["shadow_main"] != res.ShadowMain || d["public_main"] != pm {
		t.Fatalf("SHADOW_BEHIND details = %+v", d)
	}
}

// A shadow main that has its own commits isn't behind; sync reports the
// divergence.
func TestStatusDivergedShadowIsNotBehind(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	testutil.Commit(t, h.shadowGit(), "main", "shadow only", map[string]string{"sh.txt": "sh\n"})
	if res, _ := h.status(); res.ShadowBehind {
		t.Fatalf("diverged shadow main reported behind")
	}
}

// Scheduled sync wanted but its workflow not installed: the overlay is
// outdated.
func TestStatusOverlayOutdatedAndScheduledSync(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	res, warns := h.status(contract.OverlayOutdated)
	if res.OverlayStatus != "outdated" || !res.ScheduledSync {
		t.Fatalf("overlay status %q scheduled %v", res.OverlayStatus, res.ScheduledSync)
	}
	if files, _ := warns[0].Details["files"].([]string); !reflect.DeepEqual(files, []string{managed.SyncPath + " (missing)"}) {
		t.Fatalf("OVERLAY_OUTDATED details = %+v", warns[0].Details)
	}
}

func TestStatusWithFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	head := h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	res, _ := h.status()
	if len(res.Features) != 1 {
		t.Fatalf("features = %+v", res.Features)
	}
	if r := res.Features[0]; r.Name != "feat-x" || r.State != feature.StateDraft || r.Head != head {
		t.Fatalf("report = %+v", r)
	}
}

// A published feature fills the report's gate, PR and plan fields; the
// schema takes them.
func TestStatusPublishedFeatureMatchesTheSchema(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, planID := h.publish("feat-x")
	res, _ := h.status()
	if len(res.Features) != 1 {
		t.Fatalf("features = %+v", res.Features)
	}
	r := res.Features[0]
	if r.State != feature.StatePublished || r.PublicPR == nil || *r.PublicPR != pr || r.PlanID != planID || r.PRState != "open" || r.Gate == nil {
		t.Fatalf("report = %+v", r)
	}
}

func TestStatusIncludesAbandoned(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Abandoned = true })
	if res, _ := h.status(); len(res.Features) != 1 || res.Features[0].State != feature.StateAbandoned {
		t.Fatalf("features = %+v", res.Features)
	}
}

func TestFeatureListHidesAbandonedUnlessAll(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("f-live", map[string]string{"live.txt": "l\n"})
	h.startFeature("f-parked", map[string]string{"parked.txt": "p\n"})
	forgeMeta(t, h, "f-parked", func(m *feature.Meta) { m.Abandoned = true })
	names := func(all bool) []string {
		t.Helper()
		res, warns, err := h.eng.FeatureList(h.ctx, h.pair, all)
		if err != nil || len(warns) != 0 {
			t.Fatalf("feature list (all=%v): %v %+v", all, err, warns)
		}
		validateResult(t, "feature-list.schema.json", res)
		out := []string{}
		for _, r := range res.Features {
			out = append(out, r.Name)
		}
		return out
	}
	if got := names(false); !reflect.DeepEqual(got, []string{"f-live"}) {
		t.Fatalf("list = %v, want [f-live]", got)
	}
	if got := names(true); !reflect.DeepEqual(got, []string{"f-live", "f-parked"}) {
		t.Fatalf("list --all = %v, want [f-live f-parked]", got)
	}
}

func TestFeatureListEmpty(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.FeatureList(h.ctx, h.pair, false)
	if err != nil || res.Features == nil || len(res.Features) != 0 {
		t.Fatalf("feature list = %#v %v, want [] (never null)", res, err)
	}
	validateResult(t, "feature-list.schema.json", res)
}

func TestFeatureShow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("f-other", map[string]string{"other.txt": "o\n"})
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n", ".ghostbranch/n.md": "n\n"})
	s2 := testutil.Commit(t, h.shadowGit(), "feat-x", "add b\n\nmore", map[string]string{"b.txt": "b\n"})
	h.gateFeature("feat-x")
	h.gateFeature("f-other")
	res, warns, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil || len(warns) != 0 {
		t.Fatalf("feature show: %v %+v", err, warns)
	}
	validateResult(t, "feature-show.schema.json", res)
	if r := res.Feature; r.Name != "feat-x" || r.State != feature.StateGatePassed || r.Head != s2 || r.Gate == nil {
		t.Fatalf("report = %+v", r)
	}
	want := []engine.CommitInfo{
		{Source: s1, Subject: "work on feat-x", Author: "Test Author <author@example.com>", Files: []string{"a.txt"}},
		{Source: s2, Subject: "add b", Author: "Test Author <author@example.com>", Files: []string{"b.txt"}},
	}
	if !reflect.DeepEqual(res.Commits, want) {
		t.Fatalf("commits = %+v", res.Commits)
	}
	var ops []string
	for _, ent := range res.Ledger {
		if ent.Feature != "feat-x" || ent.Pair != publicName {
			t.Fatalf("ledger entry for another feature or pair: %+v", ent)
		}
		ops = append(ops, ent.Op)
	}
	if !reflect.DeepEqual(ops, []string{"feature start", "gate"}) {
		t.Fatalf("ledger ops = %v, want [feature start gate] oldest first", ops)
	}
}

// Another pair's feature of the same name shares the workspace ledger; its
// entries aren't this feature's.
func TestFeatureShowLeavesOutAnotherPairsLedgerEntries(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	if err := ledger.Append(h.eng.WS.LedgerPath(), ledger.Entry{TS: h.clock.Now(), Actor: "human:octo", Op: "gate",
		Pair: "acme/other", Feature: "feat-x", Summary: "another pair's gate"}); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Ledger) != 1 || res.Ledger[0].Op != "feature start" || res.Ledger[0].Pair != publicName {
		t.Fatalf("ledger = %+v, want only this pair's feature start", res.Ledger)
	}
}

// Public main moved after the feature started: each commit's files are its
// own changes, not the public commits it doesn't have yet.
func TestFeatureShowFilesAreEachCommitsOwn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, _, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Commits) != 1 || res.Commits[0].Source != s1 || !reflect.DeepEqual(res.Commits[0].Files, []string{"a.txt"}) {
		t.Fatalf("commits = %+v", res.Commits)
	}
}

func TestFeatureShowBranchGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	validateResult(t, "feature-show.schema.json", res)
	if res.Feature.State != feature.StateBranchGone || res.Feature.Head != "" || res.Commits == nil || len(res.Commits) != 0 {
		t.Fatalf("show = %+v", res)
	}
}

func TestFeatureShowUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	_, _, err := h.eng.FeatureShow(h.ctx, h.pair, "never-started")
	if ce := wantCode(t, err, contract.FeatureNotFound); ce.Field != "feature" {
		t.Fatalf("field = %q, want feature", ce.Field)
	}
}

// Status, feature list and feature show are reads: no push, no ledger entry.
func TestStatusFeatureListFeatureShowAreReads(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.gateFeature("feat-x")
	pushes, entries := len(h.pushes), len(h.ledger())
	if _, _, err := h.eng.Status(h.ctx, h.pair); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.FeatureList(h.ctx, h.pair, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	_, _, _ = h.eng.FeatureShow(h.ctx, h.pair, "never-started")
	if len(h.pushes) != pushes || len(h.ledger()) != entries {
		t.Fatalf("pushes %d -> %d, ledger %d -> %d; want none", pushes, len(h.pushes), entries, len(h.ledger()))
	}
}

func TestStatusAndFeatureShowNameAPendingGatesJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	h.startFeature("f-other", map[string]string{"other.txt": "o\n"})
	job := detached(t, h, "feat-x")
	if want := "gate/feat-x/" + h.meta("feat-x").Gate.Candidate[:12]; job != want {
		t.Fatalf("job = %q, want %q", job, want)
	}

	check := func(r engine.FeatureReport) {
		t.Helper()
		if r.Gate == nil || r.Gate.Checks != feature.ChecksPending || r.Gate.Job == nil || *r.Gate.Job != job {
			t.Fatalf("%s gate = %+v, want pending with job %s", r.Name, r.Gate, job)
		}
	}
	show, _, err := h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	validateResult(t, "feature-show.schema.json", show)
	check(show.Feature)

	st, _ := h.status()
	var found bool
	for _, r := range st.Features {
		switch r.Name {
		case "feat-x":
			check(r)
			found = true
		case "f-other":
			if r.Gate != nil {
				t.Fatalf("f-other gate = %+v, want never gated", r.Gate)
			}
		}
	}
	if !found {
		t.Fatalf("status features = %+v, want feat-x", st.Features)
	}

	if _, _, err := h.eng.Wait(h.ctx, h.pair, job, engine.WaitOptions{}); err != nil {
		t.Fatal(err)
	}
	show, _, err = h.eng.FeatureShow(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatal(err)
	}
	validateResult(t, "feature-show.schema.json", show)
	if g := show.Feature.Gate; g == nil || g.Checks != feature.ChecksPassed || g.Job != nil {
		t.Fatalf("gate after wait = %+v, want passed with no job", g)
	}
}
