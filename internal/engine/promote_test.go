package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func gated(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "new\n", ".ghostbranch/notes.md": "private\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatalf("gate: %v", err)
	}
	return h
}

func promote(h *harness, o engine.PromoteOptions) (*engine.PromoteResult, error) {
	if o.Feature == "" {
		o.Feature = "feat-x"
	}
	res, _, err := h.eng.Promote(h.ctx, h.pair, o)
	return res, err
}

func TestPromotePlanOnlyWritesAPlanAndTouchesNothingPublic(t *testing.T) {
	t.Parallel()
	h := gated(t)
	before := h.publicRefs()
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature\n\nIt does things.", PlanOnly: true})
	if err != nil || res.Plan == nil || res.Applied != nil || !res.Gate.Reused {
		t.Fatalf("promote = %+v %v", res, err)
	}
	p := res.Plan
	pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if p.Publish.Parent != pubMain || p.Publish.Tree != res.Gate.PublishedTree || p.Publish.TargetBranch != "feat-x" ||
		len(p.Publish.Commits) != 1 || p.Publish.Commits[0].Author.Email != "42+octo@users.noreply.github.com" || p.Publish.PR.Title != "Add feature" ||
		p.Publish.PR.Body != "It does things." {
		t.Fatalf("plan = %+v", p)
	}
	saved, _, err := plan.Load(h.eng.WS.PlansDir(), p.ID)
	if err != nil || !reflect.DeepEqual(saved, *p) {
		t.Fatalf("saved plan = %+v %v", saved, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/ghostbranch/plans/"+p.ID); got != p.Publish.Commit {
		t.Fatal("plan commit not stored in the shadow repo")
	}
	if !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("--plan touched the public repo")
	}
}

func TestPromoteYesPublishesExactlyTheGatedTree(t *testing.T) {
	t.Parallel()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature", Yes: true})
	if err != nil || res.Applied == nil || res.Applied.PublicPR != 1 {
		t.Fatalf("promote = %+v %v", res, err)
	}
	got, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/feat-x")
	if got != res.Plan.Publish.Commit {
		t.Fatalf("public branch %s != plan commit %s", got, res.Plan.Publish.Commit)
	}
	var pubPaths []string
	es, _ := h.pubGit.ListTree(h.ctx, got)
	for _, e := range es {
		pubPaths = append(pubPaths, e.Path)
	}
	if !reflect.DeepEqual(pubPaths, []string{".github/workflows/ci.yml", "README.md", "feature.txt"}) {
		t.Fatalf("published paths = %v", pubPaths)
	}
	prs := h.forge.PRs(publicName)
	if len(prs) != 1 || prs[0].Head != "feat-x" || prs[0].Base != "main" || prs[0].Draft {
		t.Fatalf("public PRs = %+v", prs)
	}
	raw, _, _ := h.shadowGit().ShowFile(h.ctx, "refs/ghostbranch/features/feat-x", feature.MetaFile)
	m, _ := feature.Decode(raw)
	if m.Promoted == nil || m.Promoted.PublicPR != 1 || m.Promoted.PlanID != res.Plan.ID {
		t.Fatalf("meta.promoted = %+v", m.Promoted)
	}
	for _, p := range h.pushes {
		if p.remote == "public" && p.op != "promote" {
			t.Fatalf("public push from %s", p.op)
		}
	}
}

// Review Focus 3a: a stale gate is re-run, never trusted.
func TestPromoteReGatesWhenPublicMainMoved(t *testing.T) {
	t.Parallel()
	h := gated(t)
	newMain := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature", PlanOnly: true})
	if err != nil || res.Gate.Reused || res.Plan.Fingerprint.PublicMain != newMain || res.Plan.Publish.Parent != newMain {
		t.Fatalf("promote = %+v %v", res, err)
	}
}

func TestPromoteReusesAFreshGate(t *testing.T) {
	t.Parallel()
	h := gated(t)
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || !res.Gate.Reused || calls.Load() != 0 {
		t.Fatalf("reuse = %+v %v calls=%d", res, err, calls.Load())
	}
}

