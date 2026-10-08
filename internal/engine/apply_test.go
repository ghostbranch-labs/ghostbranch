package engine_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func planned(t *testing.T) (*harness, string) {
	t.Helper()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return h, res.Plan.ID
}

// Review Focus 3b: a plan made against old state is refused with exit 6.
func TestApplyRefusesAStalePlan(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	ce := wantCode(t, err, contract.PlanStale)
	if !reflect.DeepEqual(ce.Details["moved"], []string{"public_main"}) || ce.Code.Exit() != 6 {
		t.Fatalf("details = %v", ce.Details)
	}
	if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok {
		t.Fatal("stale plan was published")
	}

	h2, id2 := planned(t)
	testutil.Commit(t, h2.shadowGit(), "feat-x", "more", map[string]string{"more.txt": "m\n"})
	_, _, err = h2.eng.Apply(h2.ctx, engine.ApplyOptions{Plan: id2, Yes: true})
	if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"feature_head"}) {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestApplyResumesAfterTheBranchWasPushed(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the public push but before the PR was opened.
	mirror := gitx.New(h.eng.WS.MirrorDir(h.pair))
	if err := mirror.Push(h.ctx, "public", gitx.PushOptions{}, pl.Publish.Commit+":refs/heads/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !res.Resumed || res.PublicPR != 1 {
		t.Fatalf("resume = %+v %v", res, err)
	}
	res, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	if err != nil || !res.Resumed || len(h.forge.PRs(publicName)) != 1 {
		t.Fatalf("second resume = %+v %v", res, err)
	}
}

func TestApplyRefusesAPublicBranchAtAnotherCommit(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	testutil.Commit(t, h.pubGit, "feat-x", "someone else", map[string]string{"x": "x\n"})
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	_ = wantCode(t, err, contract.BranchExists)
}

func TestApplyUnknownPlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: "nope", Yes: true})
	_ = wantCode(t, err, contract.BadFlag)
}

// A plan is only as good as the gate it was made from: if the recorded gate
// for the same candidate says the checks failed, apply refuses and pushes
// nothing.
func TestApplyRefusesAPlanWhoseGateIsNotGreen(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	sg := h.shadowGit()
	ref := "refs/ghostbranch/features/feat-x"
	old, err := sg.MustResolve(h.ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	b, ok, err := sg.ShowFile(h.ctx, old, feature.MetaFile)
	if err != nil || !ok {
		t.Fatalf("read feature.json: %v", err)
	}
	m, err := feature.Decode(b)
	if err != nil || m.Gate == nil {
		t.Fatalf("decode feature.json: %+v %v", m, err)
	}
	m.Gate.Checks = feature.ChecksFailed // same candidate, now red
	blob, err := sg.HashObject(h.ctx, feature.Encode(m))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := sg.MakeTree(h.ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", SHA: blob, Path: feature.MetaFile}})
	if err != nil {
		t.Fatal(err)
	}
	commit, err := sg.CommitTree(h.ctx, tree, []string{old}, "gate failed", testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := sg.UpdateRef(h.ctx, ref, commit); err != nil {
		t.Fatal(err)
	}
	before := h.publicRefs()
	h.pushes = nil

	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	ce := wantCode(t, err, contract.PlanStale)
	if !reflect.DeepEqual(ce.Details["moved"], []string{"gate"}) {
		t.Fatalf("details = %v", ce.Details)
	}
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("a plan with a red gate changed the public repo")
	}
	for _, p := range h.pushes {
		if p.remote == "public" {
			t.Fatalf("pushed to public: %+v", p)
		}
	}
}

// apply re-scans the published commit's message as stored, byte for byte:
// i18n.logOutputEncoding in the user's config must not re-encode it (here to
// UTF-16) past the scanner.
func TestApplyScansTheStoredCommitMessage(t *testing.T) {
	h, id := planned(t)
	pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	// Swap in a commit with the same tree and parent but a leaking message,
	// as if the plan's commit had been tampered with after the promote scan.
	sg := h.shadowGit()
	tree, err := sg.TreeOf(h.ctx, pl.Publish.Commit)
	if err != nil {
		t.Fatal(err)
	}
	leaky, err := sg.CommitTree(h.ctx, tree, []string{pl.Publish.Parent}, "Add feature\n\nTOKEN="+testutil.FakeGitHubToken()+"\n",
		testutil.Author(1), testutil.Author(1))
	if err != nil {
		t.Fatal(err)
	}
	if err := sg.UpdateRef(h.ctx, "refs/ghostbranch/plans/"+id, leaky); err != nil {
		t.Fatal(err)
	}
	pl.Publish.Commit, pl.Publish.Commits[0].Commit = leaky, leaky
	if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
		t.Fatal(err)
	}
	system := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(system, []byte("[i18n]\n\tlogOutputEncoding = UTF-16\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_SYSTEM", system)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "0")

	_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	ce := wantCode(t, err, contract.LeakFound)
	if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok {
		t.Fatalf("a leaking commit message was published: %v", ce)
	}
	if ce.Details["mode"] != "squash" || ce.Details["public"] != publicName || ce.Details["branch"] != "feat-x" {
		t.Fatalf("details = %v", ce.Details)
	}
}

// DEFERRED (sp2): approval is asked only after the plan is verified.
func TestApplyAsksOnlyAfterEveryCheck(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id})
	if ce := wantCode(t, err, contract.PlanStale); ce.Details["feature"] != "feat-x" || ce.Details["public"] != publicName || ce.Details["branch"] != "feat-x" {
		t.Fatalf("details = %v", ce.Details)
	}
	if len(h.approver.asked) != 0 {
		t.Fatal("the console must not be asked about a stale plan")
	}
}

