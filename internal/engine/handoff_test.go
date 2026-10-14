package engine_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// chooser is a fake console that also answers the Archive/Keep choice:
// pick, or the choice's default when pick is -1.
type chooser struct {
	approver
	pick  int
	asked []approval.Choice
}

func (c *chooser) ConfirmChoosing(ctx context.Context, summary string, ch approval.Choice) (int, error) {
	c.asked = append(c.asked, ch)
	if err := c.Confirm(ctx, summary); err != nil {
		return ch.Default, err
	}
	if c.pick < 0 {
		return ch.Default, nil
	}
	return c.pick, nil
}

// defaultsAvailable fails t when a choice's default isn't an available
// option: Archive when available, otherwise Keep (R2).
func defaultsAvailable(t *testing.T, asked []approval.Choice) {
	t.Helper()
	for _, ch := range asked {
		want := 0
		if ch.Options[0].Unavailable != "" {
			want = 1
		}
		if ch.Default != want || ch.Options[ch.Default].Unavailable != "" {
			t.Fatalf("the default %d is not the first available option: %+v", ch.Default, ch)
		}
	}
}

// planned is apply_test.go's (P2): it gates feat-x and saves a squash plan,
// and returns the plan ID. Don't declare it again here.

func (h *harness) applyPlan(o engine.ApplyOptions) (*engine.ApplyResult, []contract.Warning) {
	h.t.Helper()
	res, warns, err := h.eng.Apply(h.ctx, o)
	if err != nil {
		h.t.Fatalf("apply: %v", err)
	}
	validateResult(h.t, "apply.schema.json", res)
	return res, warns
}

func TestApplyArchivesWhenThePersonKeepsTheDefault(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	c := &chooser{pick: -1}
	h.eng.Approver = c
	head := h.head("feat-x")
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	if !reflect.DeepEqual(res.AfterPublish, map[string]string{"feat-x": engine.AfterArchived}) ||
		!reflect.DeepEqual(res.Archived, []string{"feat-x"}) || res.PublicPR == 0 {
		t.Fatalf("apply = %+v", res)
	}
	if len(c.asked) != 1 || c.asked[0].Title != "After publishing, this feature:" || c.asked[0].Default != 0 ||
		c.asked[0].Options[0].Name != "Archive" || c.asked[0].Options[1].Name != "Keep" {
		t.Fatalf("choice = %+v", c.asked)
	}
	defaultsAvailable(t, c.asked)
	if h.shadowRefs()["refs/ghostbranch/archive/feat-x"] != head || h.meta("feat-x").Archived == nil {
		t.Fatal("not archived")
	}
	ents := h.ledger()
	last := ents[len(ents)-1]
	if last.Op != "apply" || last.Refs["refs/ghostbranch/archive/feat-x"] != head || !strings.Contains(last.Summary, "archived feat-x") {
		t.Fatalf("ledger = %+v", last)
	}
}

func TestApplyKeepsWhenThePersonPicksKeep(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	h.eng.Approver = &chooser{pick: 1}
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	if res.AfterPublish["feat-x"] != engine.AfterKept || len(res.Archived) != 0 || h.meta("feat-x").Archived != nil {
		t.Fatalf("apply = %+v", res)
	}
	if _, ok := h.shadowRefs()["refs/heads/feat-x"]; !ok {
		t.Fatal("Keep must leave the branch")
	}
}

// Without Archive (the engine's zero value, R3) nothing is asked and
// nothing is archived: today's behaviour.
func TestApplyWithoutArchiveIsToday(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	c := &chooser{pick: 0}
	h.eng.Approver = c
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id})
	if len(c.asked) != 0 || res.AfterPublish["feat-x"] != engine.AfterKept {
		t.Fatalf("asked %v, apply = %+v", c.asked, res)
	}
}

// Under a grant with --yes, no console: Archive applies without a
// question (R4).
func TestApplyUnderAGrantArchivesWithoutAsking(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	h.grant(time.Hour)
	c := &chooser{pick: 1}
	h.eng.Approver = c
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id, Yes: true, Archive: true})
	if len(c.asked) != 0 || len(c.approver.asked) != 0 || res.AfterPublish["feat-x"] != engine.AfterArchived {
		t.Fatalf("asked %v, apply = %+v", c.asked, res)
	}
}

// A private child greys Archive out, and Keep is the default (R8).
func TestApplyUnderAPrivateChildKeeps(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	h.startOn("feat-y", "feat-x", map[string]string{"y.txt": "y\n"})
	c := &chooser{pick: -1}
	h.eng.Approver = c
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	if len(c.asked) != 1 || c.asked[0].Default != 1 ||
		c.asked[0].Options[0].Unavailable != "feat-y is built on this feature; publish or restack it first" ||
		res.AfterPublish["feat-x"] != engine.AfterKept {
		t.Fatalf("asked %+v, apply = %+v", c.asked, res)
	}
	defaultsAvailable(t, c.asked)
}

// A stack gets one choice, and every level is archived.
func TestApplyOfAStackArchivesEveryLevel(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "feat-a", "feat-b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"feat-a", "feat-b"}, Mode: "squash", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	c := &chooser{pick: -1}
	h.eng.Approver = c
	ar, _ := h.applyPlan(engine.ApplyOptions{Plan: res.Plan.ID, Archive: true})
	want := map[string]string{"feat-a": engine.AfterArchived, "feat-b": engine.AfterArchived}
	if len(c.asked) != 1 || c.asked[0].Title != "After publishing, these features:" || !reflect.DeepEqual(ar.AfterPublish, want) ||
		!reflect.DeepEqual(ar.Archived, []string{"feat-a", "feat-b"}) {
		t.Fatalf("asked %+v, apply = %+v", c.asked, ar)
	}
	defaultsAvailable(t, c.asked)
}

