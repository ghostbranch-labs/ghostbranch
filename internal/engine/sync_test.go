package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
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
	up2 := testutil.Commit(t, h.pubGit, "main", "upstream 2", map[string]string{"up.txt": "2\n"})
	res, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.Action != "fast_forward" || res.Preview == nil {
		t.Fatalf("dry ff: %+v %v", res, err)
	}
	if res.Branches[0].From != newMain || res.Branches[0].To != up2 || res.ShadowMainAfter != up2 {
		t.Fatalf("a dry run reports the target: %+v, shadow_main_after %s", res.Branches[0], res.ShadowMainAfter)
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
	// Runs were there at the first look; one more look found nothing new,
	// and a third saw every cancel had taken.
	if h.forge.PushRunsCalls != 3 {
		t.Fatalf("PushRuns calls = %d, want 3", h.forge.PushRunsCalls)
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
		!strings.Contains(warns[0].Message, "Actions write") || !reflect.DeepEqual(warns[0].Details["runs"], []map[string]any{}) {
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
	want := []map[string]any{{"id": int64(1), "url": "https://github.example/runs/ci"}, {"id": int64(2), "url": "https://github.example/runs/lint"}}
	if !reflect.DeepEqual(warns[0].Details["runs"], want) {
		t.Fatalf("details.runs = %+v", warns[0].Details["runs"])
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

// A cancel while sync waits to look for runs again leaves runs it may
// never see: it still warns.
func TestSyncInterruptedWhileLookingForRunsWarns(t *testing.T) {
	t.Parallel()
	h, _ := pushRunsHarness(t)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	sleep := h.eng.Sleep
	h.eng.Sleep = func(c context.Context, d time.Duration) error {
		if h.forge.PushRunsCalls > 0 { // the poll between looks
			cancel()
		}
		return sleep(c, d)
	}
	_, warns, err := h.eng.Sync(ctx, h.pair, engine.SyncOptions{})
	if err != nil {
		t.Fatalf("an interrupt while looking for runs is a warning, not an error: %v", err)
	}
	if h.forge.PushRunsCalls != 1 {
		t.Fatalf("PushRuns calls = %d, want 1", h.forge.PushRunsCalls)
	}
	for _, w := range warns {
		if w.Code == contract.CINotCancelled && strings.Contains(w.Message, "could not cancel") {
			return
		}
	}
	t.Fatalf("want CI_NOT_CANCELLED, got %+v", warns)
}

// seedRun seeds one run for sha with the fake's behaviour flags.
func seedRun(h *harness, r fake.Run) {
	if h.forge.Runs == nil {
		h.forge.Runs = map[string][]fake.Run{}
	}
	h.forge.Runs[shadowName] = append(h.forge.Runs[shadowName], r)
}

// cancelledIDs is the IDs in res.CancelledRuns.
func cancelledIDs(res *engine.SyncResult) []int64 {
	ids := []int64{}
	for _, c := range res.CancelledRuns {
		ids = append(ids, c.ID)
	}
	return ids
}

func TestSyncCancelHonoured(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRuns(h, next, wfRun(1, "ci", "queued"))
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || !reflect.DeepEqual(cancelledIDs(res), []int64{1}) {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if len(h.forge.ForceCancelRepos) != 0 {
		t.Fatalf("force-cancel calls %v", h.forge.ForceCancelRepos)
	}
}

// A run that ignores the cancel is force-cancelled, and then listed.
func TestSyncCancelIgnoredThenForced(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || !reflect.DeepEqual(cancelledIDs(res), []int64{1}) {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if !reflect.DeepEqual(h.forge.ForceCancelled[shadowName], []int64{1}) {
		t.Fatalf("force-cancelled %v", h.forge.ForceCancelled)
	}
}

// A run that ignores both is not listed, and the warning names it.
func TestSyncCancelIgnoredTwice(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(7, "ci", "in_progress"), IgnoreCancel: true, IgnoreForceCancel: true})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled || !strings.Contains(warns[0].Message, "7 (https://github.example/runs/ci)") ||
		!strings.Contains(warns[0].Message, "kept running after sync cancelled and force-cancelled them") ||
		!reflect.DeepEqual(warns[0].Details["runs"], []map[string]any{{"id": int64(7), "url": "https://github.example/runs/ci"}}) {
		t.Fatalf("warnings = %+v", warns)
	}
}

// A 409 for a run that has finished meanwhile: nothing to do.
func TestSyncCancelConflictOnACompletedRun(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	// Listed in progress at the first look, finished by the time the cancel lands.
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), Conflict: true, FinishAfterPolls: 1})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if len(h.forge.CancelRunRepos) != 1 || len(h.forge.ForceCancelRepos) != 0 {
		t.Fatalf("CancelRun calls %v, force-cancel calls %v", h.forge.CancelRunRepos, h.forge.ForceCancelRepos)
	}
}

// A 409 for a run that is still active goes on to the force-cancel.
func TestSyncCancelConflictOnAnActiveRun(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), Conflict: true})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || !reflect.DeepEqual(cancelledIDs(res), []int64{1}) {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if !reflect.DeepEqual(h.forge.ForceCancelled[shadowName], []int64{1}) {
		t.Fatalf("force-cancelled %v", h.forge.ForceCancelled)
	}
}

