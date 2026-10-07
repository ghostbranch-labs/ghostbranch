package contract_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

func TestEveryCodeHasItsExitCode(t *testing.T) {
	want := map[contract.Code]int{
		contract.AuthMissing: 1, contract.ForgeError: 1, contract.ShadowNotPrivate: 1,
		contract.ConfigInvalid: 1, contract.Locked: 1, contract.Internal: 1,
		contract.TokenCannotPublish: 1, contract.GuardUnsupported: 1, contract.PushBlocked: 1,
		contract.NotAPairCheckout: 2,
		contract.BadFlag:          2, contract.MissingInput: 2, contract.PairAmbiguous: 2,
		contract.NameInvalid: 2, contract.FeatureNotFound: 2,
		contract.ChecksFailed: 3, contract.NoCI: 3, contract.CITimeout: 3, contract.LeakFound: 3,
		contract.RebaseConflict: 4, contract.SyncDiverged: 4, contract.BranchExists: 4,
		contract.OverlayCollision: 4, contract.ShadowNotEmpty: 4, contract.RefMoved: 4,
		contract.ApprovalRequired: 5,
		contract.PlanStale:        6, contract.ShadowCreateDenied: 1,
		contract.MarkerInvalid: 3, contract.WaiverUnknown: 2,
	}
	all := contract.AllCodes()
	if len(all) != len(want) {
		t.Fatalf("AllCodes has %d codes, test expects %d", len(all), len(want))
	}
	for _, c := range all {
		if got := c.Exit(); got != want[c] {
			t.Errorf("%s.Exit() = %d, want %d", c, got, want[c])
		}
	}
}

func TestFromWrapsUnknownErrorsAsInternal(t *testing.T) {
	ce := contract.From(errors.New("boom"))
	if ce.Code != contract.Internal || ce.Message != "boom" {
		t.Fatalf("got %+v", ce)
	}
	orig := contract.New(contract.SyncDiverged, "diverged")
	if got := contract.From(orig); got != orig {
		t.Fatalf("From must return contract errors unchanged")
	}
	if contract.From(nil) != nil {
		t.Fatalf("From(nil) must be nil")
	}
}

func TestSuccessEnvelopeShape(t *testing.T) {
	b, err := json.Marshal(contract.NewEnvelope("sync", nil, nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m["schema_version"] != "0.4" || m["ok"] != true || m["exit_code"] != float64(0) ||
		m["command"] != "sync" || m["error"] != nil {
		t.Fatalf("bad envelope: %s", b)
	}
	if _, ok := m["data"].(map[string]any); !ok {
		t.Fatalf("data must be an object, got %s", b)
	}
	if w, ok := m["warnings"].([]any); !ok || len(w) != 0 {
		t.Fatalf("warnings must be an empty array, got %s", b)
	}
}

func TestFailureEnvelopeCarriesCodeFieldAndExit(t *testing.T) {
	err := contract.New(contract.MissingInput, "commit message required").WithField("message")
	env := contract.NewEnvelope("promote", map[string]any{"x": 1}, nil, nil, err)
	if env.OK || env.ExitCode != 2 || env.Error == nil {
		t.Fatalf("got %+v", env)
	}
	if env.Error.Code != contract.MissingInput || env.Error.Field == nil || *env.Error.Field != "message" {
		t.Fatalf("got %+v", env.Error)
	}
	b, _ := json.Marshal(contract.NewEnvelope("gate", nil, nil, nil, contract.New(contract.NoCI, "none")))
	var m map[string]map[string]any
	_ = json.Unmarshal(b, &m)
	if _, present := m["error"]["field"]; !present || m["error"]["field"] != nil {
		t.Fatalf("field must serialize as null when unset: %s", b)
	}
}

func TestEnvelopeNext(t *testing.T) {
	b, _ := json.Marshal(contract.NewEnvelope("gate", nil, nil, nil, nil))
	if !strings.Contains(string(b), `"warnings":[],"next":[],"error":null`) {
		t.Fatalf("next must be [] and sit between warnings and error: %s", b)
	}
	n := []contract.Next{{Command: []string{"ghostbranch", "apply", "x"}, Why: "w", NeedsHuman: true}}
	b, _ = json.Marshal(contract.NewEnvelope("promote", nil, nil, n, nil))
	if !strings.Contains(string(b), `"next":[{"command":["ghostbranch","apply","x"],"why":"w","needs_human":true,"needs_input":[]}]`) {
		t.Fatalf("needs_input must be [] when empty: %s", b)
	}
}

func TestWarningDetailsAreOptional(t *testing.T) {
	plain, _ := json.Marshal(contract.Warning{Code: contract.LargeFile, Message: "m"})
	if strings.Contains(string(plain), "details") {
		t.Fatalf("empty details must be omitted: %s", plain)
	}
	full, _ := json.Marshal(contract.Warning{Code: contract.LargeFile, Message: "m", Details: map[string]any{"paths": []string{"a.bin"}}})
	if !strings.Contains(string(full), `"details":{"paths":["a.bin"]}`) {
		t.Fatalf("details missing: %s", full)
	}
}

func TestSubProject4CodesAreRegistered(t *testing.T) {
	if contract.ShadowCreateDenied.Exit() != contract.ExitFailure {
		t.Fatalf("SHADOW_CREATE_DENIED exits %d", contract.ShadowCreateDenied.Exit())
	}
	for _, w := range []contract.WarningCode{contract.OverlayOutdated, contract.RepoGuard, contract.RepoGuardUnrewritable,
		contract.SecretsUnlisted, contract.SecretsMissing, contract.PushTriggersOnSync, contract.ActionsMinutesBilled,
		contract.ShadowBehind, contract.AuditIncomplete} {
		es, err := contract.Explain(string(w))
		if err != nil || len(es) != 1 || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", w, es, err)
		}
	}
	es, err := contract.Explain("SHADOW_CREATE_DENIED")
	if err != nil || !es[0].NeedsHuman || !strings.Contains(es[0].WhatToDo, "--adopt") {
		t.Fatalf("SHADOW_CREATE_DENIED doc: %+v %v", es, err)
	}
	if contract.SchemaVersion != "0.4" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
}
