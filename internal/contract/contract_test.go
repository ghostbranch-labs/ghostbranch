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
		contract.PairNotFound: 2, contract.NoOpenPR: 2, contract.PlanNotFound: 2, contract.FeatureArchived: 2,
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
		contract.LoginExpired: 1, contract.LoginUnavailable: 1, contract.TokenPermissionMissing: 1, contract.DoctorFailed: 1,
		contract.FoldNeedsRebase: 4, contract.HasChildren: 4, contract.NotAStack: 2, contract.ParentRewritten: 4,
		contract.PRChangedOnGitHub: 4,
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
	if m["schema_version"] != "0.15" || m["ok"] != true || m["exit_code"] != float64(0) ||
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
	if contract.SchemaVersion != "0.15" {
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
	if contract.SchemaVersion != "0.15" {
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
	if contract.SchemaVersion != "0.15" {
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
		es[0].Summary != "Sync didn't close a merged feature: the close failed, sync couldn't survey the features and so closed none, another feature's record couldn't be read, so sync left the merged features open, or its PR merged into the public branch below it, not main." ||
		!strings.Contains(es[0].WhatToDo, "With details.reason merged_into_parent_branch") ||
		!strings.HasPrefix(es[0].WhatToDo, "If details.deleted is set, the feature is gone: close its gate draft PR in the shadow repo by hand.") ||
		!strings.Contains(es[0].WhatToDo, "Otherwise run ghostbranch feature show <name>, then feature close <name>.") ||
		!strings.Contains(es[0].WhatToDo, "With details.unreadable") ||
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
		es[0].WhatToDo != "Run ghostbranch sync --repo <public>, then start the feature again." ||
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
	if contract.SchemaVersion != "0.15" {
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
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	if es, _ := contract.Explain(string(contract.Locked)); !strings.Contains(es[0].WhatToDo, "nothing to delete") ||
		strings.Contains(es[0].WhatToDo, "delete the lock") {
		t.Fatalf("LOCKED must say there is nothing to delete: %q", es[0].WhatToDo)
	}
}

// An interrupted or half-published promote is resumed with apply, never by
// promoting again under another name: the explain text must say so.
func TestExplainSteersAnInterruptedPublishToApply(t *testing.T) {
	t.Parallel()
	es, _ := contract.Explain(string(contract.Interrupted))
	for _, want := range []string{"plan_id", "apply", "job", "wait"} {
		if !strings.Contains(es[0].WhatToDo, want) {
			t.Errorf("INTERRUPTED what-to-do lacks %q: %q", want, es[0].WhatToDo)
		}
	}
	if strings.Contains(es[0].WhatToDo, "safe to repeat") {
		t.Errorf("INTERRUPTED still says to repeat the command blindly: %q", es[0].WhatToDo)
	}
	es, _ = contract.Explain(string(contract.BranchExists))
	for _, want := range []string{"plan_id", "apply"} {
		if !strings.Contains(es[0].WhatToDo, want) {
			t.Errorf("BRANCH_EXISTS what-to-do lacks %q: %q", want, es[0].WhatToDo)
		}
	}
}

func TestNewer(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{"0.12": false, "0.11": false, "0.10": false, "0.9": false, "0.7": false, "0.13": true, "0.100": true, "1.0": true,
		"": false, "x": false, "0.8.1": false, "0.4": false, "0.2": false,
		// A malformed version is never newer: it would lock users out.
		"+1.0": false, "1.-5": false, "0.+9": false, " 0.12": false, "0.12 ": false, "1.": false, ".9": false} {
		if got := contract.Newer(v); got != want {
			t.Errorf("Newer(%q) = %v, want %v", v, got, want)
		}
	}
}

// The 6.1 codes and their explain texts are pinned word for word: the guide
// and the user docs are generated from them, and agents act on them.
func TestSubProject61Contract(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	for c, s := range map[contract.Code]string{contract.LoginExpired: "LOGIN_EXPIRED", contract.LoginUnavailable: "LOGIN_UNAVAILABLE",
		contract.TokenPermissionMissing: "TOKEN_PERMISSION_MISSING", contract.DoctorFailed: "DOCTOR_FAILED"} {
		if string(c) != s || c.Exit() != contract.ExitFailure || !slices.Contains(contract.AllCodes(), c) {
			t.Errorf("%s: string %q, exit %d, registered %v", s, c, c.Exit(), slices.Contains(contract.AllCodes(), c))
		}
	}
	type doc struct {
		summary, whatToDo string
		causes            []string
		human             bool
	}
	for code, want := range map[string]doc{
		"LOGIN_EXPIRED": {
			summary: "The stored GitHub login has expired and can't be refreshed.",
			causes: []string{"its refresh token is past its six months",
				"the login was revoked on GitHub, or the Ghostbranch app was uninstalled or its authorization removed"},
			whatToDo: "A person runs ghostbranch login again. An agent stops and hands this to the person; an agent with a token of its own sets GHOSTBRANCH_TOKEN, which never uses the stored login.",
			human:    true,
		},
		"LOGIN_UNAVAILABLE": {
			summary: "Ghostbranch can't log in here, or can't read the stored login (error.details.reason says which).",
			causes: []string{
				"no_console: no terminal a person can use for the device flow; ghostbranch login --with-token reads a pasted token from stdin instead",
				"no_keychain: this machine has no usable OS keychain (for example Linux without a Secret Service), and Ghostbranch never stores a token in a plain file",
				"no_client_id: this build of Ghostbranch has no GitHub App client ID",
				"denied, expired_code: the person denied the request on GitHub, or the code expired before they approved it",
				"corrupt: the stored login can't be read",
				"keychain_error: the OS keychain exists but refused (for example, it is locked)",
				"refused: GitHub refused the login, for example because the app has device flow turned off",
				"refresh_failed: GitHub refused to refresh the stored login for a reason other than its expiry",
			},
			whatToDo: "Read error.details.reason. For no_console or no_keychain, set GHOSTBRANCH_TOKEN to a token instead; agents and CI always do. For no_client_id, use ghostbranch login --with-token or GHOSTBRANCH_TOKEN. For denied or expired_code, run ghostbranch login again and approve the code on GitHub. For corrupt, a person runs ghostbranch logout, then ghostbranch login. For keychain_error, unlock the keychain or set GHOSTBRANCH_TOKEN. For refused or refresh_failed, run ghostbranch login again.",
			human:    true,
		},
		"TOKEN_PERMISSION_MISSING": {
			summary: "The token in use lacks a GitHub permission this command needs: error.details.permission on error.details.repo.",
			causes: []string{
				"a fine-grained token or the Ghostbranch app wasn't granted that permission, for example Workflows: write to change .github/workflows files",
				"the repo isn't in the token's or the app installation's repository access",
				"a classic or OAuth token lacks the scope named (OAuth scope: ...)",
			},
			whatToDo: "Run ghostbranch doctor: it lists every permission the token lacks, for each command. A person grants error.details.permission on error.details.repo (in the fine-grained token's settings, or the Ghostbranch app installation's repository access), then runs the command in error.details.command again. Never ask for wider access than doctor names.",
			human:    true,
		},
		"DOCTOR_FAILED": {
			summary:  "ghostbranch doctor found failing checks; error.details.failed lists their IDs.",
			causes:   []string{"one of tools, token, pair, guard, state or ci failed; data.checks[].message says why"},
			whatToDo: "Run the fix each failing check names (data.checks[].fix, also listed in next), then run ghostbranch doctor again. Warnings don't fail doctor.",
		},
	} {
		es, err := contract.Explain(code)
		if err != nil || len(es) != 1 {
			t.Fatalf("explain %s = %+v %v", code, es, err)
		}
		e := es[0]
		if e.Kind != contract.KindError || *e.ExitCode != 1 || e.Summary != want.summary || e.WhatToDo != want.whatToDo ||
			!slices.Equal(e.Causes, want.causes) || e.NeedsHuman != want.human {
			t.Errorf("%s doc = %+v", code, e)
		}
	}
	es, _ := contract.Explain("AUTH_MISSING")
	if !strings.Contains(es[0].WhatToDo, "ghostbranch login") || !strings.Contains(strings.Join(es[0].Causes, " "), "no stored login") {
		t.Errorf("AUTH_MISSING must name ghostbranch login and the stored login: %+v", es[0])
	}
	if w := es[0].WhatToDo; strings.Index(w, "ghostbranch login") > strings.Index(w, "GHOSTBRANCH_TOKEN") || strings.Index(w, "GHOSTBRANCH_TOKEN") > strings.Index(w, "gh auth login") {
		t.Errorf("AUTH_MISSING must name ghostbranch login, then GHOSTBRANCH_TOKEN, then gh auth login: %q", w)
	}
	if contract.AgentOnPersonLogin != "AGENT_ON_PERSON_LOGIN" || !slices.Contains(contract.AllWarnings(), contract.AgentOnPersonLogin) {
		t.Errorf("AgentOnPersonLogin = %q, registered %v", contract.AgentOnPersonLogin, slices.Contains(contract.AllWarnings(), contract.AgentOnPersonLogin))
	}
	ws, err := contract.Explain("AGENT_ON_PERSON_LOGIN")
	if err != nil || len(ws) != 1 || ws[0].Kind != contract.KindWarning ||
		ws[0].Summary != "This run looks like an agent or a script, and it is using a person's stored login, which can publish." ||
		ws[0].WhatToDo != "Give the agent its own shadow-only token in GHOSTBRANCH_TOKEN (see ghostbranch guide § Tokens). The person's login still needs the console code to publish." {
		t.Errorf("AGENT_ON_PERSON_LOGIN doc = %+v %v", ws, err)
	}
}

func TestNewerRecord(t *testing.T) {
	t.Parallel()
	ce := contract.NewerRecord("plan", "0.99")
	if ce.Code != contract.UpgradeNeeded || ce.Details["record"] != "plan" || ce.Details["record_version"] != "0.99" ||
		ce.Details["supported"] != contract.RecordVersion || !strings.Contains(ce.Message, "plan record") {
		t.Fatalf("NewerRecord = %+v", ce)
	}
}

func TestSubProject5d2Contract(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	for _, c := range []contract.WarningCode{contract.LedgerNotWritten, contract.SyncPushFailed, contract.PlanFileUnreadable} {
		if !slices.Contains(contract.AllWarnings(), c) {
			t.Fatalf("%s is not in the warning list", c)
		}
	}
	es, err := contract.Explain(string(contract.LedgerNotWritten))
	if err != nil || len(es) != 1 || es[0].Summary == "" || !strings.Contains(es[0].WhatToDo, "details.refs") {
		t.Fatalf("LEDGER_NOT_WRITTEN is not documented: %+v %v", es, err)
	}
	for code, wants := range map[string][]string{
		"SHADOW_BEHIND":        {"--repo"}, // both the error and the warning
		"GATE_NOT_RECORDED":    {"details.job", "ghostbranch wait"},
		"WAIVERS_UNREADABLE":   {"newer", "upgrade"},
		"FEATURE_NOT_CLOSED":   {"survey"},
		"SYNC_PUSH_FAILED":     {"details.branch", "REF_MOVED"},
		"PLAN_FILE_UNREADABLE": {"details.path"},
		"WAIVER_UNKNOWN":       {"waivers revoke", "details.ids"},
		"CI_NOT_CANCELLED":     {"details.runs"},
	} {
		es, _ := contract.Explain(code)
		if len(es) == 0 {
			t.Errorf("%s is not documented", code)
		}
		for _, e := range es {
			for _, w := range wants {
				if !strings.Contains(e.Summary+" "+e.WhatToDo, w) {
					t.Errorf("%s (%s) lacks %q: %+v", code, e.Kind, w, e)
				}
			}
		}
	}
	if es, _ := contract.Explain("GATE_NOT_RECORDED"); strings.HasPrefix(es[0].WhatToDo, "Gate again") {
		t.Errorf("GATE_NOT_RECORDED still leads with gate again: %q", es[0].WhatToDo)
	}
	if es, _ := contract.Explain("ACTOR_UNKNOWN"); strings.Contains(es[0].Summary, "could not be read") {
		t.Errorf("ACTOR_UNKNOWN summary: %q", es[0].Summary)
	}
}

// The cause texts say which guard files must be ignored, and that a squash
// or curated default can scan only the final tree.
func TestGuardUnsupportedAndWaiverUnknownTexts(t *testing.T) {
	t.Parallel()
	for code, want := range map[string]string{
		"GUARD_UNSUPPORTED": "the guard's files there (pre-push and pre-push.ghostbranch-*) are not all git-ignored, so they could be committed",
		"WAIVER_UNKNOWN":    "except that under a squash or curated default whose older commit has a broken marker it scans only the final tree",
	} {
		es, err := contract.Explain(code)
		if err != nil || len(es) != 1 || !strings.Contains(strings.Join(es[0].Causes, "\n"), want) {
			t.Errorf("%s causes lack %q: %+v %v", code, want, es, err)
		}
	}
	if es, _ := contract.Explain("GUARD_UNSUPPORTED"); !strings.Contains(es[0].WhatToDo, "ignore pre-push and pre-push.ghostbranch-* in the hooks directory") {
		t.Errorf("GUARD_UNSUPPORTED what to do: %q", es[0].WhatToDo)
	}
}

// The 6.1a codes and their explain texts are pinned word for word: the guide
// is generated from them, and agents act on them.
func TestSubProject61aContract(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	for c, want := range map[contract.Code]struct {
		s    string
		exit int
	}{
		contract.FoldNeedsRebase: {"FOLD_NEEDS_REBASE", contract.ExitConflict},
		contract.HasChildren:     {"HAS_CHILDREN", contract.ExitConflict},
		contract.NotAStack:       {"NOT_A_STACK", contract.ExitUsage},
		contract.ParentRewritten: {"PARENT_REWRITTEN", contract.ExitConflict},
	} {
		if string(c) != want.s || c.Exit() != want.exit || !slices.Contains(contract.AllCodes(), c) {
			t.Errorf("%s: string %q, exit %d (want %d), registered %v", want.s, c, c.Exit(), want.exit, slices.Contains(contract.AllCodes(), c))
		}
	}
	type doc struct {
		summary, whatToDo string
		causes            []string
	}
	for code, want := range map[string]doc{
		"FOLD_NEEDS_REBASE": {
			summary: "feature fold only fast-forwards, and the parent has moved on since the feature was branched from it, so nothing was folded.",
			causes: []string{"commits were pushed to the parent after the feature was started from it or last restacked onto it",
				"the parent was rewritten (rebased, amended or force-pushed)"},
			whatToDo: "Bring the feature up to its parent's tip, then fold again. ghostbranch feature restack <parent> does it, and stops with git steps on a conflict. Or rebase it yourself (error.details.steps): git fetch shadow && git switch <feature> && git rebase --onto shadow/<parent> <forked_at> <feature>, resolve any conflicts, then git push --force-with-lease shadow <feature>. error.details.parent and error.details.forked_at name them. Ghostbranch never resolves conflicts.",
		},
		"HAS_CHILDREN": {
			summary:  "The feature has children stacked on it (error.details.children), so it was not abandoned or closed: they would be left on a deleted parent.",
			whatToDo: "Deal with each child in error.details.children first: fold it into this feature (ghostbranch feature fold <child>), abandon it (for abandon: ghostbranch feature abandon <child>) or close it (for close: ghostbranch feature close <child>), or move it onto main (ghostbranch feature restack <child> --onto main). An abandoned child blocks close, not abandon. Then run the command again.",
		},
		"NOT_A_STACK": {
			summary: "The features given to promote aren't a stack that can be published together; error.details.reason says why and error.details.feature names where.",
			causes: []string{
				"duplicate: a feature is listed twice",
				"order: the features aren't listed bottom first",
				"gap: a listed feature's parent is neither the feature listed just below it nor already public (for example promote a c, leaving out b)",
				"parent_not_public: the bottom feature's parent is neither main nor already published",
				"parent_merged: the bottom's published parent merged; run sync",
				"parent_moved: the bottom's published parent changed since it was published",
				"stale_gate: a listed feature has no fresh, green gate; a gate goes stale when its own branch or any branch below it moves. With error.details.update_mode, the bottom's gate built its open PR's update in another history mode (gate builds in promote.default_mode), so gating again won't help: promote that feature on its own with --mode <update_mode>, which gates it in that mode, then the branches above it. error.details.updating is true when the stack's PRs are open: the new plan updates them all.",
				"open_above: two or more lines of open PRs stack on one published feature, and one plan updates one line: close or merge every other line's PRs on that feature on GitHub, then promote it again",
				"parent_stale: a branch above wasn't restacked after the branch below moved; restack, gate up, promote",
			},
			whatToDo: "Run ghostbranch status to see the stack. List the features bottom first, starting from one whose parent is main or already published, with none left out. Gate each one that needs it (ghostbranch gate <feature>, bottom up), then run promote again.",
		},
		"PARENT_REWRITTEN": {
			summary:  "The feature's parent no longer contains the commit the feature was branched from (forked_at): the parent was rebased, amended or force-pushed.",
			whatToDo: "Run ghostbranch feature restack <parent> to replay the feature's own commits onto the parent's new tip (it stops with git steps on a conflict), or ghostbranch feature restack <feature> --onto main to move it off the parent. Then run the command again. error.details.parent and error.details.forked_at name them.",
		},
	} {
		es, err := contract.Explain(code)
		if err != nil || len(es) != 1 {
			t.Fatalf("explain %s = %+v %v", code, es, err)
		}
		e := es[0]
		if e.Kind != contract.KindError || e.Summary != want.summary || e.WhatToDo != want.whatToDo ||
			!slices.Equal(e.Causes, want.causes) || e.NeedsHuman {
			t.Errorf("%s doc = %+v", code, e)
		}
	}
	if contract.PublicBranchChanged != "PUBLIC_BRANCH_CHANGED" || !slices.Contains(contract.AllWarnings(), contract.PublicBranchChanged) {
		t.Errorf("PublicBranchChanged = %q", contract.PublicBranchChanged)
	}
	// Its texts are pinned in TestSubProject61a2Contract: from 0.12 it is an
	// error too.
}

// The 6.1a-2 codes and texts are pinned word for word: the guide is
// generated from them, and agents act on them. PUBLIC_BRANCH_CHANGED is now
// promote's error as well as sync's warning; explain shows both, the error
// first.
func TestSubProject61a2Contract(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s", contract.SchemaVersion)
	}
	if string(contract.PRChangedOnGitHub) != "PUBLIC_BRANCH_CHANGED" || string(contract.PublicBranchChanged) != "PUBLIC_BRANCH_CHANGED" ||
		contract.PRChangedOnGitHub.Exit() != contract.ExitConflict || !slices.Contains(contract.AllCodes(), contract.PRChangedOnGitHub) ||
		!slices.Contains(contract.AllWarnings(), contract.PublicBranchChanged) {
		t.Fatalf("PUBLIC_BRANCH_CHANGED: error %q exit %d, warning %q", contract.PRChangedOnGitHub, contract.PRChangedOnGitHub.Exit(), contract.PublicBranchChanged)
	}
	es, err := contract.Explain("public_branch_changed")
	if err != nil || len(es) != 2 {
		t.Fatalf("explain PUBLIC_BRANCH_CHANGED = %+v %v", es, err)
	}
	e, w := es[0], es[1]
	if e.Kind != contract.KindError || e.ExitCode == nil || *e.ExitCode != 4 || e.NeedsHuman ||
		e.Summary != "The open PR's public branch holds commits Ghostbranch didn't publish (error.details.foreign), so the PR was not updated: the update would drop them. error.details.reason, when set, names a special case." ||
		!slices.Equal(e.Causes, []string{
			"someone pushed a commit to the PR's branch",
			"a review suggestion was committed on GitHub",
			"Update branch on GitHub merged main into the PR's branch",
			"the branch was force-pushed outside Ghostbranch",
			"unrecorded_update: the branch is at an update Ghostbranch pushed whose record was never written (error.details.plan); nothing is foreign",
			"adopted_missing: a preserve update would drop commits feature pull brought in from GitHub that the feature no longer has (error.details.foreign)",
		}) ||
		e.WhatToDo != "Bring them into the feature with ghostbranch feature pull <feature>, then gate and promote again. They are already public, so the gate doesn't scan them again; it scans your commits as always. If feature pull can't replay them cleanly, it stops with the git steps to merge them in yourself. For unrecorded_update, run ghostbranch apply <error.details.plan> on the machine that made the plan: it finds the push landed and writes the record (never feature pull, which would copy Ghostbranch's own commits into the feature). For adopted_missing, cherry-pick the listed commits back onto the feature, push it to the shadow repo, then gate and promote again. Ghostbranch never resolves conflicts, and never overwrites commits made on GitHub. error.details.foreign lists each commit's sha, author and subject." {
		t.Errorf("PUBLIC_BRANCH_CHANGED error doc = %+v", e)
	}
	if w.Kind != contract.KindWarning || w.ExitCode != nil ||
		w.Summary != "A published feature's public branch holds commits Ghostbranch didn't publish, so sync didn't adopt them; or (details.reason unrecorded_update) it is at an update Ghostbranch pushed but never recorded." ||
		w.WhatToDo != "Look at the PR on GitHub: someone pushed to its branch. Bring the commits in with ghostbranch feature pull <feature> (details.feature), then gate and promote; until then promote refuses to update the PR. Ghostbranch never adopts a change it didn't scan. For unrecorded_update, run ghostbranch apply <details.plan> on the machine that made the plan: it writes the record." {
		t.Errorf("PUBLIC_BRANCH_CHANGED warning doc = %+v", w)
	}
	es, err = contract.Explain(string(contract.BranchExists))
	if err != nil || len(es) != 1 ||
		es[0].Summary != "The name is taken: by a shadow branch, by a public branch Ghostbranch didn't publish for this feature, or by the branch of this feature's closed PR." ||
		es[0].WhatToDo != "With error.details.plan_id the branch holds an earlier, interrupted publish of this feature: resume it with ghostbranch apply <plan_id> rather than promoting under another name. Otherwise use another feature name, or promote with --branch <other-name>. A feature whose own PR is open is never refused with this: promote updates that PR." {
		t.Errorf("BRANCH_EXISTS doc = %+v %v", es, err)
	}
	es, _ = contract.Explain("4")
	found := false
	for _, x := range es {
		found = found || (x.Code == "PUBLIC_BRANCH_CHANGED" && x.Kind == contract.KindError)
	}
	if !found {
		t.Errorf("explain 4 lacks PUBLIC_BRANCH_CHANGED: %+v", es)
	}
}

// 6.4a moves the JSON contract to 0.13 and leaves every stored record at
// 0.12, so a 0.12 binary keeps working on records this build writes. Newer
// compares a record with RecordVersion: a 0.13 record would come from a
// later Ghostbranch whose record format this binary doesn't know.
func TestContractKeepsRecordsAt012(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" || contract.RecordVersion != "0.12" {
		t.Fatalf("SchemaVersion %s, RecordVersion %s", contract.SchemaVersion, contract.RecordVersion)
	}
	if contract.Newer("0.12") || !contract.Newer("0.13") {
		t.Fatal("Newer must compare against RecordVersion, not the contract version")
	}
	if ce := contract.NewerRecord("feature", "0.13"); ce.Details["supported"] != contract.RecordVersion ||
		!strings.Contains(ce.Message, "this binary knows 0.12") {
		t.Fatalf("NewerRecord = %+v", ce)
	}
	if !slices.Contains(contract.AllWarnings(), contract.UpdateAvailable) || contract.UpdateAvailable != "UPDATE_AVAILABLE" {
		t.Fatal("UPDATE_AVAILABLE is not registered")
	}
	es, err := contract.Explain("UPDATE_AVAILABLE")
	if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning ||
		!strings.Contains(es[0].WhatToDo, "Never run the upgrade yourself") || !strings.Contains(es[0].WhatToDo, "GHOSTBRANCH_NO_UPDATE_CHECK") {
		t.Fatalf("UPDATE_AVAILABLE doc = %+v %v", es, err)
	}
}

// 6.3b's three codes replace wrong uses of PAIR_AMBIGUOUS and BAD_FLAG. Each
// keeps exit 2 (decision 2); the contract moves to 0.14 and records stay
// 0.12. PAIR_AMBIGUOUS no longer sends an agent to init.
func TestSubProject63bCodesAreRegistered(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" || contract.RecordVersion != "0.12" {
		t.Fatalf("SchemaVersion %s, RecordVersion %s", contract.SchemaVersion, contract.RecordVersion)
	}
	for code, name := range map[contract.Code]string{
		contract.PairNotFound: "PAIR_NOT_FOUND", contract.NoOpenPR: "NO_OPEN_PR", contract.PlanNotFound: "PLAN_NOT_FOUND",
	} {
		if string(code) != name || code.Exit() != contract.ExitUsage || !slices.Contains(contract.AllCodes(), code) {
			t.Errorf("%s: %q exits %d", name, code, code.Exit())
		}
		es, err := contract.Explain(name)
		if err != nil || len(es) != 1 || es[0].NeedsHuman || es[0].Summary == "" || es[0].WhatToDo == "" || len(es[0].Causes) == 0 {
			t.Errorf("%s doc = %+v %v", name, es, err)
		}
	}
	es, _ := contract.Explain("PAIR_NOT_FOUND")
	if !strings.Contains(es[0].Summary, "isn't set up on this machine") || !strings.Contains(es[0].WhatToDo, "init <public> --adopt --plan") {
		t.Errorf("PAIR_NOT_FOUND doc = %+v", es[0])
	}
	es, _ = contract.Explain("PAIR_AMBIGUOUS")
	if strings.Contains(es[0].WhatToDo, "init") {
		t.Errorf("PAIR_AMBIGUOUS still offers init: %q", es[0].WhatToDo)
	}
	es, _ = contract.Explain("NO_OPEN_PR")
	if !slices.Equal(es[0].Causes, []string{"not_published: it was never published", "merged: its PR merged",
		"closed: its PR was closed without merging", "branch_gone: its public branch is gone"}) {
		t.Errorf("NO_OPEN_PR causes = %q", es[0].Causes)
	}
	es, _ = contract.Explain("PLAN_NOT_FOUND")
	if len(es[0].Causes) != 2 || !strings.HasPrefix(es[0].Causes[0], "other_machine:") || !strings.HasPrefix(es[0].Causes[1], "unknown:") {
		t.Errorf("PLAN_NOT_FOUND causes = %q", es[0].Causes)
	}
}

