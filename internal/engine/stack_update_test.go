package engine_test

import (
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// fixBottom commits a review fix on a, restacks the branches above it, and
// gates the stack from the bottom: what a person does before promote a.
func fixBottom(h *harness, names ...string) {
	h.t.Helper()
	testutil.Commit(h.t, h.shadowGit(), names[0], "review fix on "+names[0], map[string]string{names[0] + ".txt": names[0] + " fixed\n"})
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: names[0]}); err != nil {
		h.t.Fatalf("restack %s: %v", names[0], err)
	}
	for _, n := range names {
		h.gateFeature(n)
	}
}

// publicPushes are the public branches pushed since push index from, in order.
func (h *harness) publicPushes(from int) []string {
	var out []string
	for _, p := range h.pushes[from:] {
		if p.remote != "public" {
			continue
		}
		for _, s := range p.refspecs {
			_, ref, _ := strings.Cut(s, ":")
			out = append(out, strings.TrimPrefix(ref, "refs/heads/"))
		}
	}
	return out
}

func (h *harness) pubAncestor(a, b string) bool {
	h.t.Helper()
	ok, err := h.pubGit.IsAncestor(h.ctx, a, b)
	if err != nil {
		h.t.Fatal(err)
	}
	return ok
}

// preserveStack gates names as a stack and publishes it in preserve mode
// (one commit each) under a grant, as publishedStack does in squash.
func preserveStack(t *testing.T, names ...string) (*harness, []plan.StackPR) {
	t.Helper()
	h := gatedStack(t, names...)
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: names, Mode: "preserve", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote %v = %+v %v", names, res, err)
	}
	h.eng.Approver = nil
	return h, res.Applied.PRs
}

// Review Focus 5: promote a, with open PRs above it, plans a, b and c
// together under one approval, and pushes them bottom up, each leased on the
// head the plan saw. Every level is a replace: a because open PRs sit above
// it, b and c as they move onto the new commits below. The PRs stay the
// same PRs.
func TestStackUpdateTogether(t *testing.T) {
	t.Parallel()
	h, prs := preserveStack(t, "a", "b", "c")
	old := map[string]string{"a": h.pubTip("a"), "b": h.pubTip("b"), "c": h.pubTip("c")}
	fixBottom(h, "a", "b", "c")
	h.eng.Approver = h.approver
	from := len(h.pushes)
	res, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve"})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a = %+v %v", res, err)
	}
	if got := chainNamesOf(res.Plan); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("plan chain = %v", got)
	}
	for i, en := range res.Plan.Chain {
		p := en.Publish
		if !p.Update || p.Style != "replace" || p.ReplaceReason != "restack" || p.ExpectedHead != old[en.Feature] {
			t.Fatalf("%s's publish = update %v style %q reason %q expected %s", en.Feature, p.Update, p.Style, p.ReplaceReason, p.ExpectedHead)
		}
		if res.Plan.PRs[i].Number != prs[i].Number || !res.Plan.PRs[i].Update || res.Plan.PRs[i].Style != "replace" {
			t.Fatalf("plan PR %d = %+v", i, res.Plan.PRs[i])
		}
	}
	if len(h.approver.asked) != 1 {
		t.Fatalf("approvals asked = %d, want one for the whole stack", len(h.approver.asked))
	}
	sum := h.approver.asked[0]
	for _, pr := range prs {
		if want := "Replace PR #" + itoa(pr.Number) + "'s branch (history changed: restack)"; !strings.Contains(sum, want) {
			t.Fatalf("the summary lacks %q:\n%s", want, sum)
		}
	}
	if got := h.publicPushes(from); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("public pushes = %v, want a, b, c bottom up", got)
	}
	if n := len(h.forge.PRs(publicName)); n != 3 {
		t.Fatalf("%d public PRs, want the same 3", n)
	}
	for i, n := range []string{"a", "b", "c"} {
		tip := h.pubTip(n)
		if tip != res.Plan.Chain[i].Publish.Commit {
			t.Fatalf("public %s = %s, want the plan's %s", n, tip, res.Plan.Chain[i].Publish.Commit)
		}
		m := h.shadowMeta(n)
		if m.Promoted.PublishedHead != tip || m.Promoted.PlanID != res.Plan.ID || m.Promoted.PublicPR != prs[i].Number {
			t.Fatalf("%s's record = %+v", n, m.Promoted)
		}
	}
	if !h.pubAncestor(h.pubTip("a"), h.pubTip("b")) || !h.pubAncestor(h.pubTip("b"), h.pubTip("c")) {
		t.Fatal("b and c don't sit on the updated branches below them")
	}
}

