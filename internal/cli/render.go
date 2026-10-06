package cli

import (
	"fmt"
	"strings"

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
	}
	return b.String()
}
