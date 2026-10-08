package engine_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// leakyID gates a feature holding the fake token and returns its finding id.
func leakyID(t *testing.T, h *harness) string {
	t.Helper()
	h.startFeature("feat-x", fixture())
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	return res.Findings[0].ID
}

func add(h *harness, reason string, ids ...string) (*engine.WaiversAddResult, error) {
	res, _, err := h.eng.WaiversAdd(h.ctx, h.pair, engine.WaiversAddOptions{Feature: "feat-x", IDs: ids, Reason: reason})
	return res, err
}

func TestWaiversAddNeedsAPersonAtAConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	h.eng.Approver = nil
	_, err := add(h, "fixture", id)
	if ce := wantCode(t, err, contract.ApprovalRequired); ce.Details["reason"] != "no_console" {
		t.Fatalf("details = %+v", ce.Details)
	}
	if _, err := os.Stat(h.eng.Waivers.Path()); !os.IsNotExist(err) {
		t.Fatal("a waiver was written without a person")
	}
}

func TestWaiversAddAtAConsole(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	// Forged gate metadata must not change what the person is shown.
	forgeRecordedTree(t, h, "feat-x", "0000000000000000000000000000000000000000")
	_, err := add(h, "  test fixture ", id, strings.ToUpper(id[2:]) /* not an id */)
	_ = wantCode(t, err, contract.BadFlag)
	res, err := add(h, "  test fixture ", id, id)
	if err != nil || len(res.Waivers) != 1 || res.Waivers[0].Reason != "test fixture" || res.Waivers[0].Path != "test/fixtures/token.txt" ||
		res.Waivers[0].CreatedBy != "human:octo" || len(res.Waivers[0].MatchSHA256) != 64 {
		t.Fatalf("WaiversAdd = %+v %v", res, err)
	}
	tok := testutil.FakeGitHubToken()
	asked := h.approver.asked[len(h.approver.asked)-1]
	for _, want := range []string{id, "test/fixtures/token.txt:1", tok[:4] + "…" + tok[len(tok)-4:], "test fixture", "waivers revoke " + id} {
		if !strings.Contains(asked, want) {
			t.Errorf("the console summary lacks %q:\n%s", want, asked)
		}
	}
	if strings.Contains(asked, tok) {
		t.Fatal("the console summary shows the whole secret")
	}
	if _, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil {
		t.Fatalf("the waived finding still blocks: %v", err)
	}
	var last = h.ledger()
	for i := len(last) - 1; i >= 0; i-- {
		if e := last[i]; e.Op == "waivers add" {
			if !reflect.DeepEqual(e.Waivers, []string{id}) || e.Reason != "test fixture" || e.Approval == nil || e.Approval.Kind != "console" {
				t.Fatalf("ledger = %+v", e)
			}
			return
		}
	}
	t.Fatal("no waivers add ledger entry")
}

func TestWaiversAddRefusals(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	for name, c := range map[string]struct {
		ids    []string
		reason string
		code   contract.Code
		field  string
	}{
		"no ids":     {nil, "r", contract.MissingInput, "id"},
		"not an id":  {[]string{"abc"}, "r", contract.BadFlag, "id"},
		"no reason":  {[]string{id}, "  ", contract.MissingInput, "reason"},
		"unknown id": {[]string{"f-000000000000"}, "r", contract.WaiverUnknown, "id"},
	} {
		_, err := add(h, c.reason, c.ids...)
		if ce := contract.From(err); ce == nil || ce.Code != c.code || ce.Field != c.field {
			t.Errorf("%s: want %s on %s, got %v", name, c.code, c.field, err)
		}
	}
	h.approver.err = &approval.Refusal{Reason: approval.NotConfirmed}
	_, err := add(h, "fixture", id)
	if ce := wantCode(t, err, contract.ApprovalRequired); ce.Details["reason"] != "not_confirmed" {
		t.Fatalf("details = %+v", ce.Details)
	}
	h.approver.err = nil
	h.eng.DryRun = true
	res, err := add(h, "fixture", id)
	h.eng.DryRun = false
	if err != nil || res.Preview == nil || len(res.Preview.Ops) != 1 || res.Preview.Ops[0]["op"] != "waive" {
		t.Fatalf("--plan = %+v %v", res, err)
	}
	if _, err := os.Stat(h.eng.Waivers.Path()); !os.IsNotExist(err) {
		t.Fatal("a refusal or a dry run wrote a waiver")
	}
}

