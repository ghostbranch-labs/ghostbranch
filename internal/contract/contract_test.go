package contract_test

import (
	"encoding/json"
	"errors"
	"slices"
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
		contract.PlanStale:        6, contract.ShadowCreateDenied: 1, contract.PushRefused: 1,
		contract.MarkerInvalid: 3, contract.WaiverUnknown: 2,
		contract.CurateInvalid: 2, contract.CurateConflict: 4, contract.CurateTreeMismatch: 4,
		contract.AuthorUnmapped: 1, contract.NothingToPublish: 1,
		contract.UpgradeNeeded: 1, contract.FeatureMoved: 4, contract.ShadowBehindError: 4,
		contract.Interrupted: 1, contract.JobNotFound: 2, contract.JobStale: 4,
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
	if m["schema_version"] != "0.8" || m["ok"] != true || m["exit_code"] != float64(0) ||
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
		if len(es) == 2 && w == contract.ShadowBehind { // also feature start's error, listed first
			es = es[1:]
		}
		if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", w, es, err)
		}
	}
	es, err := contract.Explain("SHADOW_CREATE_DENIED")
	if err != nil || !es[0].NeedsHuman || !strings.Contains(es[0].WhatToDo, "--adopt") {
		t.Fatalf("SHADOW_CREATE_DENIED doc: %+v %v", es, err)
	}
	if contract.SchemaVersion != "0.8" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
}

func TestSubProject5aCodesAreRegistered(t *testing.T) {
	want := map[contract.Code]int{
		contract.CurateInvalid: contract.ExitUsage, contract.CurateConflict: contract.ExitConflict,
		contract.CurateTreeMismatch: contract.ExitConflict, contract.AuthorUnmapped: contract.ExitFailure,
		contract.NothingToPublish: contract.ExitFailure,
	}
	for c, exit := range want {
		if c.Exit() != exit {
			t.Fatalf("%s exits %d, want %d", c, c.Exit(), exit)
		}
		es, err := contract.Explain(string(c))
		if err != nil || len(es) != 1 || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", c, es, err)
		}
	}
	if es, _ := contract.Explain("AUTHOR_UNMAPPED"); !es[0].NeedsHuman {
		t.Fatal("AUTHOR_UNMAPPED needs a person: only they may edit the overlay config's author_map")
	}
	if contract.SchemaVersion != "0.8" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
}

func TestSubProject5bCodesAreRegistered(t *testing.T) {
	t.Parallel()
	for c, exit := range map[contract.Code]int{contract.UpgradeNeeded: contract.ExitFailure, contract.FeatureMoved: contract.ExitConflict} {
		if c.Exit() != exit {
			t.Fatalf("%s exits %d, want %d", c, c.Exit(), exit)
		}
		es, err := contract.Explain(string(c))
		if err != nil || len(es) != 1 || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", c, es, err)
		}
	}
	if contract.SchemaVersion != "0.8" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
}

func TestSurveyWarningsAreRegistered(t *testing.T) {
	t.Parallel()
	for w, s := range map[contract.WarningCode]string{contract.PRStateUnknown: "PR_STATE_UNKNOWN", contract.DriftUnknown: "DRIFT_UNKNOWN"} {
		if string(w) != s || !slices.Contains(contract.AllWarnings(), w) {
			t.Fatalf("%s is not a registered warning", s)
		}
		es, err := contract.Explain(s)
		if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", s, es, err)
		}
	}
	if es, _ := contract.Explain("PR_STATE_UNKNOWN"); !strings.Contains(es[0].WhatToDo, "never closed automatically") {
		t.Fatalf("PR_STATE_UNKNOWN must say the feature is never closed automatically: %+v", es[0])
	}
}

func TestFeatureNotClosedIsRegistered(t *testing.T) {
	t.Parallel()
	if string(contract.FeatureNotClosed) != "FEATURE_NOT_CLOSED" || !slices.Contains(contract.AllWarnings(), contract.FeatureNotClosed) {
		t.Fatal("FEATURE_NOT_CLOSED is not a registered warning")
	}
	es, err := contract.Explain("FEATURE_NOT_CLOSED")
	if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning ||
		es[0].Summary != "Sync couldn't close a merged feature." ||
		!strings.HasPrefix(es[0].WhatToDo, "If details.deleted is set, the feature is gone: close its gate draft PR in the shadow repo by hand. Otherwise run ghostbranch feature show <name>, then feature close <name>.") ||
		!strings.Contains(es[0].WhatToDo, "With no details.feature") {
		t.Fatalf("FEATURE_NOT_CLOSED is not documented: %+v %v", es, err)
	}
}

