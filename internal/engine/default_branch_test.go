package engine_test

import (
	"errors"
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
)

// retargetTo makes an initialised main pair's base branch branch by hand,
// until init sets such pairs up itself: a public branch at main's tip, made
// the public repo's default, and named in the overlay config.
func (h *harness) retargetTo(branch string) {
	h.t.Helper()
	tip, err := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/"+branch, tip); err != nil {
		h.t.Fatal(err)
	}
	if err := h.forge.SetDefaultBranch(h.ctx, publicName, branch); err != nil {
		h.t.Fatal(err)
	}
	h.setConfig("shadow: "+shadowName, "shadow: "+shadowName+"\npublic_branch: "+branch)
	h.setConfig("branches: [main]", "branches: ["+branch+"]")
}

func baseOf(h *harness) string {
	h.t.Helper()
	base, err := engine.BaseOf(h.eng, h.ctx, h.pair)
	if err != nil {
		h.t.Fatalf("base: %v", err)
	}
	return base
}

func TestOpenReadsBase(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.retargetTo("trunk")
	if got := baseOf(h); got != "trunk" {
		t.Fatalf("base = %q, want trunk", got)
	}
}

func TestOpenMissingKeyMeansMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if got := baseOf(h); got != "main" {
		t.Fatalf("base = %q, want main", got)
	}
}

// A renamed default branch removes the recorded one: every command stops
// before it reaches the missing ref.
func TestBaseBranchChangedStopsEveryCommand(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.retargetTo("trunk")
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"}) // so gate gets as far as open
	if err := h.forge.RenameBranch(publicName, "trunk", "next"); err != nil {
		t.Fatal(err)
	}
	before := len(h.pushes)
	check := func(what string, err error) {
		t.Helper()
		ce := wantCode(t, err, contract.BaseBranchChanged)
		if ce.Details["recorded"] != "trunk" || ce.Details["current"] != "next" || ce.Code.Exit() != contract.ExitUsage || ce.Retryable {
			t.Fatalf("%s: %+v (exit %d)", what, ce, ce.Code.Exit())
		}
		want := "acme/widget no longer has the branch trunk this pair was set up on (its default branch is now next); " +
			"Ghostbranch doesn't follow a renamed default branch yet. See ghostbranch explain BASE_BRANCH_CHANGED."
		if ce.Message != want {
			t.Fatalf("%s: message %q", what, ce.Message)
		}
	}
	_, _, err := h.eng.Status(h.ctx, h.pair)
	check("status", err)
	_, _, err = h.eng.FeatureStart(h.ctx, h.pair, "feat-y")
	check("feature start", err)
	_, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	check("gate", err)
	_, _, err = h.eng.Sync(h.ctx, h.pair, engine.SyncOptions{})
	check("sync", err)
	if len(h.pushes) != before {
		t.Fatalf("pushes after the rename: %+v", h.pushes[before:])
	}
	// doctor reports it rather than stopping.
	h.asApp()
	res, err := h.eng.Doctor(h.ctx, doctorOpts())
	if codeOf(err) == contract.BaseBranchChanged || res == nil {
		t.Fatalf("doctor: %v", err)
	}
}

// A pair on main keeps working when GitHub's default is another branch
// (git-flow: default develop, releases on main).
func TestKeptPairOnMainWhenDefaultDiffers(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	tip, err := h.pubGit.MustResolve(h.ctx, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.pubGit, "develop", "develop work", map[string]string{"dev.txt": "dev\n"})
	if err := h.forge.SetDefaultBranch(h.ctx, publicName, "develop"); err != nil {
		t.Fatal(err)
	}
	if now, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/main"); now != tip {
		t.Fatal("main moved")
	}
	if _, _, err := h.eng.Status(h.ctx, h.pair); err != nil {
		t.Fatalf("status: %v", err)
	}
	if got := baseOf(h); got != "main" {
		t.Fatalf("base = %q, want main", got)
	}
}

// An overlay config that doesn't parse stops the command; open never
// guesses main, which would act on the wrong branch. doctor still runs and
// reports it.
func TestOpenConfigUnreadableDoesNotGuessMain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.retargetTo("trunk")
	testutil.Commit(t, h.shadowGit(), engine.OverlayBranch, "break the config",
		map[string]string{".ghostbranch.yml": "version: 1\npublic: [acme/widget\n"})
	before := len(h.pushes)
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.ConfigInvalid)
	if len(h.pushes) != before {
		t.Fatalf("pushes: %+v", h.pushes[before:])
	}
	if _, err := engine.BaseOf(h.eng, h.ctx, h.pair); codeOf(err) != contract.ConfigInvalid {
		t.Fatalf("base: %v", err)
	}
	h.asApp()
	_, c := h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorFail)
	if !strings.Contains(c["pair"].Message, "the overlay's .ghostbranch.yml") {
		t.Fatalf("pair check: %s", c["pair"].Message)
	}
}

