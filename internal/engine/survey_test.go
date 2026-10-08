package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// survey runs the read-only survey of the harness's pair.
func (h *harness) survey(names ...string) ([]engine.FeatureReport, []contract.Warning) {
	h.t.Helper()
	rs, warns, err := h.eng.SurveyForTest(h.ctx, h.pair, names...)
	if err != nil {
		h.t.Fatalf("survey: %v", err)
	}
	return rs, warns
}

// report is the survey's report for one feature.
func (h *harness) report(name string) engine.FeatureReport {
	h.t.Helper()
	rs, _ := h.survey(name)
	if len(rs) != 1 || rs[0].Name != name {
		h.t.Fatalf("survey(%s) = %+v", name, rs)
	}
	return rs[0]
}

// gateFeature gates a feature and fails the test unless it passes.
func (h *harness) gateFeature(name string) {
	h.t.Helper()
	res, _, err := h.eng.Gate(h.ctx, h.pair, name)
	if err != nil || res.Checks != feature.ChecksPassed || res.Leak != feature.LeakClean {
		h.t.Fatalf("gate %s = %+v %v", name, res, err)
	}
}

// publish starts, gates and promotes a feature with --yes under the test
// approver. It returns the public PR number and the plan ID.
func (h *harness) publish(name string) (int, string) {
	h.t.Helper()
	h.startFeature(name, map[string]string{name + ".txt": name + "\n"})
	h.gateFeature(name)
	res, err := promote(h, engine.PromoteOptions{Feature: name, Mode: "squash", Message: "Add " + name, Yes: true})
	if err != nil || res.Applied == nil {
		h.t.Fatalf("promote %s = %+v %v", name, res, err)
	}
	return res.Applied.PublicPR, res.Plan.ID
}

func TestSurveyStates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("f-draft", map[string]string{"draft.txt": "d\n"})
	h.startFeature("f-gated", map[string]string{"gated.txt": "g\n"})
	h.gateFeature("f-gated")
	pubPR, _ := h.publish("f-published")
	mergedPR, _ := h.publish("f-merged")
	h.forge.SetPRMerged(publicName, mergedPR)
	movedPR, _ := h.publish("f-moved")
	h.forge.SetPRMerged(publicName, movedPR)
	testutil.Commit(t, h.shadowGit(), "f-moved", "after the merge", map[string]string{"later.txt": "l\n"})
	closedPR, _ := h.publish("f-closed")
	h.forge.ClosePR(publicName, closedPR)
	h.startFeature("f-abandoned", map[string]string{"ab.txt": "a\n"})
	forgeMeta(t, h, "f-abandoned", func(m *feature.Meta) { m.Abandoned = true })

	rs, warns := h.survey()
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	type want struct {
		state   feature.State
		pr      int // 0: no public PR
		prState string
		moved   bool
	}
	wants := map[string]want{
		"f-abandoned": {feature.StateAbandoned, 0, "", false},
		"f-closed":    {feature.StatePRClosed, closedPR, "closed", false},
		"f-draft":     {feature.StateDraft, 0, "", false},
		"f-gated":     {feature.StateGatePassed, 0, "", false},
		"f-merged":    {feature.StateMerged, mergedPR, "merged", false},
		"f-moved":     {feature.StateMergedMoved, movedPR, "merged", true},
		"f-published": {feature.StatePublished, pubPR, "open", false},
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
		w, ok := wants[r.Name]
		if !ok {
			t.Fatalf("unexpected feature %q", r.Name)
		}
		gotPR := 0
		if r.PublicPR != nil {
			gotPR = *r.PublicPR
		}
		if r.State != w.state || gotPR != w.pr || r.PRState != w.prState || r.Moved != w.moved {
			t.Errorf("%s = state %s pr %d pr_state %q moved %v; want %+v", r.Name, r.State, gotPR, r.PRState, r.Moved, w)
		}
		if r.Branch != r.Name || r.Head == "" || r.Base == "" || r.Drift.Status != "clean" || r.Drift.Files == nil {
			t.Errorf("%s: branch %q head %q base %q drift %+v", r.Name, r.Branch, r.Head, r.Base, r.Drift)
		}
		if (w.pr != 0) != (r.PlanID != "") {
			t.Errorf("%s: plan id %q", r.Name, r.PlanID)
		}
		if r.Abandoned != (r.Name == "f-abandoned") {
			t.Errorf("%s: abandoned %v", r.Name, r.Abandoned)
		}
		if engine.ClosableWithoutConsole(r) != (r.Name == "f-merged") {
			t.Errorf("%s: closable without a console = %v, want %v", r.Name, engine.ClosableWithoutConsole(r), r.Name == "f-merged")
		}
	}
	if want := []string{"f-abandoned", "f-closed", "f-draft", "f-gated", "f-merged", "f-moved", "f-published"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v (sorted)", names, want)
	}
	if g := h.report("f-gated").Gate; g == nil || g.Checks != feature.ChecksPassed || g.Leak != feature.LeakClean || !g.Fresh || g.At.IsZero() {
		t.Fatalf("f-gated gate = %+v", g)
	}
	if g := h.report("f-draft").Gate; g != nil {
		t.Fatalf("f-draft gate = %+v, want nil", g)
	}
	if g := h.report("f-moved").Gate; g == nil || g.Fresh {
		t.Fatalf("f-moved gate = %+v, want a stale gate", g)
	}
}

