// Package approval decides whether a publish may go ahead. It is pure: the
// engine supplies whether a grant is valid, whether --yes was passed, and
// whether a front-end can ask a person. The actor label is never an input.
package approval

import "context"

// Reason says why a publish was not approved. The strings are part of the
// JSON contract (APPROVAL_REQUIRED details.reason).
type Reason string

const (
	NoConsole       Reason = "no_console"
	NotConfirmed    Reason = "not_confirmed"
	Timeout         Reason = "timeout"
	TooManyAttempts Reason = "too_many_attempts"
	GrantNeedsYes   Reason = "grant_needs_yes"
)

// Refusal is returned by an Approver when the person did not confirm.
type Refusal struct{ Reason Reason }

func (r *Refusal) Error() string { return "not approved: " + string(r.Reason) }

// Approver asks a person to confirm. It returns nil on a yes, a *Refusal
// when the person did not confirm, and any other error when asking failed.
type Approver interface {
	Confirm(ctx context.Context, summary string) error
}

type Outcome int

const (
	Refuse Outcome = iota
	UnderGrant
	Challenge
)

type Decision struct {
	Outcome Outcome
	Reason  Reason // set when Outcome is Refuse
}

// Decide applies spec §6.2, first matching row wins:
//  1. a valid grant and --yes: publish under the grant
//  2. an approver: challenge the person
//  3. a valid grant without --yes: refuse; the caller can add --yes
//  4. otherwise: refuse; a person must approve at a console
func Decide(grantValid, yes, approver bool) Decision {
	switch {
	case grantValid && yes:
		return Decision{Outcome: UnderGrant}
	case approver:
		return Decision{Outcome: Challenge}
	case grantValid:
		return Decision{Outcome: Refuse, Reason: GrantNeedsYes}
	default:
		return Decision{Outcome: Refuse, Reason: NoConsole}
	}
}

// Choice is a question asked just above the confirmation code: 6.5's
// Archive or Keep. Options[Default] is what Enter picks. An option with
// Unavailable set is shown with its reason and can't be picked.
type Choice struct {
	Title   string
	Options []Option
	Default int
}

// Option is one answer to a Choice: Key or its number picks it.
type Option struct {
	Key         string
	Name        string
	Text        string
	Unavailable string
}

// Chooser is an Approver that can also ask a Choice above the code. It
// returns the chosen option's index once the person typed the code, and
// the same refusals as Confirm.
type Chooser interface {
	Approver
	ConfirmChoosing(ctx context.Context, summary string, c Choice) (int, error)
}
