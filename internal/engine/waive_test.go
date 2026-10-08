package engine_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
)

func fixture() map[string]string {
	return map[string]string{"test/fixtures/token.txt": "token: " + testutil.FakeGitHubToken() + "\n", "feature.txt": "new\n"}
}

func TestAWaivedFindingNoLongerBlocks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", fixture())
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 1 || !res.Findings[0].Waivable || !reflect.DeepEqual(ce.Details["waivable"], []string{res.Findings[0].ID}) ||
		ce.Details["feature"] != "feat-x" {
		t.Fatalf("findings = %+v, details = %+v", res.Findings, ce.Details)
	}
	f := res.Findings[0]
	h.waive(f, "test fixture")

	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || len(res.Findings) != 0 || len(res.Waived) != 1 || res.Waived[0].ID != f.ID || res.Waived[0].Reason != "test fixture" {
		t.Fatalf("gate = %+v %v", res, err)
	}
	pr, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "Add feature", PlanOnly: true})
	if err != nil || len(pr.Waived) != 1 || len(pr.Plan.Leak.Waived) != 1 || pr.Plan.Leak.Waived[0] != (plan.Waived{ID: f.ID, Rule: f.Rule, Where: "tree", Path: f.Path, Reason: "test fixture"}) {
		t.Fatalf("promote = %+v %v", pr, err)
	}

	// Revoking between plan and apply blocks the publish.
	if _, err := h.eng.Waivers.Revoke(publicName, []string{f.ID}, false, h.clock.Now()); err != nil {
		t.Fatal(err)
	}
	before := h.publicRefs()
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pr.Plan.ID})
	_ = wantCode(t, err, contract.LeakFound)
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("published after the waiver was revoked")
	}

	// Waived again: the publish goes through, and the person saw the waiver.
	h.waive(f, "test fixture")
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pr.Plan.ID})
	if err != nil || len(ar.Waived) != 1 {
		t.Fatalf("apply = %+v %v", ar, err)
	}
	asked := h.approver.asked[len(h.approver.asked)-1]
	if !strings.Contains(asked, f.ID) || !strings.Contains(asked, "test fixture") {
		t.Fatalf("the approval summary must list the waiver:\n%s", asked)
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "apply" || !reflect.DeepEqual(last.Waivers, []string{f.ID}) {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestTypedTextIsNeverWaivable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	opt := engine.PromoteOptions{Feature: "feat-x", Message: "Add feature\n\ntoken " + testutil.FakeGitHubToken(), PlanOnly: true}
	_, _, err := h.eng.Promote(h.ctx, h.pair, opt)
	ce := wantCode(t, err, contract.LeakFound)
	fs := ce.Details["findings"].([]leak.Finding)
	if len(fs) == 0 || fs[0].Waivable || len(ce.Details["waivable"].([]string)) != 0 {
		t.Fatalf("details = %+v", ce.Details)
	}
	for _, f := range fs {
		h.waive(f, "trying anyway")
	}
	_, _, err = h.eng.Promote(h.ctx, h.pair, opt)
	_ = wantCode(t, err, contract.LeakFound)
}

// Review Focus 4.
func TestAWaiverCoversOneFileOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	body := "token: " + testutil.FakeGitHubToken() + "\n"
	h.startFeature("feat-x", map[string]string{"test/a.txt": body, "test/b.txt": body})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 2 {
		t.Fatalf("findings = %+v", res.Findings)
	}
	h.waive(res.Findings[0], "fixture a")
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	if len(res.Findings) != 1 || res.Findings[0].Path != "test/b.txt" || len(res.Waived) != 1 {
		t.Fatalf("gate = %+v", res)
	}
}