func TestSyncForceCancelForbidden(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	h.forge.ForceCancelForbidden = map[string]bool{shadowName: true}
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled || !strings.Contains(warns[0].Message, "1 (https://github.example/runs/ci)") ||
		!strings.Contains(warns[0].Message, "give it Actions write") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// A run that ignores the cancel but finishes on its own before the
// force-cancel is left alone.
func TestSyncCancelRunFinishedOnItsOwn(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	// The two looks for runs see it active; the first check sees it finished.
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true, FinishAfterPolls: 2})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if len(h.forge.ForceCancelRepos) != 0 {
		t.Fatalf("force-cancel calls %v", h.forge.ForceCancelRepos)
	}
}

// A run that ignores the force-cancel too but finishes on its own in the
// force window is left alone.
func TestSyncForceCancelledRunFinishedOnItsOwn(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	// Two looks for runs, 30 checks in the 30s verify window, then the
	// first check after the force-cancel still sees it going; the next sees
	// it finished.
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true, IgnoreForceCancel: true, FinishAfterPolls: 33})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if len(h.forge.ForceCancelRepos) != 1 || h.forge.PushRunsCalls != 34 {
		t.Fatalf("force-cancel calls %v, PushRuns calls %d", h.forge.ForceCancelRepos, h.forge.PushRunsCalls)
	}
}

// A force-cancel that fails for another reason names the error.
func TestSyncForceCancelFailure(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	h.forge.ForceCancelErr = errors.New("actions are down")
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(res.CancelledRuns) != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(warns) != 1 || warns[0].Code != contract.CINotCancelled ||
		!strings.Contains(warns[0].Message, "sync could not force-cancel them (actions are down)") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// Ctrl-C while sync checks its cancels: a warning, and the result.
func TestSyncInterruptedWhileVerifying(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	sleep := h.eng.Sleep
	h.eng.Sleep = func(c context.Context, d time.Duration) error {
		if h.forge.PushRunsCalls >= 3 { // two looks for runs, then the first check
			cancel()
		}
		return sleep(c, d)
	}
	res, warns, err := h.eng.Sync(ctx, h.pair, engine.SyncOptions{})
	if err != nil {
		t.Fatalf("an interrupt while checking cancels is a warning, not an error: %v", err)
	}
	if res == nil || res.ShadowMainAfter != next || len(res.CancelledRuns) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if h.forge.PushRunsCalls != 3 || len(h.forge.ForceCancelRepos) != 0 {
		t.Fatalf("PushRuns calls = %d, force-cancel calls %v", h.forge.PushRunsCalls, h.forge.ForceCancelRepos)
	}
	for _, w := range warns {
		if w.Code == contract.CINotCancelled && strings.Contains(w.Message, "interrupted before it could confirm") &&
			strings.Contains(w.Message, "1 (https://github.example/runs/ci)") {
			return
		}
	}
	t.Fatalf("want CI_NOT_CANCELLED, got %+v", warns)
}

// releasePush adds release to sync.branches and a public release commit
// whose push starts the push workflow, so a sync pushes two commits that
// each start runs. It returns the release commit.
func releasePush(t *testing.T, h *harness) string {
	t.Helper()
	h.setConfig("branches: [main]", "branches: [main, release]")
	return testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{
		".github/workflows/push.yml": strings.Replace(pushWorkflow, "[main]", "[main, release]", 1)})
}