// A stack hands off top first and stops at the first failure (P5): the top
// level's lease fails, so it is incomplete, and the level below stays kept
// with its branch untouched. No live child is ever left on an archived
// parent (R8).
func TestApplyOfAStackStopsAtTheFirstFailedArchive(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "feat-a", "feat-b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"feat-a", "feat-b"}, Mode: "squash", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h.eng.Approver = &chooser{pick: -1}
	sg := h.shadowGit()
	inner := h.eng.OnPush
	moved := ""
	h.eng.OnPush = func(op, remote string, specs []string) {
		inner(op, remote, specs)
		if op == "apply" && remote == "shadow" && moved == "" && strings.Contains(strings.Join(specs, " "), "refs/ghostbranch/archive/feat-b") {
			moved = testutil.Commit(t, sg, "feat-b", "pushed from another machine", map[string]string{"other.txt": "o\n"})
		}
	}
	ar, warns := h.applyPlan(engine.ApplyOptions{Plan: res.Plan.ID, Archive: true})
	want := map[string]string{"feat-a": engine.AfterKept, "feat-b": engine.AfterIncomplete}
	if !reflect.DeepEqual(ar.AfterPublish, want) || len(ar.Archived) != 0 || len(warns) == 0 {
		t.Fatalf("apply = %+v %v", ar, warns)
	}
	refs := h.shadowRefs()
	if _, ok := refs["refs/heads/feat-a"]; !ok || refs["refs/heads/feat-b"] != moved {
		t.Fatalf("both branches must survive: %v", refs)
	}
	for _, ref := range []string{"refs/ghostbranch/archive/feat-a", "refs/ghostbranch/archive/feat-b"} {
		if _, ok := refs[ref]; ok {
			t.Errorf("%s exists: nothing may be archived below a failed level", ref)
		}
	}
}

// promote without --plan hands off the same way.
func TestPromoteArchives(t *testing.T) {
	t.Parallel()
	h := gated(t)
	h.eng.Approver = &chooser{pick: -1}
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature", Archive: true})
	if err != nil || res.Applied == nil || res.Applied.AfterPublish["feat-x"] != engine.AfterArchived {
		t.Fatalf("promote = %+v %v", res, err)
	}
	validateResult(t, "promote.schema.json", res)
}

// A gate PR that won't close: the publish stands, ARCHIVE_INCOMPLETE says
// where it stopped, and feature archive finishes it.
func TestApplyArchiveIncomplete(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	h.eng.Approver = &chooser{pick: -1}
	if h.gatePR("feat-x") == 0 {
		t.Fatal("the gate opens a gate PR") // gate.go always opens a draft gate PR (P15)
	}
	h.forge.ClosePRErr = errors.New("connection reset")
	res, warns := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	if res.PublicPR == 0 || res.AfterPublish["feat-x"] != engine.AfterIncomplete {
		t.Fatalf("apply = %+v", res)
	}
	var got *contract.Warning
	for i := range warns {
		if warns[i].Code == contract.ArchiveIncomplete {
			got = &warns[i]
		}
	}
	if got == nil || got.Details["feature"] != "feat-x" || got.Details["step"] != "gate_pr" || got.Details["code"] != "FORGE_ERROR" {
		t.Fatalf("warnings = %+v", warns)
	}
	h.forge.ClosePRErr = nil
	if r, _, err := h.eng.FeatureArchive(h.ctx, h.pair, "feat-x"); err != nil || !r.Changed {
		t.Fatalf("finish = %+v %v", r, err)
	}
}

// The branch moves between the publish and the archive (Review Focus 2):
// the lease fails, the publish stands, nothing is archived or lost.
func TestApplyArchiveLosesItsLease(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	h.eng.Approver = &chooser{pick: -1}
	sg := h.shadowGit()
	inner := h.eng.OnPush
	moved := ""
	h.eng.OnPush = func(op, remote string, specs []string) {
		inner(op, remote, specs)
		if op == "apply" && remote == "shadow" && moved == "" && strings.Contains(strings.Join(specs, " "), "refs/ghostbranch/archive/") {
			moved = testutil.Commit(t, sg, "feat-x", "pushed from another machine", map[string]string{"other.txt": "o\n"})
		}
	}
	res, warns := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	if res.PublicPR == 0 || res.AfterPublish["feat-x"] != engine.AfterIncomplete || len(warns) == 0 {
		t.Fatalf("apply = %+v %v", res, warns)
	}
	if h.head("feat-x") != moved {
		t.Fatal("the moved branch must survive")
	}
	if _, ok := h.shadowRefs()["refs/ghostbranch/archive/feat-x"]; ok {
		t.Fatal("an atomic push that lost its lease archives nothing")
	}
}

// --dry-run previews the archive and changes nothing (R16).
func TestApplyDryRunPreviewsTheArchive(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	before := h.shadowRefs()
	h.eng.DryRun = true
	res, _ := h.applyPlan(engine.ApplyOptions{Plan: id, Archive: true})
	var ops []string
	for _, op := range res.Preview.Ops {
		ops = append(ops, op["op"].(string)+" "+opRef(op))
	}
	if res.AfterPublish["feat-x"] != engine.AfterArchived || !contains(ops, "push_ref refs/ghostbranch/archive/feat-x") ||
		!contains(ops, "delete_ref refs/heads/feat-x") {
		t.Fatalf("preview = %v, after = %v", ops, res.AfterPublish)
	}
	if !reflect.DeepEqual(h.shadowRefs(), before) {
		t.Fatal("--dry-run changed the shadow repo")
	}
}
