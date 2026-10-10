package engine_test

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// gatedStack starts names as a stack, the first on main and each next on
// the one before, one commit each adding <name>.txt, and gates them bottom
// first.
func gatedStack(t *testing.T, names ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.init()
	for i, n := range names {
		files := map[string]string{n + ".txt": n + "\n"}
		if i == 0 {
			h.startFeature(n, files)
		} else {
			h.startOn(n, names[i-1], files)
		}
	}
	for _, n := range names {
		h.gateFeature(n)
	}
	return h
}

// promoteStack runs promote and checks a result against its schema.
func promoteStack(h *harness, o engine.PromoteOptions) (*engine.PromoteResult, []contract.Warning, error) {
	h.t.Helper()
	res, warns, err := h.eng.Promote(h.ctx, h.pair, o)
	if res != nil {
		validateResult(h.t, "promote.schema.json", res)
	}
	return res, warns, err
}

// pubTip is the public repo's branch head.
func (h *harness) pubTip(branch string) string {
	h.t.Helper()
	sha, err := h.pubGit.MustResolve(h.ctx, "refs/heads/"+branch)
	if err != nil {
		h.t.Fatal(err)
	}
	return sha
}

func gateSources(m feature.Meta) []string {
	out := []string{}
	for _, c := range m.Gate.Commits {
		out = append(out, c.Source)
	}
	return out
}

func publishedSources(cs []plan.Commit) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.Sources...)
	}
	return out
}

// Review Focus 5: lists that aren't a stack are refused before anything is
// planned, naming the branch that breaks them.
func TestPromoteRefusesWhatIsNotAStack(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b", "c")
	before := h.publicRefs()
	refuse := func(list []string, reason, name string) *contract.Error {
		t.Helper()
		_, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Features: list, Mode: "squash", PlanOnly: true})
		ce := wantCode(t, err, contract.NotAStack)
		if ce.Details["reason"] != reason || ce.Details["feature"] != name {
			t.Fatalf("promote %v: details = %v, want reason %s naming %s", list, ce.Details, reason, name)
		}
		return ce
	}
	// A gap: b is missing between a and c.
	if ce := refuse([]string{"a", "c"}, "gap", "c"); !reflect.DeepEqual(ce.Details["want"], []string{"a", "b", "c"}) {
		t.Fatalf("gap: want = %v", ce.Details["want"])
	}
	// Not bottom first.
	refuse([]string{"b", "a"}, "order", "b")
	refuse([]string{"a", "b", "a"}, "duplicate", "a")
	// The bottom's parent is neither main nor public, and isn't listed.
	if ce := refuse([]string{"b", "c"}, "parent_not_public", "b"); !reflect.DeepEqual(ce.Details["want"], []string{"a", "b", "c"}) {
		t.Fatalf("parent_not_public: want = %v", ce.Details["want"])
	}
	refuse([]string{"c"}, "parent_not_public", "c")
	// A stale gate anywhere in the chain: a moves, so a's gate is stale ...
	testutil.Commit(t, h.shadowGit(), "a", "more a", map[string]string{"a2.txt": "a2\n"})
	refuse([]string{"a", "b", "c"}, "stale_gate", "a")
	// ... and once a is gated again, b's gate no longer builds on a as it is.
	h.gateFeature("a")
	refuse([]string{"a", "b", "c"}, "stale_gate", "b")

	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("a refused stack promote changed the public repo")
	}
	if ents, _ := os.ReadDir(h.eng.WS.PlansDir()); len(ents) != 0 {
		t.Fatalf("a refused stack promote saved %d plan(s)", len(ents))
	}
}

