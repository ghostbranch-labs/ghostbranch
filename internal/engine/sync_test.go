package engine_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestSyncNoopFastForwardAndDiverged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	res, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.Action != "noop" {
		t.Fatalf("noop: %+v %v", res, err)
	}

	newMain := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "1\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.Action != "fast_forward" || res.ShadowMainAfter != newMain {
		t.Fatalf("ff: %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != newMain {
		t.Fatalf("shadow main = %s, want %s", got, newMain)
	}

	h.eng.DryRun = true
	testutil.Commit(t, h.pubGit, "main", "upstream 2", map[string]string{"up.txt": "2\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.Action != "fast_forward" || res.Preview == nil {
		t.Fatalf("dry ff: %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != newMain {
		t.Fatal("dry run moved shadow main")
	}
	h.eng.DryRun = false

	rogue := testutil.Commit(t, h.shadowGit(), "main", "rogue", map[string]string{"rogue.txt": "x\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	ce := wantCode(t, err, contract.SyncDiverged)
	if res.Action != "diverged" || ce.Details["shadow_main"] != rogue {
		t.Fatalf("diverged: %+v %+v", res, ce)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != rogue {
		t.Fatal("sync must never force-push a diverged shadow main")
	}
}

func TestSyncFollowsEveryConfiguredBranch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{"r": "1\n"})
	h.init() // mirrors release too
	h.setConfig("branches: [main]", "branches: [main, release, dev]")
	testutil.Commit(t, h.pubGit, "dev", "dev", map[string]string{"d": "1\n"})
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n": "1\n"})
	res, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range res.Branches {
		got[b.Name] = b.Status
	}
	if got["main"] != "fast_forwarded" || got["release"] != "up_to_date" || got["dev"] != "created" || res.ShadowMainAfter != next {
		t.Fatalf("branches = %+v", res.Branches)
	}
}

func TestSyncRefusesABranchPublicLacks(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("branches: [main]", "branches: [main, ghost]")
	n := len(h.pushes)
	_, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if ce := wantCode(t, err, contract.ConfigInvalid); ce.Field != "sync.branches" {
		t.Fatalf("field = %q", ce.Field)
	}
	if len(h.pushes) != n {
		t.Fatal("sync pushed before refusing")
	}
}

func TestSyncReportsEachDivergedBranchAndPushesTheRest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{"r": "1\n"})
	h.init()
	h.setConfig("branches: [main]", "branches: [main, release]")
	shadowOnly := testutil.Commit(t, h.shadowGit(), "release", "shadow-only", map[string]string{"x": "1\n"})
	testutil.Commit(t, h.pubGit, "release", "rel2", map[string]string{"r": "2\n"})
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n": "1\n"})
	res, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	ce := wantCode(t, err, contract.SyncDiverged)
	if b, _ := ce.Details["branches"].([]string); len(b) != 1 || b[0] != "release" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if res.ShadowMainAfter != next {
		t.Fatal("main was not fast-forwarded alongside the diverged branch")
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/release"); got != shadowOnly {
		t.Fatalf("shadow release = %s, want the shadow-only commit %s left alone", got, shadowOnly)
	}
}

const pushWorkflow = "on:\n  push:\n    branches: [main]\njobs:\n  t:\n    runs-on: x\n    steps:\n      - run: echo\n"

// pushRunsHarness is an initialised pair whose public main moved on, with a
// workflow a push to main starts, so the next sync fast-forwards shadow
// main to next and looks for the runs it started.
func pushRunsHarness(t *testing.T) (h *harness, next string) {
	t.Helper()
	h = newHarness(t)
	h.init()
	h.eng.CIAppearWindow, h.eng.CIAppearPoll = 10*time.Second, time.Second
	next = testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n.txt": "1\n", ".github/workflows/push.yml": pushWorkflow})
	return h, next
}

// A pushed commit with no workflow a push to that branch starts can't have
// started runs: sync doesn't ask GitHub and doesn't wait.
func TestSyncDoesNotWaitWhenThePushStartsNoWorkflow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.CIAppearWindow, h.eng.CIAppearPoll = 10*time.Second, time.Second
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{
		".github/workflows/rel.yml": strings.Replace(pushWorkflow, "[main]", "[release]", 1)})
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	start := h.clock.Now()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if h.forge.PushRunsCalls != 0 || !h.clock.Now().Equal(start) {
		t.Fatalf("PushRuns calls = %d, waited %s", h.forge.PushRunsCalls, h.clock.Now().Sub(start))
	}
}

