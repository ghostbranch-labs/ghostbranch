package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func TestInitCreatesShadowMirrorsAndOverlay(t *testing.T) {
	t.Parallel()
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

// Init registers its pair through workspace.Update: a pair another run
// registered after this engine loaded workspace.json is kept.
func TestInitKeepsAPairAnotherRunRegistered(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	other, err := workspace.Open(filepath.Dir(filepath.Dir(h.eng.WS.Dir)), h.eng.WS.Name)
	if err != nil {
		t.Fatal(err)
	}
	other.AddPair(workspace.Pair{Public: "acme/other", Shadow: "acme/other-shadow"})
	if err := other.Save(); err != nil {
		t.Fatal(err)
	}
	h.init()
	fresh, err := workspace.Open(filepath.Dir(filepath.Dir(h.eng.WS.Dir)), h.eng.WS.Name)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []*workspace.Workspace{fresh, h.eng.WS} {
		_, okA := ws.Find(publicName)
		_, okB := ws.Find("acme/other")
		if !okA || !okB {
			t.Fatalf("pairs = %+v", ws.Pairs)
		}
	}
}

func TestInitIsResumable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	n := len(h.pushes)
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || !res.Resumed || res.CreatedShadow || res.OverlayCreated || len(h.pushes) != n {
		t.Fatalf("re-init = %+v %v (pushes %d -> %d)", res, err, n, len(h.pushes))
	}
}

func TestInitNeedsAdoptForAnExistingShadow(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t)
	testutil.Commit(t, h.pubGit, "main", "oops", map[string]string{".ghostbranch.yml": "x"})
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	_ = wantCode(t, err, contract.OverlayCollision)
}

func TestInitDryRunWritesNothing(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	h := newHarness(t)
	h.forge.SetPrivate(publicName, true)
	_, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if c := codes(warns); err != nil || len(warns) != 2 || c[contract.PublicNotPublic] != 1 || c[contract.ActionsMinutesBilled] != 1 {
		t.Fatalf("warns = %v err = %v", warns, err)
	}
}

func TestInitDeniedCreateExplainsAdopt(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// adoptable makes the shadow repo hold commit sha as main, the way a
// partial mirror does, and returns the shadow repo's git.
func (h *harness) adoptable(sha string) *gitx.Git {
	h.t.Helper()
	repo, err := h.forge.CreatePrivateRepo(h.ctx, shadowName)
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.pubGit.Push(h.ctx, repo.CloneURL, gitx.PushOptions{}, sha+":refs/heads/main"); err != nil {
		h.t.Fatal(err)
	}
	return gitx.New(repo.CloneURL)
}

// A tag the public repo lacks is unrelated content.
func TestAdoptRefusesAShadowTagNotInPublic(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	sh := h.adoptable(pubMain)
	if _, err := sh.Run(h.ctx, "update-ref", "refs/tags/v9", pubMain); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	ce := wantCode(t, err, contract.ShadowNotEmpty)
	if !strings.Contains(ce.Message, "v9") || len(h.pushes) != 0 {
		t.Fatalf("%+v (pushes %d)", ce, len(h.pushes))
	}
	// The same tag in public is a mirror, so it adopts.
	if _, err := h.pubGit.Run(h.ctx, "update-ref", "refs/tags/v9", pubMain); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true}); err != nil {
		t.Fatalf("adopting a mirrored tag: %v", err)
	}
}

// A git failure while comparing a shadow branch with public is INTERNAL,
// not a verdict on the repo's content.
func TestAdoptAncestryFailureIsInternal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	pubMain := testutil.Commit(t, h.pubGit, "main", "second", map[string]string{"two.txt": "2"})
	h.adoptable(old)
	err := h.eng.AdoptCheckForTest(h.ctx, h.pair, func(mirror string) {
		// A graft to a missing parent makes git fail walking public main.
		graft := pubMain + " " + strings.Repeat("1", 40) + string(rune(10))
		if err := os.WriteFile(filepath.Join(mirror, "info", "grafts"), []byte(graft), 0o644); err != nil {
			t.Fatal(err)
		}
	})
	_ = wantCode(t, err, contract.Internal)
}

