package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
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
