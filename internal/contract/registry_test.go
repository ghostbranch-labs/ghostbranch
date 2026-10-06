package contract

import "testing"

func TestEveryCodeIsDocumentedExactlyOnce(t *testing.T) {
	if len(errorDocs) != len(exitCodes) {
		t.Errorf("errorDocs has %d entries, exitCodes %d: a code is undocumented or a doc has no code", len(errorDocs), len(exitCodes))
	}
	if len(warningDocs) != len(warningCodes) {
		t.Errorf("warningDocs has %d entries, warningCodes %d", len(warningDocs), len(warningCodes))
	}
	if len(exitDocs) != 7 {
		t.Errorf("exitDocs has %d entries, want 7 (0-6)", len(exitDocs))
	}
	seen := map[string]bool{}
	for _, e := range Registry() {
		if e.Summary == "" || e.WhatToDo == "" {
			t.Errorf("%s: empty summary or what_to_do", e.Code)
		}
		if seen[e.Code] {
			t.Errorf("%s listed twice", e.Code)
		}
		seen[e.Code] = true
		if (e.Kind == KindWarning) != (e.ExitCode == nil) {
			t.Errorf("%s: warnings and only warnings have no exit code", e.Code)
		}
	}
	for _, c := range AllCodes() {
		if !seen[string(c)] {
			t.Errorf("error %s missing from the registry", c)
		}
	}
	for _, w := range AllWarnings() {
		if !seen[string(w)] {
			t.Errorf("warning %s missing from the registry", w)
		}
	}
}

func TestExplain(t *testing.T) {
	es, err := Explain("5")
	if err != nil || len(es) != 2 || es[0].Kind != KindExit || es[1].Code != "APPROVAL_REQUIRED" || !es[1].NeedsHuman {
		t.Fatalf("explain 5 = %+v %v", es, err)
	}
	es, err = Explain(" leak_found ")
	if err != nil || len(es) != 1 || es[0].Code != "LEAK_FOUND" || *es[0].ExitCode != 3 {
		t.Fatalf("explain leak_found = %+v %v", es, err)
	}
	es, err = Explain("ACTOR_UNKNOWN")
	if err != nil || es[0].Kind != KindWarning || es[0].ExitCode != nil {
		t.Fatalf("explain ACTOR_UNKNOWN = %+v %v", es, err)
	}
	for _, bad := range []string{"NOPE", "9", "-1", ""} {
		_, err := Explain(bad)
		if ce := From(err); ce == nil || ce.Code != BadFlag || ce.Field != "code" {
			t.Errorf("explain %q: want BAD_FLAG on field code, got %v", bad, err)
		}
	}
	if (TokenCannotPublish.Exit() != ExitFailure) || NotAPairCheckout.Exit() != ExitUsage || PushBlocked.Exit() != ExitFailure || GuardUnsupported.Exit() != ExitFailure {
		t.Fatal("new codes map to the wrong exit codes")
	}
}
