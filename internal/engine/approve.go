package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
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
		return grant.Grant{}, false, grantErr(err, "read agent grants")
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
// authors holds each public commit's author, read from the commit itself,
// not the plan file; pl.Publish.Parent must be the verified public main.
//
// Every string in it that an agent or the feature controls goes through
// visible, so a control character can't move the cursor, erase a line or
// reorder text on the person's console.
func planSummary(pl plan.Plan, files []string, authors []string, waived []WaivedFinding, warnings []contract.Warning) string {
	var b strings.Builder
	if pl.Publish.Update && len(pl.PRs) == 1 {
		n := pl.PRs[0].Number
		fmt.Fprintf(&b, "%s in %s:\n", updateHeadline(n, pl.Publish), visible(pl.Pair.Public))
		if k := len(pl.Publish.Restores); k > 0 {
			that := "that was"
			if k > 1 {
				that = "that were"
			}
			fmt.Fprintf(&b, "  puts back %s %s removed from PR #%d on GitHub\n", plural(k, "commit"), that, n)
		}
	} else {
		fmt.Fprintf(&b, "About to publish to %s:\n", visible(pl.Pair.Public))
	}
	publishLines(&b, pl.Mode, pl.Publish, pl.Typed, files, authors)
	tailLines(&b, waived, pl.CI.RepoGuards, warnings)
	fmt.Fprintf(&b, "Plan %s", visible(pl.ID))
	return b.String()
}

// publishLines is one PR's part of an approval summary: its branch, its
// history on the commit below it, each commit with its author as stored,
// the commits dropped as empty, the PR text and the files it changes. An
// update of an open PR shows only the texts the person typed (typed): the
// others stay as they are on GitHub.
func publishLines(b *strings.Builder, mode string, pub plan.Publish, typed plan.Typed, files, authors []string) {
	fmt.Fprintf(b, "  branch   %s\n", visible(pub.TargetBranch))
	fmt.Fprintf(b, "  history  %s, %d commit(s) on %s\n", visible(mode), len(pub.Commits), short(pub.Parent))
	for i, c := range pub.Commits {
		if i == 20 {
			fmt.Fprintf(b, "           … and %d more\n", len(pub.Commits)-20)
			break
		}
		fmt.Fprintf(b, "           %s %s (%s)\n", short(c.Commit), visible(c.Subject), visible(authors[i]))
	}
	if n := len(pub.DroppedEmpty); n > 0 {
		fmt.Fprintf(b, "  dropped  %d commit(s) with nothing public in them\n", n)
	}
	switch {
	case !pub.Update || typed.Title:
		fmt.Fprintf(b, "  PR title %s\n", visible(pub.PR.Title))
	case !typed.Body:
		b.WriteString("  PR       keeps its title and body\n")
	default:
		b.WriteString("  PR title keeps its title\n")
	}
	body := strings.Split(strings.TrimSpace(pub.PR.Body), "\n")
	switch {
	case pub.Update && !typed.Body:
		if typed.Title {
			b.WriteString("  PR body  keeps its body\n")
		}
	case len(body) == 1 && body[0] == "":
		b.WriteString("  PR body  (empty)\n")
	default:
		b.WriteString("  PR body\n")
		for i, l := range body {
			if i == summaryBodyLines {
				fmt.Fprintf(b, "           … %d more lines\n", len(body)-summaryBodyLines)
				break
			}
			fmt.Fprintf(b, "           %s\n", visible(strings.TrimSuffix(l, "\r")))
		}
	}
	fmt.Fprintf(b, "  files    %d changed\n", len(files))
	for i, f := range files {
		if i == 20 {
			fmt.Fprintf(b, "           … and %d more\n", len(files)-20)
			break
		}
		fmt.Fprintf(b, "           %s\n", visible(f))
	}
}

// tailLines is the end of an approval summary: the waived findings, the
// repo guards CI did not test, and the scan's warnings.
func tailLines(b *strings.Builder, waived []WaivedFinding, guards workflow.GuardReport, warnings []contract.Warning) {
	for _, w := range waived {
		fmt.Fprintf(b, "  waived   %s %s in %s (%s)\n", visible(w.ID), visible(w.Rule), visible(Location(w.Finding)), visible(w.Reason))
	}
	for _, g := range guards.Unrewritable {
		fmt.Fprintf(b, "  guard    %s skips in the shadow repo, so CI did not test it: %s\n", visible(guardWhere(g)), visible(g.Expr))
	}
	for _, g := range guards.Skipped {
		fmt.Fprintf(b, "  guard    %s skips in the shadow repo (ci.rewrite_repo_guards is warn), so CI did not test it: %s\n", visible(guardWhere(g)), visible(g.Expr))
	}
	for _, w := range warnings {
		fmt.Fprintf(b, "  warning  %s\n", visible(w.Message))
	}
}

// stackSummary is the approval view of a stack plan: the number of PRs,
// then each, bottom first, with the branch it is based on and what it
// publishes (its files measured from the PR below it), then the waived
// findings and warnings once. One code approves them all. shown holds each
// branch's commits with their subjects as stored, authors each commit's
// author as stored.
func stackSummary(pl plan.Plan, shown [][]plan.Commit, authors, files [][]string, waived []WaivedFinding, warnings []contract.Warning) string {
	var b strings.Builder
	n := len(pl.Chain)
	fmt.Fprintf(&b, "About to publish %d PR(s) to %s, bottom first; one code approves them all:\n", n, visible(pl.Pair.Public))
	for i, en := range pl.Chain {
		pub := en.Publish
		pub.Commits = shown[i]
		fmt.Fprintf(&b, "PR %d of %d: %s, based on %s\n", i+1, n, visible(en.Feature), visible(pl.PRs[i].Base))
		publishLines(&b, en.Mode, pub, en.Typed, files[i], authors[i])
		tailLines(&b, nil, en.CI.RepoGuards, nil)
	}
	tailLines(&b, waived, workflow.GuardReport{}, warnings)
	fmt.Fprintf(&b, "Plan %s", visible(pl.ID))
	return b.String()
}

// summaryBodyLines is how much of the PR body the approval summary shows.
const summaryBodyLines = 10

// visible escapes what a terminal would act on rather than show: control
// characters (tab, newline and carriage return included, since a field is
// one line), the Unicode line and paragraph separators, bidi controls and
// invalid UTF-8. Each comes out as a \uXXXX (or \xXX) escape, in the style
// of strconv.QuoteToGraphic; other text is unchanged.
func visible(s string) string {
	if !strings.ContainsFunc(s, unsafeRune) && utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case unsafeRune(r):
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

func unsafeRune(r rune) bool {
	switch {
	case unicode.Is(unicode.Cc, r), unicode.Is(unicode.Zl, r), unicode.Is(unicode.Zp, r):
		return true
	case r >= 0x202A && r <= 0x202E, r >= 0x2066 && r <= 0x2069, r == 0x200E, r == 0x200F:
		return true
	}
	return false
}

// noConsole is APPROVAL_REQUIRED for an action with no plan that only a
// person at a console may take; what names the action.
func noConsole(what string) error {
	return contract.New(contract.ApprovalRequired, "%s needs a person to type a code at a console", what).
		WithDetails(map[string]any{"reason": string(approval.NoConsole)})
}