func TestPromoteApprovalRules(t *testing.T) {
	t.Parallel()
	published := func(h *harness) bool { _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); return ok }
	reasonOf := func(t *testing.T, err error) string {
		t.Helper()
		r, _ := wantCode(t, err, contract.ApprovalRequired).Details["reason"].(string)
		return r
	}
	t.Run("--yes without a grant or a console stops at the plan", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true})
		if r := reasonOf(t, err); r != "no_console" || published(h) || res.Plan == nil {
			t.Fatalf("reason %q, published %v", r, published(h))
		}
		if id := contract.From(err).Details["plan_id"]; id != res.Plan.ID {
			t.Fatalf("details.plan_id = %v", id)
		}
	})
	t.Run("the actor label plays no part", func(t *testing.T) {
		for _, actor := range []string{"human:octo", "agent:claude-code", "app"} {
			h := gated(t)
			h.eng.Actor, h.eng.Approver = actor, nil
			if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true}); reasonOf(t, err) != "no_console" {
				t.Fatalf("%s without a console: %v", actor, err)
			}
			h = gated(t)
			h.eng.Actor = actor
			if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"}); err != nil || !published(h) {
				t.Fatalf("%s confirmed at the console: %v", actor, err)
			}
		}
	})
	t.Run("a grant plus --yes publishes without asking", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		h.grant(6 * time.Hour)
		res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true})
		if err != nil || !published(h) || res.Applied.Approval == nil || res.Applied.Approval.Kind != "grant" ||
			res.Applied.Approval.GrantID != "g-000001" || res.Applied.Approval.CreatedBy != "human:octo" {
			t.Fatalf("promote = %+v %v", res, err)
		}
		es := h.ledger()
		if last := es[len(es)-1]; last.Op != "promote" || last.Approval == nil || last.Approval.Kind != "grant" {
			t.Fatalf("ledger = %+v", last)
		}
	})
	t.Run("a grant without --yes and no console asks for --yes", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		h.grant(6 * time.Hour)
		if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"}); reasonOf(t, err) != "grant_needs_yes" || published(h) {
			t.Fatal(err)
		}
	})
	t.Run("an expired grant counts for nothing", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		h.grant(time.Hour)
		h.clock.Sleep(2 * time.Hour)
		if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true}); reasonOf(t, err) != "no_console" || published(h) {
			t.Fatal(err)
		}
	})
	t.Run("--yes at a console still asks", func(t *testing.T) {
		h := gated(t)
		if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true}); err != nil {
			t.Fatal(err)
		}
		if len(h.approver.asked) != 1 {
			t.Fatal("--yes without a grant must not skip the console")
		}
	})
	t.Run("console refusals pass their reason through", func(t *testing.T) {
		for _, r := range []approval.Reason{approval.NotConfirmed, approval.Timeout, approval.TooManyAttempts} {
			h := gated(t)
			h.approver.err = &approval.Refusal{Reason: r}
			if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"}); reasonOf(t, err) != string(r) || published(h) {
				t.Fatalf("%s: %v", r, err)
			}
		}
	})
	t.Run("a console failure is INTERNAL", func(t *testing.T) {
		h := gated(t)
		h.approver.err = errors.New("console vanished")
		_, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"})
		_ = wantCode(t, err, contract.Internal)
		if published(h) {
			t.Fatal("a console failure must not publish")
		}
	})
	t.Run("the console shows the branch and the files going public", func(t *testing.T) {
		h := gated(t)
		h.approver.err = &approval.Refusal{Reason: approval.NotConfirmed}
		_, _ = promote(h, engine.PromoteOptions{Mode: "squash", Message: "m"})
		if len(h.approver.asked) != 1 || !strings.Contains(h.approver.asked[0], "feat-x") || !strings.Contains(h.approver.asked[0], "feature.txt") {
			t.Fatalf("summary = %v", h.approver.asked)
		}
	})
	// Review Focus 2: never "no grant", never "granted", even with a console.
	t.Run("a corrupt grants file fails closed", func(t *testing.T) {
		for _, console := range []bool{false, true} {
			h := gated(t)
			if !console {
				h.eng.Approver = nil
			}
			if err := os.WriteFile(h.eng.WS.GrantsPath(), []byte("{nope"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Yes: true})
			if ce := wantCode(t, err, contract.Internal); !strings.Contains(ce.Message, "grants.json") || published(h) {
				t.Fatalf("console=%v: %s", console, ce.Message)
			}
			if len(h.approver.asked) != 0 {
				t.Fatalf("console=%v: the console was asked despite an unreadable grants file", console)
			}
		}
	})
}

