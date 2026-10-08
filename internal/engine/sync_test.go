package engine_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
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