// Adopting a partial mirror whose main is behind public fast-forwards it,
// leased on the old head.
func TestAdoptFastForwardsAStaleShadowMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	pubMain := testutil.Commit(t, h.pubGit, "main", "second", map[string]string{"two.txt": "2"})
	sh := h.adoptable(old)
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	if err != nil || !res.Adopted {
		t.Fatalf("adopt = %+v %v", res, err)
	}
	if got, _ := sh.MustResolve(h.ctx, "refs/heads/main"); got != pubMain {
		t.Fatalf("shadow main = %s, want %s", got, pubMain)
	}
	if !slices.Contains(res.MirroredBranches, "main") {
		t.Fatalf("mirrored = %v", res.MirroredBranches)
	}
	found := false
	for _, p := range h.pushes {
		found = found || slices.Contains(p.refspecs, pubMain+":refs/heads/main")
	}
	if !found {
		t.Fatalf("pushes = %+v", h.pushes)
	}
}

// A public branch named ghostbranch would collide with the overlay branch:
// refused up front, before any write, creating or adopting.
func TestInitRefusesAPublicGhostbranchBranch(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"ghostbranch", "ghostbranch/x"} {
		h := newHarness(t)
		testutil.Commit(t, h.pubGit, branch, "clash", map[string]string{"c.txt": "c"})
		h.eng.DryRun = true
		_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		h.eng.DryRun = false
		if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "public" {
			t.Fatalf("%s: --plan: %+v", branch, ce)
		}
		_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		ce := wantCode(t, err, contract.NameInvalid)
		if ce.Field != "public" || !strings.Contains(ce.Message, branch) || !strings.Contains(ce.Message, "overlay") {
			t.Fatalf("%s: %+v", branch, ce)
		}
		if _, err := h.forge.GetRepo(h.ctx, shadowName); !errors.Is(err, forge.ErrNotFound) || len(h.pushes) != 0 {
			t.Fatalf("%s: init wrote something before refusing", branch)
		}
		pubMain, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
		h.adoptable(pubMain)
		_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
		if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "public" || len(h.pushes) != 0 {
			t.Fatalf("%s: adopt: %+v", branch, ce)
		}
	}
}

// A registered pair keeps its shadow repo: a different --shadow is refused
// with the way to switch.
func TestInitRefusesAnotherShadowForARegisteredPair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	n := len(h.pushes)
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Shadow: "other/name"})
	ce := wantCode(t, err, contract.BadFlag)
	if ce.Field != "shadow" || !strings.Contains(ce.Message, "ghostbranch remove --repo "+publicName) ||
		!strings.Contains(ce.Message, shadowName) || len(h.pushes) != n {
		t.Fatalf("%+v", ce)
	}
	if _, err := h.forge.GetRepo(h.ctx, "other/name"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatal("init created the other shadow repo")
	}
	// Naming the registered shadow, in any case, still resumes.
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Shadow: "ACME/widget-shadow"}); err != nil {
		t.Fatal(err)
	}
}

// An overlay config key this binary doesn't know means a newer Ghostbranch
// set the pair up: UPGRADE_NEEDED from every command that reads the config,
// and from an adopt.
func TestUnknownConfigKeyNeedsAnUpgrade(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setConfig("promote:", "promote:"+string(rune(10))+"  future_key: x")
	check := func(what string, err error) {
		t.Helper()
		ce := wantCode(t, err, contract.UpgradeNeeded)
		if !strings.Contains(ce.Message, "promote.future_key") || !strings.Contains(ce.Message, "or fix it if it's a typo") ||
			ce.Details["unknown_key"] != "promote.future_key" {
			t.Fatalf("%s: %+v", what, ce)
		}
	}
	_, _, err := h.eng.Audit(h.ctx, h.pair)
	check("audit", err)
	_, _, err = h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	check("feature start", err)
	_, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	check("sync", err)
	h.eng.WS.RemovePair(publicName)
	_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	check("adopt", err)
}

// The adopt's fast-forward is leased on the shadow main it saw: if shadow
// main moves before the push, the push fails with REF_MOVED and changes
// nothing.
func TestAdoptFastForwardIsLeased(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	old, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	testutil.Commit(t, h.pubGit, "main", "second", map[string]string{"two.txt": "2"})
	sh := h.adoptable(old)
	record := h.eng.OnPush
	moved := ""
	h.eng.OnPush = func(op, remote string, specs []string) {
		if moved == "" {
			moved = testutil.Commit(t, sh, "main", "racing", map[string]string{"race.txt": "r"})
		}
		record(op, remote, specs)
	}
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	_ = wantCode(t, err, contract.RefMoved)
	refs, err := sh.Refs(h.ctx, "refs")
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs["refs/heads/main"] != moved {
		t.Fatalf("the shadow repo changed: %v (main should be %s)", refs, moved)
	}
	if _, ok := h.eng.WS.Find(publicName); ok {
		t.Fatal("a failed adopt registered the pair")
	}
}