// overlayConfig is the overlay's .ghostbranch.yml in the shadow repo.
func (h *harness) overlayConfig() string {
	h.t.Helper()
	raw, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/heads/"+engine.OverlayBranch, ".ghostbranch.yml")
	if err != nil || !ok {
		h.t.Fatalf("read config: %v", err)
	}
	return string(raw)
}

// overlayHead is the shadow repo's overlay branch.
func (h *harness) overlayHead() string {
	h.t.Helper()
	sha, err := h.shadowGit().MustResolve(h.ctx, "refs/heads/"+engine.OverlayBranch)
	if err != nil {
		h.t.Fatal(err)
	}
	return sha
}

// readopt drops the pair's registration and adopts its shadow repo again,
// as a second machine does.
func (h *harness) readopt() (*engine.InitResult, error) {
	h.t.Helper()
	h.eng.WS.RemovePair(publicName)
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	return res, err
}

// An empty repo is refused on its own default branch.
func TestInitEmptyNonMainDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir, err := h.forge.AddRepo("acme/empty", false)
	if err != nil {
		t.Fatal(err)
	}
	// The fake can only make an existing branch the default: make one, then
	// delete it, leaving an empty repo whose default is trunk.
	g := gitx.New(dir)
	testutil.Commit(t, g, "trunk", "initial", map[string]string{"README.md": "x\n"})
	if err := h.forge.SetDefaultBranch(h.ctx, "acme/empty", "trunk"); err != nil {
		t.Fatal(err)
	}
	if err := g.DeleteRef(h.ctx, "refs/heads/trunk"); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: "acme/empty"})
	ce := wantCode(t, err, contract.BadFlag)
	if ce.Field != "public" || ce.Details["branch"] != "trunk" ||
		!strings.HasPrefix(ce.Message, "acme/empty has no commits on trunk yet: push a first commit") {
		t.Fatalf("%+v", ce)
	}
	if _, err := h.forge.GetRepo(h.ctx, "acme/empty-shadow"); !errors.Is(err, forge.ErrNotFound) || len(h.pushes) != 0 {
		t.Fatalf("init wrote something before refusing: %v %v", err, h.pushes)
	}
}

// A main pair's config is the file earlier versions wrote: no
// public_branch key.
func TestInitMainPairUnchanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil || res.BaseBranch != "main" {
		t.Fatalf("init: %+v %v", res, err)
	}
	if got, want := h.overlayConfig(), string(config.Render(publicName, shadowName, "main")); got != want {
		t.Fatalf("config:\n%s\nwant:\n%s", got, want)
	}
}

// --adopt keeps the branch the overlay records, and never rewrites it.
func TestAdoptKeepsRecordedBranch(t *testing.T) {
	t.Parallel()
	t.Run("trunk pair", func(t *testing.T) {
		t.Parallel()
		h := newHarnessOn(t, "trunk")
		h.init()
		O := h.overlayHead()
		res, err := h.readopt()
		if err != nil || !res.Adopted || res.BaseBranch != "trunk" {
			t.Fatalf("adopt: %+v %v", res, err)
		}
		if h.overlayHead() != O {
			t.Fatal("adopt rewrote the overlay")
		}
	})
	// A pair set up on main before 8.12, whose public default is now
	// develop: it stays on main.
	t.Run("kept pair", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.init()
		testutil.Commit(t, h.pubGit, "develop", "develop work", map[string]string{"dev.txt": "dev\n"})
		if err := h.forge.SetDefaultBranch(h.ctx, publicName, "develop"); err != nil {
			t.Fatal(err)
		}
		O := h.overlayHead()
		res, err := h.readopt()
		if err != nil || !res.Adopted || res.BaseBranch != "main" {
			t.Fatalf("adopt: %+v %v", res, err)
		}
		if h.overlayHead() != O {
			t.Fatal("adopt rewrote the overlay")
		}
	})
}

// A recorded branch the public repo no longer has stops init, resuming or
// adopting, before it writes anything.
func TestAdoptStopsWhenRecordedBranchGone(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	if err := h.forge.RenameBranch(publicName, "trunk", "next"); err != nil {
		t.Fatal(err)
	}
	before, sets := len(h.pushes), len(h.forge.DefaultBranchSets)
	check := func(what string, err error) {
		t.Helper()
		ce := wantCode(t, err, contract.BaseBranchChanged)
		if ce.Details["recorded"] != "trunk" || ce.Details["current"] != "next" ||
			!strings.HasPrefix(ce.Message, "acme/widget no longer has the branch trunk this pair was set up on") {
			t.Fatalf("%s: %+v", what, ce)
		}
		if len(h.pushes) != before || len(h.forge.DefaultBranchSets) != sets {
			t.Fatalf("%s: wrote %+v", what, h.pushes[before:])
		}
	}
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	check("resume", err)
	_, err = h.readopt()
	check("adopt", err)
	if _, ok := h.eng.WS.Find(publicName); ok {
		t.Fatal("the stopped adopt registered the pair")
	}
}

