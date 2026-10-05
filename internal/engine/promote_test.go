package engine_test

import (
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

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
		p.Publish.PR.Body != "It does things." || !p.RequiresHuman {
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
	t.Run("agent needs a human, then a human applies", func(t *testing.T) {
		h := gated(t)
		h.eng.Actor = "agent:claude-code"
		res, err := promote(h, engine.PromoteOptions{Message: "m", Yes: true})
		_ = wantCode(t, err, contract.ApprovalRequired)
		if _, ok, _ := h.pubGit.Resolve(h.ctx, "refs/heads/feat-x"); ok || res.Plan == nil {
			t.Fatal("agent promote must stop at the plan")
		}
		_, _, err = h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true})
		_ = wantCode(t, err, contract.ApprovalRequired)
		h.eng.Actor = "human:octo"
		if _, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: res.Plan.ID, Yes: true}); err != nil {
			t.Fatalf("human apply: %v", err)
		}
	})
	t.Run("no tty and no --yes", func(t *testing.T) {
		h := gated(t)
		_, err := promote(h, engine.PromoteOptions{Message: "m"})
		_ = wantCode(t, err, contract.ApprovalRequired)
	})
	t.Run("prompt declined, then accepted", func(t *testing.T) {
		h := gated(t)
		var summary string
		h.eng.Prompt = func(s string) (bool, error) { summary = s; return false, nil }
		_, err := promote(h, engine.PromoteOptions{Message: "m"})
		_ = wantCode(t, err, contract.ApprovalRequired)
		if !strings.Contains(summary, "feat-x") || !strings.Contains(summary, "feature.txt") {
			t.Fatalf("summary must show the branch and the files going public:\n%s", summary)
		}
		h.eng.Prompt = func(string) (bool, error) { return true, nil }
		if _, err := promote(h, engine.PromoteOptions{Message: "m"}); err != nil {
			t.Fatal(err)
		}
	})
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