// Sync cancels the runs of every pushed commit before it checks any of
// them, then checks them all in one window: one 30s window and one
// force-cancel round, not one per commit.
func TestSyncCancelsEveryCommitsRunsBeforeCheckingAny(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	other := releasePush(t, h)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	seedRun(h, fake.Run{SHA: other, Run: wfRun(2, "lint", "in_progress"), IgnoreCancel: true})
	cancelsAtFirstCheck := -1
	sleep := h.eng.Sleep
	h.eng.Sleep = func(c context.Context, d time.Duration) error {
		// Two looks per commit; the sleep after the fourth is the first check's.
		if h.forge.PushRunsCalls == 4 && cancelsAtFirstCheck < 0 {
			cancelsAtFirstCheck = len(h.forge.CancelRunRepos)
		}
		return sleep(c, d)
	}
	start := h.clock.Now()
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 0 || !reflect.DeepEqual(cancelledIDs(res), []int64{1, 2}) {
		t.Fatalf("%+v %+v %v", res, warns, err)
	}
	if cancelsAtFirstCheck != 2 {
		t.Fatalf("%d cancel(s) sent before the first check, want both commits' (2)", cancelsAtFirstCheck)
	}
	if !reflect.DeepEqual(h.forge.ForceCancelled[shadowName], []int64{1, 2}) || len(h.forge.ForceCancelRepos) != 2 {
		t.Fatalf("force-cancelled %v, calls %v", h.forge.ForceCancelled, h.forge.ForceCancelRepos)
	}
	// 1s of looking per commit, the shared 30s window, one check after the force-cancels.
	if waited := h.clock.Now().Sub(start); waited != 33*time.Second {
		t.Fatalf("waited %s, want 33s", waited)
	}
}

// A forbidden force-cancel stops the force-cancels for every commit: one
// call, and one warning naming both runs.
func TestSyncForceCancelForbiddenStopsForEveryCommit(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	other := releasePush(t, h)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	seedRun(h, fake.Run{SHA: other, Run: wfRun(2, "lint", "in_progress"), IgnoreCancel: true})
	h.forge.ForceCancelForbidden = map[string]bool{shadowName: true}
	_, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(h.forge.ForceCancelRepos) != 1 {
		t.Fatalf("%v; force-cancel calls %v", err, h.forge.ForceCancelRepos)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Message, "1 (https://github.example/runs/ci)") ||
		!strings.Contains(warns[0].Message, "2 (https://github.example/runs/lint)") || !strings.Contains(warns[0].Message, "give it Actions write") ||
		len(warns[0].Details["runs"].([]map[string]any)) != 2 {
		t.Fatalf("warnings = %+v", warns)
	}
}

// Each stuck run is warned about with its own force-cancel error, never
// another run's.
func TestSyncForceCancelErrorsAreNamedPerRun(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true, ForceCancelErr: errors.New("boom")})
	seedRun(h, fake.Run{SHA: next, Run: wfRun(2, "lint", "in_progress"), IgnoreCancel: true, IgnoreForceCancel: true})
	_, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(warns) != 2 {
		t.Fatalf("%v %+v", err, warns)
	}
	byRun := map[string]string{}
	for _, w := range warns {
		for _, r := range w.Details["runs"].([]map[string]any) {
			byRun[fmt.Sprint(r["id"])] = w.Message
		}
	}
	if !strings.Contains(byRun["1"], "could not force-cancel them (boom)") || strings.Contains(byRun["1"], "2 (") {
		t.Fatalf("run 1: %q", byRun["1"])
	}
	if !strings.Contains(byRun["2"], "kept running after sync cancelled and force-cancelled them") || strings.Contains(byRun["2"], "boom") {
		t.Fatalf("run 2: %q", byRun["2"])
	}
}