// Review Focus 1: b's PR publishes b's own commits, exactly as b's gate
// replayed and scanned them, on a's public tip.
func TestChainPublishesEachBranchsOwnScannedCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	sg := h.shadowGit()
	a1 := h.startFeature("a", map[string]string{"a.txt": "a\n"})
	a2 := testutil.Commit(t, sg, "a", "a and notes", map[string]string{"a2.txt": "a2\n", ".ghostbranch/a.md": "private\n"})
	b1 := h.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	b2 := testutil.Commit(t, sg, "b", "b with an internal section",
		map[string]string{"b2.txt": "public\n# BEGIN-INTERNAL\nthe plan\n# END-INTERNAL\n"})
	h.gateFeature("a")
	h.gateFeature("b")
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "preserve",
		Titles: map[string]string{"a": "Add a", "b": "Add b"}, Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a b = %+v %v", res, err)
	}
	pl := res.Plan
	if len(pl.Chain) != 2 || pl.Feature != "a" || !reflect.DeepEqual(pl.Chain[0], plan.Entry{Feature: pl.Feature, Mode: pl.Mode,
		Fingerprint: pl.Fingerprint, Publish: pl.Publish, Typed: pl.Typed, CI: pl.CI}) {
		t.Fatalf("plan = %+v", pl)
	}
	ma, mb := h.shadowMeta("a"), h.shadowMeta("b")
	if got := publishedSources(pl.Chain[0].Publish.Commits); !reflect.DeepEqual(got, []string{a1, a2}) {
		t.Fatalf("a's PR publishes %v, want a's commits", got)
	}
	if got := publishedSources(pl.Chain[1].Publish.Commits); !reflect.DeepEqual(got, []string{b1, b2}) ||
		!reflect.DeepEqual(gateSources(mb), []string{b1, b2}) {
		t.Fatalf("b's PR publishes %v and b's gate scanned %v, want b's own commits", got, gateSources(mb))
	}
	pa, pb := h.pubTip("a"), h.pubTip("b")
	if pa != pl.Chain[0].Publish.Commit || pb != pl.Chain[1].Publish.Commit || pl.Chain[1].Publish.Parent != pa {
		t.Fatalf("public tips %s %s, plan %+v", pa, pb, pl.Chain)
	}
	// Byte for byte what the gate scanned: each public commit holds its
	// planned tree, the tip holds b's gated tree, and the internal section
	// is gone.
	for _, c := range pl.Chain[1].Publish.Commits {
		if tree, _ := h.pubGit.TreeOf(h.ctx, c.Commit); tree != c.Tree {
			t.Fatalf("public commit %s holds tree %s, planned %s", c.Commit, tree, c.Tree)
		}
	}
	if tree, _ := h.pubGit.TreeOf(h.ctx, pb); tree != mb.Gate.PublishedTree {
		t.Fatalf("b's public tip holds %s, b's gate scanned %s", tree, mb.Gate.PublishedTree)
	}
	if blob, ok, _ := h.pubGit.ShowFile(h.ctx, pb, "b2.txt"); !ok || string(blob) != "public\n" {
		t.Fatalf("b2.txt went public as %q", blob)
	}
	// Nothing of a's under b.
	if names, _ := h.pubGit.DiffNames(h.ctx, pa, pb); !reflect.DeepEqual(names, []string{"b.txt", "b2.txt"}) {
		t.Fatalf("b's PR changes %v", names)
	}
	if first, _ := h.pubGit.MustResolve(h.ctx, pl.Chain[1].Publish.Commits[0].Commit+"^"); first != pa {
		t.Fatalf("b's first public commit sits on %s, not a's tip %s", first, pa)
	}
	// The PRs, the native stack and the records.
	prs := h.forge.PRs(publicName)
	if len(prs) != 2 || prs[0].Head != "a" || prs[0].Base != "main" || prs[1].Head != "b" || prs[1].Base != "a" {
		t.Fatalf("public PRs = %+v", prs)
	}
	sn := res.Applied.StackNumber
	if sn == nil || !reflect.DeepEqual(res.Applied.PRs, []plan.StackPR{
		{Feature: "a", Base: "main", Number: prs[0].Number, URL: prs[0].URL},
		{Feature: "b", Base: "a", Number: prs[1].Number, URL: prs[1].URL}}) || !reflect.DeepEqual(res.PRs, res.Applied.PRs) {
		t.Fatalf("applied = %+v", res.Applied)
	}
	if st := h.forge.Stacks(publicName); len(st) != 1 || st[0].Number != *sn || !reflect.DeepEqual(st[0].PRs, []int{prs[0].Number, prs[1].Number}) {
		t.Fatalf("native stacks = %+v", st)
	}
	if p := ma.Promoted; p == nil || p.Base != "main" || p.Branch != "a" || p.PublishedHead != pa || len(p.PatchIDs) != 2 || p.Stack != *sn {
		t.Fatalf("a's record = %+v", ma.Promoted)
	}
	mb = h.shadowMeta("b")
	if p := mb.Promoted; p == nil || p.Base != "a" || p.Branch != "b" || p.PublishedHead != pb || len(p.PatchIDs) != 2 || p.Stack != *sn || p.PlanID != pl.ID {
		t.Fatalf("b's record = %+v", mb.Promoted)
	}
	if res.Applied.Approval == nil || res.Applied.Approval.Kind != "grant" || len(h.approver.asked) != 0 {
		t.Fatalf("approval = %+v, console asked %d time(s)", res.Applied.Approval, len(h.approver.asked))
	}
	es := h.ledger()
	want := fmt.Sprintf("published a as PR #1, b as PR #2 (stack #%d)", *sn)
	if last := es[len(es)-1]; last.Op != "promote" || last.Summary != want {
		t.Fatalf("ledger = %+v, want %q", last, want)
	}
}