// Review Focus 5: an apply that stops after a's update resumes from b; a
// stays updated and nothing is pushed twice.
func TestStackUpdateResumes(t *testing.T) {
	t.Parallel()
	h, prs := preserveStack(t, "a", "b", "c")
	oldC := h.pubTip("c")
	fixBottom(h, "a", "b", "c")
	res, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	pl := res.Plan
	record := h.eng.OnPush
	moved := false
	h.eng.OnPush = func(op, remote string, specs []string) {
		record(op, remote, specs)
		for _, s := range specs {
			// b's record push fails once: its lease no longer holds.
			if !moved && remote == "shadow" && strings.HasSuffix(s, ":refs/ghostbranch/features/b") {
				moved = true
				h.editRemoteMeta("b", func(*feature.Meta) {})
			}
		}
	}
	h.grant(time.Hour)
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	_ = wantCode(t, err, contract.RefMoved)
	if h.pubTip("a") != pl.Chain[0].Publish.Commit || h.shadowMeta("a").Promoted.PublishedHead != pl.Chain[0].Publish.Commit {
		t.Fatal("a must stay updated after b's failure")
	}
	if h.pubTip("b") != pl.Chain[1].Publish.Commit || h.shadowMeta("b").Promoted.PlanID == pl.ID {
		t.Fatal("b's push landed but its record must not say so yet")
	}
	if h.pubTip("c") != oldC {
		t.Fatal("c moved before b's record was written")
	}
	from := len(h.pushes)
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if err != nil || !ar.Resumed {
		t.Fatalf("resume = %+v %v", ar, err)
	}
	if got := h.publicPushes(from); !reflect.DeepEqual(got, []string{"c"}) {
		t.Fatalf("the resume pushed %v publicly, want only c", got)
	}
	for i, n := range []string{"a", "b", "c"} {
		if m := h.shadowMeta(n); m.Promoted.PlanID != pl.ID || m.Promoted.PublishedHead != pl.Chain[i].Publish.Commit || m.Promoted.PublicPR != prs[i].Number {
			t.Fatalf("%s's record after the resume = %+v", n, m.Promoted)
		}
	}
	if len(h.forge.PRs(publicName)) != 3 {
		t.Fatal("the resume opened a PR")
	}
}