// Ctrl-C during the force window: a run whose force-cancel failed keeps
// that reason, and only the run still unconfirmed is "interrupted".
func TestSyncInterruptKeepsEachStuckRunsReason(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true, ForceCancelErr: errors.New("boom")})
	seedRun(h, fake.Run{SHA: next, Run: wfRun(2, "lint", "in_progress"), IgnoreCancel: true, IgnoreForceCancel: true})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	sleep := h.eng.Sleep
	h.eng.Sleep = func(c context.Context, d time.Duration) error {
		if len(h.forge.ForceCancelRepos) == 2 { // after the force-cancel round
			cancel()
		}
		return sleep(c, d)
	}
	_, warns, err := h.eng.Sync(ctx, h.pair, engine.SyncOptions{})
	if err != nil {
		t.Fatalf("an interrupt while checking cancels is a warning, not an error: %v", err)
	}
	var boom, interrupted bool
	for _, w := range warns {
		if w.Code != contract.CINotCancelled {
			continue
		}
		boom = boom || strings.Contains(w.Message, "1 (") && strings.Contains(w.Message, "(boom)") && !strings.Contains(w.Message, "interrupted")
		interrupted = interrupted || strings.Contains(w.Message, "2 (") && strings.Contains(w.Message, "interrupted before it could confirm") && !strings.Contains(w.Message, "1 (")
	}
	if !boom || !interrupted {
		t.Fatalf("warnings = %+v", warns)
	}
}

// An interrupt while checking names every commit's runs once, and adds no
// "may have started" warning: every pushed commit was looked at.
func TestSyncInterruptedWhileVerifyingNamesEveryCommitsRuns(t *testing.T) {
	t.Parallel()
	h, next := pushRunsHarness(t)
	other := releasePush(t, h)
	seedRun(h, fake.Run{SHA: next, Run: wfRun(1, "ci", "in_progress"), IgnoreCancel: true})
	seedRun(h, fake.Run{SHA: other, Run: wfRun(2, "lint", "in_progress"), IgnoreCancel: true})
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	sleep := h.eng.Sleep
	h.eng.Sleep = func(c context.Context, d time.Duration) error {
		if h.forge.PushRunsCalls >= 4 { // both commits looked at: the first check's wait
			cancel()
		}
		return sleep(c, d)
	}
	_, warns, err := h.eng.Sync(ctx, h.pair, engine.SyncOptions{})
	if err != nil || len(h.forge.CancelRunRepos) != 2 {
		t.Fatalf("%v; CancelRun calls %v", err, h.forge.CancelRunRepos)
	}
	// closeMerged's survey also fails under the cancelled context (a
	// FEATURE_NOT_CLOSED warning): count only the cancel warnings.
	var ci []contract.Warning
	for _, w := range warns {
		if w.Code == contract.CINotCancelled {
			ci = append(ci, w)
		}
	}
	if len(ci) != 1 || !strings.Contains(ci[0].Message, "interrupted before it could confirm") ||
		!strings.Contains(ci[0].Message, "1 (") || !strings.Contains(ci[0].Message, "2 (") {
		t.Fatalf("warnings = %+v", warns)
	}
}

