package cli

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/guide"
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
		what := f.Rule
		if f.Match != "" {
			what += fmt.Sprintf(" %q", f.Match)
		}
		in := ""
		if len(f.Commits) > 0 {
			shas := make([]string, len(f.Commits))
			for i, c := range f.Commits {
				shas[i] = short(c)
			}
			in = " (commit(s) " + strings.Join(shas, ", ") + ")"
		}
		fmt.Fprintf(b, "  leak %s: %s in %s%s\n", f.ID, what, engine.Location(f), in)
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
	case *guide.Printed:
		b.WriteString(v.Markdown)
	case *guide.InstallResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Skill for %s %s: %s\n", v.Target, v.Action, v.Path)
	case *engine.DoctorResult:
		for _, c := range v.Checks {
			doctorLine(&b, c)
		}
	case *guard.Result:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Guard %s in %s.\n", v.Action, v.Status.HooksDir)
		if v.Status.Chained {
			b.WriteString("  your existing pre-push hook still runs after it\n")
		}
	case *guard.Status:
		if !v.Installed {
			fmt.Fprintf(&b, "The guard is not installed in %s. Install it with: ghostbranch guard install\n", v.HooksDir)
			break
		}
		fmt.Fprintf(&b, "The guard is installed in %s, covering %s via %s.\n", v.HooksDir, strings.Join(v.Publics, ", "), strings.Join(v.Remotes, ", "))
		if !v.BinaryFound {
			fmt.Fprintf(&b, "  warning: %s is gone, so public pushes are refused until you reinstall\n", v.Binary)
		}
	case *guard.CheckResult:
		// It runs inside every public git push: a pass says nothing.
	case *ExplainResult:
		for _, e := range v.Entries {
			exit := ""
			if e.ExitCode != nil {
				exit = fmt.Sprintf(", exit %d", *e.ExitCode)
			}
			fmt.Fprintf(&b, "%s (%s%s): %s\n", e.Code, e.Kind, exit, e.Summary)
			for _, c := range e.Causes {
				fmt.Fprintf(&b, "  cause: %s\n", c)
			}
			fmt.Fprintf(&b, "  what to do: %s\n", e.WhatToDo)
			if e.NeedsHuman {
				b.WriteString("  a person must act\n")
			}
		}
	case *engine.InitResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		if v.Ready {
			fmt.Fprintf(&b, "Shadow repo %s is ready for %s.\n", v.Shadow, v.Public)
		} else {
			fmt.Fprintf(&b, "Init of %s for %s stopped before the shadow repo was ready.\n", v.Shadow, v.Public)
		}
		if len(v.MirroredBranches)+len(v.MirroredTags) > 0 {
			fmt.Fprintf(&b, "  mirrored %d branch(es) and %d tag(s)\n", len(v.MirroredBranches), len(v.MirroredTags))
		}
		if v.OverlayCreated {
			b.WriteString("  created the ghostbranch overlay branch (.ghostbranch.yml)\n")
		}
		if v.Audit != nil {
			auditText(&b, v.Audit)
		}
		if v.GuardInstalled {
			b.WriteString("  the push guard is installed in this checkout\n")
		}
		if v.Doctor != nil {
			fmt.Fprintf(&b, "Doctor: %d ok, %d warning(s), %d failed, %d skipped\n", v.Doctor.OK, v.Doctor.Warn, v.Doctor.Fail, v.Doctor.Skipped)
			for _, c := range v.DoctorChecks {
				if c.Status != engine.DoctorOK {
					doctorLine(&b, c)
				}
			}
		}
		if v.Guided && v.Ready {
			b.WriteString(walkthrough)
		}
	case *engine.AuditResult:
		auditText(&b, v)
	case *engine.UpgradeResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			for _, f := range v.Files {
				if f.ReplacesLocalEdits {
					fmt.Fprintf(&b, "  warning: %s was edited by hand; upgrade replaces those edits\n", f.Path)
				}
			}
			break
		}
		if !v.Changed {
			b.WriteString("The overlay's managed files are current; nothing to do.\n")
		}
		for _, f := range v.Files {
			if f.Action != "unchanged" {
				fmt.Fprintf(&b, "  %s %s\n", f.Action, f.Path)
			}
			if f.ReplacesLocalEdits {
				fmt.Fprintf(&b, "  replaced hand edits in %s\n", f.Path)
			}
		}
		if v.Commit != "" {
			fmt.Fprintf(&b, "Upgraded the overlay branch in %s.\n", short(v.Commit))
		}
		if v.Audit != nil {
			auditText(&b, v.Audit)
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
		case "push_failed":
			fmt.Fprintf(&b, "Shadow main could not be pushed; it was at %s when sync read it.\n", short(v.ShadowMainBefore))
		default:
			b.WriteString("Shadow main has diverged from public main; nothing was pushed to it.\n")
		}
		for _, br := range v.Branches {
			if br.Name != "main" {
				fmt.Fprintf(&b, "  %s: %s\n", br.Name, br.Status)
			}
		}
		if n := len(v.CancelledRuns); n > 0 {
			fmt.Fprintf(&b, "  cancelled %d push-triggered workflow run(s) in the shadow repo\n", n)
		}
		for _, c := range v.Closed {
			if c.GatePRLeftOpen != 0 {
				fmt.Fprintf(&b, "  deleted merged feature %s (PR #%d), but its gate PR #%d is still open\n", c.Name, c.PublicPR, c.GatePRLeftOpen)
				continue
			}
			fmt.Fprintf(&b, "  closed merged feature %s (PR #%d)\n", c.Name, c.PublicPR)
		}
	case *engine.FeatureStartResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Started feature %s from shadow main %s.\nCheck it out with: %s\n", v.Name, short(v.Base), v.CheckoutHint)
	case *engine.GateResult:
		if pendingJob(v) {
			b.WriteString(pendingLine(v))
			fmt.Fprintf(&b, "  %d commit(s), scanned as %s\n", len(v.Commits), v.ScannedAs)
			waivedLines(&b, v.Waived)
			break
		}
		fmt.Fprintf(&b, "Gate for %s: checks %s, leak scan %s.\n", v.Feature, v.Checks, v.Leak)
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "  %d commit(s), scanned as %s\n", len(v.Commits), v.ScannedAs)
		if v.ShadowPR != nil {
			fmt.Fprintf(&b, "  candidate %s on shadow PR #%d\n", short(v.Candidate), *v.ShadowPR)
		}
		for _, c := range v.FailedChecks {
			fmt.Fprintf(&b, "  failed check: %s\n", c)
		}
		findings(&b, v.Findings)
		waivedLines(&b, v.Waived)
	case *engine.PromoteResult:
		switch {
		case v.Preview != nil:
			previewText(&b, v.Preview)
		case v.Applied != nil:
			fmt.Fprintf(&b, "Published %s as PR #%d: %s\n", v.Applied.PublicBranch, v.Applied.PublicPR, v.Applied.PRURL)
		case v.Plan != nil && v.PlanPath != "":
			fmt.Fprintf(&b, "Plan %s saved to %s.\n", v.Plan.ID, v.PlanPath)
			fmt.Fprintf(&b, "  %s: %d public commit(s)", v.Plan.Mode, len(v.Plan.Publish.Commits))
			if n := len(v.Plan.Publish.DroppedEmpty); n > 0 {
				fmt.Fprintf(&b, ", %d dropped as empty", n)
			}
			fmt.Fprintf(&b, "\nPublish it with: ghostbranch apply %s\n", v.Plan.ID)
		case v.Gate != nil && pendingJob(v.Gate):
			b.WriteString(pendingLine(v.Gate))
			b.WriteString("No plan yet: wait for the job, then promote again.\n")
		}
		findings(&b, v.Findings)
		waivedLines(&b, v.Waived)
	case *engine.ApplyResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Published %s as PR #%d: %s\n", v.PublicBranch, v.PublicPR, v.PRURL)
		waivedLines(&b, v.Waived)
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
	case *engine.WaiversAddResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Waived %d finding(s) in %s.\n", len(v.Waivers), v.Feature)
	case *engine.WaiversListResult:
		if len(v.Waivers) == 0 {
			b.WriteString("No waivers.\n")
			break
		}
		for _, w := range v.Waivers {
			fmt.Fprintf(&b, "%s  %s  %s in %s: %s (%s, %s)\n", w.ID, w.Public, w.Rule, w.Path, w.Reason, w.CreatedBy, w.CreatedAt.Format(time.RFC3339))
		}
	case *engine.WaiversRevokeResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Revoked %d waiver(s).\n", len(v.Revoked))
	case *engine.RemoveResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Removed %s from this machine: revoked %d agent grant(s) and %d waiver(s), deleted %d plan file(s) and %s.\n",
			v.Public, v.Grants, v.Waivers, len(v.Plans), v.Dir)
		fmt.Fprintf(&b, "The shadow repo %s still exists on GitHub; delete it there if you no longer need it (Settings → Delete this repository).\n", v.Shadow)
	case *engine.LogResult:
		if len(v.Entries) == 0 {
			b.WriteString("No ledger entries.\n")
		}
		for _, e := range v.Entries {
			feat := e.Feature
			if feat == "" {
				feat = "-"
			}
			fmt.Fprintf(&b, "%s  %s  %s  %s  exit %d  %s\n", e.TS.UTC().Format(time.RFC3339),
				oneLine(e.Actor), oneLine(e.Op), oneLine(feat), e.ExitCode, oneLine(e.Summary))
		}
	case *engine.StatusResult:
		mainState := "shadow main up to date"
		if v.ShadowBehind {
			mainState = "shadow main behind public main"
		}
		schedule := "off"
		if v.ScheduledSync {
			schedule = "on"
		}
		fmt.Fprintf(&b, "%s -> %s: %s, overlay %s, scheduled sync %s\n", v.Public, v.Shadow, mainState, v.OverlayStatus, schedule)
		featureTable(&b, v.Features)
	case *engine.FeatureListResult:
		featureTable(&b, v.Features)
	case *engine.FeatureShowResult:
		featureShowText(&b, v)
	case *engine.FeatureAbandonResult:
		switch {
		case v.Preview != nil:
			previewText(&b, v.Preview)
		case !v.Changed && v.Abandoned:
			fmt.Fprintf(&b, "%s was already abandoned; nothing changed.\n", oneLine(v.Feature))
		case !v.Changed:
			fmt.Fprintf(&b, "%s was not abandoned; nothing changed.\n", oneLine(v.Feature))
		case v.Abandoned:
			fmt.Fprintf(&b, "Abandoned %s (its branch is kept; undo with --undo).\n", oneLine(v.Feature))
		default:
			fmt.Fprintf(&b, "Restored %s; it is an active feature again.\n", oneLine(v.Feature))
		}
	case *engine.FeatureCloseResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		if v.GatePRLeftOpen != 0 {
			fmt.Fprintf(&b, "Deleted %d ref(s) of %s (%s) from the shadow repo, but its gate PR #%d is still open.\n",
				len(v.Deleted), oneLine(v.Feature), v.State, v.GatePRLeftOpen)
		} else {
			fmt.Fprintf(&b, "Closed %s (%s): deleted %d ref(s) from the shadow repo", oneLine(v.Feature), v.State, len(v.Deleted))
			if v.GatePR != nil {
				fmt.Fprintf(&b, "; closed gate PR #%d", *v.GatePR)
			}
			b.WriteString(".\n")
		}
		refs := make([]string, 0, len(v.Deleted))
		for ref := range v.Deleted {
			refs = append(refs, ref)
		}
		sort.Strings(refs)
		for _, ref := range refs {
			fmt.Fprintf(&b, "  %s  %s\n", oneLine(ref), short(v.Deleted[ref]))
		}
		b.WriteString("The ledger keeps each SHA (ghostbranch log), so a deleted branch can be pushed back.\n")
	case *LoginResult:
		who := v.Account
		if who == "" {
			who = "an account GitHub didn't name"
		}
		if v.Kind == "app" {
			fmt.Fprintf(&b, "Logged in to GitHub as %s with the Ghostbranch GitHub App (token expires %s; it refreshes itself).\n", who, when(v.Expires))
		} else {
			fmt.Fprintf(&b, "Stored the %s token for %s in the OS keychain.\n", v.TokenKind, who)
		}
		if v.InstallURL != "" {
			fmt.Fprintf(&b, "Next, pick the repos Ghostbranch may use (your public repo and its shadow): %s\n", v.InstallURL)
		}
		if v.EnvToken != "" {
			fmt.Fprintf(&b, "Note: %s is set in this shell and wins over the stored login.\n", v.EnvToken)
		}
	case *LogoutResult:
		switch {
		case v.Removed && v.Account != "":
			fmt.Fprintf(&b, "Removed the stored GitHub login for %s.\n", v.Account)
		case v.Removed:
			b.WriteString("Removed the unreadable stored GitHub login.\n")
		default:
			b.WriteString("No GitHub login was stored.\n")
		}
		if v.RevokeURL != "" {
			fmt.Fprintf(&b, "To revoke the app's access at GitHub too: %s\n", v.RevokeURL)
		}
		if v.EnvToken != "" {
			fmt.Fprintf(&b, "Note: %s is still set, so commands keep using it.\n", v.EnvToken)
		}
	case *AuthStatusResult:
		switch {
		case v.Source == "" && v.Problem != "":
			fmt.Fprintf(&b, "No usable token: %s (see ghostbranch explain %s).\n", v.Problem, v.Problem)
		case v.Source == "":
			b.WriteString("No GitHub token. Run ghostbranch login, or set GHOSTBRANCH_TOKEN.\n")
		default:
			who := v.Account
			if who == "" {
				who = "unknown account"
			}
			fmt.Fprintf(&b, "Token: from %s, %s token, %s (valid: %s)\n", v.Source, v.TokenKind, who, v.Valid)
			if v.Pair != "" {
				fmt.Fprintf(&b, "Can publish to %s: %s\n", v.Pair, v.CanPublish)
			}
		}
		switch {
		case v.StoredLogin != nil:
			s := v.StoredLogin
			fmt.Fprintf(&b, "Stored login: %s (%s)", s.Account, s.Kind)
			if s.Expires != nil {
				fmt.Fprintf(&b, ", token expires %s, refreshable until %s", when(s.Expires), when(s.RefreshExpires))
			}
			b.WriteString("\n")
		case v.StoredLoginChecked && (v.Problem == string(contract.LoginUnavailable) || v.Problem == string(contract.UpgradeNeeded)):
			b.WriteString("Stored login: can't be read (see the problem above)\n")
		case v.StoredLoginChecked:
			b.WriteString("Stored login: none\n")
		default:
			b.WriteString("Stored login: not checked (GHOSTBRANCH_TOKEN wins)\n")
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
			fmt.Fprintf(&b, "The %s token from %s can publish to %s: %s\n", v.TokenKind, v.Token.Source, v.Pair, v.TokenCanPublish)
		}
	}
	return b.String()
}

