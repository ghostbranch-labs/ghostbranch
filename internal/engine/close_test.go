package engine_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// shadowRefs is every ref in the shadow repo.
func (h *harness) shadowRefs() map[string]string {
	h.t.Helper()
	refs, err := h.shadowGit().Refs(h.ctx, "refs")
	if err != nil {
		h.t.Fatal(err)
	}
	return refs
}

// shadowMeta reads a feature's metadata straight from the shadow repo.
func (h *harness) shadowMeta(name string) feature.Meta {
	h.t.Helper()
	raw, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/"+name, feature.MetaFile)
	if err != nil || !ok {
		h.t.Fatalf("read the metadata of %s: %v", name, err)
	}
	m, err := feature.Decode(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// gatePR is the open gate draft PR of a feature, or 0.
func (h *harness) gatePR(name string) int {
	h.t.Helper()
	pr, err := h.forge.FindOpenPR(h.ctx, shadowName, "ghostbranch-gate/"+name, "main")
	if err != nil {
		h.t.Fatal(err)
	}
	if pr == nil {
		return 0
	}
	return pr.Number
}

// closePushes counts the pushes feature close made.
func (h *harness) closePushes() int {
	n := 0
	for _, p := range h.pushes {
		if p.op == "feature close" {
			n++
		}
	}
	return n
}

// featureRefs is what closing name must delete, read from the shadow repo:
// its branch, gate branch, metadata ref and the plan ref of planID ("" for none).
func (h *harness) featureRefs(name, planID string) map[string]string {
	h.t.Helper()
	all := h.shadowRefs()
	want := map[string]string{}
	for _, ref := range []string{"refs/heads/" + name, "refs/heads/ghostbranch-gate/" + name, "refs/ghostbranch/features/" + name} {
		if sha, ok := all[ref]; ok {
			want[ref] = sha
		}
	}
	if planID != "" {
		ref := "refs/ghostbranch/plans/" + planID
		sha, ok := all[ref]
		if !ok {
			h.t.Fatalf("no %s in the shadow repo", ref)
		}
		want[ref] = sha
	}
	return want
}

// mirrorHas reports whether the pair's mirror has ref.
func (h *harness) mirrorHas(ref string) bool {
	h.t.Helper()
	_, ok, err := gitx.New(h.eng.WS.MirrorDir(h.pair)).Resolve(h.ctx, ref)
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

func TestAbandonAndUndo(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	before := h.shadowRefs()
	entries := len(h.ledger())

	h.eng.DryRun = true
	res, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", false)
	h.eng.DryRun = false
	if err != nil || res.Preview == nil || !res.Abandoned {
		t.Fatalf("abandon --plan = %+v %v", res, err)
	}
	validateResult(t, "feature-abandon.schema.json", res)
	meta := "refs/ghostbranch/features/feat-x"
	if len(res.Preview.Ops) != 1 || res.Preview.Ops[0]["op"] != "push_ref" || res.Preview.Ops[0]["ref"] != meta ||
		res.Preview.Ops[0]["from"] != before[meta] {
		t.Fatalf("abandon --plan ops = %+v", res.Preview.Ops)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || len(h.ledger()) != entries {
		t.Fatal("abandon --plan changed something")
	}

	res, _, err = h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", false)
	if err != nil || !res.Abandoned || !res.Changed || res.Preview != nil {
		t.Fatalf("abandon = %+v %v", res, err)
	}
	validateResult(t, "feature-abandon.schema.json", res)
	if !h.shadowMeta("feat-x").Abandoned || h.report("feat-x").State != feature.StateAbandoned {
		t.Fatal("abandon did not mark the feature abandoned")
	}
	if after := h.shadowRefs(); after["refs/heads/feat-x"] != before["refs/heads/feat-x"] {
		t.Fatal("abandon must keep the branch")
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "feature abandon" || last.Feature != "feat-x" || last.Summary != "abandoned" || last.ExitCode != 0 {
		t.Fatalf("ledger = %+v", last)
	}

	res, _, err = h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", true)
	if err != nil || res.Abandoned || !res.Changed {
		t.Fatalf("abandon --undo = %+v %v", res, err)
	}
	if h.shadowMeta("feat-x").Abandoned || h.report("feat-x").State == feature.StateAbandoned {
		t.Fatal("--undo did not restore the feature")
	}
	es = h.ledger()
	if last := es[len(es)-1]; last.Op != "feature abandon" || last.Summary != "restored" {
		t.Fatalf("ledger = %+v", last)
	}
	same := h.shadowRefs()
	if res, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", true); err != nil || res.Changed || res.Abandoned {
		t.Fatalf("a second --undo = %+v %v", res, err)
	}
	validateResult(t, "feature-abandon.schema.json", res)
	es = h.ledger()
	if last := es[len(es)-1]; last.Summary != "already active; nothing changed" || !reflect.DeepEqual(h.shadowRefs(), same) {
		t.Fatalf("a second --undo: ledger %+v", last)
	}
	if len(h.approver.asked) != 0 {
		t.Fatalf("abandon asked the console: %v", h.approver.asked)
	}
	_, _, err = h.eng.FeatureAbandon(h.ctx, h.pair, "nope", false)
	wantCode(t, err, contract.FeatureNotFound)
}

func TestCloseMergedNeedsNoConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	gate := h.gatePR("feat-x")
	if gate == 0 {
		t.Fatal("the gate left no draft PR")
	}
	want := h.featureRefs("feat-x", id)
	if len(want) != 4 {
		t.Fatalf("refs to delete = %v", want)
	}
	planPath := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	if _, err := os.Stat(planPath); err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil

	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatalf("close: %v", err)
	}
	validateResult(t, "feature-close.schema.json", res)
	if res.Feature != "feat-x" || res.State != feature.StateMerged || res.Approval != nil || res.Preview != nil ||
		res.GatePR == nil || *res.GatePR != gate {
		t.Fatalf("close = %+v", res)
	}
	if !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("deleted = %v, want %v", res.Deleted, want)
	}
	after := h.shadowRefs()
	for ref := range want {
		if _, ok := after[ref]; ok {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
	if h.gatePR("feat-x") != 0 {
		t.Fatal("the gate PR is still open")
	}
	if h.closePushes() != 1 {
		t.Fatalf("close pushed %d times, want one atomic push", h.closePushes())
	}
	for _, ref := range []string{"refs/remotes/shadow/feat-x", "refs/remotes/shadow/ghostbranch-gate/feat-x",
		"refs/shadow-meta/features/feat-x", "refs/shadow-meta/plans/" + id} {
		if h.mirrorHas(ref) {
			t.Errorf("the mirror still has %s", ref)
		}
	}
	if _, err := os.Stat(planPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the plan file is still there: %v", err)
	}
	es := h.ledger()
	last := es[len(es)-1]
	if last.Op != "feature close" || last.Feature != "feat-x" || last.ExitCode != 0 || last.Approval != nil ||
		!reflect.DeepEqual(last.Refs, want) {
		t.Fatalf("ledger = %+v", last)
	}
	raw, _ := json.Marshal(last)
	validateSchema(t, "ledger-entry.schema.json", raw)
	if rs, _ := h.survey(); len(rs) != 0 {
		t.Fatalf("the survey still finds %+v", rs)
	}
}

// Merged, and the branch was deleted by hand: nothing is left to lose.
func TestCloseMergedWithTheBranchGone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	want := h.featureRefs("feat-x", id)
	h.eng.Approver = nil
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil || res.State != feature.StateBranchGone || res.Approval != nil {
		t.Fatalf("close = %+v %v", res, err)
	}
	if _, ok := res.Deleted["refs/heads/feat-x"]; ok || !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("deleted = %v, want %v", res.Deleted, want)
	}
}

func TestCloseUnpublishedNeedsAConsole(t *testing.T) {
	t.Parallel()
	rows := []struct {
		state feature.State
		name  string                  // the subtest, when not the state
		setup func(h *harness) string // returns the plan ID, or ""
	}{
		{feature.StateInReview, "", func(h *harness) string {
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			h.gateFeature("feat-x")
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "after the gate", map[string]string{"y.txt": "y\n"})
			return ""
		}},
		{feature.StateBranchGone, "branch_gone_unpublished", func(h *harness) string {
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
				h.t.Fatal(err)
			}
			return ""
		}},
		{feature.StateBranchGone, "branch_gone_pr_open", func(h *harness) string {
			_, id := h.publish("feat-x")
			if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
				h.t.Fatal(err)
			}
			return id
		}},
		{feature.StateDraft, "", func(h *harness) string {
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			return ""
		}},
		{feature.StateGatePassed, "", func(h *harness) string {
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			h.gateFeature("feat-x")
			return ""
		}},
		{feature.StateAbandoned, "", func(h *harness) string {
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-x", false); err != nil {
				h.t.Fatal(err)
			}
			return ""
		}},
		{feature.StatePublished, "", func(h *harness) string {
			_, id := h.publish("feat-x")
			return id
		}},
		{feature.StateMergedMoved, "", func(h *harness) string {
			pr, id := h.publish("feat-x")
			h.forge.SetPRMerged(publicName, pr)
			testutil.Commit(h.t, h.shadowGit(), "feat-x", "after the merge", map[string]string{"later.txt": "l\n"})
			return id
		}},
		{feature.StatePRClosed, "", func(h *harness) string {
			pr, id := h.publish("feat-x")
			h.forge.ClosePR(publicName, pr)
			return id
		}},
	}
	for _, row := range rows {
		name := row.name
		if name == "" {
			name = string(row.state)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			id := row.setup(h)
			if got := h.report("feat-x").State; got != row.state {
				t.Fatalf("set up %s, got %s", row.state, got)
			}
			before := h.shadowRefs()
			want := h.featureRefs("feat-x", id)
			asked := len(h.approver.asked)
			nothingDeleted := func(step string) {
				t.Helper()
				if !reflect.DeepEqual(h.shadowRefs(), before) || h.closePushes() != 0 {
					t.Fatalf("%s deleted something", step)
				}
				if !h.mirrorHas("refs/shadow-meta/features/feat-x") {
					t.Fatalf("%s deleted the mirror's metadata ref", step)
				}
			}

			h.eng.Approver = nil
			_, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
			ce := wantCode(t, err, contract.ApprovalRequired)
			if ce.Details["reason"] != string(approval.NoConsole) {
				t.Fatalf("details = %v", ce.Details)
			}
			nothingDeleted("no console")

			h.eng.Approver = &approver{err: &approval.Refusal{Reason: approval.NotConfirmed}}
			_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
			ce = wantCode(t, err, contract.ApprovalRequired)
			if ce.Details["reason"] != string(approval.NotConfirmed) {
				t.Fatalf("details = %v", ce.Details)
			}
			nothingDeleted("a declined console")

			h.eng.Approver = h.approver
			res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
			if err != nil {
				t.Fatalf("close: %v", err)
			}
			validateResult(t, "feature-close.schema.json", res)
			if res.State != row.state || res.Approval == nil || res.Approval.Kind != "console" {
				t.Fatalf("close = %+v", res)
			}
			if !reflect.DeepEqual(res.Deleted, want) {
				t.Fatalf("deleted = %v, want %v", res.Deleted, want)
			}
			if len(h.approver.asked) != asked+1 {
				t.Fatalf("console asked %d times", len(h.approver.asked)-asked)
			}
			summary := h.approver.asked[len(h.approver.asked)-1]
			for ref, sha := range want {
				if !strings.Contains(summary, ref+" at "+sha[:12]) {
					t.Errorf("the summary doesn't name %s at %s:\n%s", ref, sha[:12], summary)
				}
			}
			if !strings.Contains(summary, string(row.state)) {
				t.Errorf("the summary lacks the state:\n%s", summary)
			}
			for ref, sha := range want {
				if strings.HasPrefix(ref, "refs/heads/") && !strings.Contains(summary, sha+":"+ref) {
					t.Errorf("the summary lacks the recovery command for %s:\n%s", ref, summary)
				}
			}
			for ref := range want {
				if _, ok := h.shadowRefs()[ref]; ok {
					t.Errorf("%s is still in the shadow repo", ref)
				}
			}
			es := h.ledger()
			if last := es[len(es)-1]; last.Op != "feature close" || last.Approval == nil || last.Approval.Kind != "console" ||
				!reflect.DeepEqual(last.Refs, want) {
				t.Fatalf("ledger = %+v", last)
			}
		})
	}
}