// Review Focus 1: nothing of the parent's goes public under the child's
// approval, in one stack promote or on top of a parent published earlier.
func TestChainNeverPublishesTheParentUnderTheChild(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash"})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a b = %+v %v", res, err)
	}
	if len(h.approver.asked) != 1 {
		t.Fatalf("one code approves the stack; the console was asked %d times", len(h.approver.asked))
	}
	sum := h.approver.asked[0]
	aPart, bPart, ok := strings.Cut(sum, "PR 2 of 2: b, based on a")
	if !ok || !strings.Contains(aPart, "PR 1 of 2: a, based on main") || !strings.Contains(aPart, "a.txt") || strings.Contains(aPart, "b.txt") ||
		!strings.Contains(bPart, "b.txt") || strings.Contains(bPart, "a.txt") {
		t.Fatalf("each PR's part of the summary must list only its own files:\n%s", sum)
	}
	mb := h.shadowMeta("b")
	if got := publishedSources(res.Plan.Chain[1].Publish.Commits); !reflect.DeepEqual(got, gateSources(mb)) {
		t.Fatalf("b's squash publishes %v, b's gate scanned %v", got, gateSources(mb))
	}
	pa, pb := h.pubTip("a"), h.pubTip("b")
	if names, _ := h.pubGit.DiffNames(h.ctx, pa, pb); !reflect.DeepEqual(names, []string{"b.txt"}) {
		t.Fatalf("b's PR changes %v", names)
	}
	if n, _ := h.pubGit.Run(h.ctx, "rev-list", "--count", pa+".."+pb); n != "1" {
		t.Fatalf("b's PR holds %s commits, want 1", n)
	}

	// The parent was published on its own earlier: promoting b alone builds
	// on a's published head and never pushes a.
	h2 := newHarness(t)
	h2.init()
	h2.startFeature("a", map[string]string{"a.txt": "a\n"})
	h2.gateFeature("a")
	h2.grant(time.Hour)
	resA, err := promote(h2, engine.PromoteOptions{Feature: "a", Mode: "squash", Message: "Add a", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	h2.startOn("b", "a", map[string]string{"b.txt": "b\n"})
	h2.gateFeature("b")
	h2.pushes = nil
	resB, _, err := promoteStack(h2, engine.PromoteOptions{Feature: "b", Mode: "squash", Message: "Add b", Yes: true})
	if err != nil || resB.Applied == nil || len(resB.Plan.Chain) != 1 || resB.Plan.PRs[0].Base != "a" {
		t.Fatalf("promote b = %+v %v", resB, err)
	}
	for _, p := range h2.pushes {
		for _, s := range p.refspecs {
			if p.remote == "public" && strings.HasSuffix(s, ":refs/heads/a") {
				t.Fatalf("promote b pushed a: %v", p)
			}
		}
	}
	pa2, pb2 := h2.pubTip("a"), h2.pubTip("b")
	if pa2 != resA.Plan.Publish.Commit || resB.Plan.Publish.Parent != pa2 {
		t.Fatalf("a moved (%s, published %s) or b isn't on it (%s)", pa2, resA.Plan.Publish.Commit, resB.Plan.Publish.Parent)
	}
	if names, _ := h2.pubGit.DiffNames(h2.ctx, pa2, pb2); !reflect.DeepEqual(names, []string{"b.txt"}) {
		t.Fatalf("b's PR changes %v", names)
	}
	prs := h2.forge.PRs(publicName)
	if len(prs) != 2 || prs[1].Head != "b" || prs[1].Base != "a" {
		t.Fatalf("public PRs = %+v", prs)
	}
	// b joins a's PR in a native stack: a's PR is part of it, nothing of a's is republished.
	if st := h2.forge.Stacks(publicName); len(st) != 1 || !reflect.DeepEqual(st[0].PRs, []int{prs[0].Number, prs[1].Number}) {
		t.Fatalf("native stacks = %+v", st)
	}
}

// An apply that stopped part-way resumes at the first PR it hadn't opened,
// opens nothing twice, and registers the stack once.
func TestApplyResumesAStackAfterAFailure(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	id := res.Plan.ID
	record := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		record(op, remote, specs)
		for _, s := range specs {
			if remote == "public" && strings.HasSuffix(s, ":refs/heads/b") {
				// b's PR will fail to open, after a's opened.
				h.forge.MissingPermission = map[string]string{"CreatePR": "Pull requests: write"}
			}
		}
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true}); err == nil {
		t.Fatal("apply must fail when b's PR can't be opened")
	}
	if prs := h.forge.PRs(publicName); len(prs) != 1 || h.shadowMeta("a").Promoted == nil || h.shadowMeta("b").Promoted != nil {
		t.Fatalf("after the failure: PRs %+v", prs)
	}
	h.forge.MissingPermission = nil
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !ar.Resumed || ar.StackNumber == nil || len(h.forge.PRs(publicName)) != 2 || len(h.forge.Stacks(publicName)) != 1 {
		t.Fatalf("resume = %+v %v; PRs %+v", ar, err, h.forge.PRs(publicName))
	}
	ar, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || len(h.forge.PRs(publicName)) != 2 || len(h.forge.Stacks(publicName)) != 1 || *ar.StackNumber != h.forge.Stacks(publicName)[0].Number {
		t.Fatalf("a second resume = %+v %v", ar, err)
	}
	saved, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil || saved.StackNumber == nil || saved.PRs[1].Number != ar.PRs[1].Number {
		t.Fatalf("the plan file doesn't record the PRs: %+v %v", saved, err)
	}
}

