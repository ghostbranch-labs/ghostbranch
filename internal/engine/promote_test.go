package engine_test

import (
	"errors"
	"os"
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
	h := gated(t)
	before := h.publicRefs()
	res, err := promote(h, engine.PromoteOptions{Message: "Add feature\n\nIt does things.", PlanOnly: true})
	if err != nil || res.Plan == nil || res.Applied != nil || !res.Gate.Reused {
		t.Fatalf("promote = %+v %v", res, err)
	}
	p := res.Plan
	pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if p.Publish.Parent != pubMain || p.Publish.Tree != res.Gate.PublishedTree || p.Publish.TargetBranch != "feat-x" ||
		p.Publish.Author.Email != "42+octo@users.noreply.github.com" || p.Publish.PR.Title != "Add feature" ||
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
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Message: "Add feature", Yes: true})
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
	h := gated(t)
	newMain := testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
	res, err := promote(h, engine.PromoteOptions{Message: "Add feature", PlanOnly: true})
	if err != nil || res.Gate.Reused || res.Plan.Fingerprint.PublicMain != newMain || res.Plan.Publish.Parent != newMain {
		t.Fatalf("promote = %+v %v", res, err)
	}
}

func TestPromoteReusesAFreshGate(t *testing.T) {
	h := gated(t)
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	res, err := promote(h, engine.PromoteOptions{Message: "m", PlanOnly: true})
	if err != nil || !res.Gate.Reused || calls.Load() != 0 {
		t.Fatalf("reuse = %+v %v calls=%d", res, err, calls.Load())
	}
}

func TestPromoteApprovalRules(t *testing.T) {
	published := func(h *harness) bool { _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); return ok }
	reasonOf := func(t *testing.T, err error) string {
		t.Helper()
		r, _ := wantCode(t, err, contract.ApprovalRequired).Details["reason"].(string)
		return r
	}
	t.Run("--yes without a grant or a console stops at the plan", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		res, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true})
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
			if _, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true}); reasonOf(t, err) != "no_console" {
				t.Fatalf("%s without a console: %v", actor, err)
			}
			h = gated(t)
			h.eng.Actor = actor
			if _, err := promote(h, engine.PromoteOptions{Message: "m"}); err != nil || !published(h) {
				t.Fatalf("%s confirmed at the console: %v", actor, err)
			}
		}
	})
	t.Run("a grant plus --yes publishes without asking", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		h.grant(6 * time.Hour)
		res, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true})
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
		if _, err := promote(h, engine.PromoteOptions{Message: "m"}); reasonOf(t, err) != "grant_needs_yes" || published(h) {
			t.Fatal(err)
		}
	})
	t.Run("an expired grant counts for nothing", func(t *testing.T) {
		h := gated(t)
		h.eng.Approver = nil
		h.grant(time.Hour)
		h.clock.Sleep(2 * time.Hour)
		if _, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true}); reasonOf(t, err) != "no_console" || published(h) {
			t.Fatal(err)
		}
	})
	t.Run("--yes at a console still asks", func(t *testing.T) {
		h := gated(t)
		if _, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true}); err != nil {
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
			if _, err := promote(h, engine.PromoteOptions{Message: "m"}); reasonOf(t, err) != string(r) || published(h) {
				t.Fatalf("%s: %v", r, err)
			}
		}
	})
	t.Run("a console failure is INTERNAL", func(t *testing.T) {
		h := gated(t)
		h.approver.err = errors.New("console vanished")
		_, err := promote(h, engine.PromoteOptions{Message: "m"})
		_ = wantCode(t, err, contract.Internal)
		if published(h) {
			t.Fatal("a console failure must not publish")
		}
	})
	t.Run("the console shows the branch and the files going public", func(t *testing.T) {
		h := gated(t)
		h.approver.err = &approval.Refusal{Reason: approval.NotConfirmed}
		_, _ = promote(h, engine.PromoteOptions{Message: "m"})
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
			_, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true})
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
	h := gated(t)
	testutil.Commit(t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"}) // the gate is now stale
	var calls atomic.Int32
	h.forge.Checks = func(string, string) []forge.Check { calls.Add(1); return nil }
	pushes := len(h.pushes)
	for _, c := range []struct {
		o    engine.PromoteOptions
		code contract.Code
	}{
		{engine.PromoteOptions{}, contract.MissingInput},
		{engine.PromoteOptions{Message: "m", Branch: "main"}, contract.NameInvalid},
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
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Message: "Subject\n\nBody text", Title: "  Custom title  ", PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "Custom title" || res.Plan.Publish.PR.Body != "Body text" {
		t.Fatalf("plan PR = %+v %v", res.Plan.Publish.PR, err)
	}
	res, err = promote(h, engine.PromoteOptions{Message: "Subject", Body: "  only body  ", PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "Subject" || res.Plan.Publish.PR.Body != "only body" {
		t.Fatalf("plan PR = %+v %v", res.Plan.Publish.PR, err)
	}
}

func TestPromoteInputErrors(t *testing.T) {
	h := gated(t)
	_, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if ce := wantCode(t, err, contract.MissingInput); ce.Field != "message" {
		t.Fatalf("field = %q", ce.Field)
	}
	h.eng.Editor = func(string) (string, error) { return "# only a comment\n\n", nil }
	_, err = promote(h, engine.PromoteOptions{PlanOnly: true})
	_ = wantCode(t, err, contract.MissingInput)
	h.eng.Editor = func(string) (string, error) { return "From the editor\n# ignored\nbody line\n", nil }
	res, err := promote(h, engine.PromoteOptions{PlanOnly: true})
	if err != nil || res.Plan.Publish.PR.Title != "From the editor" || res.Plan.Publish.PR.Body != "body line" {
		t.Fatalf("editor message = %+v %v", res, err)
	}
	_, err = promote(h, engine.PromoteOptions{Message: "m", Mode: "preserve"})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "mode" {
		t.Fatalf("field = %q", ce.Field)
	}
	_, err = promote(h, engine.PromoteOptions{Message: "m", Branch: "main", PlanOnly: true})
	_ = wantCode(t, err, contract.NameInvalid)
}

func TestPromoteScansTheMessageAndPRText(t *testing.T) {
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Message: "Add feature with " + testutil.FakeGitHubToken(), PlanOnly: true})
	_ = wantCode(t, err, contract.LeakFound)
	if res.Plan != nil || len(res.Findings) == 0 || res.Findings[0].Where != "commit_message" {
		t.Fatalf("promote = %+v", res)
	}
}

func TestPromoteUnderAnotherBranchName(t *testing.T) {
	h := gated(t)
	res, err := promote(h, engine.PromoteOptions{Message: "m", Branch: "public-name", Yes: true})
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
	h := gated(t)
	if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/ghostbranch-gate/feat-x"); err != nil {
		t.Fatal(err)
	}
	res, err := promote(h, engine.PromoteOptions{Message: "m", PlanOnly: true})
	if err != nil || res.Gate.Reused {
		t.Fatalf("promote = %+v %v", res, err)
	}
	if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}