// pendingJob is whether a gate result is a job still waiting on CI.
func pendingJob(g *engine.GateResult) bool {
	return g.Checks == feature.ChecksPending && g.Job != nil && g.ShadowPR != nil
}

// pendingLine is a pending gate job in a line: what CI runs on, and the
// job's id for wait.
func pendingLine(g *engine.GateResult) string {
	return fmt.Sprintf("Gate for %s: CI running on candidate %s (shadow PR #%d); job %s.\n", g.Feature, short(g.Candidate), *g.ShadowPR, *g.Job)
}

// oneLine escapes what a terminal would act on rather than show (control
// characters, bidi controls, invalid UTF-8), so a ledger field an agent
// wrote stays on its own line and can't rewrite the screen.
func oneLine(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return !unicode.IsGraphic(r) }) {
		return s
	}
	q := strconv.QuoteToGraphic(s)
	return q[1 : len(q)-1]
}

func waivedLines(b *strings.Builder, ws []engine.WaivedFinding) {
	for _, w := range ws {
		fmt.Fprintf(b, "  waived %s: %s in %s (%s)\n", w.ID, w.Rule, engine.Location(w.Finding), w.Reason)
	}
}

func auditText(b *strings.Builder, v *engine.AuditResult) {
	fmt.Fprintf(b, "CI audit of %s:\n", v.Shadow)
	if len(v.Guards) == 0 {
		b.WriteString("  repo guards: none\n")
	}
	for _, g := range v.Guards {
		how := "rewritten by the gate"
		switch {
		case !g.Rewritable:
			how = "NOT rewritable: skips in the shadow repo"
		case v.GuardMode != "rewrite":
			how = "skips in the shadow repo (ci.rewrite_repo_guards: " + v.GuardMode + ")"
		}
		step := ""
		if g.Step != nil {
			step = " step " + *g.Step
		}
		fmt.Fprintf(b, "  repo guard: %s job %s%s (%s)\n", g.Path, g.Job, step, how)
	}
	names := make([]string, 0, len(v.Secrets.Referenced))
	for _, r := range v.Secrets.Referenced {
		names = append(names, r.Name)
	}
	switch {
	case len(names) == 0:
		b.WriteString("  secrets: none referenced\n")
	case !v.Secrets.Listed:
		fmt.Fprintf(b, "  secrets referenced (check they exist; the token can't list them): %s\n", strings.Join(names, ", "))
	case len(v.Secrets.Missing) > 0:
		fmt.Fprintf(b, "  secrets missing in the shadow repo: %s\n", strings.Join(v.Secrets.Missing, ", "))
	default:
		fmt.Fprintf(b, "  secrets: all %d present\n", len(names))
	}
	if v.Minutes.ScheduledSync != nil {
		fmt.Fprintf(b, "  scheduled sync: %s (bills Actions minutes)\n", *v.Minutes.ScheduledSync)
	} else {
		b.WriteString("  scheduled sync: off (run ghostbranch sync yourself)\n")
	}
	for _, f := range v.Overlay {
		fmt.Fprintf(b, "  managed %s: %s\n", f.Path, f.Status)
	}
}

