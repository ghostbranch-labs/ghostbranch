package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

var approvalWhy = map[approval.Reason]string{
	approval.NoConsole:       "publishing needs a person to type a code at a console, or an agent grant (ghostbranch agents allow)",
	approval.NotConfirmed:    "not confirmed",
	approval.Timeout:         "no answer at the console within 5 minutes",
	approval.TooManyAttempts: "the code was typed wrong 3 times",
	approval.GrantNeedsYes:   "agents may publish here under a grant; pass --yes to do so",
}

// grantNoConsole is no_console for agents allow, which publishes nothing.
const grantNoConsole = "making an agent grant needs a person to type a code at a console"

// needsApproval is APPROVAL_REQUIRED with its machine-readable reason. A
// planID of "" is for approvals that have no plan (agents allow).
func needsApproval(planID string, r approval.Reason) error {
	details := map[string]any{"reason": string(r)}
	if planID == "" {
		why := approvalWhy[r]
		if r == approval.NoConsole {
			why = grantNoConsole
		}
		return contract.New(contract.ApprovalRequired, "%s", why).WithDetails(details)
	}
	details["plan_id"] = planID
	return contract.New(contract.ApprovalRequired, "%s; plan %s is saved", approvalWhy[r], planID).WithDetails(details)
}

// grantFor returns the grant in force for p. An unreadable grants file is an
// error: it never counts as "no grant", and never as a grant.
func (e *Engine) grantFor(p workspace.Pair) (grant.Grant, bool, error) {
	if e.Grants == nil {
		return grant.Grant{}, false, nil
	}
	g, ok, err := e.Grants.Valid(p.Public, p.Shadow, e.now())
	if err != nil {
		return grant.Grant{}, false, internalErr(err, "read agent grants")
	}
	return g, ok, nil
}

// confirm asks the person at the console. A refusal keeps its reason.
func (e *Engine) confirm(ctx context.Context, planID, summary string) error {
	err := e.Approver.Confirm(ctx, summary)
	var ref *approval.Refusal
	if errors.As(err, &ref) {
		return needsApproval(planID, ref.Reason)
	}
	if err != nil {
		return internalErr(err, "ask for confirmation on the console")
	}
	return nil
}

// authorize is spec §6.2: the last step before a public push. The actor
// label is never consulted.
func (e *Engine) authorize(ctx context.Context, p workspace.Pair, planID, summary string, yes bool) (*ledger.Approval, error) {
	g, valid, err := e.grantFor(p)
	if err != nil {
		return nil, err
	}
	d := approval.Decide(valid, yes, e.Approver != nil)
	switch d.Outcome {
	case approval.UnderGrant:
		return &ledger.Approval{Kind: "grant", GrantID: g.ID, CreatedBy: g.CreatedBy}, nil
	case approval.Challenge:
		if err := e.confirm(ctx, planID, summary); err != nil {
			return nil, err
		}
		return &ledger.Approval{Kind: "console"}, nil
	}
	return nil, needsApproval(planID, d.Reason)
}

// planSummary is the "what goes public" view shown before confirming.
func planSummary(pl plan.Plan, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "About to publish to %s:\n", pl.Pair.Public)
	fmt.Fprintf(&b, "  branch   %s\n", pl.Publish.TargetBranch)
	fmt.Fprintf(&b, "  commit   %s (parent %s)\n", short(pl.Publish.Commit), short(pl.Publish.Parent))
	fmt.Fprintf(&b, "  author   %s <%s>\n", pl.Publish.Author.Name, pl.Publish.Author.Email)
	fmt.Fprintf(&b, "  PR title %s\n", pl.Publish.PR.Title)
	fmt.Fprintf(&b, "  files    %d changed\n", len(files))
	for i, f := range files {
		if i == 20 {
			fmt.Fprintf(&b, "           … and %d more\n", len(files)-20)
			break
		}
		fmt.Fprintf(&b, "           %s\n", f)
	}
	fmt.Fprintf(&b, "Plan %s", pl.ID)
	return b.String()
}