// One branch's push failing doesn't stop the others; each failure is a
// SYNC_PUSH_FAILED warning, and sync's error is the first failure's.
func TestSyncPushFailureOnOneBranchStillPushesTheOthers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("branches: [main]", "branches: [main, release]")
	next := testutil.Commit(t, h.pubGit, "main", "next", map[string]string{"n.txt": "1\n"})
	rel := testutil.Commit(t, h.pubGit, "release", "rel", map[string]string{"r.txt": "1\n"})
	moved := ""
	onPush := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		onPush(op, remote, specs)
		if op == "sync" && moved == "" && slices.Contains(specs, next+":refs/heads/main") {
			// Someone else moves shadow main first: the leased push is refused.
			moved = testutil.Commit(t, h.shadowGit(), "main", "elsewhere", map[string]string{"e.txt": "1\n"})
		}
	}
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	_ = wantCode(t, err, contract.RefMoved)
	got := map[string]engine.SyncBranch{}
	for _, b := range res.Branches {
		got[b.Name] = b
	}
	if m := got["main"]; m.Status != "push_failed" || m.To != m.From || res.Action != "push_failed" || res.ShadowMainAfter != m.From {
		t.Fatalf("main = %+v, action %s", m, res.Action)
	}
	if r := got["release"]; r.Status != "created" || r.To != rel {
		t.Fatalf("release = %+v", r)
	}
	if sha, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/release"); sha != rel {
		t.Fatal("release was not pushed after main's push failed")
	}
	if codes(warns)[contract.SyncPushFailed] != 1 {
		t.Fatalf("warnings = %+v", warns)
	}
	for _, w := range warns {
		if w.Code == contract.SyncPushFailed && (w.Details["branch"] != "main" || w.Details["code"] != string(contract.RefMoved)) {
			t.Fatalf("details = %+v", w.Details)
		}
	}
	validateResult(t, "sync.schema.json", res)
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
	if !res.Surveyed || res.ActiveFeatures != 1 || res.Attention == nil || len(res.Attention) != 0 {
		t.Fatalf("surveyed = %v, active = %d, attention = %+v", res.Surveyed, res.ActiveFeatures, res.Attention)
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
	testutil.ValidateSchema(t, "ledger-entry.schema.json", raw)
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
	if len(warns) != 0 || res.Surveyed || res.Closed == nil || len(res.Closed) != 0 || len(res.Attention) != 0 || res.ActiveFeatures != 0 {
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

// sync --plan warns about a merged feature whose close can't even be
// previewed, as the real sync would, instead of dropping it silently.
func TestSyncPlanWarnsWhenACloseCantBePreviewed(t *testing.T) {
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
	// The metadata names another branch, at feat-x's published head so
	// feat-x still reads as merged: closeTargets refuses it.
	if err := sh.UpdateRef(h.ctx, "refs/heads/feat-2", head); err != nil {
		t.Fatal(err)
	}
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Branch = "feat-2" })
	before := h.shadowRefs()
	entries := len(h.ledger())
	h.eng.DryRun = true
	res, warns := h.syncOK(engine.SyncOptions{})
	h.eng.DryRun = false
	if res.Preview == nil || len(res.Closed) != 0 {
		t.Fatalf("sync --plan = %+v", res)
	}
	for _, op := range res.Preview.Ops {
		if op["op"] == "delete_ref" || op["op"] == "close_pr" {
			t.Fatalf("sync --plan previews a close it can't make: %+v", op)
		}
	}
	if len(warns) != 1 || warns[0].Code != contract.FeatureNotClosed || warns[0].Details["feature"] != "feat-x" ||
		warns[0].Details["error_code"] != string(contract.Internal) {
		t.Fatalf("warnings = %+v", warns)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || len(h.ledger()) != entries {
		t.Fatal("sync --plan changed something")
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

// An unreadable plan file (even another pair's) is skipped with a warning:
// the merged feature still closes, every ref of it.
func TestSyncSkipsAnUnreadablePlanFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	link := unreadablePlanFile(t, h)
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" || !reflect.DeepEqual(res.Closed[0].Deleted, want) {
		t.Fatalf("closed = %+v", res.Closed)
	}
	if len(warns) != 1 || warns[0].Code != contract.PlanFileUnreadable || warns[0].Details["path"] != link {
		t.Fatalf("warnings = %+v", warns)
	}
	after := h.shadowRefs()
	for ref := range want {
		if _, ok := after[ref]; ok {
			t.Errorf("%s is still in the shadow repo", ref)
		}
	}
}

// The ledger can't take sync's entries: the close still stands, and each
// entry becomes a warning; the close's carries its deleted refs.
func TestALedgerFailureAfterSyncClosesIsAWarning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	ledgerPath := h.eng.WS.LedgerPath()
	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledgerPath, 0o755); err != nil { // appending to a folder fails
		t.Fatal(err)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || !reflect.DeepEqual(res.Closed[0].Deleted, want) {
		t.Fatalf("closed = %+v", res.Closed)
	}
	ws := h.eng.LedgerWarnings()
	var closeW, syncW *contract.Warning
	for i := range ws {
		if ws[i].Code != contract.LedgerNotWritten {
			t.Fatalf("ledger warnings = %+v", ws)
		}
		switch ws[i].Details["op"] {
		case "feature close":
			closeW = &ws[i]
		case "sync":
			syncW = &ws[i]
		}
	}
	if len(ws) != 2 || closeW == nil || syncW == nil {
		t.Fatalf("ledger warnings = %+v", ws)
	}
	if closeW.Details["feature"] != "feat-x" || !reflect.DeepEqual(closeW.Details["refs"], want) {
		t.Fatalf("the close's warning = %+v", *closeW)
	}
	if _, ok := syncW.Details["refs"]; ok {
		t.Fatalf("the sync's warning has refs: %+v", *syncW)
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
// whose feature.json doesn't parse: the survey reports it unreadable.
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

// A corrupt record doesn't stop sync's closes: the merged feature closes,
// and the unreadable one waits for a person. --no-close never surveys.
func TestSyncClosesBesideAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, id := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	want := h.featureRefs("feat-x", id)
	h.corruptMeta("feat-bad")
	res, warns := h.syncOK(engine.SyncOptions{})
	if !res.Surveyed || res.ActiveFeatures != 1 || len(res.Closed) != 1 || res.Closed[0].Name != "feat-x" ||
		!reflect.DeepEqual(res.Closed[0].Deleted, want) {
		t.Fatalf("sync = %+v", res)
	}
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-bad" || res.Attention[0].State != feature.StateUnreadable ||
		res.Attention[0].Error == "" {
		t.Fatalf("attention = %+v", res.Attention)
	}
	if len(warns) != 0 {
		t.Fatalf("warnings = %+v", warns)
	}
	if _, ok := h.shadowRefs()["refs/ghostbranch/features/feat-bad"]; !ok {
		t.Fatal("sync deleted the unreadable record")
	}
	res, warns = h.syncOK(engine.SyncOptions{NoClose: true})
	if res.Surveyed || len(warns) != 0 {
		t.Fatalf("--no-close surveyed: %+v %+v", res, warns)
	}
}

// An unreadable record doesn't mask a diverged branch: still SYNC_DIVERGED,
// and the record is in attention.
func TestAnUnreadableRecordKeepsSyncDiverged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.corruptMeta("feat-bad")
	testutil.Commit(t, h.shadowGit(), "main", "rogue", map[string]string{"rogue.txt": "x\n"})
	res, warns, err := h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	_ = wantCode(t, err, contract.SyncDiverged)
	if res == nil || res.Action != "diverged" || len(warns) != 0 {
		t.Fatalf("sync = %+v %+v", res, warns)
	}
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-bad" || res.Attention[0].State != feature.StateUnreadable {
		t.Fatalf("attention = %+v", res.Attention)
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

// A merged feature whose branch is gone closes unattended only when its
// gate (if any) is for what was published: a gate for a newer head, or a
// gate branch moved off the recorded candidate, may hold commits never
// published, so a person decides.
func TestSyncLeavesABranchGoneFeatureWithANewerGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sh := h.shadowGit()
	prX, _ := h.publish("feat-x")
	prY, _ := h.publish("feat-y")
	h.forge.SetPRMerged(publicName, prX)
	h.forge.SetPRMerged(publicName, prY)
	// feat-x gets one more commit and is gated again: its gate is now for
	// a head that was never published.
	testutil.Commit(t, sh, "feat-x", "after the merge", map[string]string{"later.txt": "l\n"})
	h.gateFeature("feat-x")
	for _, b := range []string{"feat-x", "feat-y"} {
		if err := sh.DeleteRef(h.ctx, "refs/heads/"+b); err != nil {
			t.Fatal(err)
		}
	}
	if r := h.report("feat-x"); r.State != feature.StateBranchGone || r.PRState != "merged" || engine.ClosableWithoutConsole(r) {
		t.Fatalf("gated past the publish: %+v closable %v", r, engine.ClosableWithoutConsole(r))
	}
	// feat-y's gate is for what was published, but only while its gate
	// branch is at the recorded candidate.
	gateRef := "refs/heads/ghostbranch-gate/feat-y"
	candidate := h.shadowRefs()[gateRef]
	main := h.shadowRefs()["refs/heads/main"]
	if err := sh.UpdateRef(h.ctx, gateRef, main); err != nil {
		t.Fatal(err)
	}
	if r := h.report("feat-y"); r.State != feature.StateBranchGone || engine.ClosableWithoutConsole(r) {
		t.Fatalf("gate branch moved: %+v closable %v", r, engine.ClosableWithoutConsole(r))
	}
	if err := sh.UpdateRef(h.ctx, gateRef, candidate); err != nil {
		t.Fatal(err)
	}
	keepX := h.featureRefs("feat-x", "")
	h.eng.Approver = nil

	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Name != "feat-y" || res.Closed[0].PublicPR != prY {
		t.Fatalf("closed = %+v, want only feat-y", res.Closed)
	}
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-x" || res.Attention[0].State != feature.StateBranchGone {
		t.Fatalf("attention = %+v", res.Attention)
	}
	after := h.shadowRefs()
	for ref, sha := range keepX {
		if after[ref] != sha {
			t.Errorf("sync deleted or moved %s of the feature gated past its publish", ref)
		}
	}
	for _, ent := range h.closeEntries() {
		if ent.Feature == "feat-x" {
			t.Fatalf("a feature close of feat-x was recorded: %+v", ent)
		}
	}
}