// Review M5: every details key the three codes carry is documented where
// explain and the guide read it. Review M4: other_machine also covers a
// plan made in another workspace or GHOSTBRANCH_HOME with this shadow repo.
func TestSubProject63bDetailsAreDocumented(t *testing.T) {
	t.Parallel()
	for code, keys := range map[string][]string{
		"PAIR_NOT_FOUND": {"error.details.public"},
		"NO_OPEN_PR":     {"error.details.reason", "error.details.feature", "error.details.public_pr"},
		"PLAN_NOT_FOUND": {"error.details.reason", "error.details.plan", "error.details.public", "error.details.feature"},
	} {
		es, err := contract.Explain(code)
		if err != nil {
			t.Fatal(err)
		}
		text := es[0].Summary + " " + es[0].WhatToDo
		for _, k := range keys {
			if !strings.Contains(text, k) {
				t.Errorf("%s doesn't document %s: %q", code, k, text)
			}
		}
	}
	// NO_OPEN_PR carries only spec § 4.3's keys (re-review N1, N3).
	if es, _ := contract.Explain("NO_OPEN_PR"); strings.Contains(es[0].Summary+es[0].WhatToDo, "details.plan") {
		t.Errorf("NO_OPEN_PR documents a plan: %+v", es[0])
	}
	es, _ := contract.Explain("PLAN_NOT_FOUND")
	if !strings.Contains(es[0].Causes[0], "another workspace or GHOSTBRANCH_HOME") || strings.Contains(es[0].Causes[1], "another workspace") {
		t.Errorf("PLAN_NOT_FOUND causes = %q", es[0].Causes)
	}
}

