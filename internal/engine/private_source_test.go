package engine_test

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

// agents allow names the Ghostbranch home the grant is saved in: with two
// homes (a sandbox GHOSTBRANCH_HOME and the default) a grant made in the
// wrong one is otherwise invisible.
func TestAgentsAllowNamesTheHome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if _, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(h.approver.asked) != 1 || !strings.Contains(h.approver.asked[0], "saved in "+h.eng.WS.GrantsPath()) {
		t.Fatalf("summary = %v", h.approver.asked)
	}
}

// --yes with no grant and no console names the home it looked in.
func TestYesWithoutAGrantNamesTheHome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.Approver = nil
	err := engine.AuthorizeForTest(h.eng, h.ctx, h.pair, "20261123T000000Z-abcdef", true)
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.ApprovalRequired || ce.Details["reason"] != string(approval.NoConsole) ||
		!strings.Contains(ce.Message, "no agent grant for "+publicName+" in "+h.eng.WS.GrantsPath()) {
		t.Fatalf("err = %v", err)
	}
	// Without --yes the message is as before.
	err = engine.AuthorizeForTest(h.eng, h.ctx, h.pair, "20261123T000000Z-abcdef", false)
	if ce := contract.From(err); ce == nil || strings.Contains(ce.Message, "no agent grant") {
		t.Fatalf("err = %v", err)
	}
}

// A private source repo is a supported setup: init's warning and doctor's
// pair check say so, name its real visibility, and say scheduled sync
// isn't available.
func TestPrivateSourceRepoWording(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.forge.SetPrivate(publicName, true)
	_, warns, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName})
	if err != nil {
		t.Fatal(err)
	}
	var msg string
	for _, w := range warns {
		if w.Code == contract.PublicNotPublic {
			msg = w.Message
		}
	}
	const want = "acme/widget is private: that's supported. Its private branches live in acme/widget-shadow, a separate private repo, so only people with access to it can see them. Scheduled sync isn't supported for a private source repo (a token that could read it could also write it), so run ghostbranch sync yourself"
	if msg != want {
		t.Fatalf("init warning %q", msg)
	}
	// doctor's pair check passes for it: a supported setup isn't a warning.
	_, c := h.doctor(doctorOpts())
	pair := c["pair"].Message
	wantStatus(t, c["pair"], engine.DoctorOK)
	if !strings.Contains(pair, "acme/widget (private) and acme/widget-shadow (private) are reachable") ||
		!strings.HasSuffix(pair, "; private source repo: run ghostbranch sync yourself (scheduled sync isn't supported)") ||
		strings.Contains(pair, "meant for public repos") {
		t.Fatalf("doctor pair %q", pair)
	}
}

// init --sync is refused for a private source repo, before anything is
// created: scheduled sync isn't supported there.
func TestInitRefusesScheduledSyncForAPrivateSource(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.forge.SetPrivate(publicName, true)
	_, _, err := h.eng.Init(h.ctx, engine.InitOptions{Public: publicName, Schedule: engine.DefaultSyncSchedule})
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.BadFlag || ce.Field != "sync" ||
		!strings.Contains(ce.Message, "scheduled sync isn't supported for a private source repo") {
		t.Fatalf("err = %v", err)
	}
	if _, ok := h.eng.WS.Find(publicName); ok || len(h.pushes) != 0 {
		t.Fatalf("registered %v, pushes %v", ok, h.pushes)
	}
}