// DEFERRED (sp2): input problems are reported before the gate can run.
func TestPromoteChecksInputBeforeAnyCI(t *testing.T) {
	t.Parallel()
	h := gated(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"}) // the gate is now stale
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	pushes := len(h.pushes)
	for _, c := range []struct {
		o    engine.PromoteOptions
		code contract.Code
	}{
		{engine.PromoteOptions{Mode: "squash"}, contract.MissingInput},
		{engine.PromoteOptions{Mode: "squash", Message: "m", Branch: "main"}, contract.NameInvalid},
		{engine.PromoteOptions{Message: "m", Mode: "preserve"}, contract.BadFlag},
	} {
		_, err := promote(h, c.o)
		_ = wantCode(t, err, c.code)
	}
	if calls.Load() != 0 || len(h.pushes) != pushes {
		t.Fatal("input errors must be reported before any gate or push")
	}
}

// DEFERRED (sp2): --title alone keeps the body; both are trimmed.
func TestPromoteTitleAndBody(t *testing.T) {
	t.Parallel()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Subject\n\nBody text", Title: "  Custom title  ", PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "Custom title" || res.Plan.Publish.PR.Body != "Body text" {
		t.Fatalf("plan PR = %+v %v", res.Plan.Publish.PR, err)
	}
	res, err = promote(h, engine.PromoteOptions{Mode: "squash", Message: "Subject", Body: "  only body  ", PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "Subject" || res.Plan.Publish.PR.Body != "only body" {
		t.Fatalf("plan PR = %+v %v", res.Plan.Publish.PR, err)
	}
}

func TestPromoteInputErrors(t *testing.T) {
	t.Parallel()
	h := gated(t)
	_, err := promote(h, engine.PromoteOptions{Mode: "squash", PlanOnly: true})
	if ce := wantCode(t, err, contract.MissingInput); ce.Field != "message" {
		t.Fatalf("field = %q", ce.Field)
	}
	h.eng.Editor = func(string) (string, error) { return "# only a comment\n\n", nil }
	_, err = promote(h, engine.PromoteOptions{Mode: "squash", PlanOnly: true})
	_ = wantCode(t, err, contract.MissingInput)
	h.eng.Editor = func(string) (string, error) { return "From the editor\n# ignored\nbody line\n", nil }
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "From the editor" || res.Plan.Publish.PR.Body != "body line" {
		t.Fatalf("editor message = %+v %v", res, err)
	}
	_, err = promote(h, engine.PromoteOptions{Message: "m", Mode: "rebase"})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "mode" {
		t.Fatalf("field = %q", ce.Field)
	}
	_, err = promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Branch: "main", PlanOnly: true})
	_ = wantCode(t, err, contract.NameInvalid)
}

func TestPromoteScansTheMessageAndPRText(t *testing.T) {
	t.Parallel()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add feature with " + testutil.FakeGitHubToken(), PlanOnly: true})
	_ = wantCode(t, err, contract.LeakFound)
	if res.Plan != nil || len(res.Findings) == 0 || res.Findings[0].Where != "commit_message" {
		t.Fatalf("promote = %+v", res)
	}
}

func TestPromoteUnderAnotherBranchName(t *testing.T) {
	t.Parallel()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", Branch: "public-name", Yes: true})
	if err != nil || res.Applied.PublicBranch != "public-name" {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/public-name"); !ok {
		t.Fatal("public branch not created")
	}
	if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok {
		t.Fatal("the feature name must not leak as a branch")
	}
}

func TestPromoteReGatesWhenTheGateBranchIsGone(t *testing.T) {
	t.Parallel()
	h := gated(t)
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/ghostbranch-gate/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func TestPromoteReportsAFileWarningOnceWhenItRunsTheGate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{".github/workflows/release.yml": "on: push\n"})
	_, warns, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, w := range warns {
		if w.Code == contract.WorkflowChanged {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one WORKFLOW_CHANGED warning, got %d in %+v", n, warns)
	}
}

// A gate recorded without the commit mapping (an older binary) is stale.
func TestPromoteReGatesAGateWithoutCommitMapping(t *testing.T) {
	t.Parallel()
	h := gated(t)
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Gate.Commits = nil })
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused {
		t.Fatalf("promote must re-gate: %+v %v", res, err)
	}
}

// A reused gate reports the mode its scan covered, never the promote's or
// the default; a gate recorded without it is not reused.
func TestPromoteReusedGateReportsTheModeItScanned(t *testing.T) {
	t.Parallel()
	h := gated(t) // gated under the default, preserve
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || !res.Gate.Reused || res.Gate.ScannedAs != "preserve" {
		t.Fatalf("reused gate = %+v %v", res.Gate, err)
	}
	forgeMeta(t, h, "feat-x", func(m *feature.Meta) { m.Gate.ScannedAs = "" })
	res, err = promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused || res.Gate.ScannedAs != "squash" {
		t.Fatalf("a gate without scanned_as must not be reused: %+v %v", res.Gate, err)
	}
}