// A workflow whose every job carries the audit's push guard skips push runs
// in the shadow repo, so its push starts nothing: sync doesn't look.
func TestSyncDoesNotWaitWhenEveryJobCarriesThePushGuard(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.CIAppearWindow, h.eng.CIAppearPoll = 10*time.Second, time.Second
	guarded := strings.Replace(pushWorkflow, "    runs-on: x", "    if: github.event_name != 'push' || github.repository == 'acme/widget'\n    runs-on: x", 1)
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{".github/workflows/push.yml": guarded})
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	start := h.clock.Now()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if h.forge.PushRunsCalls != 0 || !h.clock.Now().Equal(start) {
		t.Fatalf("PushRuns calls = %d, waited %s", h.forge.PushRunsCalls, h.clock.Now().Sub(start))
	}
}

func seedRuns(h *harness, sha string, runs ...forge.Run) {
	if h.forge.Runs == nil {
		h.forge.Runs = map[string][]fake.Run{}
	}
	for _, r := range runs {
		h.forge.Runs[shadowName] = append(h.forge.Runs[shadowName], fake.Run{SHA: sha, Run: r})
	}
}

func wfRun(id int64, workflow, status string) forge.Run {
	return forge.Run{ID: id, Workflow: workflow, Status: status, URL: "https://github.example/runs/" + workflow}
}

func TestSyncCancelsTheWorkflowRunsItsPushStarts(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"), wfRun(2, "lint", "in_progress"), wfRun(3, "old", "completed"),
		wfRun(4, "deploy", "waiting"), wfRun(5, "x", "requested"), wfRun(6, "y", "pending"))
	seedRuns(h, "0000000000000000000000000000000000000000", wfRun(9, "other", "queued"))
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 {
		t.Fatalf("%v %+v", err, warns)
	}
	var ids []int64
	for _, c := range res.CancelledRuns {
		ids = append(ids, c.ID)
	}
	if !reflect.DeepEqual(ids, []int64{1, 2, 4, 5, 6}) || !reflect.DeepEqual(h.forge.Cancelled[shadowName], ids) {
		t.Fatalf("cancelled = %+v, forge cancelled %v", res.CancelledRuns, h.forge.Cancelled)
	}
	if c := res.CancelledRuns[0]; c.Workflow != "ci" || c.URL != "https://github.example/runs/ci" {
		t.Fatalf("cancelled run = %+v", c)
	}
	// Runs were there at the first look; one more look found nothing new.
	if h.forge.PushRunsCalls != 2 {
		t.Fatalf("PushRuns calls = %d, want 2", h.forge.PushRunsCalls)
	}
	last := h.ledger()[len(h.ledger())-1]
	if last.Op != "sync" || !strings.Contains(last.Summary, "cancelled 5 workflow run(s)") {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestSyncLeavesFinishedRunsAlone(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "completed"))
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || res.CancelledRuns == nil || len(res.CancelledRuns) != 0 || len(h.forge.Cancelled) != 0 {
		t.Fatalf("%+v %+v %v cancelled %v", res, warns, err, h.forge.Cancelled)
	}
}

func TestSyncWaitsOnlyTheWindowForRunsToAppear(t *testing.T) {
	t.Parallel()
	h, _ := pushRunsHarness(t)
	start := h.clock.Now()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if waited := h.clock.Now().Sub(start); waited < 10*time.Second || waited > 12*time.Second {
		t.Fatalf("waited %s, want the 10s window", waited)
	}
	if h.forge.PushRunsCalls != 11 {
		t.Fatalf("PushRuns calls = %d, want 11", h.forge.PushRunsCalls)
	}
}

