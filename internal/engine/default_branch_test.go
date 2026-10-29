package engine_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
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
	h := newHarnessOn(t, "trunk")
	h.init()
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
	// doctor reports it as a failed pair check rather than stopping.
	h.asApp()
	_, c := h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorFail)
	if !strings.Contains(c["pair"].Message, "no longer has the branch trunk") || c["pair"].Fix != "ghostbranch explain BASE_BRANCH_CHANGED" {
		t.Fatalf("pair check: %+v", c["pair"])
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
		if len(h.pushes) != 0 || len(h.forge.DefaultBranchSets) != 0 || !maps.Equal(after, refs) {
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

// A hand-edited plan can't publish to the base branch, though GitHub's
// default has moved to another branch: apply re-checks every target with
// the pair's base, its own and each chain entry's.
func TestApplyRefusesAPlanTargetingTheBase(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	testutil.Commit(t, h.pubGit, "develop", "develop work", map[string]string{"dev.txt": "dev\n"})
	if err := h.forge.SetDefaultBranch(h.ctx, publicName, "develop"); err != nil {
		t.Fatal(err)
	}
	stack := []plan.Entry{{Feature: "feat-x", Publish: plan.Publish{TargetBranch: "feat-x"}}, {Feature: "feat-y", Publish: plan.Publish{TargetBranch: "trunk"}}}
	for i, tc := range []struct {
		target string
		chain  []plan.Entry
	}{{"trunk", nil}, {"feat-x", stack}} {
		id := "20261029T120000Z-00000" + string(rune('a'+i))
		pl := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: id, Pair: plan.Pair{Public: publicName, Shadow: shadowName},
			Feature: "feat-x", Mode: "squash", Publish: plan.Publish{TargetBranch: tc.target}, Chain: tc.chain}
		if _, err := plan.Save(h.eng.WS.PlansDir(), pl); err != nil {
			t.Fatal(err)
		}
		before := len(h.pushes)
		_, _, err := h.eng.Apply(h.ctx, engine.ApplyOptions{Plan: id, Yes: true})
		ce := wantCode(t, err, contract.NameInvalid)
		if ce.Field != "plan" || !strings.Contains(ce.Message, `"trunk"`) || len(h.pushes) != before {
			t.Fatalf("plan %d: %+v (pushes %+v)", i, ce, h.pushes[before:])
		}
	}
}

// prBase is the base of the newest PR in full whose head is head, "" when
// there is none.
func (h *harness) prBase(full, head string) string {
	h.t.Helper()
	base := ""
	for _, pr := range h.forge.PRs(full) {
		if pr.Head == head {
			base = pr.Base
		}
	}
	return base
}

// noPRBase fails the test if any PR in either repo has base branch.
func (h *harness) noPRBase(branch string) {
	h.t.Helper()
	for _, full := range []string{publicName, shadowName} {
		for _, pr := range h.forge.PRs(full) {
			if pr.Base == branch {
				h.t.Fatalf("%s PR #%d (%s) has base %s", full, pr.Number, pr.Head, branch)
			}
		}
	}
}

// noPushTo fails the test if any push from the from'th on writes ref.
func (h *harness) noPushTo(from int, ref string) {
	h.t.Helper()
	for _, p := range h.pushes[from:] {
		for _, s := range p.refspecs {
			if strings.HasSuffix(s, ":"+ref) {
				h.t.Fatalf("%s pushed %s", p.op, s)
			}
		}
	}
}

// loopOn runs one feature through the whole loop on a pair whose base
// branch is base: start, gate, promote --plan and apply; then GitHub merges
// the public PR, landing the published commit on base, and sync closes the
// feature. Every PR base and the record's base must be base.
func (h *harness) loopOn(base, name string) {
	h.t.Helper()
	h.startFeature(name, map[string]string{name + ".txt": name + "\n"})
	h.gateFeature(name)
	if got := h.prBase(shadowName, "ghostbranch-gate/"+name); got != base {
		h.t.Fatalf("gate PR base = %q, want %s", got, base)
	}
	pres, err := promote(h, engine.PromoteOptions{Feature: name, Mode: "squash", Message: "Add " + name, PlanOnly: true})
	if err != nil || pres.Plan == nil || len(pres.PRs) != 1 || pres.PRs[0].Base != base {
		h.t.Fatalf("promote --plan = %+v %v", pres, err)
	}
	ares, _ := h.applyPlan(engine.ApplyOptions{Plan: pres.Plan.ID, Yes: true})
	if len(ares.PRs) != 1 || ares.PRs[0].Base != base {
		h.t.Fatalf("apply PRs = %+v", ares.PRs)
	}
	if got := h.prBase(publicName, name); got != base {
		h.t.Fatalf("public PR base = %q, want %s", got, base)
	}
	if m := h.meta(name); m.Promoted == nil || m.Promoted.Base != base {
		h.t.Fatalf("promoted = %+v", m.Promoted)
	}
	h.forge.SetPRMerged(publicName, ares.PublicPR)
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/"+base, h.pubTip(name)); err != nil {
		h.t.Fatal(err)
	}
	res, _ := h.syncOK(engine.SyncOptions{})
	if len(res.Closed) != 1 || res.Closed[0].Name != name {
		h.t.Fatalf("closed = %+v", res.Closed)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/"+base); got != h.pubTip(base) {
		h.t.Fatalf("shadow %s = %s, want public %s", base, got, h.pubTip(base))
	}
}