// DEFERRED: a reused gate result has no shadow PR, not PR 0.
func TestPromoteReusedGateHasNoShadowPR(t *testing.T) {
	t.Parallel()
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
	if err != nil || !res.Gate.Reused || res.Gate.ShadowPR != nil {
		t.Fatalf("reused gate = %+v %v", res.Gate, err)
	}
}

// gatedHistory gates a feature of three commits: a, notes only, b.
func gatedHistory(t *testing.T) (*harness, []string) {
	t.Helper()
	h := newHarness(t)
	h.init()
	s1 := h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	sg := h.shadowGit()
	s2 := testutil.Commit(t, sg, "feat-x", "notes", map[string]string{".ghostbranch/notes.md": "private\n"})
	s3 := testutil.Commit(t, sg, "feat-x", "add b\n\nb matters", map[string]string{"b.txt": "b\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatalf("gate: %v", err)
	}
	return h, []string{s1, s2, s3}
}

func TestPromotePreservePublishesEachCommit(t *testing.T) {
	t.Parallel()
	h, src := gatedHistory(t)
	res, err := promote(h, engine.PromoteOptions{Title: "Add a and b", Yes: true}) // mode from config: preserve
	if err != nil || res.Plan.Mode != "preserve" || len(res.Plan.Publish.Commits) != 2 ||
		!reflect.DeepEqual(res.Plan.Publish.DroppedEmpty, []string{src[1]}) {
		t.Fatalf("promote = %+v %v", res.Plan, err)
	}
	c := res.Plan.Publish.Commits
	if !reflect.DeepEqual(c[0].Sources, []string{src[0]}) || !reflect.DeepEqual(c[1].Sources, []string{src[2]}) ||
		c[1].Subject != "add b" || c[1].Author.Email != "42+octo@users.noreply.github.com" ||
		!c[1].Author.Date.Equal(testutil.Author(3).When) {
		t.Fatalf("commits = %+v", c)
	}
	tipSHA, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/feat-x")
	if tipSHA != c[1].Commit || res.Plan.Publish.Commit != tipSHA {
		t.Fatalf("public tip %s, plan %+v", tipSHA, res.Plan.Publish)
	}
	if p, _ := h.pubGit.MustResolve(h.ctx, tipSHA+"^"); p != c[0].Commit {
		t.Fatal("public history must be the two commits on public main")
	}
	msg, _ := h.pubGit.CommitMessage(h.ctx, tipSHA)
	if msg != "add b\n\nb matters\n" {
		t.Fatalf("message = %q", msg)
	}
	// The fake forge keeps no PR texts; apply opens the PR with the plan's.
	if pr := res.Plan.Publish.PR; pr.Title != "Add a and b" || pr.Body != "- work on feat-x\n- add b" || len(h.forge.PRs(publicName)) != 1 {
		t.Fatalf("PR = %+v", pr)
	}
}

func TestPromotePreserveNeedsATitleBeforeCI(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "add b", map[string]string{"b.txt": "b\n"})
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	_, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if ce := wantCode(t, err, contract.MissingInput); ce.Field != "title" || calls.Load() != 0 {
		t.Fatalf("field %q, checks polled %d times", ce.Field, calls.Load())
	}
}

func TestPromotePreserveOneCommitUsesItsSubject(t *testing.T) {
	t.Parallel()
	h := gated(t) // one commit: "work on feat-x", feature.txt plus notes
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "work on feat-x" || len(res.Plan.Publish.Commits) != 1 {
		t.Fatalf("promote = %+v %v", res.Plan, err)
	}
}

func TestPromoteMessageOnlyForSquash(t *testing.T) {
	t.Parallel()
	h := gated(t)
	_, err := promote(h, engine.PromoteOptions{Message: "m", PlanOnly: true}) // default preserve
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "message" {
		t.Fatalf("field = %q", ce.Field)
	}
	if err := engine.ValidatePromoteOptions(&engine.PromoteOptions{Feature: "feat-x", Mode: "preserve", Message: "m"}); contract.From(err).Field != "message" {
		t.Fatalf("explicit preserve with -m: %v", err)
	}
}