// Review Focus 5.
func TestACorruptWaiversFileBlocksWithAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", fixture())
	res, _, _ := h.eng.Gate(h.ctx, h.pair, "feat-x")
	h.waive(res.Findings[0], "fixture")
	if err := os.WriteFile(h.eng.Waivers.Path(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	var saw bool
	for _, w := range warns {
		saw = saw || (w.Code == contract.WaiversUnreadable && strings.Contains(w.Message, filepath.Base(h.eng.Waivers.Path())))
	}
	if !saw {
		t.Fatalf("want WAIVERS_UNREADABLE naming the file, got %+v", warns)
	}
}

func TestApplyScansThePublishedCommitsRealAuthor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns(`corp\.internal`)
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n"})
	pr, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "Add feature", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	// Swap in a commit with the same tree and parent but a private author,
	// as a hand-edited plan and plan ref would.
	sh := h.shadowGit()
	who := gitx.Identity{Name: "Dev", Email: "dev@corp.internal", When: h.clock.Now()}
	P2, err := sh.CommitTree(h.ctx, pr.Plan.Publish.Tree, []string{pr.Plan.Publish.Parent}, "Add feature", who, who)
	if err != nil {
		t.Fatal(err)
	}
	if err := sh.UpdateRef(h.ctx, "refs/ghostbranch/plans/"+pr.Plan.ID, P2); err != nil {
		t.Fatal(err)
	}
	pl, path, err := plan.Load(h.eng.WS.PlansDir(), pr.Plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	pl.Publish.Commit = P2
	if _, err := plan.Save(filepath.Dir(path), pl); err != nil {
		t.Fatal(err)
	}
	before := h.publicRefs()
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pr.Plan.ID})
	ce := wantCode(t, err, contract.LeakFound)
	var where []string
	for _, f := range ce.Details["findings"].([]leak.Finding) {
		where = append(where, f.Where)
	}
	if !reflect.DeepEqual(where, []string{"author", "committer"}) {
		t.Fatalf("want the real author and committer flagged, got %v", where)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("published a commit with a private author")
	}
}

func TestFileWarningsReachThePlanAndTheApprover(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{".github/workflows/release.yml": "on: push\n"})
	_, warns, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || !hasWarning(warns, contract.WorkflowChanged) {
		t.Fatalf("gate warnings = %+v %v", warns, err)
	}
	pr, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "Add release", PlanOnly: true})
	if err != nil || !hasWarning(pr.Plan.Leak.Warnings, contract.WorkflowChanged) {
		t.Fatalf("plan warnings = %+v %v", pr.Plan.Leak.Warnings, err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pr.Plan.ID}); err != nil {
		t.Fatal(err)
	}
	var warnLine bool
	asked := h.approver.asked[len(h.approver.asked)-1]
	for _, l := range strings.Split(asked, "\n") {
		warnLine = warnLine || (strings.HasPrefix(l, "  warning  ") && strings.Contains(l, ".github/workflows/release.yml"))
	}
	if !warnLine {
		t.Fatalf("the approval summary must show the workflow warning:\n%s", asked)
	}
}

func hasWarning(ws []contract.Warning, code contract.WarningCode) bool {
	for _, w := range ws {
		if w.Code == code {
			return true
		}
	}
	return false
}

func TestPromoteReportsAFileWarningOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{".github/workflows/release.yml": "on: push\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	pr, warns, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Message: "Add release", Yes: true})
	if err != nil || pr.Applied == nil {
		t.Fatalf("promote = %+v %v", pr, err)
	}
	n := 0
	for _, w := range warns {
		if w.Code == contract.WorkflowChanged {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want one WORKFLOW_CHANGED, got %d in %+v", n, warns)
	}
}

// A waiver covers exactly the finding it was made for: every field of the
// match must agree.
func TestAWaiverThatDiffersInOneFieldDoesNotApply(t *testing.T) {
	t.Parallel()
	cases := map[string]func(w *waiver.Waiver){
		"match hash": func(w *waiver.Waiver) { w.MatchSHA256 = strings.Repeat("0", 64) },
		"rule":       func(w *waiver.Waiver) { w.Rule = "other-rule" },
		"path":       func(w *waiver.Waiver) { w.Path = "test/other.txt" },
		"where":      func(w *waiver.Waiver) { w.Where = "path" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", fixture())
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			_ = wantCode(t, err, contract.LeakFound)
			f := res.Findings[0]
			w := waiver.Waiver{ID: f.ID, Public: publicName, Rule: f.Rule, Where: f.Where, Path: f.Path,
				MatchSHA256: f.MatchSHA256, Reason: "r", CreatedAt: h.clock.Now(), CreatedBy: "human:octo"}
			mutate(&w)
			if err := h.eng.Waivers.Add([]waiver.Waiver{w}, h.clock.Now()); err != nil {
				t.Fatal(err)
			}
			res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
			_ = wantCode(t, err, contract.LeakFound)
			if len(res.Findings) != 1 || len(res.Waived) != 0 {
				t.Fatalf("gate = %+v", res)
			}
		})
	}
}