func TestSyncWarnsWhenTheTokenCannotListRuns(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	h.forge.RunsForbidden = map[string]bool{shadowName: true}
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("sync must still succeed: %+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled || !strings.Contains(warns[0].Message, "may have started workflow runs") ||
		!strings.Contains(warns[0].Message, "Actions write") {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestSyncWarnsWhenTheTokenCannotCancelRuns(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"), wfRun(2, "lint", "in_progress"), wfRun(3, "old", "completed"))
	h.forge.CancelForbidden = map[string]bool{shadowName: true}
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled ||
		!strings.Contains(warns[0].Message, "the push started 2 workflow run(s) in the shadow repo, and the token can't cancel them") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// Once a cancel is forbidden every other one would be too: sync stops
// looking, for this commit and the rest, and counts the runs it saw.
func TestSyncStopsLookingOnceACancelIsForbidden(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	h.setConfig("branches: [main]", "branches: [main, release]")
	seedRuns(h, next, wfRun(1, "ci", "queued"), wfRun(2, "lint", "in_progress"))
	other := testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{
		".github/workflows/push.yml": strings.Replace(pushWorkflow, "[main]", "[main, release]", 1)})
	seedRuns(h, other, wfRun(3, "ci", "queued"))
	h.forge.CancelForbidden = map[string]bool{shadowName: true}
	start := h.clock.Now()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if h.forge.PushRunsCalls != 1 || len(h.forge.CancelRunRepos) != 1 || !h.clock.Now().Equal(start) {
		t.Fatalf("PushRuns calls = %d, CancelRun calls = %d, waited %s", h.forge.PushRunsCalls, len(h.forge.CancelRunRepos), h.clock.Now().Sub(start))
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled ||
		!strings.Contains(warns[0].Message, "the push started 2 workflow run(s) in the shadow repo, and the token can't cancel them") {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestSyncWarnsOnOtherCancelErrors(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"), wfRun(2, "lint", "queued"))
	h.forge.CancelErr = errors.New("actions are down")
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled || !strings.Contains(warns[0].Message, "actions are down") {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestSyncWarnsWhenListingRunsFails(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	h.forge.RunsErr = errors.New("actions are down")
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.ShadowMainAfter != next {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled || !strings.Contains(warns[0].Message, "actions are down") {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestSyncNoCancelDryRunAndNoopNeverLookForRuns(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	h.eng.DryRun = true
	res, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	h.eng.DryRun = false
	if err != nil || res.CancelledRuns == nil || len(res.CancelledRuns) != 0 {
		t.Fatalf("dry run: %+v %v", res, err)
	}
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{NoCancel: true})
	if err != nil || len(warns) != 0 || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("--no-cancel: %+v %+v %v", res, warns, err)
	}
	if _, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{}); err != nil { // nothing to push
		t.Fatal(err)
	}
	if h.forge.PushRunsCalls != 0 || len(h.forge.Cancelled) != 0 {
		t.Fatalf("PushRuns calls = %d, cancelled %v", h.forge.PushRunsCalls, h.forge.Cancelled)
	}
}

// syncOK runs Sync, fails the test on an error, and checks the result
// against the schema.
func (h *harness) syncOK(o engine.SyncOptions) (*engine.SyncResult, []contract.Warning) {
	h.t.Helper()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, o)
	if err != nil {
		h.t.Fatalf("sync: %v", err)
	}
	validateResult(h.t, "sync.schema.json", res)
	return res, warns
}

// closeEntries is the ledger's feature close entries.
func (h *harness) closeEntries() []ledger.Entry {
	h.t.Helper()
	var out []ledger.Entry
	for _, ent := range h.ledger() {
		if ent.Op == "feature close" {
			out = append(out, ent)
		}
	}
	return out
}

func TestSyncClosesMergedFeatures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.publish("feat-y") // its PR stays open
	gateX, gateY := h.gatePR("feat-x"), h.gatePR("feat-y")
	wantX := h.featureRefs("feat-x", id)
	keepY := h.shadowRefs()["refs/heads/feat-y"]
	planPath := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	h.eng.Approver = nil // sync never asks

	res, warns := h.syncOK(engine.SyncOptions{})
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" || res.Closed[0].PublicPR != pr ||
		!reflect.DeepEqual(res.Closed[0].Deleted, wantX) {
		t.Fatalf("closed = %+v, want feat-x (PR #%d) deleting %v", res.Closed, pr, wantX)
	}
	if res.ActiveFeatures != 1 || res.Attention == nil || len(res.Attention) != 0 {
		t.Fatalf("active = %d, attention = %+v", res.ActiveFeatures, res.Attention)
	}
	after := h.shadowRefs()
	for ref := range wantX {
		if _, ok := after[ref]; ok {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
	if after["refs/heads/feat-y"] != keepY || h.gatePR("feat-y") != gateY {
		t.Fatal("sync touched the open feature")
	}
	if gateX == 0 || h.gatePR("feat-x") != 0 {
		t.Fatalf("the merged feature's gate PR #%d is still open", gateX)
	}
	if h.mirrorHas("refs/shadow-meta/features/feat-x") || !h.mirrorHas("refs/shadow-meta/features/feat-y") {
		t.Fatal("the mirror's metadata refs are wrong")
	}
	if _, err := os.Stat(planPath); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the plan file is still there: %v", err)
	}
	closes := h.closeEntries()
	if len(closes) != 1 || closes[0].Feature != "feat-x" || closes[0].Actor != h.eng.Actor || closes[0].ExitCode != 0 ||
		!reflect.DeepEqual(closes[0].Refs, wantX) || closes[0].Approval != nil {
		t.Fatalf("feature close entries = %+v", closes)
	}
	raw, _ := json.Marshal(closes[0])
	validateSchema(t, "ledger-entry.schema.json", raw)
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "sync" || !strings.Contains(last.Summary, "closed 1 merged feature(s)") {
		t.Fatalf("sync ledger = %+v", last)
	}
	if rs, _ := h.survey(); len(rs) != 1 || rs[0].Name != "feat-y" {
		t.Fatalf("the survey finds %+v", rs)
	}
}

// Review Focus 1: merged, then one more commit. Never deleted unattended.
func TestSyncLeavesAMovedMergedFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	testutil.Commit(t, h.shadowGit(), "feat-x", "after the merge", map[string]string{"later.txt": "l\n"})
	before := h.shadowRefs()
	gate := h.gatePR("feat-x")

	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || res.ActiveFeatures != 1 {
		t.Fatalf("closed = %+v, active = %d", res.Closed, res.ActiveFeatures)
	}
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-x" || res.Attention[0].State != feature.StateMergedMoved {
		t.Fatalf("attention = %+v", res.Attention)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.gatePR("feat-x") != gate || len(h.closeEntries()) != 0 {
		t.Fatal("sync deleted something of a moved feature")
	}
}

// A closed PR and an abandoned feature: attention only for the first, and
// the abandoned one isn't active.
func TestSyncAttentionAndActiveFeatures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-c")
	h.forge.ClosePR(publicName, pr)
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	if _, _, err := h.eng.FeatureAbandon(h.ctx, h.pair, "feat-a", false); err != nil {
		t.Fatal(err)
	}
	h.startFeature("feat-d", map[string]string{"d.txt": "d\n"})
	before := h.shadowRefs()
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || len(res.Attention) != 1 || res.Attention[0].Name != "feat-c" || res.Attention[0].State != feature.StatePRClosed {
		t.Fatalf("closed = %+v, attention = %+v", res.Closed, res.Attention)
	}
	if res.ActiveFeatures != 2 { // feat-c and feat-d
		t.Fatalf("active = %d, want 2", res.ActiveFeatures)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("sync deleted something")
	}
}

func TestSyncNoClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	before := h.shadowRefs()
	gate := h.gatePR("feat-x")
	res, warns := h.syncOK(engine.SyncOptions{NoClose: true})
	if len(warns) != 0 || res.Closed == nil || len(res.Closed) != 0 || len(res.Attention) != 0 || res.ActiveFeatures != 0 {
		t.Fatalf("--no-close: %+v %+v", res, warns)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.gatePR("feat-x") != gate || len(h.closeEntries()) != 0 {
		t.Fatal("--no-close deleted something")
	}
	raw, _ := json.Marshal(res)
	if !strings.Contains(string(raw), `"closed":[]`) || !strings.Contains(string(raw), `"attention":[]`) {
		t.Fatalf("closed and attention must be [] in JSON: %s", raw)
	}
}

func TestSyncPlanPreviewsCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	before := h.shadowRefs()
	want := h.featureRefs("feat-x", id)
	gate := h.gatePR("feat-x")
	entries := len(h.ledger())
	planPath := filepath.Join(h.eng.WS.PlansDir(), id+".json")

	h.eng.DryRun = true
	res, _ := h.syncOK(engine.SyncOptions{})
	h.eng.DryRun = false
	if res.Preview == nil || len(res.Closed) != 0 {
		t.Fatalf("sync --plan = %+v", res)
	}
	deletes := map[string]string{}
	closesPR := false
	for _, op := range res.Preview.Ops {
		switch op["op"] {
		case "delete_ref":
			ref, _ := op["ref"].(string)
			sha, _ := op["sha"].(string)
			deletes[ref] = sha
		case "close_pr":
			closesPR = fmt.Sprint(op["number"]) == fmt.Sprint(gate)
		}
	}
	if !reflect.DeepEqual(deletes, want) || !closesPR {
		t.Fatalf("preview ops = %+v, want deletes of %v and a close of gate PR #%d", res.Preview.Ops, want, gate)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || h.gatePR("feat-x") != gate || len(h.ledger()) != entries {
		t.Fatal("sync --plan changed something")
	}
	if _, err := os.Stat(planPath); err != nil || !h.mirrorHas("refs/shadow-meta/features/feat-x") {
		t.Fatalf("sync --plan cleaned up this machine: %v", err)
	}
}

// A commit races onto the branch between the survey and the delete: the
// close fails with FEATURE_MOVED, nothing is deleted, and sync still
// succeeds, with a FEATURE_NOT_CLOSED warning.
func TestSyncCloseFailureIsAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	before := h.featureRefs("feat-x", id)
	gate := h.gatePR("feat-x")
	next := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "1\n"})
	record := h.eng.OnPush
	raced := ""
	h.eng.OnPush = func(op, remote string, specs []string) {
		if op == "sync" && raced == "" && len(specs) > 0 && strings.HasPrefix(specs[0], ":") {
			raced = testutil.Commit(t, h.shadowGit(), "feat-x", "racing commit", map[string]string{"race.txt": "r\n"})
		}
		record(op, remote, specs)
	}

	res, warns := h.syncOK(engine.SyncOptions{})
	if raced == "" {
		t.Fatal("the race never ran")
	}
	if res.ShadowMainAfter != next || len(res.Closed) != 0 {
		t.Fatalf("sync = %+v", res)
	}
	if len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed ||
		!strings.Contains(warns[0].Message, "feat-x") || !strings.Contains(warns[0].Message, string(contract.FeatureMoved)) ||
		warns[0].Details["feature"] != "feat-x" || warns[0].Details["error_code"] != string(contract.FeatureMoved) {
		t.Fatalf("warnings = %+v", warns)
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
	if closes := h.closeEntries(); len(closes) != 1 || closes[0].ErrorCode != string(contract.FeatureMoved) || len(closes[0].Refs) != 0 {
		t.Fatalf("feature close entries = %+v", closes)
	}
}

// An unreadable plan file (even another pair's) fails the close with
// INTERNAL before anything is deleted: a warning, never a failed sync.
func TestSyncUnreadablePlanFileIsAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	// other-pair.json links to a directory: listed as no directory, but
	// reading it fails. Windows makes a junction, which needs no privilege.
	link, target := filepath.Join(h.eng.WS.PlansDir(), "other-pair.json"), t.TempDir()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
			t.Skipf("can't link a directory here: %v %s", err, out)
		}
	}
	if _, err := os.ReadFile(link); err == nil {
		t.Skip("the link reads as a file here")
	}
	before := h.shadowRefs()
	res, warns := h.syncOK(engine.SyncOptions{})
	if res.Closed == nil || len(res.Closed) != 0 {
		t.Fatalf("closed = %+v", res.Closed)
	}
	if len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed || warns[0].Details["error_code"] != string(contract.Internal) {
		t.Fatalf("warnings = %+v", warns)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("sync deleted something")
	}
}