func TestPromoteNothingToPublish(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	src := h.startFeature("feat-x", map[string]string{".ghostbranch/notes.md": "private\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatal(err)
	}
	for _, o := range []engine.PromoteOptions{{PlanOnly: true}, {Mode: "squash", Message: "m", PlanOnly: true}} {
		_, err := promote(h, o)
		if ce := wantCode(t, err, contract.NothingToPublish); !reflect.DeepEqual(ce.Details["dropped_empty"], []string{src}) {
			t.Fatalf("%+v: details = %v", o, ce.Details)
		}
	}
}

func TestPromoteKeepAndMapAuthors(t *testing.T) {
	t.Parallel()
	h, _ := gatedHistory(t)
	h.setConfig("author_email: noreply", "author_email: keep")
	res, err := promote(h, engine.PromoteOptions{Title: "t", PlanOnly: true})
	if err != nil || res.Plan.Publish.Commits[0].Author.Email != "author@example.com" {
		t.Fatalf("keep = %+v %v", res.Plan, err)
	}
	h.setConfig("author_email: keep", "author_email: map\n  author_map:\n    \"someone@else.example\": \"S <s@else.example>\"")
	_, err = promote(h, engine.PromoteOptions{Title: "t", PlanOnly: true})
	if ce := wantCode(t, err, contract.AuthorUnmapped); !reflect.DeepEqual(ce.Details["emails"], []string{"author@example.com"}) {
		t.Fatalf("details = %v", ce.Details)
	}
}

func TestPromoteScansEveryCommitMessage(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("JIRA-[0-9]+")
	h.startFeature("feat-x", map[string]string{"a.txt": "a\n"})
	leaky := testutil.Commit(t, h.shadowGit(), "feat-x", "fix JIRA-123", map[string]string{"a.txt": "a2\n"})
	_, err := promote(h, engine.PromoteOptions{Title: "t", Body: "b", PlanOnly: true}) // the default body would repeat the subject
	ce := wantCode(t, err, contract.LeakFound)
	fs := ce.Details["findings"].([]leak.Finding)
	if len(fs) != 1 || fs[0].Where != "commit_message" || !reflect.DeepEqual(fs[0].Commits, []string{leaky}) || ce.Details["mode"] != "preserve" {
		t.Fatalf("findings = %+v details = %v", fs, ce.Details)
	}
}

func TestPromoteLeavesNoPlanWhenItsRefCannotBePushed(t *testing.T) {
	t.Parallel()
	h := gated(t)
	h.installShadowHook("GH013: refused")
	_, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	_ = wantCode(t, err, contract.PushRefused)
	if ents, _ := os.ReadDir(h.eng.WS.PlansDir()); len(ents) != 0 {
		t.Fatalf("a plan whose ref never reached the shadow repo must not be left behind: %v", ents)
	}
}

func TestPromoteDetachedReturnsTheGateJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true, Detach: true})
	if err != nil || res.Gate == nil || res.Gate.Checks != feature.ChecksPending || res.Gate.Job == nil || res.Plan != nil {
		t.Fatalf("detached promote = %+v %v", res, err)
	}
	// Again while pending: the same job, no new candidate.
	candidates := 0
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if strings.HasSuffix(s, ":refs/heads/ghostbranch-gate/feat-x") {
				candidates++
			}
		}
	}
	again, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true, Detach: true})
	if err != nil || *again.Gate.Job != *res.Gate.Job || candidates != 0 {
		t.Fatalf("second detached promote = %+v %v (%d candidate pushes)", again, err, candidates)
	}
}

func TestPromoteWaitsOnAMatchingJob(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	if _, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true, Detach: true}); err != nil {
		t.Fatal(err)
	}
	candidates := 0
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if strings.HasSuffix(s, ":refs/heads/ghostbranch-gate/feat-x") {
				candidates++
			}
		}
	}
	res, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true})
	if err != nil || res.Plan == nil || !res.Gate.Reused || candidates != 0 {
		t.Fatalf("promote = %+v %v (%d candidate pushes)", res, err, candidates)
	}
}

// A cancelled promote is INTERRUPTED, in its result and in the ledger.
func TestAnInterruptedPromoteIsInterrupted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	ctx, cancel := context.WithCancel(h.ctx)
	cancel()
	_, _, err := h.eng.Promote(ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true})
	if ce := wantCode(t, err, contract.Interrupted); ce.Details["feature"] != "feat-x" {
		t.Fatalf("details = %+v", ce.Details)
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "promote" || last.ErrorCode != string(contract.Interrupted) {
		t.Fatalf("ledger = %+v", last)
	}
}

