package engine_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// meta reads a feature's metadata from the shadow repo.
func (h *harness) meta(name string) feature.Meta {
	h.t.Helper()
	raw, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/"+name, feature.MetaFile)
	if err != nil || !ok {
		h.t.Fatalf("read %s metadata: %v", name, err)
	}
	m, err := feature.Decode(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}

// editRemoteMeta commits edit's change to a feature's metadata straight
// into the shadow repo, as another machine would.
func (h *harness) editRemoteMeta(name string, edit func(*feature.Meta)) {
	h.t.Helper()
	sg, ref := h.shadowGit(), "refs/ghostbranch/features/"+name
	old, err := sg.MustResolve(h.ctx, ref)
	if err != nil {
		h.t.Fatal(err)
	}
	m := h.meta(name)
	edit(&m)
	blob, err := sg.HashObject(h.ctx, feature.Encode(m))
	if err != nil {
		h.t.Fatal(err)
	}
	tree, err := sg.MakeTree(h.ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", SHA: blob, Path: feature.MetaFile}})
	if err != nil {
		h.t.Fatal(err)
	}
	c, err := sg.CommitTree(h.ctx, tree, []string{old}, "another machine", testutil.Author(2), testutil.Author(2))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := sg.UpdateRef(h.ctx, ref, c); err != nil {
		h.t.Fatal(err)
	}
}

func passing(string, string) []forge.Check {
	return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "success"}}
}

func TestParseJob(t *testing.T) {
	t.Parallel()
	ref, err := engine.ParseJob("gate/team/feat-x/0123456789ab")
	if err != nil || ref.Feature != "team/feat-x" || ref.Candidate != "0123456789ab" {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, bad := range []string{"", "gate/", "gate/x", "gate/x/0123", "gate/x/0123456789AB", "test/x/0123456789ab", "gate/../0123456789ab"} {
		if _, err := engine.ParseJob(bad); codeOf(err) != contract.BadFlag {
			t.Errorf("%q: want BAD_FLAG, got %v", bad, err)
		}
	}
}

func TestGateDetachedRecordsAPendingJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	listed := false
	h.forge.Checks = func(string, string) []forge.Check { listed = true; return nil }
	res, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPending || res.Job == nil || *res.Job != "gate/feat-x/"+res.Candidate[:12] || res.ShadowPR == nil {
		t.Fatalf("detached gate = %+v %v", res, err)
	}
	if listed {
		t.Fatal("a detached gate waited on CI")
	}
	validateResult(t, "gate.schema.json", res)
	g := h.meta("feat-x").Gate
	if !g.Pending() || g.Candidate != res.Candidate || g.ShadowPR != *res.ShadowPR || !g.StartedAt.Equal(h.clock.Now().UTC().Truncate(time.Second)) || !g.At.Equal(g.StartedAt) {
		t.Fatalf("pending record = %+v", g)
	}
	// Nothing validated a pending record before jobs: check it against the schema.
	raw, _, err := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	if err != nil {
		t.Fatal(err)
	}
	testutil.ValidateSchema(t, "feature.schema.json", raw)
	unlock, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now(), "test")
	if err != nil {
		t.Fatalf("the pair lock is still held: %v", err)
	}
	unlock()
}

func TestTheLockIsFreeWhileTheGateWaitsOnCI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	lockErr := errors.New("checks were never listed")
	h.forge.Checks = func(r, s string) []forge.Check {
		unlock, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now(), "test")
		if lockErr = err; err == nil {
			unlock()
		}
		return passing(r, s)
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPassed || res.Job == nil {
		t.Fatalf("gate = %+v %v", res, err)
	}
	if lockErr != nil {
		t.Fatalf("the pair lock was held during the CI wait: %v", lockErr)
	}
	if g := h.meta("feat-x").Gate; !g.Passed() || g.ShadowPR != *res.ShadowPR || g.StartedAt.IsZero() {
		t.Fatalf("final record = %+v", g)
	}
}

// Review Focus 2.
func TestFinishKeepsAResultAnotherWaiterRecorded(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	h.forge.Checks = func(string, string) []forge.Check {
		// While this gate waits, another waiter finishes the same job as passed.
		if g := h.meta("feat-x").Gate; g.Pending() {
			h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Gate.Checks = feature.ChecksPassed })
		}
		return []forge.Check{{Name: "lint", Status: "completed", Conclusion: "failure"}}
	}
	metaPushes := 0
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if strings.HasSuffix(s, ":refs/ghostbranch/features/feat-x") {
				metaPushes++
			}
		}
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil || res.Checks != feature.ChecksPassed {
		t.Fatalf("want the recorded passed result, got %+v %v", res, err)
	}
	if metaPushes != 1 { // the pending record only
		t.Fatalf("want 1 metadata push, got %d", metaPushes)
	}
}

// Review Focus 3, at the finish.
func TestFinishIsStaleWhenTheRecordWasReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	const newer = "1111111111111111111111111111111111111111"
	h.forge.Checks = func(r, s string) []forge.Check {
		if g := h.meta("feat-x").Gate; g.Candidate != newer {
			h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Gate.Candidate = newer }) // a newer gate, still pending
		}
		return passing(r, s)
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.JobStale)
	if ce.Details["current_job"] != "gate/feat-x/"+newer[:12] {
		t.Fatalf("details = %+v", ce.Details)
	}
	if g := h.meta("feat-x").Gate; g.Candidate != newer || !g.Pending() {
		t.Fatalf("the newer record was overwritten: %+v", g)
	}
}

