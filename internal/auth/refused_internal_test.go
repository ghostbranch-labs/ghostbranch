package auth

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// A declined login, and a code GitHub sends that Ghostbranch doesn't know,
// say what happened and offer the two ways without the device flow. The
// code and reason stay as they were.
func TestRefusedOffersTheTokenWays(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ code, reason, says string }{
		{"access_denied", "denied", "the login was declined on GitHub"},
		{"weird_code", "refused", "GitHub refused the login (weird_code)"},
	} {
		ce := contract.From(refused(c.code))
		if ce.Code != contract.LoginUnavailable || ce.Details["reason"] != c.reason {
			t.Fatalf("%s: %v %v", c.code, ce.Code, ce.Details)
		}
		for _, want := range []string{c.says, "nothing was stored", "ghostbranch login --with-token", "GHOSTBRANCH_TOKEN"} {
			if !strings.Contains(ce.Message, want) {
				t.Errorf("%s: %q lacks %q", c.code, ce.Message, want)
			}
		}
	}
}
