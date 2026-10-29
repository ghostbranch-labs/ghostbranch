package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

// A LOCKED from workspace.Update reaches the caller as it is, holder and all,
// so it can retry; any other failure is INTERNAL and says what was being saved.
func TestUpdateErr(t *testing.T) {
	t.Parallel()
	locked := contract.New(contract.Locked, "workspace.json is held").
		WithDetails(map[string]any{"held_by": map[string]any{"pid": 42, "command": "promote"}})
	cases := []struct {
		name     string
		err      error
		wantCode contract.Code
		same     bool
	}{
		{"locked is passed on unchanged", locked, contract.Locked, true},
		{"a plain error becomes internal", errors.New("disk full"), contract.Internal, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := updateErr(tc.err, "save workspace.json")
			ce := contract.From(got)
			if ce.Code != tc.wantCode {
				t.Fatalf("code = %s, want %s", ce.Code, tc.wantCode)
			}
			if tc.same {
				if got != tc.err || ce.Details["held_by"] == nil {
					t.Fatalf("the LOCKED error was changed: %v %v", got, ce.Details)
				}
				return
			}
			if !strings.Contains(ce.Message, "save workspace.json") || !strings.Contains(ce.Message, "disk full") {
				t.Fatalf("message = %q", ce.Message)
			}
		})
	}
}

// A stale_gate refusal of a PR update names the pair's base branch among
// what may have moved, not main (8.12).
func TestUpdateGateErrNamesTheBase(t *testing.T) {
	t.Parallel()
	m := feature.Meta{Gate: &feature.Gate{}}
	ce := updateGateErr("a", []string{"a"}, m, &prUpdate{PR: 7}, "squash", "preserve", false, false, "trunk")
	if ce.Details["reason"] != "stale_gate" || !strings.Contains(ce.Message, "(a, public trunk, the PR's head or base") || strings.Contains(ce.Message, "main") {
		t.Fatalf("stale_gate = %q %v", ce.Message, ce.Details)
	}
}