// The deletes went through but the gate PR didn't close: a warning, and the
// ledger still lists every ref deleted.
func TestSyncRecordsTheDeletesWhenTheGatePRWontClose(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	h.forge.ClosePRErr = errors.New("boom")
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed || !strings.Contains(warns[0].Message, string(contract.ForgeError)) ||
		!reflect.DeepEqual(warns[0].Details["deleted"], want) {
		t.Fatalf("warnings = %+v", warns)
	}
	// Its refs are gone, so it is closed; the warning keeps the PR in view.
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" || res.Closed[0].PublicPR != pr || !reflect.DeepEqual(res.Closed[0].Deleted, want) {
		t.Fatalf("closed = %+v", res.Closed)
	}
	after := h.shadowRefs()
	for ref := range want {
		if _, ok := after[ref]; ok {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
	if closes := h.closeEntries(); len(closes) != 1 || closes[0].ErrorCode != string(contract.ForgeError) || !reflect.DeepEqual(closes[0].Refs, want) {
		t.Fatalf("feature close entries = %+v", closes)
	}
}

// corruptMeta points name's metadata ref in the shadow repo at a commit
// whose feature.json doesn't parse, so every survey of the pair fails.
func (h *harness) corruptMeta(name string) {
	h.t.Helper()
	sh := h.shadowGit()
	sha := testutil.Commit(h.t, sh, "tmp-corrupt", "corrupt", map[string]string{feature.MetaFile: "not json\n"})
	if _, err := sh.Run(h.ctx, "update-ref", "refs/ghostbranch/features/"+name, sha); err != nil {
		h.t.Fatal(err)
	}
	if err := sh.DeleteRef(h.ctx, "refs/heads/tmp-corrupt"); err != nil {
		h.t.Fatal(err)
	}
}

// A survey that fails never fails the sync: it closes nothing and says so
// in a FEATURE_NOT_CLOSED warning with no feature. --no-close never surveys.
func TestSyncSurveyFailureIsAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.corruptMeta("feat-bad")
	next := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "1\n"})
	before := h.shadowRefs()
	delete(before, "refs/heads/main")

	res, warns := h.syncOK(engine.SyncOptions{})
	if res.ShadowMainAfter != next || len(res.Closed) != 0 || len(res.Attention) != 0 || res.ActiveFeatures != 0 {
		t.Fatalf("sync = %+v", res)
	}
	if len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed ||
		!strings.HasPrefix(warns[0].Message, "couldn't survey features, so nothing was closed: INTERNAL: ") ||
		warns[0].Details["error_code"] != string(contract.Internal) || warns[0].Details["feature"] != nil {
		t.Fatalf("warnings = %+v", warns)
	}
	after := h.shadowRefs()
	delete(after, "refs/heads/main")
	if !reflect.DeepEqual(after, before) || len(h.closeEntries()) != 0 {
		t.Fatal("sync deleted something without a survey")
	}

	_, warns = h.syncOK(engine.SyncOptions{NoClose: true})
	if len(warns) != 0 {
		t.Fatalf("--no-close surveyed: %+v", warns)
	}
}