// 6.5's code and warning: FEATURE_ARCHIVED is a usage error (exit 2), and
// ARCHIVE_INCOMPLETE never changes the exit code. The contract moves to
// 0.15; records stay 0.12 (archived is an optional field).
func TestSubProject65CodesAreRegistered(t *testing.T) {
	t.Parallel()
	if contract.SchemaVersion != "0.15" || contract.RecordVersion != "0.12" {
		t.Fatalf("SchemaVersion %s, RecordVersion %s", contract.SchemaVersion, contract.RecordVersion)
	}
	if contract.FeatureArchived != "FEATURE_ARCHIVED" || contract.FeatureArchived.Exit() != contract.ExitUsage ||
		!slices.Contains(contract.AllCodes(), contract.FeatureArchived) {
		t.Fatalf("FEATURE_ARCHIVED = %q, exit %d", contract.FeatureArchived, contract.FeatureArchived.Exit())
	}
	if contract.ArchiveIncomplete != "ARCHIVE_INCOMPLETE" || !slices.Contains(contract.AllWarnings(), contract.ArchiveIncomplete) {
		t.Fatalf("ARCHIVE_INCOMPLETE = %q", contract.ArchiveIncomplete)
	}
	es, err := contract.Explain("FEATURE_ARCHIVED")
	if err != nil || len(es) != 1 || es[0].NeedsHuman || len(es[0].Causes) != 2 ||
		!strings.Contains(es[0].Summary, "promote, gate, feature pull and waivers add") || // P25
		!strings.Contains(es[0].WhatToDo, "git push origin HEAD:<error.details.branch>") ||
		!strings.Contains(es[0].WhatToDo, "ghostbranch sync closes the feature") {
		t.Fatalf("FEATURE_ARCHIVED doc = %+v %v", es, err)
	}
	es, err = contract.Explain("ARCHIVE_INCOMPLETE")
	if err != nil || len(es) != 1 || es[0].Kind != contract.KindWarning ||
		!strings.Contains(es[0].WhatToDo, "ghostbranch feature archive <details.feature>") {
		t.Fatalf("ARCHIVE_INCOMPLETE doc = %+v %v", es, err)
	}
	es, _ = contract.Explain("NO_OPEN_PR")
	if !strings.HasPrefix(es[0].Summary, "feature pull and feature archive need a feature's open public PR") {
		t.Fatalf("NO_OPEN_PR summary = %q", es[0].Summary)
	}
}

// P25 (6.6): FEATURE_ARCHIVED also covers building on an archived feature,
// and its exit code stays 2, so the contract stays 0.15.
func TestFeatureArchivedNamesTheStackingCommands(t *testing.T) {
	t.Parallel()
	es, err := contract.Explain("FEATURE_ARCHIVED")
	if err != nil || len(es) != 1 || *es[0].ExitCode != contract.ExitUsage ||
		!strings.Contains(es[0].Summary, "feature start --from and feature restack --onto can't build on it") {
		t.Fatalf("FEATURE_ARCHIVED = %+v %v", es, err)
	}
	if contract.SchemaVersion != "0.15" {
		t.Fatalf("SchemaVersion = %s, want 0.15", contract.SchemaVersion)
	}
}