// Review Focus 3: a commit lands on the branch just before the delete. The
// push is atomic, so nothing at all is deleted.
func TestCloseAbortsWhenTheBranchMoved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	gate := h.gatePR("feat-x")
	planPath := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	h.eng.Approver = nil
	record := h.eng.OnPush
	var raced string
	h.eng.OnPush = func(op, remote string, specs []string) {
		if op == "feature close" && raced == "" {
			raced = testutil.Commit(t, h.shadowGit(), "feat-x", "racing commit", map[string]string{"race.txt": "r\n"})
		}
		record(op, remote, specs)
	}
	before := h.featureRefs("feat-x", id)

	_, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.FeatureMoved)
	if raced == "" {
		t.Fatal("the race never ran")
	}
	if ce.Details["feature"] != "feat-x" || ce.Details["ref"] != "refs/heads/feat-x" {
		t.Fatalf("details = %v", ce.Details)
	}
	after := h.shadowRefs()
	for ref, sha := range before {
		if ref == "refs/heads/feat-x" {
			sha = raced
		}
		if after[ref] != sha {
			t.Errorf("%s = %q, want %q: nothing may be deleted", ref, after[ref], sha)
		}
	}
	if h.gatePR("feat-x") != gate {
		t.Fatal("the gate PR was closed although nothing was deleted")
	}
	if !h.mirrorHas("refs/shadow-meta/features/feat-x") || !h.mirrorHas("refs/shadow-meta/plans/"+id) {
		t.Fatal("the mirror refs were deleted")
	}
	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("the plan file went: %v", err)
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "feature close" || last.ErrorCode != string(contract.FeatureMoved) || len(last.Refs) != 0 {
		t.Fatalf("ledger = %+v", last)
	}
}