// Review Focus 2, for a stack: a PR branch that moved after the plan is
// never overwritten, whether it moved before apply started or during it.
func TestStackUpdateLeaseRefusesAMovedBranch(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	oldA := h.pubTip("a")
	fixBottom(h, "a", "b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h.grant(time.Hour)
	theirs := testutil.Commit(t, h.pubGit, "b", "someone's push", map[string]string{"theirs.txt": "x\n"})
	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
	ce := wantCode(t, err, contract.PlanStale)
	if !slices.Contains(stringsOf(ce.Details["moved"]), "public_branch") || !reflect.DeepEqual(stringsOf(ce.Details["features"]), []string{"a", "b"}) ||
		h.pubTip("a") != oldA || h.pubTip("b") != theirs {
		t.Fatalf("details %+v; a %s, b %s", ce.Details, h.pubTip("a"), h.pubTip("b"))
	}

	// Moved during the apply: a's push lands, b's lease fails, b keeps the push.
	h2, _ := preserveStack(t, "a", "b")
	fixBottom(h2, "a", "b")
	res, _, err = promoteStack(h2, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	h2.grant(time.Hour)
	record := h2.eng.OnPush
	var theirs2 string
	h2.eng.OnPush = func(op, remote string, specs []string) {
		record(op, remote, specs)
		if remote == "public" && theirs2 == "" && strings.HasSuffix(specs[0], ":refs/heads/a") {
			theirs2 = testutil.Commit(t, h2.pubGit, "b", "someone's push", map[string]string{"theirs.txt": "x\n"})
		}
	}
	_, _, err = h2.eng.Apply(h2.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
	ce = wantCode(t, err, contract.PlanStale)
	if !slices.Contains(stringsOf(ce.Details["moved"]), "public_branch") {
		t.Fatalf("details = %+v", ce.Details)
	}
	if h2.pubTip("a") != res.Plan.Chain[0].Publish.Commit || h2.pubTip("b") != theirs2 {
		t.Fatalf("a %s, b %s (theirs %s)", h2.pubTip("a"), h2.pubTip("b"), theirs2)
	}
	// Planning again names their commit, and still overwrites nothing.
	_, _, err = promoteStack(h2, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	if ce := wantCode(t, err, contract.PRChangedOnGitHub); ce.Details["feature"] != "b" {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// A branch above that wasn't restacked onto the new commits below it is
// parent_stale; restacked but not gated, it is stale_gate. Nothing is planned.
func TestStackUpdateRefusesWhatIsNotRestackedAndGated(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	testutil.Commit(t, h.shadowGit(), "a", "review fix", map[string]string{"a.txt": "a fixed\n"})
	h.gateFeature("a")
	opt := engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true}
	_, _, err := promoteStack(h, opt)
	ce := wantCode(t, err, contract.NotAStack)
	if ce.Details["reason"] != "parent_stale" || ce.Details["feature"] != "b" || ce.Details["parent"] != "a" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("a")
	_, _, err = promoteStack(h, opt)
	if ce := wantCode(t, err, contract.NotAStack); ce.Details["reason"] != "stale_gate" || ce.Details["feature"] != "b" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if n := len(h.forge.PRs(publicName)); n != 2 {
		t.Fatalf("%d PRs", n)
	}
}

// Review Focus 3, for a stack: a commit someone pushed to a PR above stops
// the plan, naming it; nothing goes public.
func TestStackUpdateRefusesForeignCommitsAbove(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	theirs := testutil.Commit(t, h.pubGit, "b", "a reviewer's suggestion", map[string]string{"b.txt": "b, suggested\n"})
	fixBottom(h, "a", "b")
	h.grant(time.Hour)
	from := len(h.pushes)
	_, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Yes: true})
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	fs, _ := ce.Details["foreign"].([]map[string]any)
	if ce.Details["feature"] != "b" || len(fs) != 1 || fs[0]["sha"] != theirs || fs[0]["subject"] != "a reviewer's suggestion" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if got := h.publicPushes(from); len(got) != 0 || h.pubTip("b") != theirs {
		t.Fatalf("pushed %v; b at %s", got, h.pubTip("b"))
	}
}

// M2: every level's PR is read for foreign commits before any style or
// gate is checked. A foreign commit on b is reported even though a, below
// it, has only its first-publish gate (stale for an update).
func TestStackUpdateReadsEveryPRBeforeTheGates(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	theirs := testutil.Commit(t, h.pubGit, "b", "a reviewer's suggestion", map[string]string{"b.txt": "b, suggested\n"})
	testutil.Commit(t, h.shadowGit(), "a", "review fix", map[string]string{"a.txt": "a fixed\n"})
	_, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	ce := wantCode(t, err, contract.PRChangedOnGitHub)
	if ce.Details["feature"] != "b" || ce.Details["public_head"] != theirs {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// Two open PRs on one branch are two lines of PRs: one plan can't update both.
func TestStackUpdateRefusesTwoOpenLinesAbove(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b")
	h.grant(time.Hour)
	h.startOn("b2", "a", map[string]string{"b2.txt": "b2\n"})
	h.gateFeature("b2")
	if _, _, err := promoteStack(h, engine.PromoteOptions{Feature: "b2", Mode: "preserve", Yes: true}); err != nil {
		t.Fatal(err)
	}
	_, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	ce := wantCode(t, err, contract.NotAStack)
	if ce.Details["reason"] != "open_above" || !reflect.DeepEqual(stringsOf(ce.Details["children"]), []string{"b", "b2"}) {
		t.Fatalf("details = %+v", ce.Details)
	}
}

// An open PR on a branch of the plan that the plan doesn't update (b2,
// beside the listed b) would be left on a's old commits: the plan is
// refused, naming it, and nothing goes public.
func TestStackUpdateRefusesAnOpenSiblingOutsideThePlan(t *testing.T) {
	t.Parallel()
	h, _ := preserveStack(t, "a", "b2")
	h.grant(time.Hour)
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	fixBottom(h, "a", "b2", "b")
	from := len(h.pushes)
	_, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "preserve", Yes: true})
	ce := wantCode(t, err, contract.NotAStack)
	if ce.Details["reason"] != "open_above" || ce.Details["feature"] != "a" || !reflect.DeepEqual(stringsOf(ce.Details["children"]), []string{"b2"}) {
		t.Fatalf("details = %+v", ce.Details)
	}
	if got := h.publicPushes(from); len(got) != 0 {
		t.Fatalf("pushed %v publicly", got)
	}
}

// A PR above whose base on GitHub isn't the branch below it in the plan
// (here retargeted to main) is parent_moved: the plan would push it onto a
// branch its PR doesn't sit on.
func TestStackUpdateRefusesAPRBasedElsewhere(t *testing.T) {
	t.Parallel()
	h, prs := preserveStack(t, "a", "b")
	fixBottom(h, "a", "b")
	h.forge.SetPRBase(publicName, prs[1].Number, "main")
	from := len(h.pushes)
	_, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", PlanOnly: true})
	ce := wantCode(t, err, contract.NotAStack)
	if ce.Details["reason"] != "parent_moved" || ce.Details["feature"] != "b" || ce.Details["parent"] != "a" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if got := h.publicPushes(from); len(got) != 0 {
		t.Fatalf("pushed %v publicly", got)
	}
}

// With an open PR above it, the bottom is replaced too (its gate built a
// restack), and the branch above moves onto its new head.
func TestStackUpdateReplacesTheBottomWithOpenPRsAbove(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	if _, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "preserve", Yes: true}); err != nil {
		t.Fatal(err)
	}
	oldA, oldB := h.pubTip("a"), h.pubTip("b")
	testutil.Commit(t, h.shadowGit(), "a", "a second change", map[string]string{"a2.txt": "a2\n"})
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("a")
	h.gateFeature("b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Title: "Add a", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a = %+v %v", res, err)
	}
	a, b := res.Plan.Chain[0].Publish, res.Plan.Chain[1].Publish
	if !a.Update || a.Style != "replace" || a.ReplaceReason != "restack" || a.ExpectedHead != oldA {
		t.Fatalf("a's publish = %+v", a)
	}
	if !b.Update || b.Style != "replace" || b.ReplaceReason != "restack" || b.ExpectedHead != oldB {
		t.Fatalf("b's publish = %+v", b)
	}
	newA, newB := h.pubTip("a"), h.pubTip("b")
	if newA != a.Commit || newB != b.Commit {
		t.Fatalf("public a %s, b %s; want the plan's %s, %s", newA, newB, a.Commit, b.Commit)
	}
	if h.pubAncestor(oldB, newB) || !h.pubAncestor(newA, newB) {
		t.Fatal("b must be replaced onto a's new head")
	}
	// The typed title belongs to a, the one feature listed: b's PR keeps its own.
	if !res.Plan.Chain[0].Typed.Title || res.Plan.Chain[1].Typed.Title {
		t.Fatalf("typed = %+v, %+v", res.Plan.Chain[0].Typed, res.Plan.Chain[1].Typed)
	}
}

