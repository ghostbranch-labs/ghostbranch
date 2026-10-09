package engine

import (
	"errors"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
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