// A failed survey doesn't mask a diverged branch: still SYNC_DIVERGED.
func TestSyncSurveyFailureKeepsSyncDiverged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.corruptMeta("feat-bad")
	testutil.Commit(t, h.shadowGit(), "main", "rogue", map[string]string{"rogue.txt": "x\n"})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	_ = wantCode(t, err, contract.SyncDiverged)
	if res == nil || res.Action != "diverged" || len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed {
		t.Fatalf("sync = %+v %+v", res, warns)
	}
}

// Main diverged, and a merged feature: sync still closes the feature, then
// fails with SYNC_DIVERGED, whose message is only about the branch.
func TestSyncDivergedStillCloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	testutil.Commit(t, h.shadowGit(), "main", "rogue", map[string]string{"rogue.txt": "x\n"})
	res, _, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	ce := wantCode(t, err, contract.SyncDiverged)
	if !strings.Contains(ce.Message, "nothing was pushed to it") {
		t.Fatalf("message = %q", ce.Message)
	}
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" || !reflect.DeepEqual(res.Closed[0].Deleted, want) {
		t.Fatalf("closed = %+v", res.Closed)
	}
}

// A published feature whose branch was deleted: closed by sync once its PR
// merged, left in attention while the PR is open.
func TestSyncBranchGoneFeatures(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sh := h.shadowGit()
	mergedPR, _ := h.publish("feat-merged")
	h.forge.SetPRMerged(publicName, mergedPR)
	h.publish("feat-open")
	for _, b := range []string{"feat-merged", "feat-open"} {
		if err := sh.DeleteRef(h.ctx, "refs/heads/"+b); err != nil {
			t.Fatal(err)
		}
	}
	keep := h.shadowRefs()["refs/ghostbranch/features/feat-open"]
	h.eng.Approver = nil

	res, warns := h.syncOK(engine.SyncOptions{})
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-merged" || res.Closed[0].PublicPR != mergedPR {
		t.Fatalf("closed = %+v", res.Closed)
	}
	if _, ok := res.Closed[0].Deleted["refs/heads/feat-merged"]; ok {
		t.Fatalf("deleted a branch that was already gone: %+v", res.Closed[0].Deleted)
	}
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-open" || res.Attention[0].State != feature.StateBranchGone {
		t.Fatalf("attention = %+v", res.Attention)
	}
	after := h.shadowRefs()
	if _, ok := after["refs/ghostbranch/features/feat-merged"]; ok {
		t.Fatal("the merged feature's metadata is still there")
	}
	if after["refs/ghostbranch/features/feat-open"] != keep {
		t.Fatal("sync touched the feature whose PR is open")
	}
}