// With no open PR above it (the branch above was never published), the
// bottom appends as its gate built it, and the branch above publishes for
// the first time on the bottom's new head.
func TestStackUpdateAppendsTheBottomUnderAFirstPublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(time.Hour)
	h.startFeature("a", map[string]string{"a.txt": "a\n"})
	h.gateFeature("a")
	pr, err := promote(h, engine.PromoteOptions{Feature: "a", Mode: "preserve", Yes: true})
	if err != nil || pr.Applied == nil {
		t.Fatalf("promote a = %+v %v", pr, err)
	}
	oldA := h.pubTip("a")
	h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	testutil.Commit(t, h.shadowGit(), "a", "a second change", map[string]string{"a2.txt": "a2\n"})
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "a"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("a")
	h.gateFeature("b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "preserve", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a b = %+v %v", res, err)
	}
	a, b := res.Plan.Chain[0].Publish, res.Plan.Chain[1].Publish
	if !a.Update || a.Style != "append" || a.ExpectedHead != oldA || len(a.Commits) != 1 || a.Parent != oldA {
		t.Fatalf("a's publish = %+v", a)
	}
	if b.Update || res.Plan.PRs[1].Base != "a" {
		t.Fatalf("b's publish = %+v, PR %+v", b, res.Plan.PRs[1])
	}
	newA := h.pubTip("a")
	if parent, _ := h.pubGit.MustResolve(h.ctx, newA+"^"); parent != oldA {
		t.Fatalf("a's PR didn't gain exactly one commit on %s: its new head's parent is %s", oldA, parent)
	}
	if parent, _ := h.pubGit.MustResolve(h.ctx, h.pubTip("b")+"^"); parent != newA {
		t.Fatalf("b sits on %s, want a's new head %s", parent, newA)
	}
	if n := len(h.forge.PRs(publicName)); n != 2 {
		t.Fatalf("%d public PRs, want a's and b's new one", n)
	}
}