// The gate PR went between the look and the close: not an error.
func TestCloseToleratesAGatePRThatVanished(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	h.forge.ClosePRErr = fmt.Errorf("PR %s#%d: %w", shadowName, h.gatePR("feat-x"), forge.ErrNotFound)
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil || res.GatePR != nil || !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("close = %+v %v", res, err)
	}
}

// The metadata names another feature's branch: close refuses before it
// deletes anything, even on the merged path that asks no console.
func TestCloseRefusesABranchThatIsNotTheFeatures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.startFeature("feat-2", map[string]string{"two.txt": "2\n"})
	sh := h.shadowGit()
	head, err := sh.MustResolve(h.ctx, "refs/heads/feat-x")
	if err != nil {
		t.Fatal(err)
	}
	// feat-2 at feat-x's published head, so feat-x still reads as merged.
	if err := sh.UpdateRef(h.ctx, "refs/heads/feat-2", head); err != nil {
		t.Fatal(err)
	}
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Branch = "feat-2" })
	if st := h.report("feat-x").State; st != feature.StateMerged {
		t.Fatalf("state = %s, want merged", st)
	}
	before := h.shadowRefs()
	gate := h.gatePR("feat-x")
	h.eng.Approver = nil
	_, _, err = h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.Internal)
	if !strings.Contains(ce.Message, `"feat-2"`) || !strings.Contains(ce.Message, `"feat-x"`) {
		t.Fatalf("message = %q", ce.Message)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.closePushes() != 0 || h.gatePR("feat-x") != gate {
		t.Fatal("close deleted something")
	}
}

