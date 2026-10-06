package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func findings(b *strings.Builder, fs []leak.Finding) {
	for _, f := range fs {
		loc := f.Where
		if f.Path != "" {
			loc = fmt.Sprintf("%s:%d", f.Path, f.Line)
		}
		fmt.Fprintf(b, "  leak: %s in %s\n", f.Rule, loc)
	}
}

func previewText(b *strings.Builder, p *preview.Preview) {
	b.WriteString("Dry run; nothing was changed. This would:\n")
	for _, line := range p.Describe() {
		fmt.Fprintf(b, "  - %s\n", line)
	}
	if len(p.Ops) == 0 {
		b.WriteString("  (nothing)\n")
	}
}

// human renders a result for people. It never prints secret values.
func human(r any) string {
	var b strings.Builder
	switch v := r.(type) {
	case *engine.InitResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Shadow repo %s is ready for %s.\n", v.Shadow, v.Public)
		if len(v.MirroredBranches)+len(v.MirroredTags) > 0 {
			fmt.Fprintf(&b, "  mirrored %d branch(es) and %d tag(s)\n", len(v.MirroredBranches), len(v.MirroredTags))
		}
		if v.OverlayCreated {
			b.WriteString("  created the ghostbranch overlay branch (.ghostbranch.yml)\n")
		}
	case *engine.SyncResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		switch v.Action {
		case "noop":
			fmt.Fprintf(&b, "Shadow main is up to date with public main (%s).\n", short(v.PublicMain))
		case "fast_forward":
			fmt.Fprintf(&b, "Shadow main fast-forwarded %s -> %s.\n", short(v.ShadowMainBefore), short(v.PublicMain))
		default:
			b.WriteString("Shadow main has diverged from public main; nothing was pushed.\n")
		}
	case *engine.FeatureStartResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Started feature %s from shadow main %s.\nCheck it out with: %s\n", v.Name, short(v.Base), v.Checkout)
	case *engine.GateResult:
		fmt.Fprintf(&b, "Gate for %s: checks %s, leak scan %s.\n", v.Feature, v.Checks, v.Leak)
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		if v.ShadowPR > 0 {
			fmt.Fprintf(&b, "  candidate %s on shadow PR #%d\n", short(v.Candidate), v.ShadowPR)
		}
		for _, c := range v.FailedChecks {
			fmt.Fprintf(&b, "  failed check: %s\n", c)
		}
		findings(&b, v.Findings)
	case *engine.PromoteResult:
		switch {
		case v.Preview != nil:
			previewText(&b, v.Preview)
		case v.Applied != nil:
			fmt.Fprintf(&b, "Published %s as PR #%d: %s\n", v.Applied.PublicBranch, v.Applied.PublicPR, v.Applied.PRURL)
		case v.Plan != nil && v.PlanPath != "":
			fmt.Fprintf(&b, "Plan %s saved to %s.\nPublish it with: ghostbranch apply %s\n", v.Plan.ID, v.PlanPath, v.Plan.ID)
		}
		findings(&b, v.Findings)
	case *engine.ApplyResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Published %s as PR #%d: %s\n", v.PublicBranch, v.PublicPR, v.PRURL)
	case *engine.AgentsAllowResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		until := "until revoked"
		if v.Grant.ExpiresAt != nil {
			until = "until " + v.Grant.ExpiresAt.UTC().Format("2006-01-02 15:04 UTC")
		}
		fmt.Fprintf(&b, "Agents on this machine may publish to %s with --yes, %s.\nTake it back with: ghostbranch agents revoke --repo %s\n",
			v.Grant.Public, until, v.Grant.Public)
	case *engine.AgentsRevokeResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		if len(v.Revoked) == 0 {
			b.WriteString("There was no agent grant to revoke.\n")
		}
		for _, g := range v.Revoked {
			fmt.Fprintf(&b, "Agents may no longer publish to %s.\n", g.Public)
		}
	case *engine.AgentsStatusResult:
		if len(v.Grants) == 0 {
			b.WriteString("No agent grants on this machine.\n")
		}
		for _, g := range v.Grants {
			switch {
			case g.Expired:
				fmt.Fprintf(&b, "%s: expired\n", g.Public)
			case g.RemainingSeconds == nil:
				fmt.Fprintf(&b, "%s: agents may publish, until revoked\n", g.Public)
			default:
				fmt.Fprintf(&b, "%s: agents may publish for %s more\n", g.Public, (time.Duration(*g.RemainingSeconds) * time.Second).String())
			}
		}
		if v.Pair != "" {
			fmt.Fprintf(&b, "The token from %s can publish to %s: %s\n", v.Token.Source, v.Pair, v.TokenCanPublish)
		}
	}
	return b.String()
}