// SHADOW_BEHIND is both feature start's error (exit 4) and the warning
// other commands give; explain shows both, the error first.
func TestShadowBehindIsAnErrorAndAWarning(t *testing.T) {
	t.Parallel()
	if string(contract.ShadowBehindError) != string(contract.ShadowBehind) || contract.ShadowBehindError.Exit() != contract.ExitConflict {
		t.Fatal("SHADOW_BEHIND error must share the warning's string and exit 4")
	}
	es, err := contract.Explain("shadow_behind")
	if err != nil || len(es) != 2 || es[0].Kind != contract.KindError || *es[0].ExitCode != 4 ||
		es[0].WhatToDo != "Run ghostbranch sync, then start the feature again." ||
		es[1].Kind != contract.KindWarning || es[1].ExitCode != nil {
		t.Fatalf("explain SHADOW_BEHIND = %+v %v", es, err)
	}
	es, _ = contract.Explain("4")
	found := false
	for _, e := range es {
		found = found || e.Code == "SHADOW_BEHIND"
	}
	if !found {
		t.Fatalf("explain 4 lacks SHADOW_BEHIND: %+v", es)
	}
}

func TestMirrorNotUpdatedIsRegistered(t *testing.T) {
	t.Parallel()
	es, err := contract.Explain("MIRROR_NOT_UPDATED")
	if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning || !strings.Contains(es[0].WhatToDo, "next command's fetch") ||
		!slices.Contains(contract.AllWarnings(), contract.MirrorNotUpdated) {
		t.Fatalf("MIRROR_NOT_UPDATED = %+v %v", es, err)
	}
}

// feature close and remove need a console and ignore grants, so exit 5 and
// APPROVAL_REQUIRED never promise that a grant helps every command.
func TestApprovalDocsDontPromiseAGrantForEverything(t *testing.T) {
	t.Parallel()
	for _, key := range []string{"5", "APPROVAL_REQUIRED"} {
		es, err := contract.Explain(key)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range es {
			if e.Code != "5" && e.Code != "APPROVAL_REQUIRED" {
				continue
			}
			text := e.Summary + " " + e.WhatToDo
			if !strings.Contains(text, "person at a console") && !strings.Contains(text, "typed at a console") {
				t.Errorf("%s: no console: %q", e.Code, text)
			}
			if !strings.Contains(e.WhatToDo, "for promote and apply, an agent grant") && !strings.Contains(e.WhatToDo, "For promote and apply, an agent grant") {
				t.Errorf("%s: the grant must be scoped to promote and apply: %q", e.Code, e.WhatToDo)
			}
			if strings.Contains(text, "or an agent grant they make") || strings.Contains(text, "Publishing needs a person") {
				t.Errorf("%s: still promises a grant for every command: %q", e.Code, text)
			}
		}
	}
}

func TestSubProject5cCodesAreRegistered(t *testing.T) {
	t.Parallel()
	for c, exit := range map[contract.Code]int{contract.Interrupted: contract.ExitFailure,
		contract.JobNotFound: contract.ExitUsage, contract.JobStale: contract.ExitConflict} {
		if c.Exit() != exit {
			t.Fatalf("%s exits %d, want %d", c, c.Exit(), exit)
		}
		es, err := contract.Explain(string(c))
		if err != nil || len(es) != 1 || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", c, es, err)
		}
	}
	if contract.SchemaVersion != "0.8" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
}

func TestSubProject5d1CodesAreRegistered(t *testing.T) {
	t.Parallel()
	for _, w := range []contract.WarningCode{contract.RecordNewer, contract.RecordUnreadable} {
		if !slices.Contains(contract.AllWarnings(), w) {
			t.Fatalf("%s is not in the warning list", w)
		}
		es, err := contract.Explain(string(w))
		if err != nil || len(es) != 1 || es[0].Summary == "" || es[0].WhatToDo == "" {
			t.Fatalf("%s is not documented: %+v %v", w, es, err)
		}
	}
	if contract.SchemaVersion != "0.8" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	if es, _ := contract.Explain(string(contract.Locked)); strings.Contains(es[0].WhatToDo, "delete the lock") {
		t.Fatalf("LOCKED still says to delete the lock file: %q", es[0].WhatToDo)
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"0.8": false, "0.7": false, "0.10": true, "0.9": true, "1.0": true,
		"": false, "x": false, "0.8.1": false, "0.4": false,
		// A malformed version is never newer: it would lock users out.
		"+1.0": false, "1.-5": false, "0.+9": false, " 0.9": false, "0.9 ": false, "1.": false, ".9": false} {
		if got := contract.Newer(v); got != want {
			t.Errorf("Newer(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestNewerRecord(t *testing.T) {
	t.Parallel()
	ce := contract.NewerRecord("plan", "0.99")
	if ce.Code != contract.UpgradeNeeded || ce.Details["record"] != "plan" || ce.Details["record_version"] != "0.99" ||
		ce.Details["supported"] != contract.SchemaVersion || !strings.Contains(ce.Message, "plan record") {
		t.Fatalf("NewerRecord = %+v", ce)
	}
}