// A server-side hook refuses only the metadata ref: the push is atomic, so
// every ref survives and nothing is recorded as deleted.
func TestCloseDeletesAllOrNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	gate := h.gatePR("feat-x")
	before := h.shadowRefs()
	want := h.featureRefs("feat-x", id)
	hook := "#!/bin/sh\ncase \"$1\" in\n  refs/ghostbranch/features/*) echo \"no deleting metadata\" >&2; exit 1 ;;\nesac\nexit 0\n"
	if err := os.WriteFile(filepath.Join(h.forge.RepoDir(shadowName), "hooks", "update"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = nil
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err == nil || res != nil {
		t.Fatalf("close = %+v %v, want an error and no result", res, err)
	}
	after := h.shadowRefs()
	for ref, sha := range want {
		if after[ref] != sha {
			t.Errorf("%s = %q, want %q: the push must delete all or nothing", ref, after[ref], sha)
		}
	}
	if !reflect.DeepEqual(after, before) || h.gatePR("feat-x") != gate {
		t.Fatal("close changed the shadow repo or closed the gate PR")
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "feature close" || last.ExitCode == 0 || len(last.Refs) != 0 {
		t.Fatalf("ledger = %+v", last)
	}
}

// A close the console said yes to, that then fails, still records the yes.
func TestCloseRecordsTheConsoleWhenItFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	record := h.eng.OnPush
	raced := false
	h.eng.OnPush = func(op, remote string, specs []string) {
		if op == "feature close" && !raced {
			raced = true
			testutil.Commit(t, h.shadowGit(), "feat-x", "racing commit", map[string]string{"race.txt": "r\n"})
		}
		record(op, remote, specs)
	}
	_, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	wantCode(t, err, contract.FeatureMoved)
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "feature close" || last.Approval == nil || last.Approval.Kind != "console" || len(last.Refs) != 0 {
		t.Fatalf("ledger = %+v", last)
	}
}

// Review Focus 4: the gate PR was closed by hand.
func TestCloseToleratesAMissingGatePR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.forge.ClosePR(shadowName, h.gatePR("feat-x"))
	want := h.featureRefs("feat-x", id)
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil || res.GatePR != nil || !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("close = %+v %v", res, err)
	}
}