// Every PLAN_STALE names the feature and the public repo, so the next
// command can be spelled out; this is the missing-plan-commit one.
func TestApplyPlanStaleWhenThePlanCommitIsGone(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/ghostbranch/plans/"+id); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
	ce := wantCode(t, err, contract.PlanStale)
	if !reflect.DeepEqual(ce.Details["moved"], []string{"publish.commit"}) ||
		ce.Details["feature"] != "feat-x" || ce.Details["public"] != publicName {
		t.Fatalf("details = %v", ce.Details)
	}
	if len(h.approver.asked) != 0 {
		t.Fatal("the console must not be asked about a stale plan")
	}
}

// DEFERRED (sp2): the local plan file is not trusted about where it publishes.
func TestApplyRejectsATamperedTargetBranch(t *testing.T) {
	t.Parallel()
	h, id := planned(t)
	path := filepath.Join(h.eng.WS.PlansDir(), id+".json")
	// "Main" passes ValidateName and is caught as the public default branch.
	for _, bad := range []string{"main", "Main", "ghostbranch", "a/../b", ""} {
		pl, _, err := plan.Load(h.eng.WS.PlansDir(), id)
		if err != nil {
			t.Fatal(err)
		}
		pl.Publish.TargetBranch = bad
		b, _ := json.Marshal(pl)
		if err := os.WriteFile(path, b, 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
		if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "plan" {
			t.Fatalf("%q: field = %q", bad, ce.Field)
		}
	}
	if len(h.approver.asked) != 0 {
		t.Fatal("a tampered plan must be refused before asking anyone")
	}
}

func plannedHistory(t *testing.T) (*harness, *plan.Plan) {
	t.Helper()
	h, _ := gatedHistory(t)
	res, err := promote(h, engine.PromoteOptions{Title: "t", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	return h, res.Plan
}

// Review Focus 5.
func TestApplyRefusesATamperedChain(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, h *harness, pl *plan.Plan, field string) {
		t.Helper()
		_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
		if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{field}) {
			t.Fatalf("details = %v", ce.Details)
		}
		if len(h.approver.asked) != 0 {
			t.Fatal("asked a person before refusing")
		}
		if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok {
			t.Fatal("a tampered plan was published")
		}
	}
	t.Run("plan file names another middle tree", func(t *testing.T) {
		t.Parallel()
		h, pl := plannedHistory(t)
		pl.Publish.Commits[0].Tree = pl.Publish.Commits[1].Tree
		if _, err := plan.Save(h.eng.WS.PlansDir(), *pl); err != nil {
			t.Fatal(err)
		}
		refused(t, h, pl, "publish.commits")
	})
	t.Run("plan ref moved to a different chain", func(t *testing.T) {
		t.Parallel()
		h, pl := plannedHistory(t)
		sg := h.shadowGit()
		who := testutil.Author(9)
		mid, err := sg.CommitTree(h.ctx, pl.Publish.Commits[1].Tree, []string{pl.Publish.Parent}, "swapped", who, who)
		if err != nil {
			t.Fatal(err)
		}
		forged, err := sg.CommitTree(h.ctx, pl.Publish.Commits[1].Tree, []string{mid}, "add b", who, who)
		if err != nil {
			t.Fatal(err)
		}
		if err := sg.UpdateRef(h.ctx, "refs/ghostbranch/plans/"+pl.ID, forged); err != nil {
			t.Fatal(err)
		}
		pl.Publish.Commit = forged
		if _, err := plan.Save(h.eng.WS.PlansDir(), *pl); err != nil {
			t.Fatal(err)
		}
		refused(t, h, pl, "publish.commits")
	})
	// The approval's file list is measured from the parent: a plan file
	// whose parent is the tip would show nothing changing.
	t.Run("plan file names the tip as its parent", func(t *testing.T) {
		t.Parallel()
		h, pl := plannedHistory(t)
		pl.Publish.Parent = pl.Publish.Commit
		if _, err := plan.Save(h.eng.WS.PlansDir(), *pl); err != nil {
			t.Fatal(err)
		}
		refused(t, h, pl, "publish.parent")
	})
}

func TestApplyRefusesAnOlderPlanVersion(t *testing.T) {
	t.Parallel()
	h, pl := plannedHistory(t)
	pl.SchemaVersion = "0.4"
	if _, err := plan.Save(h.eng.WS.PlansDir(), *pl); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if ce := wantCode(t, err, contract.PlanStale); !reflect.DeepEqual(ce.Details["moved"], []string{"schema_version"}) {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestApplyPublishesAPreservedPlan(t *testing.T) {
	t.Parallel()
	h, pl := plannedHistory(t)
	h.grant(time.Hour)
	res, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID, Yes: true})
	if err != nil || res.PublicPR != 1 {
		t.Fatalf("apply = %+v %v", res, err)
	}
	tip, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/feat-x")
	if tip != pl.Publish.Commits[1].Commit {
		t.Fatal("apply must publish the planned tip")
	}
}

// A control sequence in a commit subject or the PR text is shown escaped
// on the console, never acted on, and the PR body is shown, cut to ten
// lines.
func TestApplySummaryEscapesControlCharacters(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "\x1b[2K\rinnocent", map[string]string{"b.txt": "b\n"})
	body := "line 1\u202e\n" + strings.Repeat("more\n", 11) + "last"
	res, err := promote(h, engine.PromoteOptions{Title: "t\x07itle", Body: body, PlanOnly: true})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID}); err != nil || len(h.approver.asked) != 1 {
		t.Fatalf("apply: %v, asked %d times", err, len(h.approver.asked))
	}
	s := h.approver.asked[0]
	for _, want := range []string{`\u001b[2K\u000dinnocent (octo`, `PR title t\u0007itle`, `line 1\u202e`, "… 3 more lines", "work on feat-x"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	if strings.ContainsAny(s, "\x1b\r\x07\u202e") || strings.Contains(s, "last") {
		t.Fatalf("summary holds a raw control character or untruncated body:\n%q", s)
	}
}

// The console shows each public commit of a preserved plan, with its
// subject and the author read from the commit object.
func TestApplySummaryListsEachPublicCommit(t *testing.T) {
	t.Parallel()
	h, pl := plannedHistory(t)
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: pl.ID}); err != nil {
		t.Fatal(err)
	}
	if len(h.approver.asked) != 1 {
		t.Fatalf("asked %d times", len(h.approver.asked))
	}
	s := h.approver.asked[0]
	author := "(octo <42+octo@users.noreply.github.com>)"
	for _, want := range []string{"preserve, 2 commit(s)", "work on feat-x " + author, "add b " + author, "dropped  1 commit(s)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("summary lacks %q:\n%s", want, s)
		}
	}
}