// Review Focus 2: when the published head isn't recorded, the moved check
// falls back to the plan file, then the gate record, and never to "safe".
func TestSurveyMovedFallbacks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	planPath := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	saved, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}
	check := func(step string, want feature.State) {
		t.Helper()
		if r := h.report("feat-x"); r.State != want || r.Moved != (want == feature.StateMergedMoved) {
			t.Fatalf("%s: state %s moved %v, want %s", step, r.State, r.Moved, want)
		}
	}

	closable := func(step string, want bool) {
		t.Helper()
		if got := engine.ClosableWithoutConsole(h.report("feat-x")); got != want {
			t.Fatalf("%s: closable without a console = %v, want %v", step, got, want)
		}
	}

	// Each step keeps only the baseline it is about, so no other one can
	// say unmoved for it.
	var gate *feature.Gate
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { gate, m.Gate = m.Gate, nil })
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	check("recorded head only", feature.StateMerged)

	if err := os.WriteFile(planPath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Promoted.FeatureHead = "" })
	check("plan file only", feature.StateMerged)

	// A plan without a feature head falls back to the gate record only when
	// the gate's candidate is the one the plan published.
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Gate = gate })
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	if pl.Fingerprint.GateCandidate != gate.Candidate {
		t.Fatalf("plan gate candidate %s != gate candidate %s", pl.Fingerprint.GateCandidate, gate.Candidate)
	}
	pl.Fingerprint.FeatureHead = ""
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	check("plan without a head, gate candidate matches", feature.StateMerged)
	pl.Fingerprint.GateCandidate = "0000000000000000000000000000000000000000"
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	check("plan without a head, gate candidate differs", feature.StateMergedMoved)

	// Without the plan, nothing ties the gate record to the publish: a
	// re-gate may have overwritten it.
	if err := os.Remove(planPath); err != nil {
		t.Fatal(err)
	}
	check("no recorded head, no plan file, gate record", feature.StateMergedMoved)
	closable("no recorded head, no plan file, gate record", false)

	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Gate = nil })
	check("nothing to compare against", feature.StateMergedMoved)
	closable("nothing to compare against", false)

	// Only the plan file records the published head, and the branch moved.
	if err := os.WriteFile(planPath, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	check("plan file only, before the branch moved", feature.StateMerged)
	testutil.Commit(t, h.shadowGit(), "feat-x", "after the merge", map[string]string{"later.txt": "l\n"})
	check("plan file only, branch moved", feature.StateMergedMoved)
	closable("plan file only, branch moved", false)
}

