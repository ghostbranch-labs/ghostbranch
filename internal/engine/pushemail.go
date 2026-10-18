package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

const emailSettingsURL = "https://github.com/settings/emails"

// explainGH007 explains GitHub's GH007 refusal ("your push would publish a
// private email address"). GitHub checks the tip commit of each pushed ref,
// so it looks up the first GH007 ref's new tip in the mirror g, with no
// network call, and says which commit and email tripped it. It returns the
// message and the extra error details; ok is false when the refusal is not
// GH007 or the lookup fails, and the caller keeps its own message.
func explainGH007(ctx context.Context, g *gitx.Git, refspecs []string, refused *gitx.RefusedError, public string) (msg string, details map[string]any, ok bool) {
	ref := ""
	for i, r := range refused.Refs {
		if i < len(refused.RefReasons) && strings.Contains(refused.RefReasons[i], "GH007") {
			ref = r
			break
		}
	}
	if ref == "" {
		if !strings.Contains(refused.Reason, "GH007") || len(refused.Refs) == 0 {
			return "", nil, false
		}
		ref = refused.Refs[0]
	}
	src := ""
	for _, spec := range refspecs {
		s, dst, found := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if found && dst == ref {
			src = s
			break
		}
	}
	if src == "" {
		return "", nil, false
	}
	tip, found, err := g.Resolve(ctx, src+"^{commit}")
	if err != nil || !found {
		return "", nil, false
	}
	out, err := g.Run(ctx, "log", "-1", "--format=%s%x00%ae", tip)
	if err != nil {
		return "", nil, false
	}
	subject, email, _ := strings.Cut(out, "\x00")
	onPublic := false
	if pub, there, err := g.Resolve(ctx, "refs/remotes/public/main"); err == nil && there {
		if onPublic, err = g.IsAncestor(ctx, tip, pub); err != nil {
			return "", nil, false
		}
	}
	branch := strings.TrimPrefix(ref, "refs/heads/")
	var b strings.Builder
	fmt.Fprintf(&b, "GitHub refused the push because the newest commit on %s (%s, \"%s\")\n", branch, tip[:7], subject)
	fmt.Fprintf(&b, "has your private email, %s, and your account blocks pushes that expose it.\n", email)
	if onPublic {
		fmt.Fprintf(&b, "That commit is already public in %s; your own commits are fine.\n", public)
		b.WriteString("Fix one of these:\n")
		fmt.Fprintf(&b, "  - untick \"Block command line pushes that expose my email\" at %s, or\n", emailSettingsURL)
		fmt.Fprintf(&b, "  - add any new commit to %s's main using your no-reply address, then run this again", public)
	} else {
		b.WriteString("Fix one of these:\n")
		fmt.Fprintf(&b, "  - untick \"Block command line pushes that expose my email\" at %s, or\n", emailSettingsURL)
		b.WriteString("  - set git's user.email to your GitHub no-reply address (shown on that page), recreate the commit, and push again")
	}
	return b.String(), map[string]any{
		"cause": "email_private", "commit": tip, "email": email, "branch": branch, "settings_url": emailSettingsURL,
	}, true
}
