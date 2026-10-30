package cli

import (
	"cmp"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/guide"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
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

// nothingLine is previewText's line for a preview with no ops. A run that
// ended in an error never prints it (withoutNothing): the run stopped, so
// "nothing" would read as the whole answer (F-13).
const nothingLine = "  (nothing)\n"

func previewText(b *strings.Builder, p *preview.Preview) {
	b.WriteString("Dry run; nothing was changed. This would:\n")
	for _, line := range p.Describe() {
		fmt.Fprintf(b, "  - %s\n", line)
	}
	if len(p.Ops) == 0 {
		b.WriteString(nothingLine)
	}
}

// withoutNothing drops nothingLine from a result's text when the command
// failed, with the "This would:" header above it, so no empty list dangles.
func withoutNothing(out string, failed bool) string {
	if !failed {
		return out
	}
	out = strings.ReplaceAll(out, " This would:\n"+nothingLine, "\n")
	return strings.ReplaceAll(out, nothingLine, "")
}

// restackStopText is a restack dry run that stopped: what it would do
// before stopping, and where it stops. Only a conflict says the branch
// doesn't restack cleanly; Ctrl-C or a git failure says where it stopped.
func restackStopText(b *strings.Builder, v *engine.RestackResult) {
	b.WriteString("Dry run; nothing was changed.\n")
	if len(v.Preview.Ops) > 0 {
		b.WriteString("Before stopping, it would:\n")
		for _, line := range v.Preview.Describe() {
			fmt.Fprintf(b, "  - %s\n", line)
		}
	}
	if v.StoppedOnConflict {
		fmt.Fprintf(b, "It would stop at %s: it doesn't restack cleanly (see the error below).\n", oneLine(v.Pending[0]))
		return
	}
	fmt.Fprintf(b, "It stopped at %s (see the error below).\n", oneLine(v.Pending[0]))
}

// baseName is a result's base branch for its text: the name the engine
// read, or main when the result has none (a result from before open).
func baseName(branch string) string { return cmp.Or(branch, feature.MainParent) }

// human renders a result for people. It never prints secret values.
func human(r any, st style.Styler) string {
	var b strings.Builder
	switch v := r.(type) {
	case *VersionResult:
		fmt.Fprintf(&b, "ghostbranch %s (commit %s), JSON contract %s\n", oneLine(v.Version), oneLine(v.Commit), v.SchemaVersion)
	case *ReferenceResult:
		for _, c := range v.Commands {
			fmt.Fprintf(&b, "%s: %s\n", c.Path, oneLine(c.Short))
		}
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
			doctorLine(&b, c, st)
		}
		if v.UpdateCheck != nil {
			b.WriteString(v.UpdateCheck.Line() + "\n")
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
			fmt.Fprintf(&b, "  %s %s is gone, so public pushes are refused until you reinstall\n", st.Warn("warning:"), v.Binary)
		}
	case *guard.CheckResult:
		// It runs inside every public git push: a pass says nothing.
	case *ExplainResult:
		for _, e := range v.Entries {
			switch {
			case e.Kind == contract.KindExit:
				// "exit 5: …", not "5 (exit, exit 5): …" (F-23).
				fmt.Fprintf(&b, "exit %s: %s\n", e.Code, e.Summary)
			case e.ExitCode != nil:
				fmt.Fprintf(&b, "%s (%s, exit %d): %s\n", e.Code, e.Kind, *e.ExitCode, e.Summary)
			default:
				fmt.Fprintf(&b, "%s (%s): %s\n", e.Code, e.Kind, e.Summary)
			}
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
		switch {
		case v.Ready && v.BaseBranch != "" && v.BaseBranch != feature.MainParent:
			// A main pair's text is as before 8.12; another names its branch.
			fmt.Fprintf(&b, "Shadow repo %s is ready for %s on branch %s.\n", v.Shadow, v.Public, v.BaseBranch)
		case v.Ready:
			fmt.Fprintf(&b, "Shadow repo %s is ready for %s.\n", v.Shadow, v.Public)
		default:
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
					doctorLine(&b, c, st)
				}
			}
		}
		if v.Guided && v.Ready {
			if v.Audit != nil && v.Audit.Minutes.Billed {
				// Said again here, where a person reads on: the warning
				// scrolls by among init's progress lines (6.1 acceptance).
				fmt.Fprintf(&b, "\nNote: %s is private, so the CI that every gate runs bills your GitHub Actions minutes.\n", v.Shadow)
			}
			fmt.Fprintf(&b, walkthrough, baseName(v.BaseBranch))
		}
	case *engine.AuditResult:
		auditText(&b, v)
	case *engine.UpgradeResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			for _, f := range v.Files {
				if f.ReplacesLocalEdits {
					fmt.Fprintf(&b, "  %s %s was edited by hand; upgrade replaces those edits\n", st.Warn("warning:"), f.Path)
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
		base := baseName(v.BaseBranch)
		switch v.Action {
		case "noop":
			fmt.Fprintf(&b, "Shadow %s is up to date with public %s (%s).\n", base, base, short(v.PublicMain))
		case "fast_forward":
			fmt.Fprintf(&b, "Shadow %s fast-forwarded %s -> %s.\n", base, short(v.ShadowMainBefore), short(v.PublicMain))
		case "push_failed":
			fmt.Fprintf(&b, "Shadow %s could not be pushed; it was at %s when sync read it.\n", base, short(v.ShadowMainBefore))
		default:
			fmt.Fprintf(&b, "Shadow %s has diverged from public %s; nothing was pushed to it.\n", base, base)
		}
		for _, br := range v.Branches {
			if br.Name != base {
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
		featureStartText(&b, v, st)
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
			k := -1
			if v.Plan != nil {
				k = len(v.Plan.Publish.Commits)
			}
			b.WriteString(publishedLine(v.Applied, k))
			archivedLines(&b, v.Applied)
		case v.Plan != nil && v.PlanPath != "":
			planLines(&b, v.Plan, v.PlanPath)
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
		b.WriteString(publishedLine(v, -1))
		archivedLines(&b, v)
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
		base := baseName(v.BaseBranch)
		mainState := "shadow " + base + " up to date"
		if v.ShadowBehind {
			mainState = "shadow " + base + " behind public " + base
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
	case *engine.FoldResult:
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		fmt.Fprintf(&b, "Folded %s into %s: %s moved %s -> %s, and %s is closed (%d ref(s) deleted).\n",
			oneLine(v.Feature), oneLine(v.Parent), oneLine(v.Parent), short(v.From), short(v.To), oneLine(v.Feature), len(v.Deleted))
		if len(v.Reparented) > 0 {
			fmt.Fprintf(&b, "Now stacked on %s: %s.\n", oneLine(v.Parent), oneLine(strings.Join(v.Reparented, ", ")))
		}
		b.WriteString("The ledger keeps each deleted ref's SHA (ghostbranch log).\n")
	case *engine.RestackResult:
		if v.Preview != nil && len(v.Pending) > 0 {
			restackStopText(&b, v)
			break
		}
		if v.Preview != nil {
			previewText(&b, v.Preview)
			break
		}
		if len(v.Restacked) == 0 && len(v.Pending) == 0 {
			fmt.Fprintf(&b, "Nothing to restack: every feature above %s is on its parent's tip.\n", oneLine(v.Feature))
		}
		for _, r := range v.Restacked {
			// A parent of "main" is the base branch, which --onto names.
			parent := r.Parent
			if parent == feature.MainParent {
				parent = baseName(v.Onto)
			}
			if r.From == r.To {
				fmt.Fprintf(&b, "  %s: on %s already; record updated\n", oneLine(r.Feature), oneLine(parent))
				continue
			}
			fmt.Fprintf(&b, "  %s onto %s: %s -> %s\n", oneLine(r.Feature), oneLine(parent), short(r.From), short(r.To))
		}
		if len(v.Pending) > 0 {
			fmt.Fprintf(&b, "Stopped at %s.\n", oneLine(strings.Join(v.Pending, ", ")))
		}
		if len(v.Restacked) > 0 {
			b.WriteString("These branches were rewritten in the shadow repo: in each checkout, git fetch, then git reset --keep <remote>/<branch>.\n")
		}
	case *engine.PullResult:
		switch {
		case v.Preview != nil:
			previewText(&b, v.Preview)
		case len(v.Adopted) == 0:
			fmt.Fprintf(&b, "Nothing to pull: the public PR of %s holds only what Ghostbranch published.\n", oneLine(v.Feature))
		default:
			fmt.Fprintf(&b, "Brought %d commit(s) from the public PR into %s:\n", len(v.Adopted), oneLine(v.Feature))
			for _, c := range v.Adopted {
				fmt.Fprintf(&b, "  %s %s (%s)\n", short(c.SHA), oneLine(c.Subject), oneLine(c.Author))
			}
			if v.From != v.To {
				fmt.Fprintf(&b, "%s moved %s -> %s in the shadow repo: in your checkout, git fetch, then git reset --keep <remote>/%s.\n",
					oneLine(v.Feature), short(v.From), short(v.To), oneLine(v.Feature))
			}
		}
	case *engine.FeatureArchiveResult:
		ref, head := "", ""
		if v.Archived != nil {
			ref, head = v.Archived.Ref, v.Archived.Head
		}
		switch {
		case v.Preview != nil:
			previewText(&b, v.Preview)
		case !v.Changed && v.PRState == "merged":
			fmt.Fprintf(&b, "%s is already archived at %s, and PR #%d merged; sync closes it.\n",
				oneLine(v.Feature), oneLine(ref), v.PublicPR)
		case !v.Changed && v.PRState == "closed":
			fmt.Fprintf(&b, "%s is already archived at %s, and PR #%d was closed without merging; a person closes it at the console.\n",
				oneLine(v.Feature), oneLine(ref), v.PublicPR)
		case !v.Changed:
			fmt.Fprintf(&b, "%s is already archived at %s; fix PR #%d by pushing to %s.\n",
				oneLine(v.Feature), oneLine(ref), v.PublicPR, oneLine(v.Branch))
		default:
			fmt.Fprintf(&b, "Archived %s at %s (%s); PR #%d is handed off: push fixes to %s.\n",
				oneLine(v.Feature), oneLine(ref), short(head), v.PublicPR, oneLine(v.Branch))
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
			b.WriteString("Next, install the Ghostbranch app. It acts on GitHub only on the repos you pick.\n")
			fmt.Fprintf(&b, "Open this on the account that owns your public repo and pick that repo (ghostbranch init creates the shadow repo, and the app gets it automatically): %s\n", v.InstallURL)
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
	case *engine.AgentsTokenResult:
		agentsTokenText(&b, v, st)
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

// agentsTokenText is agents token's result: the token's check and where to
// set it, or, when no token was read, the steps for a person.
func agentsTokenText(b *strings.Builder, v *engine.AgentsTokenResult, st style.Styler) {
	if v.Check == nil {
		owner, _, _ := strings.Cut(v.Select, "/")
		b.WriteString(attention(st, fmt.Sprintf("Make the agent's token on GitHub, for %s: %s", owner, v.Link)) + "\n")
		b.WriteString(attention(st, fmt.Sprintf("On that page, choose Only select repositories, then %s. Keep the permissions the link set.", v.Select)) + "\n")
		b.WriteString("Then set it in the agent's shell, and restart the agent there:\n")
	} else {
		public := v.Check.Public
		if public == "" {
			public = "the public repo"
		}
		b.WriteString(st.OK(fmt.Sprintf("This token (account %s) reaches %s, and it can't push to %s.", v.Check.Account, v.Check.Shadow, public)) + "\n")
		b.WriteString("Set it in the agent's shell (paste the token for <paste>); Ghostbranch doesn't keep it:\n")
	}
	fmt.Fprintf(b, "  sh, bash, zsh: %s\n  PowerShell:    %s\n", v.SetLine["sh"], v.SetLine["powershell"])
	if v.Check != nil {
		b.WriteString("Then restart your agent in a shell with this set; it can check with ghostbranch agents status.\n")
	}
}

// planLines is a saved promote plan: where it is, what it publishes, and
// the apply command. A stack plan names each PR, bottom first, with its
// own commit count (F-11); Plan.Publish is only the bottom's.
func planLines(b *strings.Builder, p *plan.Plan, path string) {
	if len(p.Chain) < 2 {
		fmt.Fprintf(b, "Plan %s saved to %s.\n", p.ID, path)
		fmt.Fprintf(b, "  %s: %d public commit(s)", p.Mode, len(p.Publish.Commits))
		if n := len(p.Publish.DroppedEmpty); n > 0 {
			fmt.Fprintf(b, ", %d dropped as empty", n)
		}
		fmt.Fprintf(b, "\nPublish it with: ghostbranch apply %s\n", p.ID)
		return
	}
	fmt.Fprintf(b, "Plan %s saved to %s: %d PRs, bottom first.\n", p.ID, path, len(p.Chain))
	for i, e := range p.Chain {
		fmt.Fprintf(b, "  %s (%s): %d public commit(s)", oneLine(e.Feature), e.Mode, len(e.Publish.Commits))
		if n := len(e.Publish.DroppedEmpty); n > 0 {
			fmt.Fprintf(b, ", %d dropped as empty", n)
		}
		if i < len(p.PRs) {
			fmt.Fprintf(b, ", PR on %s", oneLine(p.PRs[i].Base))
		}
		b.WriteString("\n")
	}
	fmt.Fprintf(b, "Publish it with: ghostbranch apply %s\n", p.ID)
}

// archivedLines says which features were handed off, and where their
// PRs are fixed now.
func archivedLines(b *strings.Builder, r *engine.ApplyResult) {
	for i, p := range r.PRs {
		if r.AfterPublish[p.Feature] == engine.AfterArchived {
			fmt.Fprintf(b, "Archived %s's shadow branch at refs/ghostbranch/archive/%s: fix PR #%d by pushing to %s.\n",
				oneLine(p.Feature), oneLine(p.Feature), p.Number, oneLine(prBranch(r, i)))
		}
	}
}

// publishedLine is what an apply did: published a new PR, or updated an
// open one, by adding k commits (k < 0 when the count isn't at hand) or by
// replacing its branch. A stack plan says, for each of its PRs, bottom
// first, what happened to it (F-15: a fresh stack once named only its top).
// A PR never opened (number 0: the apply failed first) gets no line.
func publishedLine(r *engine.ApplyResult, k int) string {
	if len(r.PRs) > 1 {
		var b strings.Builder
		for i, p := range r.PRs {
			if p.Number == 0 {
				continue
			}
			// Each PR's branch is the base of the PR above it; the top's is
			// the result's.
			branch := oneLine(r.PublicBranch)
			if i+1 < len(r.PRs) {
				branch = oneLine(r.PRs[i+1].Base)
			}
			switch {
			case !p.Update:
				fmt.Fprintf(&b, "Published %s as PR #%d: %s\n", branch, p.Number, p.URL)
			case p.Style == plan.StyleAppend:
				fmt.Fprintf(&b, "Updated PR #%d on %s: %s\n", p.Number, branch, p.URL)
			default:
				fmt.Fprintf(&b, "Replaced PR #%d's branch %s: %s\n", p.Number, branch, p.URL)
			}
		}
		return b.String()
	}
	if len(r.PRs) == 1 && r.PRs[0].Update {
		if r.PRs[0].Style == plan.StyleAppend {
			if k < 0 {
				return fmt.Sprintf("Updated PR #%d on %s: %s\n", r.PublicPR, r.PublicBranch, r.PRURL)
			}
			s := "s"
			if k == 1 {
				s = ""
			}
			return fmt.Sprintf("Updated PR #%d (added %d commit%s): %s\n", r.PublicPR, k, s, r.PRURL)
		}
		return fmt.Sprintf("Replaced PR #%d's branch %s: %s\n", r.PublicPR, r.PublicBranch, r.PRURL)
	}
	if r.PublicPR == 0 {
		return ""
	}
	return fmt.Sprintf("Published %s as PR #%d: %s\n", r.PublicBranch, r.PublicPR, r.PRURL)
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
	for _, row := range treeOrder(rs) {
		r := rs[row.i]
		name := oneLine(r.Name)
		if row.depth > 0 {
			name = strings.Repeat("  ", row.depth-1) + "└─ " + name
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", name, r.State, oneLine(r.Drift.Status), gateCell(r.Gate), prCell(r))
	}
	_ = tw.Flush()
	for _, r := range rs {
		if r.Error != "" {
			fmt.Fprintf(b, "%s couldn't be read: %s\n", oneLine(r.Name), oneLine(r.Error))
		}
	}
}

// treeRow is a feature's line in featureTable: its index in the reports
// and its depth below main.
type treeRow struct{ i, depth int }

// treeOrder lists rs as trees, in rs's (name) order: each feature whose
// parent is main, or isn't among rs, then its children under it, depth
// first. Records whose parents loop (a corrupt pair) have no root; they
// are listed flat at the end.
func treeOrder(rs []engine.FeatureReport) []treeRow {
	idx := map[string]int{}
	for i, r := range rs {
		idx[r.Name] = i
	}
	kids := map[string][]int{}
	var roots []int
	for i, r := range rs {
		if _, in := idx[r.Parent]; in && r.Parent != r.Name {
			kids[r.Parent] = append(kids[r.Parent], i)
		} else {
			roots = append(roots, i)
		}
	}
	var out []treeRow
	seen := map[int]bool{}
	var walk func(i, depth int)
	walk = func(i, depth int) {
		if seen[i] {
			return
		}
		seen[i] = true
		out = append(out, treeRow{i, depth})
		for _, k := range kids[rs[i].Name] {
			walk(k, depth+1)
		}
	}
	for _, i := range roots {
		walk(i, 0)
	}
	for i := range rs {
		walk(i, 0)
	}
	return out
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

// prCell is a feature's public PR and its state, `(behind)` when the
// feature has commits its open PR doesn't, or `(changed on GitHub)` when the
// PR holds commits Ghostbranch didn't publish, or "-"; then the native
// GitHub stack the PR is in, if any.
func prCell(r engine.FeatureReport) string {
	out := "-"
	if r.PublicPR != nil {
		out = fmt.Sprintf("#%d", *r.PublicPR)
		switch {
		case r.PR != nil && r.PR.Foreign > 0:
			out += " (changed on GitHub)"
		case r.State == feature.StatePRBehind:
			out += " (behind)"
		case r.PRState != "":
			out += " " + oneLine(r.PRState)
		}
	}
	if r.Stack != nil {
		out += fmt.Sprintf(" stack #%d", *r.Stack)
	}
	return out
}

// featureShowText is feature show's detail block. Names, subjects, authors
// and paths are escaped: they come from commits anyone could have pushed.
func featureShowText(b *strings.Builder, v *engine.FeatureShowResult) {
	r := v.Feature
	fmt.Fprintf(b, "Feature %s: %s\n", oneLine(r.Name), r.State)
	switch {
	case r.Archived != nil:
		fmt.Fprintf(b, "  branch: %s (archived at %s, %s), based on %s\n", oneLine(r.Branch), oneLine(r.Archived.Ref), short(r.Archived.Head), short(r.Base))
	case r.Head == "":
		fmt.Fprintf(b, "  branch: %s (deleted), based on %s\n", oneLine(r.Branch), short(r.Base))
	default:
		fmt.Fprintf(b, "  branch: %s at %s, based on %s\n", oneLine(r.Branch), short(r.Head), short(r.Base))
	}
	if (r.Parent != "" && r.Parent != feature.MainParent) || len(r.Children) > 0 {
		kids := "none"
		if len(r.Children) > 0 {
			kids = oneLine(strings.Join(r.Children, ", "))
		}
		parent := r.Parent
		switch parent {
		case "":
			parent = "unknown" // the record couldn't be read
		case feature.MainParent:
			parent = baseName(v.BaseBranch)
		}
		fmt.Fprintf(b, "  stack: on %s; branches on it: %s\n", oneLine(parent), kids)
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
	fmt.Fprintf(b, "Commits since public %s (%d):\n", baseName(v.BaseBranch), len(v.Commits))
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
func doctorLine(b *strings.Builder, c engine.DoctorCheck, st style.Styler) {
	mark := doctorMarks[c.Status]
	switch c.Status {
	case engine.DoctorOK:
		mark = st.OK(mark)
	case engine.DoctorWarn:
		mark = st.Warn(mark)
	case engine.DoctorFail:
		mark = st.Fail(mark)
	case engine.DoctorSkipped:
		mark = st.Dim(mark)
	}
	msg, fix := c.Message, c.Fix
	if c.PersonMessage != "" {
		msg, fix = c.PersonMessage, strings.Join(c.PersonFixArgv, " ")
	}
	fmt.Fprintf(b, "%s %-6s %s\n", mark, c.ID, msg)
	if fix != "" {
		fmt.Fprintf(b, "    fix: %s\n", fix)
	}
}

// setupMarks are the setup checklist's marks for people: done, needs you
// (or blocked), and Ghostbranch will do it. A skipped step isn't shown.
var setupMarks = map[string]string{engine.SetupDone: "✓", engine.SetupNeedsHuman: "!", engine.SetupBlocked: "!", engine.SetupTodo: "·"}

// attention is text that needs the person: bold yellow.
func attention(st style.Styler, text string) string { return st.Bold(st.Warn(text)) }

// checklistLines is the setup checklist as guided init shows it: one
// "<mark> <message>" line per step that applies, coloured by what it asks
// of the person: done is green, needs you bold yellow, Ghostbranch will do
// it dim. A step in replace shows those lines instead (already styled).
func checklistLines(s *engine.Setup, st style.Styler, replace map[string][]string) []string {
	var out []string
	for _, step := range s.Steps {
		mark, ok := setupMarks[step.Status]
		if !ok {
			continue
		}
		if lines, ok := replace[step.ID]; ok {
			out = append(out, lines...)
			continue
		}
		line := mark + " " + step.Message
		switch step.Status {
		case engine.SetupDone:
			line = st.OK(line)
		case engine.SetupTodo:
			line = st.Dim(line)
		default:
			line = attention(st, line)
		}
		out = append(out, line)
	}
	return out
}

// walkthrough ends a guided init: the first feature, and agents' tokens.
// Its one %s is the base branch's name.
const walkthrough = `
Your first feature:
  1. ghostbranch feature start <name>   creates the branch in the shadow repo and switches this checkout to it;
                                        commit there, then: git push shadow <name>
  2. ghostbranch gate <name>            rebases on public %s, scans for leaks and runs CI
  3. ghostbranch promote <name>         publishes it to the public repo as a PR, once you confirm
Coding agents: give each agent its own token that reaches only the shadow repo, as GHOSTBRANCH_TOKEN
in its environment, never your login. See "Tokens" in ghostbranch guide.
`