// interruptAtThePublicPush cancels the context as feat-x's public branch
// is pushed, so the push never happens.
func interruptAtThePublicPush(h *harness, cancel context.CancelFunc) {
	record := h.eng.OnPush
	h.eng.OnPush = func(op, remote string, specs []string) {
		record(op, remote, specs)
		for _, s := range specs {
			if remote == "public" && strings.HasSuffix(s, ":refs/heads/feat-x") {
				cancel()
			}
		}
	}
}

// A promote interrupted once its plan is saved points at apply <plan>:
// promoting again would make new commits.
func TestAnInterruptAfterThePlanIsSavedPointsAtApply(t *testing.T) {
	t.Parallel()
	h := gated(t)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	interruptAtThePublicPush(h, cancel)
	res, _, err := h.eng.Promote(ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add feature"})
	ce := wantCode(t, err, contract.Interrupted)
	if res == nil || res.Plan == nil || ce.Details["plan_id"] != res.Plan.ID || ce.Details["feature"] != "feat-x" {
		t.Fatalf("result = %+v, details = %+v", res, ce.Details)
	}
	if !strings.Contains(ce.Message, "ghostbranch apply "+res.Plan.ID) {
		t.Fatalf("message = %q", ce.Message)
	}
	if _, ok := h.publicRefs()["refs/heads/feat-x"]; ok {
		t.Fatal("the interrupted push reached the public repo")
	}
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "promote" || last.ErrorCode != string(contract.Interrupted) {
		t.Fatalf("ledger = %+v", last)
	}
}

// cancelAtFindPR is the fake forge, but the first look for an open PR in
// the public repo cancels the context and fails as a cancelled request
// would: an interrupt after the public push, before the PR is opened.
type cancelAtFindPR struct {
	*fake.Forge
	cancel context.CancelFunc
	done   bool
}

func (f *cancelAtFindPR) FindOpenPR(ctx context.Context, full, head, base string) (*forge.PR, error) {
	if full == publicName && !f.done {
		f.done = true
		f.cancel()
		return nil, ctx.Err()
	}
	return f.Forge.FindOpenPR(ctx, full, head, base)
}

// Review Focus 5: apply <plan> after a promote interrupted between the
// public push and the PR publishes once: the branch the push left, one PR,
// no BRANCH_EXISTS.
func TestApplyResumesAPromoteInterruptedAfterThePush(t *testing.T) {
	t.Parallel()
	h := gated(t)
	ctx, cancel := context.WithCancel(h.ctx)
	defer cancel()
	h.eng.Forge = &cancelAtFindPR{Forge: h.forge, cancel: cancel}
	res, _, err := h.eng.Promote(ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add feature"})
	ce := wantCode(t, err, contract.Interrupted)
	if res == nil || res.Plan == nil || ce.Details["plan_id"] != res.Plan.ID {
		t.Fatalf("result = %+v, details = %+v", res, ce.Details)
	}
	id, P := res.Plan.ID, res.Plan.Publish.Commit
	if got := h.publicRefs()["refs/heads/feat-x"]; got != P || len(h.forge.PRs(publicName)) != 0 {
		t.Fatalf("after the interrupt: branch at %s (plan %s), PRs %+v", got, P, h.forge.PRs(publicName))
	}
	// Nothing was published as a PR: the ledger says the plan was saved.
	es := h.ledger()
	if last := es[len(es)-1]; last.Op != "promote" || last.ErrorCode != string(contract.Interrupted) || last.Summary != "plan "+id+" saved" {
		t.Fatalf("ledger = %+v", last)
	}

	h.eng.Forge = h.forge
	ar, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id})
	if err != nil || ar.PublicPR != 1 || !ar.Resumed {
		t.Fatalf("apply = %+v %v", ar, err)
	}
	if got := h.publicRefs()["refs/heads/feat-x"]; got != P {
		t.Fatalf("public branch at %s, want the plan's commit %s", got, P)
	}
	if prs := h.forge.PRs(publicName); len(prs) != 1 || prs[0].Head != "feat-x" {
		t.Fatalf("public PRs = %+v", prs)
	}
	pushes := 0
	for _, p := range h.pushes {
		for _, s := range p.refspecs {
			if p.remote == "public" && strings.HasSuffix(s, ":refs/heads/feat-x") {
				pushes++
			}
		}
	}
	if pushes != 1 {
		t.Fatalf("%d pushes of the public branch, want 1", pushes)
	}
	if m := h.meta("feat-x"); m.Promoted == nil || m.Promoted.PlanID != id || m.Promoted.PublicPR != 1 {
		t.Fatalf("record = %+v", m.Promoted)
	}
}

