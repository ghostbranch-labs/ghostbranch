package engine_test

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func lastEntry(h *harness) (op, code string) {
	es := h.ledger()
	if len(es) == 0 {
		return "", ""
	}
	return es[len(es)-1].Op, es[len(es)-1].ErrorCode
}

func TestAgentsAllowNeedsAPersonAtAConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.Approver = nil
	_, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{})
	if ce := wantCode(t, err, contract.ApprovalRequired); ce.Details["reason"] != "no_console" || ce.Details["plan_id"] != nil ||
		ce.Message != "making an agent grant needs a person to type a code at a console" {
		t.Fatalf("details = %v", ce.Details)
	}
	if gs, _ := h.eng.Grants.Load(); len(gs) != 0 {
		t.Fatal("no grant may be stored without a person")
	}
	if op, code := lastEntry(h); op != "agents allow" || code != "APPROVAL_REQUIRED" {
		t.Fatalf("ledger = %s %s", op, code)
	}
}

func TestAgentsAllowAtAConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	now := h.clock.Now()
	res, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{})
	if err != nil || res.Grant.ExpiresAt == nil || !res.Grant.ExpiresAt.Equal(now.Add(6*time.Hour)) || res.Replaced != nil || res.Grant.CreatedBy != "human:octo" {
		t.Fatalf("allow = %+v %v", res, err)
	}
	if len(h.approver.asked) != 1 || !strings.Contains(h.approver.asked[0], publicName) || !strings.Contains(h.approver.asked[0], "6h") {
		t.Fatalf("summary = %v", h.approver.asked)
	}
	if _, ok, _ := h.eng.Grants.Valid(publicName, shadowName, now.Add(time.Hour)); !ok {
		t.Fatal("grant not stored")
	}
	if op, _ := lastEntry(h); op != "grant_created" || h.ledger()[len(h.ledger())-1].Grant.ID != res.Grant.ID {
		t.Fatalf("ledger = %+v", h.ledger()[len(h.ledger())-1])
	}
	res2, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{UntilRevoked: true})
	if err != nil || res2.Grant.ExpiresAt != nil || res2.Replaced == nil || res2.Replaced.ID != res.Grant.ID {
		t.Fatalf("replace = %+v %v", res2, err)
	}
	if op, _ := lastEntry(h); op != "grant_replaced" {
		t.Fatal(op)
	}
	res3, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{For: "30d"})
	if err != nil || !res3.Grant.ExpiresAt.Equal(now.Add(720*time.Hour)) {
		t.Fatalf("30d = %+v %v", res3, err)
	}
}

func TestAgentsAllowRefusalsAndBadInput(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	_, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{For: "7h"})
	if ce := wantCode(t, err, contract.BadFlag); ce.Field != "for" || len(h.approver.asked) != 0 {
		t.Fatal("a bad duration fails before anyone is asked")
	}
	h.approver.err = &approval.Refusal{Reason: approval.Timeout}
	_, _, err = h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{})
	if ce := wantCode(t, err, contract.ApprovalRequired); ce.Details["reason"] != "timeout" {
		t.Fatal(ce.Details)
	}
	h.approver.err = nil
	h.eng.DryRun = true
	res, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{For: "1d"})
	h.eng.DryRun = false
	if err != nil || res.Preview == nil || res.Preview.Ops[0]["op"] != "grant" || len(h.approver.asked) != 1 {
		t.Fatalf("dry run = %+v %v (asked %d)", res, err, len(h.approver.asked))
	}
	if gs, _ := h.eng.Grants.Load(); len(gs) != 0 {
		t.Fatal("--dry-run stored a grant")
	}
}

func TestAgentsRevokeAndStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.grant(6 * time.Hour)
	h.clock.Sleep(time.Hour)
	st, _, err := h.eng.AgentsStatus(h.ctx, &h.pair)
	if err != nil || len(st.Grants) != 1 || st.Grants[0].Expired || *st.Grants[0].RemainingSeconds != 5*3600 ||
		st.TokenCanPublish != "true" || st.Token.HTTPStatus != 200 || st.Pair != publicName {
		t.Fatalf("status = %+v %v", st, err)
	}
	h.forge.PushDenied = map[string]bool{publicName: true}
	if st, _, _ := h.eng.AgentsStatus(h.ctx, &h.pair); st.TokenCanPublish != "false" || st.Token.HTTPStatus != 403 {
		t.Fatalf("shadow-only token = %+v", st)
	}
	if st, _, _ := h.eng.AgentsStatus(h.ctx, nil); st.TokenCanPublish != "unknown" || len(st.Grants) != 1 {
		t.Fatalf("no pair = %+v", st)
	}
	h.eng.DryRun = true
	res, _, err := h.eng.AgentsRevoke(h.ctx, &h.pair, false)
	h.eng.DryRun = false
	if err != nil || len(res.Revoked) != 1 || res.Preview.Ops[0]["op"] != "revoke" {
		t.Fatalf("revoke preview = %+v %v", res, err)
	}
	res, _, err = h.eng.AgentsRevoke(h.ctx, &h.pair, false)
	if err != nil || len(res.Revoked) != 1 {
		t.Fatalf("revoke = %+v %v", res, err)
	}
	if op, _ := lastEntry(h); op != "grant_revoked" {
		t.Fatal(op)
	}
	if _, ok, _ := h.eng.Grants.Valid(publicName, shadowName, h.clock.Now()); ok {
		t.Fatal("still granted")
	}
}

// Review Focus 2: an unreadable grants file fails before the person is
// asked for a code that could never be saved.
func TestAgentsAllowWithACorruptGrantsFileAsksNoOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if err := os.WriteFile(h.eng.WS.GrantsPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.AgentsAllow(h.ctx, h.pair, engine.AgentsAllowOptions{})
	if ce := wantCode(t, err, contract.Internal); !strings.Contains(ce.Message, "grants.json") {
		t.Fatalf("message = %s", ce.Message)
	}
	if len(h.approver.asked) != 0 {
		t.Fatal("the person was asked for a code despite an unreadable grants file")
	}
}

// Review Focus 2.
func TestRevokeAllClearsACorruptGrantsFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	if err := os.WriteFile(h.eng.WS.GrantsPath(), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := h.eng.AgentsStatus(h.ctx, &h.pair)
	_ = wantCode(t, err, contract.Internal)
	if _, _, err := h.eng.AgentsRevoke(h.ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	if st, _, err := h.eng.AgentsStatus(h.ctx, &h.pair); err != nil || len(st.Grants) != 0 {
		t.Fatalf("%+v %v", st, err)
	}
}

// --plan --all on a grants.json that doesn't decode previews replacing it, as
// revoke --all does; a newer file is refused, as the real run refuses it.
func TestAgentsRevokeAllPlanOnACorruptFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	path := h.eng.WS.GrantsPath()
	if err := os.WriteFile(path, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.eng.DryRun = true
	res, _, err := h.eng.AgentsRevoke(h.ctx, nil, true)
	h.eng.DryRun = false
	if err != nil || len(res.Revoked) != 0 || res.Preview == nil || len(res.Preview.Ops) != 1 ||
		res.Preview.Ops[0]["op"] != "write_file" || res.Preview.Ops[0]["path"] != path {
		t.Fatalf("--plan --all = %+v %v", res, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "{nope" {
		t.Fatal("the dry run changed the file")
	}
	if err := os.WriteFile(path, []byte(`{"schema_version":"0.99","grants":"reshaped"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h.eng.DryRun = true
	_, _, err = h.eng.AgentsRevoke(h.ctx, nil, true)
	h.eng.DryRun = false
	_ = wantCode(t, err, contract.UpgradeNeeded)
	// agents status reads the same file and says the same.
	_, _, err = h.eng.AgentsStatus(h.ctx, nil)
	_ = wantCode(t, err, contract.UpgradeNeeded)
}

func TestAgentsStatusNamesTheTokenKindAndAnAgentOnALogin(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.Token, h.eng.TokenSource = "github_pat_test", "GHOSTBRANCH_TOKEN"
	st, warns, err := h.eng.AgentsStatus(h.ctx, &h.pair)
	if err != nil || st.TokenKind != "fine-grained" || st.TokenCanPublish != "true" || len(warns) != 0 {
		t.Fatalf("%+v %v %v", st, warns, err)
	}
	h.eng.Token, h.eng.TokenSource, h.eng.LooksLikeAgent = "ghu_test", "login", true
	st, warns, err = h.eng.AgentsStatus(h.ctx, &h.pair)
	if err != nil || st.TokenKind != "app" || warning(warns, contract.AgentOnPersonLogin) == nil {
		t.Fatalf("%+v %v %v", st, warns, err)
	}
	if strings.Contains(fmt.Sprint(st, warns), "ghu_test") {
		t.Fatal("agents status shows the token")
	}
}