// An older binary that rewrote the pending record dropped its shadow_pr:
// that record is no job, so the finish records nothing over it and says
// to gate again.
func TestFinishIsJobNotFoundWhenAnOlderBinaryDroppedTheShadowPR(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	var edited string
	h.forge.Checks = func(r, s string) []forge.Check {
		if g := h.meta("feat-x").Gate; g.Pending() {
			h.editRemoteMeta("feat-x", func(m *feature.Meta) { m.Gate.ShadowPR = 0 })
			edited = h.shadowRefs()["refs/ghostbranch/features/feat-x"]
		}
		return passing(r, s)
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.JobNotFound)
	if ce.Details["feature"] != "feat-x" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if edited == "" || h.shadowRefs()["refs/ghostbranch/features/feat-x"] != edited {
		t.Fatal("the finish wrote over the record")
	}
	if g := h.meta("feat-x").Gate; g.Checks != feature.ChecksPending || g.ShadowPR != 0 {
		t.Fatalf("record = %+v", g)
	}
}

// Review Focus 5, the second half.
func TestFinishGivesUpOnABusyLock(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	taken := false
	h.forge.Checks = func(r, s string) []forge.Check {
		if !taken {
			taken = true
			drop, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now(), "test")
			if err != nil {
				t.Errorf("take the lock: %v", err)
			} else {
				t.Cleanup(drop) // held past the gate; the handle must close before TempDir's cleanup
			}
		}
		return passing(r, s)
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.Locked)
	if res.Job == nil || ce.Details["job"] != *res.Job || !h.meta("feat-x").Gate.Pending() {
		t.Fatalf("want LOCKED with the job and a pending record: %+v %+v", ce.Details, h.meta("feat-x").Gate)
	}
}

// The final write is leased on the commit just read: when another machine
// moved the metadata, the finish re-reads once and records over a record
// still pending, or keeps one another waiter already finished.
func TestFinishRetriesOnceWhenTheMetadataMoved(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		edit   func(*feature.Meta)
		pushes int
	}{
		{"unchanged", func(*feature.Meta) {}, 3},
		{"finished elsewhere", func(m *feature.Meta) { m.Gate.Checks, m.Gate.FailedChecks = feature.ChecksFailed, []string{"other"} }, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			h.forge.Checks = passing
			metaPushes := 0
			var edited *feature.Gate
			h.eng.OnPush = func(_, _ string, specs []string) {
				for _, s := range specs {
					if strings.HasSuffix(s, ":refs/ghostbranch/features/feat-x") {
						if metaPushes++; metaPushes == 2 { // the final record: move the metadata under it
							h.editRemoteMeta("feat-x", c.edit)
							edited = h.meta("feat-x").Gate
						}
					}
				}
			}
			res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
			if metaPushes != c.pushes {
				t.Fatalf("want %d metadata pushes, got %d", c.pushes, metaPushes)
			}
			g := h.meta("feat-x").Gate
			if c.pushes == 3 {
				if err != nil || res.Checks != feature.ChecksPassed || !g.Passed() {
					t.Fatalf("want passed recorded: %+v %v %+v", res, err, g)
				}
				return
			}
			ce := wantCode(t, err, contract.ChecksFailed)
			if !reflect.DeepEqual(ce.Details["failed"], []string{"other"}) || !reflect.DeepEqual(g, edited) {
				t.Fatalf("want the other waiter's record kept: %+v, record %+v (was %+v)", ce.Details, g, edited)
			}
		})
	}
}

// Promote holds the pair lock from its gate to the publish, so nothing
// moves under it while CI runs: both when it runs the gate itself and when
// it waits on a matching detached job.
func TestPromoteKeepsTheLockThroughItsGate(t *testing.T) {
	t.Parallel()
	for _, detachFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "its own gate", true: "a matching job"}[detachFirst], func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
			opt := engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true}
			if detachFirst {
				d := opt
				d.Detach = true
				if _, _, err := h.eng.Promote(h.ctx, h.pair, d); err != nil {
					t.Fatal(err)
				}
			}
			lockErr := errors.New("checks were never listed")
			h.forge.Checks = func(r, s string) []forge.Check {
				unlock, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now(), "test")
				if lockErr = err; err == nil {
					unlock()
				}
				return passing(r, s)
			}
			res, _, err := h.eng.Promote(h.ctx, h.pair, opt)
			if err != nil || res.Plan == nil {
				t.Fatalf("promote = %+v %v", res, err)
			}
			if codeOf(lockErr) != contract.Locked {
				t.Fatalf("the pair lock was free during promote's CI wait: %v", lockErr)
			}
		})
	}
}

// A feature closed while its gate waited on CI has no record to finish:
// FEATURE_NOT_FOUND, and the metadata ref is not re-created.
func TestFinishOnAClosedFeatureIsFeatureNotFound(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	const ref = "refs/ghostbranch/features/feat-x"
	deleted := false
	h.forge.Checks = func(r, s string) []forge.Check {
		if !deleted { // feature close, from another machine
			deleted = true
			if err := h.shadowGit().DeleteRef(h.ctx, ref); err != nil {
				t.Errorf("delete the metadata: %v", err)
			}
		}
		return passing(r, s)
	}
	_, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.FeatureNotFound)
	if _, ok, err := h.shadowGit().Resolve(h.ctx, ref); ok || err != nil {
		t.Fatalf("the metadata ref came back (%v)", err)
	}
}