func TestPlanRecordsWhichTextsWereTyped(t *testing.T) {
	t.Parallel()
	h := gated(t)
	for _, c := range []struct {
		o    engine.PromoteOptions
		want plan.Typed
	}{
		{engine.PromoteOptions{Mode: "squash", Message: "Add feature\n\nIt does things.", PlanOnly: true}, plan.Typed{}},
		{engine.PromoteOptions{Mode: "squash", Message: "Add feature", Title: "T", PlanOnly: true}, plan.Typed{Title: true}},
		{engine.PromoteOptions{Mode: "squash", Message: "Add feature", Body: " B ", PlanOnly: true}, plan.Typed{Body: true}},
	} {
		res, err := promote(h, c.o)
		if err != nil || res.Plan.Typed != c.want {
			t.Errorf("%+v: typed = %+v %v, want %+v", c.o, res.Plan.Typed, err, c.want)
		}
	}
}

// A --curate file is read only once the mode is known: under a default
// mode other than curated it doesn't apply (BAD_FLAG), whatever is in it.
func TestPromoteReadsTheCurateFileOnlyInCuratedMode(t *testing.T) {
	t.Parallel()
	bad := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(bad, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := engine.ValidatePromoteOptions(&engine.PromoteOptions{Feature: "feat-x", Curate: bad}); err != nil {
		t.Fatalf("with the mode not known yet the file must not be read: %v", err)
	}
	h := gated(t) // promote.default_mode: preserve
	_, err := promote(h, engine.PromoteOptions{Curate: bad, PlanOnly: true})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "curate" {
		t.Fatalf("field = %q", ce.Field)
	}
	h.setConfig("default_mode: preserve", "default_mode: curated")
	_, err = promote(h, engine.PromoteOptions{Curate: bad, PlanOnly: true})
	_ = wantCode(t, err, contract.CurateInvalid)
}

func TestPromoteWarnsAnUnusedAuthorMap(t *testing.T) {
	t.Parallel()
	h, _ := gatedHistory(t)
	h.setConfig("author_email: noreply", "author_email: noreply\n  author_map:\n    \"a@b.example\": \"A <a@b.example>\"")
	_, warns, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Title: "t", PlanOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var w *contract.Warning
	for i := range warns {
		if warns[i].Code == contract.ConfigKeyIgnored {
			w = &warns[i]
		}
	}
	if w == nil || w.Details["key"] != "promote.author_map" || !strings.Contains(w.Message, "noreply") {
		t.Fatalf("warnings = %+v", warns)
	}
	h.setConfig("author_email: noreply", "author_email: map")
	h.setConfig("\"a@b.example\": \"A <a@b.example>\"", "\"author@example.com\": \"A <a@b.example>\"")
	_, warns, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Title: "t", PlanOnly: true})
	if err != nil || hasWarning(warns, contract.ConfigKeyIgnored) {
		t.Fatalf("a used map must not warn: %+v %v", warns, err)
	}
}

// A promote that waited on the gate job and then reused the green gate
// keeps the job's shadow PR and id.
func TestPromoteThatWaitedOnTheJobKeepsIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	o := engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true}
	o.Detach = true
	first, _, err := h.eng.Promote(h.ctx, h.pair, o)
	if err != nil || first.Gate.Job == nil || first.Gate.ShadowPR == nil {
		t.Fatalf("detached promote = %+v %v", first, err)
	}
	o.Detach = false
	res, _, err := h.eng.Promote(h.ctx, h.pair, o)
	if err != nil || res.Plan == nil || !res.Gate.Reused || res.Gate.Job == nil || *res.Gate.Job != *first.Gate.Job ||
		res.Gate.ShadowPR == nil || *res.Gate.ShadowPR != *first.Gate.ShadowPR {
		t.Fatalf("promote = %+v %v", res.Gate, err)
	}
}

