package approval_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
)

// Spec §6.2, every combination. --yes without a grant never approves.
func TestDecide(t *testing.T) {
	cases := []struct {
		grant, yes, console bool
		want                approval.Decision
	}{
		{true, true, true, approval.Decision{Outcome: approval.UnderGrant}},
		{true, true, false, approval.Decision{Outcome: approval.UnderGrant}},
		{true, false, true, approval.Decision{Outcome: approval.Challenge}},
		{false, true, true, approval.Decision{Outcome: approval.Challenge}},
		{false, false, true, approval.Decision{Outcome: approval.Challenge}},
		{true, false, false, approval.Decision{Outcome: approval.Refuse, Reason: approval.GrantNeedsYes}},
		{false, true, false, approval.Decision{Outcome: approval.Refuse, Reason: approval.NoConsole}},
		{false, false, false, approval.Decision{Outcome: approval.Refuse, Reason: approval.NoConsole}},
	}
	for _, c := range cases {
		if got := approval.Decide(c.grant, c.yes, c.console); got != c.want {
			t.Errorf("grant=%v yes=%v console=%v: got %+v, want %+v", c.grant, c.yes, c.console, got, c.want)
		}
	}
}

func TestRefusalIsAnError(t *testing.T) {
	var err error = &approval.Refusal{Reason: approval.Timeout}
	if err.Error() != "not approved: timeout" {
		t.Fatal(err)
	}
}