// A re-gate after publishing overwrites the gate record with commits that
// were never published. Without the recorded head or the plan file, the
// survey must not take the gate record for the published head.
func TestSurveyRegateAfterPublishIsMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	testutil.Commit(t, h.shadowGit(), "feat-x", "never published", map[string]string{"unpublished.txt": "u\n"})
	h.gateFeature("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	if r := h.report("feat-x"); r.State != feature.StateMergedMoved || engine.ClosableWithoutConsole(r) {
		t.Fatalf("with the recorded head: %+v", r)
	}
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Promoted.FeatureHead = "" })
	if err := os.Remove(filepath.Join(h.eng.WS.PlansDir(), id+".json")); err != nil {
		t.Fatal(err)
	}
	r := h.report("feat-x")
	if r.State != feature.StateMergedMoved || !r.Moved || engine.ClosableWithoutConsole(r) {
		t.Fatalf("re-gated after publish, no recorded head, no plan: state %s moved %v closable %v",
			r.State, r.Moved, engine.ClosableWithoutConsole(r))
	}
}

// Review Focus 5: a deleted branch and an empty pair are states, not errors.
func TestSurveyBranchGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if rs, warns := h.survey(); rs == nil || len(rs) != 0 || len(warns) != 0 {
		t.Fatalf("empty pair: %#v %+v", rs, warns)
	}

	sh := h.shadowGit()
	h.startFeature("feat-gone", map[string]string{"gone.txt": "g\n"})
	if err := sh.DeleteRef(h.ctx, "refs/heads/feat-gone"); err != nil {
		t.Fatal(err)
	}
	r := h.report("feat-gone")
	if r.State != feature.StateBranchGone || r.Head != "" || r.Drift.Status != "n/a" || r.Gate != nil || r.PublicPR != nil {
		t.Fatalf("report = %+v", r)
	}
	if engine.ClosableWithoutConsole(r) {
		t.Fatal("an unpublished feature whose branch is gone needs a person")
	}

	openPR, _ := h.publish("feat-open")
	if err := sh.DeleteRef(h.ctx, "refs/heads/feat-open"); err != nil {
		t.Fatal(err)
	}
	if r := h.report("feat-open"); r.State != feature.StateBranchGone || r.PRState != "open" || engine.ClosableWithoutConsole(r) {
		t.Fatalf("published, PR open, branch gone: %+v closable %v", r, engine.ClosableWithoutConsole(r))
	}
	h.forge.SetPRMerged(publicName, openPR)
	if r := h.report("feat-open"); r.State != feature.StateBranchGone || r.PRState != "merged" || !engine.ClosableWithoutConsole(r) {
		t.Fatalf("published, PR merged, branch gone: %+v closable %v", r, engine.ClosableWithoutConsole(r))
	}
}

func TestSurveyDrift(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-readme", map[string]string{"README.md": "# widget, the feature's way\n"})
	h.startFeature("feat-clean", map[string]string{"clean.txt": "c\n"})
	testutil.Commit(t, h.pubGit, "main", "upstream readme", map[string]string{"README.md": "# widget, upstream's way\n"})

	rs, warns := h.survey()
	if len(rs) != 2 || len(warns) != 0 {
		t.Fatalf("survey = %+v %+v", rs, warns)
	}
	got := map[string]engine.Drift{}
	for _, r := range rs {
		got[r.Name] = r.Drift
	}
	want := map[string]engine.Drift{
		"feat-clean":  {Status: "clean", Files: []string{}},
		"feat-readme": {Status: "conflicts", Files: []string{"README.md"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("drift = %+v, want %+v", got, want)
	}
}

func TestSurveyPRStateUnknown(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.forge.GetPRErr = errors.New("github is down")
	rs, warns := h.survey()
	if len(rs) != 1 {
		t.Fatalf("survey = %+v", rs)
	}
	r := rs[0]
	if r.PRState != "unknown" || r.State != feature.StatePublished || r.PublicPR == nil || *r.PublicPR != pr || engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v", r)
	}
	if len(warns) != 1 || warns[0].Code != contract.PRStateUnknown || warns[0].Details["feature"] != "feat-x" {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestApplyRecordsThePublishedHead(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	_, id := h.publish("feat-x")
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	m, err := feature.Decode(raw)
	if err != nil || m.Promoted == nil || pl.Fingerprint.FeatureHead == "" || m.Promoted.FeatureHead != pl.Fingerprint.FeatureHead {
		t.Fatalf("meta.promoted = %+v, plan feature head %s, %v", m.Promoted, pl.Fingerprint.FeatureHead, err)
	}
}