// Without native stacks the PRs stay chained, each based on the branch below.
func TestStackFallsBackToChainedPRs(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	h.forge.StacksUnavailable = true
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil || res.Applied.StackNumber != nil || res.StackNumber != nil {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if prs := h.forge.PRs(publicName); len(prs) != 2 || prs[1].Base != "a" {
		t.Fatalf("PRs = %+v", prs)
	}
	if h.shadowMeta("b").Promoted.Stack != 0 {
		t.Fatal("b records a stack that doesn't exist")
	}
}

// A branch promoted on top of a published stack joins that stack.
func TestPromoteOnTopOfAPublishedStackAddsToIt(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil {
		t.Fatal(err)
	}
	h.startOn("c", "b", map[string]string{"c.txt": "c\n"})
	h.gateFeature("c")
	resC, _, err := promoteStack(h, engine.PromoteOptions{Feature: "c", Mode: "squash", Message: "Add c", Yes: true})
	if err != nil || resC.Applied.StackNumber == nil || *resC.Applied.StackNumber != *res.Applied.StackNumber {
		t.Fatalf("promote c = %+v %v", resC, err)
	}
	st := h.forge.Stacks(publicName)
	if len(st) != 1 || len(st[0].PRs) != 3 || h.shadowMeta("c").Promoted.Stack != st[0].Number {
		t.Fatalf("native stacks = %+v", st)
	}
}

// Without a grant or a console nothing goes public, and the plan is kept.
func TestAStackNeedsItsApproval(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.eng.Approver = nil
	before := h.publicRefs()
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash"})
	ce := wantCode(t, err, contract.ApprovalRequired)
	if ce.Details["plan_id"] != res.Plan.ID || !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestStackPromoteFlagsTakeOneFeature(t *testing.T) {
	t.Parallel()
	two := []string{"a", "b"}
	for _, c := range []struct {
		opt   engine.PromoteOptions
		field string
	}{
		{engine.PromoteOptions{Features: two, Title: "t"}, "title"},
		{engine.PromoteOptions{Features: two, Body: "b"}, "body"},
		{engine.PromoteOptions{Features: two, Message: "m"}, "message"},
		{engine.PromoteOptions{Features: two, Branch: "x"}, "branch"},
		{engine.PromoteOptions{Features: two, Mode: "curated", Curate: "c.json"}, "curate"},
		{engine.PromoteOptions{Features: two, Detach: true}, "detach"},
		{engine.PromoteOptions{Features: two, Modes: map[string]string{"c": "squash"}}, "mode-for"},
		{engine.PromoteOptions{Features: two, Modes: map[string]string{"b": "curated"}}, "mode-for"},
		{engine.PromoteOptions{Features: two, Titles: map[string]string{"c": "t"}}, "title-for"},
	} {
		err := engine.ValidatePromoteOptions(&c.opt)
		if ce := wantCode(t, err, contract.BadFlag); ce.Field != c.field {
			t.Errorf("%+v: field %q, want %q", c.opt, ce.Field, c.field)
		}
	}
	one := engine.PromoteOptions{Features: []string{"a"}, Modes: map[string]string{"a": "squash"}, Titles: map[string]string{"a": "T"}}
	if err := engine.ValidatePromoteOptions(&one); err != nil || one.Feature != "a" || one.Mode != "squash" || one.Title != "T" {
		t.Fatalf("one feature: %+v %v", one, err)
	}
}

// Levels link by tree, not SHA: a parent gated again on the same content
// builds a new rebased tip with the same published tree, and the child's
// gate still stands on it.
func TestStackLinksLevelsByTree(t *testing.T) {
	t.Parallel()
	h := gatedStack(t, "a", "b")
	h.clock.Sleep(time.Minute) // the new replay gets other commit dates
	h.gateFeature("a")
	ma, mb := h.shadowMeta("a"), h.shadowMeta("b")
	if mb.Gate.Base == rOf(t, ma.Gate) || mb.Gate.BaseTree != ma.Gate.PublishedTree {
		t.Fatalf("b's gate is on %s (tree %s); a's new R is %s (tree %s)", mb.Gate.Base, mb.Gate.BaseTree, rOf(t, ma.Gate), ma.Gate.PublishedTree)
	}
	h.grant(time.Hour)
	res, _, err := promoteStack(h, engine.PromoteOptions{Features: []string{"a", "b"}, Mode: "squash", Yes: true})
	if err != nil || res.Applied == nil {
		t.Fatalf("promote a b after a's re-gate = %+v %v", res, err)
	}
	if names, _ := h.pubGit.DiffNames(h.ctx, h.pubTip("a"), h.pubTip("b")); !reflect.DeepEqual(names, []string{"b.txt"}) {
		t.Fatalf("b's PR changes %v", names)
	}
}

// A feature moved onto main that keeps its fork point (as sync leaves one
// whose parent merged) publishes only its own commits: the preserve title
// check and the curated pre-check count forked_at..head, not every commit
// public main lacks.
func TestPromoteCountsOnlyOwnCommitsAfterReparentingToMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-a", map[string]string{"a.txt": "a\n"})
	bHead := h.startOn("feat-b", "feat-a", map[string]string{"b.txt": "b\n"})
	h.editRemoteMeta("feat-b", func(m *feature.Meta) { m.Parent = "" })
	h.gateFeature("feat-b")
	res, err := promote(h, engine.PromoteOptions{Feature: "feat-b", Mode: "preserve", PlanOnly: true})
	if err != nil || res.Plan == nil || len(res.Plan.Publish.Commits) != 1 || res.Plan.Publish.PR.Title != "work on feat-b" {
		t.Fatalf("preserve promote of feat-b = %+v %v", res, err)
	}
	res, err = promote(h, engine.PromoteOptions{Feature: "feat-b", Mode: "curated", Curate: "c.json",
		Groups: []plan.Group{{From: []string{bHead}, Message: "Add b"}}, PlanOnly: true})
	if err != nil || res.Plan == nil || !reflect.DeepEqual(publishedSources(res.Plan.Publish.Commits), []string{bHead}) {
		t.Fatalf("curated promote of feat-b = %+v %v", res, err)
	}
}