// featureTable is the features table status and feature list print. Every
// name is escaped: a feature name comes from whoever started it.
func featureTable(b *strings.Builder, rs []engine.FeatureReport) {
	if len(rs) == 0 {
		b.WriteString("No features.\n")
		return
	}
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "NAME\tSTATE\tDRIFT\tGATE\tPR")
	for _, r := range rs {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", oneLine(r.Name), r.State, oneLine(r.Drift.Status), gateCell(r.Gate), prCell(r))
	}
	_ = tw.Flush()
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(b, "%s couldn't be read: %s\n", oneLine(r.Name), oneLine(r.Error))
		}
	}
}

// gateCell is a feature's last gate in a word: passed, a failed check
// status, or leak, with the scan when it didn't run; "-" when never gated,
// "(stale)" when the feature or the repos moved since; a pending gate names
// its job.
func gateCell(g *engine.GateSummary) string {
	if g == nil {
		return "-"
	}
	out := g.Checks
	switch {
	case g.Job != nil:
		out = "pending (job " + *g.Job + ")"
	case g.Leak == feature.LeakFound:
		out = "leak"
	case g.Leak == feature.LeakNotRun && g.Checks != feature.ChecksNotRun:
		out = g.Checks + ", scan not_run"
	}
	if !g.Fresh {
		out += " (stale)"
	}
	return oneLine(out)
}