// The deletes went through but the gate PR didn't close: the error says
// so, and the ledger still lists every ref deleted.
func TestCloseReportsAGatePRItCouldNotClose(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"forbidden", fmt.Errorf("%w: close PR", forge.ErrForbidden), "the token may not close its gate PR"},
		{"other", errors.New("boom"), "couldn't close its gate PR"},
		// Transient, but the refs are gone: a retry finds no feature.
		{"transient", fmt.Errorf("%w: 502", forge.ErrTransient), "couldn't close its gate PR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			pr, id := h.publish("feat-x")
			h.forge.SetPRMerged(publicName, pr)
			gate := h.gatePR("feat-x")
			want := h.featureRefs("feat-x", id)
			h.forge.ClosePRErr = tc.err
			res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
			ce := wantCode(t, err, contract.ForgeError)
			if ce.Retryable {
				t.Fatal("a close that deleted the refs is never retryable")
			}
			if !strings.Contains(ce.Message, tc.want) || !strings.Contains(ce.Message, fmt.Sprintf("#%d", gate)) {
				t.Fatalf("message = %q", ce.Message)
			}
			if res == nil || res.GatePR != nil || !reflect.DeepEqual(res.Deleted, want) {
				t.Fatalf("close = %+v", res)
			}
			for ref := range want {
				if _, ok := h.shadowRefs()[ref]; ok {
					t.Errorf("%s is still in the shadow repo", ref)
				}
			}
			es := h.ledger()
			if last := es[len(es)-1]; last.Op != "feature close" || last.ErrorCode != string(contract.ForgeError) ||
				!reflect.DeepEqual(last.Refs, want) {
				t.Fatalf("ledger = %+v", last)
			}
		})
	}
}

func TestCloseUnknownFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	_, _, err := h.eng.FeatureClose(h.ctx, h.pair, "nope")
	wantCode(t, err, contract.FeatureNotFound)
	if h.closePushes() != 0 {
		t.Fatal("close pushed")
	}
}

// --plan previews the PR close and every delete, asks no console and
// changes nothing.
func TestClosePlanChangesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	_, id := h.publish("feat-x")
	gate := h.gatePR("feat-x")
	before := h.shadowRefs()
	want := h.featureRefs("feat-x", id)
	entries := len(h.ledger())
	asked := len(h.approver.asked)
	h.eng.Approver = nil
	h.eng.DryRun = true
	res, _, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	h.eng.DryRun = false
	if err != nil || res.Preview == nil || len(res.Deleted) != 0 || res.GatePR != nil || res.Approval != nil {
		t.Fatalf("close --plan = %+v %v", res, err)
	}
	validateResult(t, "feature-close.schema.json", res)
	// In the order they run: the deletes, then the gate PR, then the plan file.
	ops := res.Preview.Ops
	if len(ops) != len(want)+2 {
		t.Fatalf("ops = %+v", ops)
	}
	for _, o := range ops[:len(want)] {
		if o["op"] != "delete_ref" || o["repo"] != shadowName || want[o["ref"].(string)] != o["sha"] {
			t.Fatalf("op = %+v, want a delete of one of %v", o, want)
		}
	}
	if o := ops[len(want)]; o["op"] != "close_pr" || o["repo"] != shadowName || o["number"] != gate {
		t.Fatalf("op = %+v, want the gate PR #%d closed", o, gate)
	}
	if o := ops[len(want)+1]; o["op"] != "remove_file" || o["path"] != filepath.Join(h.eng.WS.PlansDir(), id+".json") {
		t.Fatalf("op = %+v, want the plan file removed", o)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.gatePR("feat-x") != gate || len(h.ledger()) != entries ||
		len(h.approver.asked) != asked || h.closePushes() != 0 || !h.mirrorHas("refs/shadow-meta/plans/"+id) {
		t.Fatal("close --plan changed something")
	}
	if _, err := os.Stat(filepath.Join(h.eng.WS.PlansDir(), id+".json")); err != nil {
		t.Fatalf("close --plan removed the plan file: %v", err)
	}
}

// The refs were deleted, but the mirror's copy couldn't be: the close
// succeeds with MIRROR_NOT_UPDATED.
func TestCloseWarnsWhenTheMirrorCantBeCleanedUp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	meta := "refs/shadow-meta/features/feat-x"
	h.lockMirrorRef(meta)
	res, warns, err := h.eng.FeatureClose(h.ctx, h.pair, "feat-x")
	if err != nil || !reflect.DeepEqual(res.Deleted, want) {
		t.Fatalf("close = %+v %v", res, err)
	}
	ws := warningsFor(warns, contract.MirrorNotUpdated)
	if len(ws) != 1 || ws[0].Details["ref"] != meta {
		t.Fatalf("warnings = %+v", warns)
	}
	if _, ok := h.shadowRefs()["refs/ghostbranch/features/feat-x"]; ok {
		t.Fatal("the shadow repo still has the metadata")
	}
}
