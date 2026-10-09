package engine_test

import (
	"errors"
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
	validateSchema(t, "feature.schema.json", raw)
	unlock, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now())
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
		unlock, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now())
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
			if _, err := workspace.Lock(h.eng.WS.LockPath(h.pair), h.clock.Now()); err != nil {
				t.Errorf("take the lock: %v", err)
			} // and never release it
		}
		return passing(r, s)
	}
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	ce := wantCode(t, err, contract.Locked)
	if res.Job == nil || ce.Details["job"] != *res.Job || !h.meta("feat-x").Gate.Pending() {
		t.Fatalf("want LOCKED with the job and a pending record: %+v %+v", ce.Details, h.meta("feat-x").Gate)
	}
}