// Review Focus 3: a record from a newer Ghostbranch is never closed
// unattended, even published, merged and unmoved; status still lists it.
func TestSyncNeverClosesANewerRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.newerMeta("feat-x")
	before := h.shadowRefs()
	h.eng.Approver = nil

	res, warns := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || warning(warns, contract.RecordNewer) == nil {
		t.Fatalf("closed = %+v, warnings = %+v", res.Closed, warns)
	}
	// Sync's output points at it: attention lists it, and RECORD_NEWER says why.
	if len(res.Attention) != 1 || res.Attention[0].Name != "feat-x" || res.Attention[0].State != feature.StateMerged {
		t.Fatalf("attention = %+v", res.Attention)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || len(h.closeEntries()) != 0 {
		t.Fatal("sync deleted something of a newer record")
	}
	st, _ := h.status(contract.RecordNewer)
	if len(st.Features) != 1 || st.Features[0].Name != "feat-x" || st.Features[0].State != feature.StateMerged {
		t.Fatalf("status features = %+v", st.Features)
	}
}

// A newer record is never closed unattended by the branch_gone rule either.
func TestSyncNeverClosesANewerBranchGoneRecord(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.newerMeta("feat-x")
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	before := h.shadowRefs()
	h.eng.Approver = nil
	res, warns := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 0 || warning(warns, contract.RecordNewer) == nil {
		t.Fatalf("closed = %+v, warnings = %+v", res.Closed, warns)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) || len(h.closeEntries()) != 0 {
		t.Fatal("sync deleted something of a newer record")
	}
}

// A record from before 0.6 doesn't say which head was published, so a
// gate can't be tied to the publish: merged with its branch gone, it
// still waits for a person.
func TestABranchGoneFeatureWithAnOldRecordNeedsAPerson(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	pr, _ := h.publish("feat-x")
	h.forge.SetPRMerged(publicName, pr)
	h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Promoted.FeatureHead = "" })
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	r := h.report("feat-x")
	if r.State != feature.StateBranchGone || r.PRState != "merged" || r.Gate == nil || engine.ClosableWithoutConsole(r) {
		t.Fatalf("report = %+v, closable %v", r, engine.ClosableWithoutConsole(r))
	}
}
