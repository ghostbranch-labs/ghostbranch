package engine_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestInitCreatesShadowMirrorsAndOverlay(t *testing.T) {
	h := newHarness(t)
	res, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || len(warns) != 1 || warns[0].Code != contract.ActionsMinutesBilled {
		t.Fatalf("init: %v %v", warns, err)
	}
	if !res.CreatedShadow || res.Shadow != shadowName || !res.OverlayCreated || !res.DefaultBranchSet ||
		!reflect.DeepEqual(res.MirroredBranches, []string{"main"}) {
		t.Fatalf("result = %+v", res)
	}
	sh, _ := h.forge.GetRepo(h.ctx, shadowName)
	if !sh.Private || sh.DefaultBranch != engine.OverlayBranch {
		t.Fatalf("shadow repo = %+v", sh)
	}
	pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if shMain, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); shMain != pubMain {
		t.Fatalf("shadow main %s != public main %s", shMain, pubMain)
	}
	raw, ok, _ := h.shadowGit().ShowFile(h.ctx, "refs/heads/ghostbranch", ".ghostbranch.yml")
	if !ok {
		t.Fatal("overlay branch has no .ghostbranch.yml")
	}
	if cfg, _, err := config.Parse(raw); err != nil || cfg.Public != publicName || cfg.Shadow != shadowName {
		t.Fatalf("overlay config = %+v %v", cfg, err)
	}
	if _, ok := h.eng.WS.Find(publicName); !ok {
		t.Fatal("pair not registered")
	}
	if l := h.ledger(); len(l) != 1 || l[0].Op != "init" || l[0].ExitCode != 0 || l[0].Actor != "human:octo" {
		t.Fatalf("ledger = %+v", l)
	}
	for _, p := range h.pushes {
		if p.remote != "shadow" {
			t.Fatalf("init pushed to %s", p.remote)
		}
	}
}

func TestInitIsResumable(t *testing.T) {
	h := newHarness(t)
	h.init()
	n := len(h.pushes)
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || !res.Resumed || res.CreatedShadow || res.OverlayCreated || len(h.pushes) != n {
		t.Fatalf("re-init = %+v %v (pushes %d -> %d)", res, err, n, len(h.pushes))
	}
}

func TestInitNeedsAdoptForAnExistingShadow(t *testing.T) {
	h := newHarness(t)
	if _, err := h.forge.CreatePrivateRepo(h.ctx, shadowName); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "adopt" {
		t.Fatalf("field = %q", ce.Field)
	}
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	if err != nil || !res.Adopted {
		t.Fatalf("adopt = %+v %v", res, err)
	}
	_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: "acme/other", Shadow: "acme/missing", Adopt: true})
	if codeOf(err) != contract.BadFlag && codeOf(err) != contract.ForgeError {
		t.Fatalf("adopting a missing repo: %v", err)
	}
}

func TestAdoptAcceptsAPartialMirrorButNotUnrelatedContent(t *testing.T) {
	// A shadow repo holding only public's main (what the E2E reset leaves
	// behind) is adoptable.
	h := newHarness(t)
	repo, err := h.forge.CreatePrivateRepo(h.ctx, shadowName)
	if err != nil {
		t.Fatal(err)
	}
	pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if err := h.pubGit.Push(h.ctx, repo.CloneURL, gitx.PushOptions{}, pubMain+":refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true}); err != nil {
		t.Fatalf("partial mirror must be adoptable: %v", err)
	}

	// A shadow repo with an unrelated branch is not.
	h2 := newHarness(t)
	repo2, err := h2.forge.CreatePrivateRepo(h2.ctx, shadowName)
	if err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, gitx.New(repo2.CloneURL), "random", "unrelated", map[string]string{"x": "y"})
	_, _, err = h2.eng.Init(h2.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	_ = wantCode(t, err, contract.ShadowNotEmpty)
}

func TestInitRefusesAPublicOrForkedShadow(t *testing.T) {
	h := newHarness(t)
	if _, err := h.forge.AddRepo(shadowName, false); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	_ = wantCode(t, err, contract.ShadowNotPrivate)
	h.forge.SetPrivate(shadowName, true)
	h.forge.SetFork(shadowName, true)
	_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	_ = wantCode(t, err, contract.ShadowNotPrivate)
	if len(h.pushes) != 0 {
		t.Fatal("nothing may be pushed to a non-private shadow")
	}
}

func TestInitRejectsOverlayPathsInPublic(t *testing.T) {
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "oops", map[string]string{".ghostbranch.yml": "x"})
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	_ = wantCode(t, err, contract.OverlayCollision)
}

func TestInitDryRunWritesNothing(t *testing.T) {
	h := newHarness(t)
	h.eng.DryRun = true
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || res.Preview == nil || len(res.Preview.Ops) == 0 || !res.CreatedShadow {
		t.Fatalf("dry run = %+v %v", res, err)
	}
	if _, err := h.forge.GetRepo(h.ctx, shadowName); !errors.Is(err, forge.ErrNotFound) {
		t.Fatal("dry run created the shadow repo")
	}
	if len(h.ledger()) != 0 || len(h.pushes) != 0 {
		t.Fatal("dry run wrote to the ledger or pushed")
	}
}

func TestInitWarnsWhenPublicIsPrivate(t *testing.T) {
	h := newHarness(t)
	h.forge.SetPrivate(publicName, true)
	_, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if c := codes(warns); err != nil || len(warns) != 2 || c[contract.PublicNotPublic] != 1 || c[contract.ActionsMinutesBilled] != 1 {
		t.Fatalf("warns = %v err = %v", warns, err)
	}
}

func TestInitDeniedCreateExplainsAdopt(t *testing.T) {
	h := newHarness(t)
	h.forge.CreateForbidden = true
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	ce := wantCode(t, err, contract.ShadowCreateDenied)
	if res == nil || res.CreatedShadow {
		t.Fatalf("a denied create must not report created_shadow: %+v", res)
	}
	steps, _ := ce.Details["steps"].([]string)
	if len(steps) != 3 || !strings.Contains(steps[0], "gh repo create acme/widget-shadow --private") ||
		!strings.Contains(steps[2], "ghostbranch init acme/widget --adopt") || ce.Details["shadow"] != shadowName {
		t.Fatalf("details = %+v", ce.Details)
	}
	// the person creates it; adopt then works
	h.forge.CreateForbidden = false
	if _, err := h.forge.AddRepo(shadowName, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
}

func TestInitWritesNoSyncWorkflowByDefaultAndAudits(t *testing.T) {
	h := newHarness(t)
	res, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.shadowGit().ShowFile(h.ctx, "refs/heads/"+engine.OverlayBranch, managed.SyncPath); ok {
		t.Fatal("init installed the sync workflow although sync.schedule is off")
	}
	if res.Audit == nil || res.Audit.Overlay[0].Status != managed.Current {
		t.Fatalf("audit = %+v", res.Audit)
	}
	if codes(warns)[contract.ActionsMinutesBilled] != 1 {
		t.Fatalf("warnings = %+v", warns)
	}
}

func TestInitOnACompletePairWarnsWhenTheOverlayIsOutdated(t *testing.T) {
	h := newHarness(t)
	h.init()
	h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
	_, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil {
		t.Fatal(err)
	}
	if codes(warns)[contract.OverlayOutdated] != 1 {
		t.Fatalf("warnings = %+v", warns)
	}
}