// prCell is a feature's public PR and its state, or "-".
func prCell(r engine.FeatureReport) string {
	if r.PublicPR == nil {
		return "-"
	}
	out := fmt.Sprintf("#%d", *r.PublicPR)
	if r.PRState != "" {
		out += " " + oneLine(r.PRState)
	}
	return out
}

// featureShowText is feature show's detail block. Names, subjects, authors
// and paths are escaped: they come from commits anyone could have pushed.
func featureShowText(b *strings.Builder, v *engine.FeatureShowResult) {
	r := v.Feature
	fmt.Fprintf(b, "Feature %s: %s\n", oneLine(r.Name), r.State)
	if r.Head == "" {
		fmt.Fprintf(b, "  branch: %s (deleted), based on %s\n", oneLine(r.Branch), short(r.Base))
	} else {
		fmt.Fprintf(b, "  branch: %s at %s, based on %s\n", oneLine(r.Branch), short(r.Head), short(r.Base))
	}
	if len(r.Drift.Files) > 0 {
		files := make([]string, len(r.Drift.Files))
		for i, f := range r.Drift.Files {
			files[i] = oneLine(f)
		}
		fmt.Fprintf(b, "  drift: %s in %s\n", oneLine(r.Drift.Status), strings.Join(files, ", "))
	} else {
		fmt.Fprintf(b, "  drift: %s\n", oneLine(r.Drift.Status))
	}
	if g := r.Gate; g == nil {
		b.WriteString("  gate: never run\n")
	} else if g.Job != nil {
		stale := ""
		if !g.Fresh {
			stale = ", stale" // the job is for an outdated candidate
		}
		fmt.Fprintf(b, "  gate: pending (job %s)%s\n", oneLine(*g.Job), stale)
	} else {
		fresh := "fresh"
		if !g.Fresh {
			fresh = "stale"
		}
		fmt.Fprintf(b, "  gate: checks %s, leak scan %s, scanned as %s, %s, at %s\n",
			oneLine(g.Checks), oneLine(g.Leak), oneLine(g.ScannedAs), fresh, g.At.UTC().Format(time.RFC3339))
	}
	if r.PublicPR == nil {
		b.WriteString("  public PR: none\n")
	} else {
		fmt.Fprintf(b, "  public PR: %s", prCell(r))
		if r.PlanID != "" {
			fmt.Fprintf(b, ", plan %s", oneLine(r.PlanID))
		}
		if r.Moved {
			b.WriteString(", moved since publish")
		}
		b.WriteString("\n")
	}
	if r.Abandoned {
		b.WriteString("  abandoned\n")
	}
	fmt.Fprintf(b, "Commits since public main (%d):\n", len(v.Commits))
	for _, c := range v.Commits {
		fmt.Fprintf(b, "  %s  %s  (%s)\n", short(c.Source), oneLine(c.Subject), oneLine(c.Author))
		for _, f := range c.Files {
			fmt.Fprintf(b, "      %s\n", oneLine(f))
		}
	}
	fmt.Fprintf(b, "Ledger (%d):\n", len(v.Ledger))
	for _, e := range v.Ledger {
		line := fmt.Sprintf("  %s  %s  %s  exit %d", e.TS.UTC().Format(time.RFC3339), oneLine(e.Actor), oneLine(e.Op), e.ExitCode)
		if e.Summary != "" {
			line += "  " + oneLine(e.Summary)
		}
		b.WriteString(line + "\n")
	}
}

// when formats a time for people; nil is "never".
func when(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04 UTC")
}

// doctorMarks are doctor's status marks for people.
var doctorMarks = map[string]string{engine.DoctorOK: "✓", engine.DoctorWarn: "!", engine.DoctorFail: "✗", engine.DoctorSkipped: "–"}

// doctorLine is one check: mark, id and message, with the fix indented
// below.
func doctorLine(b *strings.Builder, c engine.DoctorCheck) {
	fmt.Fprintf(b, "%s %-6s %s\n", doctorMarks[c.Status], c.ID, c.Message)
	if c.Fix != "" {
		fmt.Fprintf(b, "    fix: %s\n", c.Fix)
	}
}

// walkthrough ends a guided init: the first feature, and agents' tokens.
const walkthrough = `
Your first feature:
  1. ghostbranch feature start <name>   creates the branch in the shadow repo; commit and push to it there
  2. ghostbranch gate <name>            rebases on public main, scans for leaks and runs CI
  3. ghostbranch promote <name>         publishes it to the public repo as a PR, once you confirm
Coding agents: give each agent its own token that reaches only the shadow repo, as GHOSTBRANCH_TOKEN
in its environment, never your login. See "Tokens" in ghostbranch guide.
`