func TestWaiversListAndRevoke(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	if _, err := add(h, "fixture", id); err != nil {
		t.Fatal(err)
	}
	for _, p := range []bool{true, false} {
		var res *engine.WaiversListResult
		var err error
		if p {
			res, _, err = h.eng.WaiversList(h.ctx, &h.pair)
		} else {
			res, _, err = h.eng.WaiversList(h.ctx, nil)
		}
		if err != nil || len(res.Waivers) != 1 || res.Waivers[0].ID != id {
			t.Fatalf("list (pair %v) = %+v %v", p, res, err)
		}
	}
	for name, c := range map[string]struct {
		pair  bool
		ids   []string
		all   bool
		code  contract.Code
		field string
	}{
		"nothing named": {true, nil, false, contract.MissingInput, "id"},
		"ids and --all": {true, []string{id}, true, contract.BadFlag, "id"},
		"ids, no repo":  {false, []string{id}, false, contract.BadFlag, "repo"},
	} {
		p := &h.pair
		if !c.pair {
			p = nil
		}
		_, _, err := h.eng.WaiversRevoke(h.ctx, p, c.ids, c.all)
		if ce := contract.From(err); ce == nil || ce.Code != c.code || ce.Field != c.field {
			t.Errorf("%s: want %s on %s, got %v", name, c.code, c.field, err)
		}
	}
	h.eng.DryRun = true
	pv, _, err := h.eng.WaiversRevoke(h.ctx, &h.pair, []string{id}, false)
	h.eng.DryRun = false
	if err != nil || pv.Preview == nil || len(pv.Revoked) != 1 {
		t.Fatalf("revoke --plan = %+v %v", pv, err)
	}
	res, _, err := h.eng.WaiversRevoke(h.ctx, &h.pair, []string{strings.ToUpper(id)}, false)
	if err != nil || len(res.Revoked) != 1 {
		t.Fatalf("revoke = %+v %v", res, err)
	}
	if es := h.ledger(); es[len(es)-1].Op != "waivers revoke" || !reflect.DeepEqual(es[len(es)-1].Waivers, []string{id}) {
		t.Fatalf("ledger = %+v", es[len(es)-1])
	}
	_, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)

	if err := os.WriteFile(h.eng.Waivers.Path(), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err = h.eng.WaiversList(h.ctx, nil)
	_ = wantCode(t, err, contract.Internal)
	if _, _, err := h.eng.WaiversRevoke(h.ctx, nil, nil, true); err != nil {
		t.Fatalf("revoke --all must clear a corrupt file: %v", err)
	}
}

func TestWaiverCommandsNeverPush(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	id := leakyID(t, h)
	n, before := len(h.pushes), h.publicRefs()
	if _, err := add(h, "fixture", id); err != nil {
		t.Fatal(err)
	}
	_, _, _ = h.eng.WaiversList(h.ctx, &h.pair)
	_, _, _ = h.eng.WaiversRevoke(h.ctx, &h.pair, []string{id}, false)
	if len(h.pushes) != n || !reflect.DeepEqual(h.publicRefs(), before) {
		t.Fatal("a waivers command pushed")
	}
}

// A waiver covers a finding in a commit the tip no longer holds, and the
// gate result that lists it waived, with its commits, matches the schema.
func TestWaiversAddCoversAFindingInAnOldCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("project-falcon")
	first := h.startFeature("feat-x", map[string]string{"docs/codename.txt": "project-falcon\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "drop it", map[string]string{"docs/codename.txt": testutil.Delete, "a.txt": "a\n"})
	res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x")
	_ = wantCode(t, err, contract.LeakFound)
	ar, _, err := h.eng.WaiversAdd(h.ctx, h.pair, engine.WaiversAddOptions{Feature: "feat-x", IDs: []string{res.Findings[0].ID}, Reason: "public name"})
	if err != nil || len(ar.Waivers) != 1 {
		t.Fatalf("waivers add = %+v %v", ar, err)
	}
	res, _, err = h.eng.Gate(h.ctx, h.pair, "feat-x")
	if err != nil {
		t.Fatalf("gate after the waiver: %v", err)
	}
	if len(res.Waived) != 1 || !reflect.DeepEqual(res.Waived[0].Commits, []string{first}) {
		t.Fatalf("waived = %+v", res.Waived)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	validateSchema(t, "gate.schema.json", raw)
}

// Under a squash default, a finding only in an older commit is invisible to
// the default gate but blocks promote --mode preserve; waivers add scans as
// preserve, so the person can still waive it (and an unmapped author, which
// only matters for publishing, doesn't stop them).
func TestWaiversAddUnderASquashDefaultCoversAnOldCommit(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.setBlockPatterns("project-falcon") // rewrites the whole config, so first
	h.setConfig("default_mode: preserve", "default_mode: squash")
	h.startFeature("feat-x", map[string]string{"docs/codename.txt": "project-falcon\n"})
	testutil.Commit(t, h.shadowGit(), "feat-x", "drop it", map[string]string{"docs/codename.txt": testutil.Delete, "a.txt": "a\n"})
	if res, _, err := h.eng.Gate(h.ctx, h.pair, "feat-x"); err != nil || len(res.Findings) != 0 {
		t.Fatalf("squash gate = %+v %v", res, err)
	}
	pr, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "preserve", Title: "t", PlanOnly: true})
	_ = wantCode(t, err, contract.LeakFound)
	if pr == nil || len(pr.Findings) != 1 || !pr.Findings[0].Waivable {
		t.Fatalf("preserve promote = %+v", pr)
	}
	id := pr.Findings[0].ID
	h.setConfig("author_email: noreply", "author_email: map\n  author_map:\n    \"x@y.example\": \"X <x@y.example>\"")
	ar, err := add(h, "public name", id)
	if err != nil || len(ar.Waivers) != 1 || ar.Waivers[0].ID != id {
		t.Fatalf("waivers add = %+v %v", ar, err)
	}
}

// validateSchema checks raw against schemas/v0/<name>.
func validateSchema(t *testing.T, name string, raw []byte) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("..", "..", "schemas", "v0", name))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	s, err := c.Compile(path)
	if err != nil {
		t.Fatalf("compile %s: %v", name, err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Fatalf("the result does not match %s:\n%v\n%s", name, err, raw)
	}
}