// Adopting a partial mirror fast-forwards the shadow's copy of the base
// branch, leased on its old head.
func TestAdoptFastForwardsAStaleShadowBase(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	old, _ := h.pubGit.MustResolve(h.ctx, "refs/heads/trunk")
	tip := testutil.Commit(t, h.pubGit, "trunk", "second", map[string]string{"two.txt": "2"})
	repo, err := h.forge.CreatePrivateRepo(h.ctx, shadowName)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.pubGit.Push(h.ctx, repo.CloneURL, gitx.PushOptions{}, old+":refs/heads/trunk"); err != nil {
		t.Fatal(err)
	}
	res, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
	if err != nil || !res.Adopted || !slices.Contains(res.MirroredBranches, "trunk") {
		t.Fatalf("adopt = %+v %v", res, err)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/trunk"); got != tip {
		t.Fatalf("shadow trunk = %s, want %s", got, tip)
	}
	found := false
	for _, p := range h.pushes {
		found = found || slices.Contains(p.refspecs, tip+":refs/heads/trunk")
	}
	if !found {
		t.Fatalf("pushes = %+v", h.pushes)
	}
}

// init --sync on a trunk repo renders the sync workflow for trunk.
func TestInitScheduleRendersBase(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	if _, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Schedule: engine.DefaultSyncSchedule}); err != nil {
		t.Fatal(err)
	}
	body, ok, err := h.shadowGit().ShowFile(h.ctx, "refs/heads/"+engine.OverlayBranch, managed.SyncPath)
	if err != nil || !ok || !strings.Contains(string(body), `BRANCHES: "trunk"`) {
		t.Fatalf("sync workflow (%v %v):\n%s", ok, err, body)
	}
	if cfg := h.overlayConfig(); !strings.Contains(cfg, "branches: [trunk]") {
		t.Fatalf("config:\n%s", cfg)
	}
}

// An overlay init can't read, or one for another repo, is refused before
// any write, though its branch can't be read or isn't this pair's.
func TestAdoptWithAForeignOrUnreadableOverlayWritesNothing(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]string{
		"unreadable": "version: 1\npublic: [acme/widget\n",
		"foreign":    string(config.Render("acme/other", "acme/other-shadow", "gone")),
	} {
		h := newHarnessOn(t, "trunk")
		repo, err := h.forge.CreatePrivateRepo(h.ctx, shadowName)
		if err != nil {
			t.Fatal(err)
		}
		sh := gitx.New(repo.CloneURL)
		testutil.Commit(t, sh, engine.OverlayBranch, "someone else's", map[string]string{".ghostbranch.yml": cfg})
		refs, _ := sh.Refs(h.ctx, "refs")
		_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
		_ = wantCode(t, err, contract.ShadowNotEmpty)
		after, _ := sh.Refs(h.ctx, "refs")
		if len(h.pushes) != 0 || len(h.forge.DefaultBranchSets) != 0 || len(after) != len(refs) {
			t.Fatalf("%s: wrote %+v %v %v", name, h.pushes, h.forge.DefaultBranchSets, after)
		}
		if _, ok := h.eng.WS.Find(publicName); ok {
			t.Fatalf("%s: registered", name)
		}
	}
}

// A default branch the sync workflow can't carry, or named like a gate
// branch, can't be a base branch: init refuses before anything is written,
// creating or adopting.
func TestInitRefusesADefaultBranchItCantUse(t *testing.T) {
	t.Parallel()
	for _, branch := range []string{"ghostbranch-x", "feat+x"} {
		h := newHarnessOn(t, branch)
		check := func(what string, err error) {
			t.Helper()
			ce := wantCode(t, err, contract.NameInvalid)
			if ce.Field != "public" || ce.Details["branch"] != branch || !strings.Contains(ce.Message, branch) {
				t.Fatalf("%s %s: %+v", branch, what, ce)
			}
			if len(h.pushes) != 0 || len(h.forge.DefaultBranchSets) != 0 {
				t.Fatalf("%s %s: wrote %+v", branch, what, h.pushes)
			}
		}
		h.eng.DryRun = true
		_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		check("--plan", err)
		h.eng.DryRun = false
		_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
		check("init", err)
		if _, err := h.forge.GetRepo(h.ctx, shadowName); !errors.Is(err, forge.ErrNotFound) {
			t.Fatalf("%s: a shadow repo was created: %v", branch, err)
		}
		if _, err := h.forge.CreatePrivateRepo(h.ctx, shadowName); err != nil {
			t.Fatal(err)
		}
		_, _, err = h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Adopt: true})
		check("adopt", err)
	}
}

// A feature can't take the base branch's name, nor publish to it.
func TestFeatureNameIsNotTheBase(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	before := len(h.pushes)
	_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "trunk")
	if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "name" || len(h.pushes) != before {
		t.Fatalf("start: %+v", ce)
	}
	// Refused once the pair is read, before promote reads the feature.
	_, _, err = h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Branch: "trunk", Mode: "squash", Message: "m", PlanOnly: true})
	if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "branch" || len(h.pushes) != before {
		t.Fatalf("promote: %+v", ce)
	}
}