// The plain chained-PR squash path end to end: after a squash merge of a,
// sync marks b public_pr_needs_update; restack --onto main, gate and
// promote b replace b's PR branch onto main, and the flag clears.
func TestPlainChainedSquashPathEndToEnd(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	h.forge.StacksUnavailable = true
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	prs := res.Applied.PRs
	tree, err := h.pubGit.TreeOf(h.ctx, h.pubTip("a"))
	if err != nil {
		t.Fatal(err)
	}
	squash, err := h.pubGit.CommitTree(h.ctx, tree, []string{h.pubTip("main")}, "Squash of a", testutil.Author(60), testutil.Author(60))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/main", squash); err != nil {
		t.Fatal(err)
	}
	h.forge.SetPRMerged(publicName, prs[0].Number)
	h.forge.SetPRBase(publicName, prs[1].Number, "main")
	h.syncOK(engine.SyncOptions{})
	if !h.shadowMeta("b").Promoted.NeedsUpdate {
		t.Fatal("sync must mark b public_pr_needs_update")
	}
	if _, _, err := h.eng.FeatureRestack(h.ctx, h.pair, engine.RestackOptions{Feature: "b", Onto: "main"}); err != nil {
		t.Fatal(err)
	}
	h.gateFeature("b")
	pr, err := promote(h, engine.PromoteOptions{Feature: "b", Mode: "squash", Message: "Add b", Yes: true})
	if err != nil || pr.Applied == nil || !pr.Plan.Publish.Update || pr.Plan.Publish.Style != "replace" {
		t.Fatalf("promote b = %+v %v", pr, err)
	}
	if parent, _ := h.pubGit.MustResolve(h.ctx, h.pubTip("b")+"^"); parent != squash {
		t.Fatalf("b's PR branch sits on %s, want public main %s", parent, squash)
	}
	m := h.shadowMeta("b")
	if m.Promoted.NeedsUpdate || m.Promoted.PublicPR != prs[1].Number {
		t.Fatalf("b's record = %+v", m.Promoted)
	}
	if r := h.report("b"); r.State != feature.StatePublished {
		t.Fatalf("b is %s", r.State)
	}
}

func chainNamesOf(pl *plan.Plan) []string {
	out := []string{}
	for _, en := range pl.Chain {
		out = append(out, en.Feature)
	}
	return out
}

func stringsOf(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, len(l))
		for i, x := range l {
			out[i], _ = x.(string)
		}
		return out
	}
	return nil
}

func itoa(n int) string { return strconv.Itoa(n) }