// The whole single-feature loop on a repo whose only branch is trunk.
func TestDefaultBranchLoop(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	h.loopOn("trunk", "feat-a")
	h.noPRBase("main")
	h.noPushTo(0, "refs/heads/main")
}

// Review Focus 1: a stale main next to a default master may be mirrored,
// but nothing targets it: no PR base, push, comparison or sync.
func TestDefaultBranchStaleMainIsNeverTargeted(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "master")
	if err := h.pubGit.UpdateRef(h.ctx, "refs/heads/main", h.pubTip("master")); err != nil {
		t.Fatal(err)
	}
	testutil.Commit(t, h.pubGit, "master", "master only", map[string]string{".github/workflows/extra.yml": "name: extra\non: [pull_request]\n" +
		"jobs:\n  t:\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo ${{ secrets.MASTER_ONLY }}\n"})
	h.init()
	before := len(h.pushes)
	h.loopOn("master", "feat-a")

	// audit reads master's workflows, not stale main's.
	ares, _, err := h.eng.Audit(h.ctx, h.pair)
	if err != nil || !slices.ContainsFunc(ares.Secrets.Referenced, func(r engine.SecretRef) bool { return r.Name == "MASTER_ONLY" }) {
		t.Fatalf("audit = %+v %v", ares, err)
	}
	// doctor and status count shadow master behind public master.
	tip := testutil.Commit(t, h.pubGit, "master", "upstream", map[string]string{"up.txt": "1\n"})
	h.asApp()
	dres, c := h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorWarn)
	if dres.BaseBranch != "master" || !strings.Contains(c["pair"].Message, "shadow master is 1 commit(s) behind public master") {
		t.Fatalf("doctor: %q %+v", dres.BaseBranch, c["pair"])
	}
	st, warns := h.status(contract.ShadowBehind)
	if st.PublicMain != tip || !st.ShadowBehind || warns[0].Details["behind"] != 1 ||
		warns[0].Message != "shadow master is 1 commit(s) behind public master; run ghostbranch sync --repo acme/widget" {
		t.Fatalf("status = %+v %+v", st, warns)
	}
	// sync.branches is [master]: public main moving doesn't move shadow main.
	staleShadow, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main")
	testutil.Commit(t, h.pubGit, "main", "main moves", map[string]string{"m.txt": "1\n"})
	res, _ := h.syncOK(engine.SyncOptions{})
	if res.PublicMain != tip || res.ShadowMainAfter != tip || len(res.Branches) != 1 || res.Branches[0].Name != "master" {
		t.Fatalf("sync = %+v", res)
	}
	if got, _ := h.shadowGit().MustResolve(h.ctx, "refs/heads/main"); got != staleShadow {
		t.Fatalf("shadow main moved: %s, was %s", got, staleShadow)
	}
	h.noPRBase("main")
	h.noPushTo(before, "refs/heads/main")
}

// doctor's pair check names the base branch: ok on a trunk pair, failed
// once trunk is gone, and a warning on a pair kept on main whose public
// default is now develop.
func TestDoctorBaseBranch(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	h.asApp()
	res, c := h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorOK)
	if res.BaseBranch != "trunk" {
		t.Fatalf("base_branch = %q", res.BaseBranch)
	}
	if err := h.forge.RenameBranch(publicName, "trunk", "main"); err != nil {
		t.Fatal(err)
	}
	res, c = h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorFail)
	if res.BaseBranch != "trunk" || !strings.Contains(c["pair"].Message, "acme/widget no longer has the branch trunk this pair was set up on (its default branch is now main)") ||
		c["pair"].Fix != "ghostbranch explain BASE_BRANCH_CHANGED" {
		t.Fatalf("renamed: %q %+v", res.BaseBranch, c["pair"])
	}

	k := newHarness(t)
	k.init()
	testutil.Commit(t, k.pubGit, "develop", "develop work", map[string]string{"dev.txt": "dev\n"})
	if err := k.forge.SetDefaultBranch(k.ctx, publicName, "develop"); err != nil {
		t.Fatal(err)
	}
	k.asApp()
	res, c = k.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorWarn)
	if res.BaseBranch != "main" || !strings.Contains(c["pair"].Message, "develop") || !strings.Contains(c["pair"].Message, "main") {
		t.Fatalf("kept pair: %q %+v", res.BaseBranch, c["pair"])
	}
}

// A feature can't take the base branch's name, under --plan too.
func TestFeatureStartRefusesBaseName(t *testing.T) {
	t.Parallel()
	h := newHarnessOn(t, "trunk")
	h.init()
	for _, dry := range []bool{true, false} {
		h.eng.DryRun = dry
		_, _, err := h.eng.FeatureStart(h.ctx, h.pair, "trunk")
		if ce := wantCode(t, err, contract.NameInvalid); ce.Field != "name" || !strings.Contains(ce.Message, `"trunk"`) {
			t.Fatalf("dry run %v: %+v", dry, ce)
		}
	}
}