// A gate scanned as squash checks only the final tree; a preserve promote
// that reuses it finds an older commit's broken marker in its own chain.
// The error names the feature and mode, so next can offer the history steps.
func TestPromoteChainMarkerInvalidNamesTheFeature(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("default_mode: preserve", "default_mode: squash")
	s1 := h.startFeature("feat-x", map[string]string{"app.go": "ok\n// BEGIN-INTERNAL\nx\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "fix the marker", map[string]string{"app.go": "ok\n"})
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatalf("a squash gate scans only the final tree: %v", err)
	}
	_, err := promote(h, engine.PromoteOptions{Mode: "preserve", Title: "t", PlanOnly: true})
	ce := wantCode(t, err, contract.MarkerInvalid)
	if ce.Details["feature"] != "feat-x" || ce.Details["mode"] != "preserve" || ce.Details["commit"] != s1 {
		t.Fatalf("details = %v", ce.Details)
	}
}

// candidatePushes counts pushes of name's gate candidate from now on.
func candidatePushes(h *harness, name string) *int {
	n := new(int)
	h.eng.OnPush = func(_, _ string, specs []string) {
		for _, s := range specs {
			if strings.HasSuffix(s, ":refs/heads/ghostbranch-gate/"+name) {
				*n++
			}
		}
	}
	return n
}

// detachedJob starts a squash promote's gate job for feat-x and leaves it pending.
func detachedJob(t *testing.T) *harness {
	t.Helper()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"feature.txt": "x\n"})
	if _, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add x", PlanOnly: true, Detach: true}); err != nil {
		t.Fatal(err)
	}
	if !h.meta("feat-x").Gate.Pending() {
		t.Fatal("no pending job")
	}
	return h
}

// A matching job whose CI fails: promote waits on it and returns the
// recorded failure, with no new candidate and no plan.
func TestPromoteReturnsAFailedJobWithoutAPlan(t *testing.T) {
	t.Parallel()
	h := detachedJob(t)
	h.forge.Checks = func(string, string) []forge.Check {
		return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "failure"}}
	}
	n := candidatePushes(h, "feat-x")
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add x", PlanOnly: true})
	_ = wantCode(t, err, contract.ChecksFailed)
	if res == nil || res.Gate == nil || res.Gate.Checks != feature.ChecksFailed || res.Plan != nil || *n != 0 {
		t.Fatalf("promote = %+v (%d candidate pushes)", res, *n)
	}
	if g := h.meta("feat-x").Gate; g.Checks != feature.ChecksFailed {
		t.Fatalf("recorded gate = %+v", g)
	}
}

// A dry run never finishes a pending job (that would wait on CI and write):
// it says it would run the gate, pushes nothing, and the job stays pending.
func TestPromoteDryRunLeavesAPendingJobAlone(t *testing.T) {
	t.Parallel()
	h := detachedJob(t)
	pushes := len(h.pushes)
	h.eng.DryRun = true
	res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "Add x"})
	h.eng.DryRun = false
	if err != nil || res.Preview == nil || len(res.Preview.Ops) != 1 || res.Preview.Ops[0]["op"] != "run_gate" || res.Plan != nil {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	if len(h.pushes) != pushes || !h.meta("feat-x").Gate.Pending() {
		t.Fatalf("the dry run pushed (%d -> %d) or finished the job", pushes, len(h.pushes))
	}
}

// A pending job scanned for another mode isn't this promote's: it gates
// again, scanning for the promote's mode.
func TestPromoteReGatesAJobScannedInAnotherMode(t *testing.T) {
	t.Parallel()
	h := detachedJob(t) // scanned as squash
	n := candidatePushes(h, "feat-x")
	res, err := promote(h, engine.PromoteOptions{Mode: "preserve", PlanOnly: true})
	if err != nil || res.Plan == nil || res.Gate.Reused || res.Gate.ScannedAs != "preserve" || *n != 1 {
		t.Fatalf("promote = %+v %v (%d candidate pushes)", res, err, *n)
	}
}

// A recorded mapping that doesn't end at the candidate's parent is stale,
// as is an empty mapping whose candidate isn't on public main.
func TestPromoteReGatesWhenTheMappingDoesNotEndAtTheCandidate(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*feature.Meta){
		"last commit rebased elsewhere": func(m *feature.Meta) { m.Gate.Commits[len(m.Gate.Commits)-1].Rebased = m.Gate.PublicMain },
		"empty mapping off main":        func(m *feature.Meta) { m.Gate.Commits = []feature.GateCommit{} },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := gated(t)
			forgeMeta(t, h, "feat-x", edit)
			res, err := promote(h, engine.PromoteOptions{Mode: "squash", Message: "m", PlanOnly: true})
			if err != nil || res.Gate.Reused {
				t.Fatalf("promote must re-gate: %+v %v", res.Gate, err)
			}
		})
	}
}
