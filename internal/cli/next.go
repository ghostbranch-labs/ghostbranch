package cli

import (
	"context"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// usedWorkspace is the workspace this run acted in: the engine's when there
// is one, otherwise the one the flags and environment name.
func (a *app) usedWorkspace() string {
	if a.eng != nil && a.eng.WS != nil {
		return a.eng.WS.Name
	}
	return a.workspaceName()
}

// gb is a ghostbranch argv, with --workspace unless this run used the
// default workspace (it may have come from $GHOSTBRANCH_WORKSPACE, which the
// next shell need not have).
func (a *app) gb(args ...string) []string {
	out := append([]string{"ghostbranch"}, args...)
	if ws := a.usedWorkspace(); ws != "default" {
		out = append(out, "--workspace", ws)
	}
	return out
}

// scoped adds --repo for the pair this run acted on.
func (a *app) scoped(args ...string) []string {
	if a.pairFull.Public != "" {
		args = append(args, "--repo", a.pairFull.Public)
	}
	return a.gb(args...)
}

// again is this run's own argv plus extra flags, with --workspace when gb
// would add it. For a person (forHuman) it drops --non-interactive, which
// hides the console they need, and --json, and names the pair with --repo:
// they run it in their own terminal.
func (a *app) again(forHuman bool, extra ...string) []string {
	out := []string{"ghostbranch"}
	hasWS := false
	for _, arg := range a.args {
		if arg == "--workspace" || strings.HasPrefix(arg, "--workspace=") {
			hasWS = true
		}
		if forHuman && (arg == "--json" || strings.HasPrefix(arg, "--json=") ||
			arg == "--non-interactive" || strings.HasPrefix(arg, "--non-interactive=")) {
			continue
		}
		out = append(out, arg)
	}
	out = append(out, extra...)
	// A person runs it in their own terminal, which may not be in this
	// checkout: name the pair this run found here.
	if _, hasRepo := flagValue(a.args, "--repo"); forHuman && !hasRepo && a.pairFull.Public != "" {
		out = append(out, "--repo", a.pairFull.Public)
	}
	if ws := a.usedWorkspace(); !hasWS && ws != "default" {
		out = append(out, "--workspace", ws)
	}
	return out
}

// next is spec section 9.3: what to run now, exactly as given. Flags the
// caller must supply are in NeedsInput; nothing here is ever a placeholder.
// An error that names a gate job leads with wait on it: the gate's CI
// passed but its result couldn't be written, and wait records it. A stale
// or missing job is dead, and INTERRUPTED and LOCKED offer their own steps.
func (a *app) next(err error) []contract.Next {
	out := a.steps(err)
	if ce := contract.From(err); ce != nil {
		if job, _ := ce.Details["job"].(string); job != "" {
			switch ce.Code {
			case contract.Interrupted, contract.Locked, contract.JobStale, contract.JobNotFound:
			default:
				out = append([]contract.Next{{Command: a.scoped("wait", job),
					Why: "the gate passed but its result wasn't recorded; wait records it"}}, out...)
			}
		}
	}
	return out
}

// steps is next without the lead wait on an error's job.
func (a *app) steps(err error) []contract.Next {
	if ce := contract.From(err); ce != nil {
		switch ce.Code {
		case contract.ApprovalRequired:
			reason, _ := ce.Details["reason"].(string)
			planID, _ := ce.Details["plan_id"].(string)
			switch {
			case reason == "grant_needs_yes" && planID != "":
				// The plan is saved: publish it rather than build it again.
				return []contract.Next{{Command: a.gb("apply", a.planRef(planID), "--yes"), Why: "a person allowed agents to publish here; publish the saved plan with --yes"}}
			case reason == "grant_needs_yes":
				return []contract.Next{{Command: a.again(false, "--yes"), Why: "a person allowed agents to publish here; repeat with --yes"}}
			case planID != "":
				return []contract.Next{a.applyNext(planID, false)}
			default:
				return []contract.Next{{Command: a.again(true), NeedsHuman: true,
					Why: "a person must run this in their own terminal and type the code it shows"}}
			}
		case contract.LeakFound, contract.MarkerInvalid:
			return a.leakNext(ce)
		case contract.PairAmbiguous:
			if pub, _ := ce.Details["public"].(string); pub != "" {
				return []contract.Next{
					{Command: a.gb("init", pub, "--adopt", "--plan", "--json"),
						Why: "its shadow repo already exists (set up on another machine, or in another GHOSTBRANCH_HOME): adopt it — this only registers the pair here and checks the shadow repo. If the shadow repo has a non-default name, adopt it with --shadow <owner/name> instead"},
					{Command: a.gb("init", pub, "--plan", "--json"),
						Why: "no shadow repo yet: create one"},
				}
			}
		case contract.ShadowCreateDenied:
			pub, _ := ce.Details["public"].(string)
			sh, _ := ce.Details["shadow"].(string)
			args := []string{"init", pub, "--adopt"}
			if explicit, _ := ce.Details["explicit_shadow"].(bool); explicit {
				args = append(args, "--shadow", sh)
			}
			// The adopt can't succeed until a person has done steps 1 and 2.
			return []contract.Next{{Command: a.gb(args...), NeedsHuman: true,
				Why: "a person first creates the empty private repo and adds it to the token (error.details.steps 1 and 2); then run this to adopt it"}}
		case contract.PlanStale:
			f, _ := ce.Details["feature"].(string)
			pub, _ := ce.Details["public"].(string)
			mode, _ := ce.Details["mode"].(string)
			if f != "" && pub != "" {
				if mode == "" {
					mode = config.ModeSquash // a 0.4 plan
				}
				args := replanArgs(ce, f, pub, mode, true)
				if title, _ := ce.Details["title"].(string); title != "" {
					args = append(args, "--title", title)
				}
				if body, _ := ce.Details["body"].(string); body != "" {
					args = append(args, "--body", body)
				}
				return []contract.Next{{Command: a.gb(args...), Why: "the repos moved since the plan was made; make a new plan", NeedsInput: modeInputs(mode)}}
			}
		case contract.ShadowBehindError:
			return []contract.Next{
				{Command: a.scoped("sync"), Why: "shadow main is behind public main; sync it (fast-forward only)"},
				{Command: a.startAgain(), Why: "then start the feature again, from the synced shadow main"},
			}
		case contract.SyncDiverged:
			// A person sorts out the diverged branch (error.what_to_do), but
			// the features sync surveyed still need what they need.
			if r, ok := a.result.(*engine.SyncResult); ok {
				return a.featureNexts(nil, r.Attention)
			}
		case contract.Interrupted:
			// A promote stopped after saving its plan: apply resumes it.
			if id, _ := ce.Details["plan_id"].(string); id != "" {
				return []contract.Next{a.applyNext(id, true)}
			}
			if job, _ := ce.Details["job"].(string); job != "" {
				out := []contract.Next{{Command: a.scoped("wait", job), Why: "the gate's CI is still running; finish the job"}}
				if a.command == "promote" {
					out = append(out, contract.Next{Command: withoutSwitch(a.again(false), "--detach"),
						Why: "once the job has passed, promote again; it reuses the green gate"})
				}
				return out
			}
			return []contract.Next{{Command: a.again(false), Why: "nothing is pending; run it again"}}
		case contract.BranchExists:
			// The public branch holds an earlier plan's commits: resume that publish.
			if id, _ := ce.Details["plan_id"].(string); id != "" {
				return []contract.Next{a.applyNext(id, true)}
			}
		case contract.JobStale:
			if job, _ := ce.Details["current_job"].(string); job != "" {
				return []contract.Next{{Command: a.scoped("wait", job), Why: "a newer gate replaced the job; wait on that one instead"}}
			}
			return a.gateAgain(ce, "a newer gate replaced the job and nothing is pending now; gate the feature again")
		case contract.JobNotFound:
			return a.gateAgain(ce, "the feature has no gate job to wait on; gate it")
		case contract.Locked:
			if job, _ := ce.Details["job"].(string); job != "" {
				return []contract.Next{{Command: a.scoped("wait", job),
					Why: "another ghostbranch command held the lock when the result was ready; wait records it"}}
			}
		case contract.FeatureMoved:
			if f, _ := ce.Details["feature"].(string); f != "" {
				return []contract.Next{{Command: a.scoped("feature", "show", f),
					Why: "the feature changed while it was being closed and nothing was deleted; look at it again before closing it"}}
			}
		case contract.ForgeError:
			// feature close deleted the refs, then couldn't close the gate PR.
			if r, ok := a.result.(*engine.FeatureCloseResult); ok && len(r.Deleted) > 0 {
				return []contract.Next{{Command: a.scoped("log", "--feature", r.Feature, "--op", "feature close"), NeedsHuman: true,
					Why: "the feature's refs are deleted and the ledger lists them with their SHAs; a person closes its gate draft PR in the shadow repo on GitHub (error.message names it)"}}
			}
		case contract.CurateInvalid, contract.CurateConflict, contract.CurateTreeMismatch:
			why := map[contract.Code]string{
				contract.CurateInvalid:      "fix the --curate file (error.details lists the problem SHAs; the gate result lists the feature's commits) and run this again",
				contract.CurateConflict:     "reorder the groups in the --curate file so commits that touch the same lines keep their order, or reorder on your feature branch; then run this again",
				contract.CurateTreeMismatch: "keep commits that touch the same lines in their original order in the --curate file; then run this again",
			}[ce.Code]
			if a.command == "apply" {
				// The plan's groups no longer replay: make a new plan.
				f, _ := ce.Details["feature"].(string)
				pub, _ := ce.Details["public"].(string)
				if f == "" || pub == "" {
					return nil
				}
				return []contract.Next{{Command: a.gb(replanArgs(ce, f, pub, config.ModeCurated, true)...), NeedsInput: []string{"--curate"},
					Why: "the plan's curated groups no longer replay in order (error.details names the commit or tree): make a new plan with a --curate file that keeps commits touching the same lines in their original order"}}
			}
			return []contract.Next{{Command: withoutFlags(a.again(false), "--curate"), NeedsInput: []string{"--curate"}, Why: why}}
		case contract.FeatureNotFound:
			if a.pairFull.Public != "" {
				return []contract.Next{{Command: a.scoped("feature", "list", "--all"),
					Why: "no feature by that name here; list every feature, abandoned ones included, to find it"}}
			}
		case contract.AuthorUnmapped:
			return []contract.Next{{Command: a.again(true), NeedsHuman: true,
				Why: "a person adds error.details.emails to promote.author_map in .ghostbranch.yml on the overlay branch (or sets author_email: noreply), then runs this again"}}
		case contract.MissingInput:
			switch ce.Field {
			case "title":
				return []contract.Next{{Command: withoutFlags(a.again(false), "--title"), NeedsInput: []string{"--title"},
					Why: "write a PR title that sums up the feature's commits"}}
			case "curate":
				return []contract.Next{{Command: withoutFlags(a.again(false), "--curate"), NeedsInput: []string{"--curate"},
					Why: "write a --curate file grouping the feature's commits (the gate result lists them)"}}
			case "message":
				return []contract.Next{{Command: withoutFlags(a.again(false), "-m", "--message"), NeedsInput: []string{"--message"},
					Why: "write the squash commit's message"}}
			}
		case contract.BadFlag:
			switch ce.Field {
			case "message":
				return []contract.Next{{Command: withoutFlags(a.again(false), "-m", "--message"),
					Why: "-m is only for --mode squash; drop it (use --title/--body for the PR) or add --mode squash"}}
			case "curate":
				if strings.HasPrefix(ce.Message, "read the --curate file") {
					return []contract.Next{{Command: withoutFlags(a.again(false), "--curate"), NeedsInput: []string{"--curate"},
						Why: "the --curate file could not be read; pass the path of a readable one"}}
				}
				return []contract.Next{{Command: withoutFlags(a.again(false), "--curate"),
					Why: "--curate is only for --mode curated; drop it, or add --mode curated"}}
			}
		}
		return nil
	}
	out := a.syncNext()
	switch r := a.result.(type) {
	case *engine.FeatureStartResult:
		if r.Preview == nil {
			why := "check out with `" + r.CheckoutHint + "`, commit on " + r.Name + ", "
			switch a.shadowRemote {
			case "":
				why += "push it to the shadow repo (never origin), then gate it"
			case "origin":
				why += "push it with `git push origin " + r.Name + "` (origin is the shadow repo here), then gate it"
			default:
				why += "push it with `git push " + a.shadowRemote + " " + r.Name + "` (the shadow repo, never origin), then gate it"
			}
			out = append(out, contract.Next{Command: a.scoped("gate", r.Name), Why: why})
		}
	case *engine.GateResult:
		if r.Preview == nil && r.Checks == feature.ChecksPassed && r.Leak == feature.LeakClean {
			mode := r.ScannedAs
			need := modeInputs(mode)
			if mode != config.ModeSquash && len(r.Commits) > 1 {
				need = append(need, "--title")
			}
			out = append(out, contract.Next{Command: a.scoped("promote", r.Feature, "--mode", mode, "--plan"),
				Why: "the gate passed; plan the publish", NeedsInput: need})
		} else if r.Checks == feature.ChecksPending && r.Job != nil {
			out = append(out, a.waitNext(*r.Job))
		}
	case *engine.PromoteResult:
		switch {
		case r.Preview == nil && r.Applied == nil && r.Plan != nil && r.PlanPath != "":
			out = append(out, a.applyNext(r.Plan.ID, true))
		case r.Gate != nil && r.Gate.Checks == feature.ChecksPending && r.Gate.Job != nil:
			out = append(out, a.waitNext(*r.Gate.Job),
				contract.Next{Command: withoutSwitch(a.again(false), "--detach"), Why: "once the job has passed, promote again; it reuses the green gate"})
		}
	case *engine.FeatureCloseResult:
		if r.Preview != nil {
			why := "the preview lists what closing deletes; it is merged with no newer commits, so it closes without a console"
			if r.NeedsConsole {
				why = "the preview lists what closing deletes; it is " + string(r.State) + ", so a person closes it at the console"
			}
			out = append(out, contract.Next{Command: a.scoped("feature", "close", r.Feature), NeedsHuman: r.NeedsConsole, Why: why})
		}
	case *engine.UpgradeResult:
		if r.Preview == nil && r.Audit != nil && a.hasAuditFindings() {
			out = append(out, contract.Next{Command: a.scoped("audit"), Why: "the audit after upgrade has warnings; read them"})
		}
	case *engine.StatusResult:
		out = a.featureNexts(out, r.Features)
	case *engine.FeatureListResult:
		out = a.featureNexts(out, r.Features)
	case *engine.SyncResult:
		// Attention holds only features sync can't close itself.
		out = a.featureNexts(out, r.Attention)
		if r.Preview == nil && r.Surveyed && r.ActiveFeatures == 0 && !a.closeFailed() {
			out = append(out, contract.Next{Command: a.scoped("feature", "start"), NeedsInput: []string{"<name>"},
				Why: "no feature is in progress; start one: put its name (the <name> argument) after start"})
		}
	}
	switch r := a.result.(type) {
	case *engine.InitResult:
		if r.Preview == nil {
			out = append(out, a.guardNext()...)
		}
	case *engine.FeatureStartResult:
		if r.Preview == nil {
			out = append(out, a.guardNext()...)
		}
	case *engine.GateResult:
		if r.Preview == nil {
			out = append(out, a.guardNext()...)
		}
	}
	return append(out, a.upgradeNext()...)
}

// waitNext finishes the pending gate job.
func (a *app) waitNext(job string) contract.Next {
	return contract.Next{Command: a.scoped("wait", job),
		Why: "CI is running on the gate candidate; wait finishes the job and records the result (with --json it streams progress)"}
}

// gateAgain gates again the feature a job error is about: details.feature,
// else the feature in details.job's id. It is nil when neither names one.
func (a *app) gateAgain(ce *contract.Error, why string) []contract.Next {
	f, _ := ce.Details["feature"].(string)
	if f == "" {
		job, _ := ce.Details["job"].(string)
		ref, err := engine.ParseJob(job)
		if err != nil {
			return nil
		}
		f = ref.Feature
	}
	return []contract.Next{{Command: a.scoped("gate", f), Why: why}}
}

// startAgain is feature start for the name this run was given, scoped like
// the sync before it, with --plan when this run had it.
func (a *app) startAgain() []string {
	args := []string{"feature", "start", a.startName}
	if a.dryRun {
		args = append(args, "--plan")
	}
	return a.scoped(args...)
}

// syncNext leads next when shadow main is behind public main. Syncing moves
// only shadow branches, none of which a gate candidate or a plan's
// fingerprint depends on, so it is safe before any other step.
func (a *app) syncNext() []contract.Next {
	if a.command == "sync" || !a.hasWarning(contract.ShadowBehind) {
		return nil
	}
	return []contract.Next{{Command: a.scoped("sync"), Why: "shadow main is behind public main; sync it (fast-forward only)"}}
}

// featureNexts adds what the features need, from their reports, to out
// (which leads with syncNext's sync when shadow main is behind). Sync
// closes merged, unmoved features, so they share one sync step, folded into
// that leading sync when there is one; then, by name, the features a person
// must close and the ones to gate again. Abandoned features need nothing.
func (a *app) featureNexts(out []contract.Next, rs []engine.FeatureReport) []contract.Next {
	newer := a.newerRecords()
	var merged []string
	var rest []contract.Next
	for _, r := range rs {
		closeCmd := a.scoped("feature", "close", r.Name)
		if newer[r.Name] && (r.State == feature.StateMerged || (r.State == feature.StateBranchGone && r.PRState == "merged")) {
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its PR merged, but a newer Ghostbranch wrote its record, so this binary won't close it unattended: upgrade ghostbranch on this machine and sync, or a person closes it at the console"})
			continue
		}
		if engine.ClosableWithoutConsole(r) {
			merged = append(merged, r.Name)
			continue
		}
		switch r.State {
		case feature.StateUnreadable:
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its record couldn't be read; a person at the console can close it (feature show " + r.Name + " gives the read error)"})
		case feature.StateBranchGone:
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its branch was deleted; a person closes it at the console"})
		case feature.StateMergedMoved:
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its PR merged but there are newer commits; promote them or a person closes it"})
			if r.Gate != nil && r.Gate.Job != nil {
				rest = append(rest, contract.Next{Command: a.scoped("wait", *r.Gate.Job),
					Why: "its gate is waiting on CI; finish it (wait streams progress with --json)"})
			}
		case feature.StatePRClosed:
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its PR was closed without merging"})
		case feature.StateDraft, feature.StateInReview, feature.StateGatePassed, feature.StatePublished:
			if r.Gate != nil && r.Gate.Job != nil {
				rest = append(rest, contract.Next{Command: a.scoped("wait", *r.Gate.Job),
					Why: "its gate is waiting on CI; finish it (wait streams progress with --json)"})
			} else if r.Drift.Status == "conflicts" {
				rest = append(rest, contract.Next{Command: a.scoped("gate", r.Name),
					Why: "no longer merges cleanly onto public main"})
			}
		}
	}
	if len(merged) > 0 {
		closes := make([]string, len(merged))
		for i, n := range merged {
			closes[i] = "`" + shellJoin(a.scoped("feature", "close", n)) + "`"
		}
		why := "sync closes merged features; or run " + strings.Join(closes, ", ")
		if sync := a.scoped("sync"); len(out) > 0 && slices.Equal(out[0].Command, sync) {
			out[0].Why += "; " + why
		} else {
			out = append(out, contract.Next{Command: sync, Why: why})
		}
	}
	return append(out, rest...)
}

// newerRecords names the features whose record a newer Ghostbranch wrote:
// the RECORD_NEWER warnings this run raised.
func (a *app) newerRecords() map[string]bool {
	out := map[string]bool{}
	for _, w := range a.warnings {
		if f, _ := w.Details["feature"].(string); w.Code == contract.RecordNewer && f != "" {
			out[f] = true
		}
	}
	return out
}

// closeFailed is whether sync left a merged feature in place: a
// FEATURE_NOT_CLOSED that names a feature and deleted none of its refs.
func (a *app) closeFailed() bool {
	for _, w := range a.warnings {
		if w.Code != contract.FeatureNotClosed {
			continue
		}
		_, deleted := w.Details["deleted"]
		if f, _ := w.Details["feature"].(string); f != "" && !deleted {
			return true
		}
	}
	return false
}

// upgradeNext trails next when managed overlay files are out of date.
// Upgrading moves the overlay branch, which the gate candidate and the plan
// fingerprint include, so it comes after the steps that use them.
func (a *app) upgradeNext() []contract.Next {
	if a.command == "upgrade" || !a.hasWarning(contract.OverlayOutdated) {
		return nil
	}
	// Upgrading after a gate or a plan would make the plan stale.
	const afterPlan = "after the plan is applied (upgrade moves the overlay and would make the plan stale)"
	planNext := false
	switch r := a.result.(type) {
	case *engine.GateResult:
		planNext = true
	case *engine.PromoteResult:
		planNext = r.Applied == nil
	}
	if a.handEdited() {
		why := "a managed file was edited by hand; preview what upgrade would replace"
		if planNext {
			why += ", and upgrade only " + afterPlan
		}
		return []contract.Next{{Command: a.scoped("upgrade", "--plan"), Why: why}}
	}
	why := "managed overlay files are out of date; preview with --plan, then upgrade"
	if planNext {
		why = "managed overlay files are out of date; upgrade " + afterPlan
	}
	return []contract.Next{{Command: a.scoped("upgrade"), Why: why}}
}

// handEdited is whether an OVERLAY_OUTDATED warning names a managed file
// edited by hand, which upgrade would replace.
func (a *app) handEdited() bool {
	for _, w := range a.warnings {
		if w.Code != contract.OverlayOutdated {
			continue
		}
		files, _ := w.Details["files"].([]string)
		for _, f := range files {
			if strings.HasSuffix(f, " ("+string(managed.Modified)+")") {
				return true
			}
		}
	}
	return false
}

func (a *app) hasWarning(code contract.WarningCode) bool {
	for _, w := range a.warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

// hasAuditFindings is whether the audit raised a warning worth reading in
// full. ACTIONS_MINUTES_BILLED is left out: every private shadow repo has it.
func (a *app) hasAuditFindings() bool {
	for _, w := range a.warnings {
		switch w.Code {
		case contract.OverlayOutdated, contract.RepoGuard, contract.RepoGuardUnrewritable,
			contract.SecretsUnlisted, contract.SecretsMissing, contract.PushTriggersOnSync:
			return true
		}
	}
	return false
}

// applyNext publishes plan id: under a grant when one is in force (and
// checkGrant is set), otherwise by a person at a console.
func (a *app) applyNext(id string, checkGrant bool) contract.Next {
	if checkGrant && a.eng != nil && a.eng.Grants != nil && a.pairFull.Public != "" {
		if _, ok, err := a.eng.Grants.Valid(a.pairFull.Public, a.pairFull.Shadow, a.env.Now()); err == nil && ok {
			return contract.Next{Command: a.gb("apply", a.planRef(id), "--yes"), Why: "a person allowed agents to publish here; publish the plan"}
		}
	}
	return contract.Next{Command: a.gb("apply", a.planRef(id)), NeedsHuman: true,
		Why: "publish the plan: a person runs this in their own terminal and types the code it shows (or allows agents first with ghostbranch agents allow)"}
}

// planRef is how to name plan id in an apply command: the path this apply
// loaded it from (apply <id> looks only in the plans directory), else the id.
func (a *app) planRef(id string) string {
	pos := positionals(a.args)
	if a.command != "apply" || len(pos) < 2 || pos[0] != "apply" || !strings.HasSuffix(pos[1], ".json") {
		return id
	}
	path := pos[1]
	if !filepath.IsAbs(path) && a.env.Getwd != nil {
		if wd, err := a.env.Getwd(); err == nil {
			path = filepath.Join(wd, path)
		}
	}
	if p, _, err := plan.Load("", path); err != nil || p.ID != id {
		return id // another plan (an earlier one BRANCH_EXISTS names)
	}
	return path
}

func (a *app) guardNext() []contract.Next {
	if a.env.GuardState == nil || a.pairFull.Public == "" {
		return nil
	}
	if in, installed := a.env.GuardState(a.pairFull.Public); !in || installed {
		return nil
	}
	return []contract.Next{{Command: a.gb("guard", "install"), NeedsHuman: true,
		Why: "block pushing feature work to the public remote from this checkout; the person decides whether to install it"}}
}

// guardState is the default Env.GuardState: is the current directory a
// checkout of public, and is the guard installed there?
func guardState(public string) (bool, bool) {
	ctx := context.Background()
	co, err := guard.Open(ctx, ".")
	if err != nil {
		return false, false
	}
	remotes, err := co.Remotes(ctx)
	if err != nil {
		return false, false
	}
	in := false
	for _, u := range remotes {
		if s, ok := guard.Slug(u); ok && strings.EqualFold(s, public) {
			in = true
		}
	}
	if !in {
		return false, false
	}
	st, err := guard.Inspect(co)
	return true, err == nil && st.Installed
}

// safeRemoteName is what a remote name must match to be printed unquoted
// into a command: git accepts far more in config than a shell does.
var safeRemoteName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// shadowRemote is the default Env.ShadowRemote.
func shadowRemote(shadow string) (remote, free string, inCheckout bool) {
	ctx := context.Background()
	co, err := guard.Open(ctx, ".")
	if err != nil {
		return "", "shadow", false
	}
	remotes, err := co.Remotes(ctx)
	if err != nil {
		return "", "shadow", true
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	free = freeRemoteName(remotes)
	for _, n := range names {
		if s, ok := guard.Slug(remotes[n]); ok && strings.EqualFold(s, shadow) && safeRemoteName.MatchString(n) {
			return n, free, true
		}
	}
	return "", free, true
}

// freeRemoteName is the first of shadow, ghostbranch-shadow,
// ghostbranch-shadow-2, ... that no remote uses.
func freeRemoteName(remotes map[string]string) string {
	free := "shadow"
	for i := 1; ; i++ {
		if _, used := remotes[free]; !used {
			return free
		}
		free = "ghostbranch-shadow"
		if i > 1 {
			free += "-" + strconv.Itoa(i)
		}
	}
}

// checkoutArgv is the exact git commands, in order, that get feature name
// into the caller's checkout: through the remote that points at the shadow
// repo, one added for it under the name free, or a fresh clone.
func checkoutArgv(name, shadow, remote, free string, inCheckout bool) [][]string {
	url := "https://github.com/" + shadow + ".git"
	switch {
	case remote != "":
		return [][]string{{"git", "fetch", remote}, {"git", "switch", name}}
	case inCheckout:
		return [][]string{{"git", "remote", "add", free, url}, {"git", "fetch", free}, {"git", "switch", name}}
	}
	return [][]string{{"git", "clone", "--origin", "shadow", "--branch", name, url}}
}

// pushRemote is the remote that reaches the shadow repo once checkoutArgv
// has run.
func pushRemote(remote, free string, inCheckout bool) string {
	switch {
	case remote != "":
		return remote
	case inCheckout:
		return free
	}
	return "shadow"
}

// leakWheres is the set of the findings' Where values.
func leakWheres(ce *contract.Error) map[string]bool {
	fs, _ := ce.Details["findings"].([]leak.Finding)
	out := map[string]bool{}
	for _, f := range fs {
		out[f.Where] = true
	}
	return out
}

// valueFlags are the flags that take a value, by every name they can be
// given as, so a value is never read as a flag.
// TestValueFlagsMatchTheCommands keeps it in step with the commands.
var valueFlags = map[string]bool{
	"--repo": true, "--workspace": true, "--shadow": true, "--for": true, "--reason": true, "--timeout": true,
	"--mode": true, "--message": true, "-m": true, "--curate": true, "--title": true, "--body": true, "--branch": true,
	"--install": true, "--uninstall": true, "--feature": true, "--op": true, "--actor": true, "--since": true,
	"--limit": true,
}

// shortValue splits "-mVALUE" or "-m=VALUE" into "-m" and VALUE when -m
// takes a value.
func shortValue(arg string) (flag, value string, ok bool) {
	if len(arg) <= 2 || arg[0] != '-' || arg[1] == '-' || !valueFlags[arg[:2]] {
		return "", "", false
	}
	return arg[:2], strings.TrimPrefix(arg[2:], "="), true
}

// withoutFlags drops each named flag and its value from argv, in the
// "--flag value", "--flag=value" and "-mVALUE" forms. Another flag's value
// is skipped with its flag, so it is never taken for a named one; argv
// after "--" is kept as is.
func withoutFlags(argv []string, flags ...string) []string {
	named := map[string]bool{}
	for _, f := range flags {
		named[f] = true
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		long, _, inline := strings.Cut(arg, "=")
		short, _, isShort := shortValue(arg)
		switch {
		case arg == "--":
			return append(out, argv[i:]...)
		case named[arg]:
			if valueFlags[arg] {
				i++ // and its value
			}
		case inline && strings.HasPrefix(arg, "--") && named[long]:
		case isShort && named[short]:
		case valueFlags[arg] && i+1 < len(argv):
			out = append(out, arg, argv[i+1])
			i++
		default:
			out = append(out, arg)
		}
	}
	return out
}

// flagValue is the value argv last gives a long flag such as "--title", in
// the "--flag value" or "--flag=value" form; ok is false when it isn't given.
func flagValue(argv []string, flag string) (value string, ok bool) {
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return value, ok
		case arg == flag && i+1 < len(argv):
			value, ok = argv[i+1], true
			i++
		case strings.HasPrefix(arg, flag+"="):
			value, ok = strings.TrimPrefix(arg, flag+"="), true
		case valueFlags[arg]:
			i++ // another flag's value
		}
	}
	return value, ok
}

// positionals is argv without its flags and their values.
func positionals(argv []string) []string {
	var out []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--":
			return append(out, argv[i+1:]...)
		case valueFlags[arg]:
			i++
		case strings.HasPrefix(arg, "-") && arg != "-":
		default:
			out = append(out, arg)
		}
	}
	return out
}

// withoutSwitch drops the boolean flag name from argv, as the exact token
// or as name=value; unlike withoutFlags it never takes the next argument.
func withoutSwitch(argv []string, name string) []string {
	out := make([]string, 0, len(argv))
	for _, arg := range argv {
		if arg != name && !strings.HasPrefix(arg, name+"=") {
			out = append(out, arg)
		}
	}
	return out
}

// historyNexts is what to do about a problem the scan found in the
// feature's commits (each finding's commits, or a marker error's
// details.commit, names them), which may be only in an older commit that
// the final tree no longer has. The steps an agent can run come first:
// publish so those commits never ship as they are (squash; curated, which
// can fold a commit into the one that undoes it or give it a new message),
// or rewrite them on the feature branch and gate again. An author leak also
// needs a person to change how authors are published. cmd builds a command
// for the pair; mode is the mode the problem was found in; keep are the
// flags and values (keptTexts) the suggested promotes keep.
func (a *app) historyNexts(cmd func(...string) []string, f, mode string, wheres map[string]bool, marker bool, keep []string) []contract.Next {
	files := marker || wheres["tree"] || wheres["path"]
	// A curated promote's messages come from its --curate file, not the
	// feature's commits: a new file fixes them, not a rewrite.
	curateMsgs := mode == config.ModeCurated && wheres["commit_message"]
	msgs, authors := wheres["commit_message"] && !curateMsgs, wheres["author"]
	var out []contract.Next
	if mode != config.ModeSquash {
		why := "publish the feature as one commit"
		switch {
		case files:
			why = "if the problem is only in older commits (the final tree no longer has it), " + why + ", so they never ship"
		default:
			why += ", so the feature commits never ship as they are"
		}
		if msgs || curateMsgs {
			why += "; write a --message that doesn't leak"
		}
		if authors {
			why += "; the publishing account authors it"
		}
		out = append(out, contract.Next{Command: cmd(append([]string{"promote", f, "--mode", config.ModeSquash, "--plan"}, keep...)...),
			NeedsInput: []string{"--message"}, Why: why})
	}
	if mode != config.ModeCurated && (files || msgs) {
		why := "or curate the commits in a --curate file (see the gate result's commits)"
		if files {
			why += ": group each commit the finding names with a later one that removes the problem"
		}
		if msgs {
			why += ", with new messages that don't leak"
		}
		need := []string{"--curate"}
		if !keeps(keep, "--title") {
			need = append(need, "--title")
		}
		out = append(out, contract.Next{Command: cmd(append([]string{"promote", f, "--mode", config.ModeCurated, "--plan"}, keep...)...),
			NeedsInput: need, Why: why})
	}
	var fix []string
	if files {
		fix = append(fix, "fix the files")
	}
	if msgs {
		fix = append(fix, "reword the messages")
	}
	if authors {
		fix = append(fix, "re-author them")
	}
	where := "each finding's commits"
	if marker {
		where = "error.details.commit"
	}
	if len(fix) > 0 {
		why := "or rewrite the commits in " + where + " on the feature branch in the shadow repo (" + strings.Join(fix, ", ") +
			"), push with " + a.forcePush(f) + ", then gate again"
		if curateMsgs {
			why += "; then promote again with a --curate file whose messages don't leak"
		}
		out = append(out, contract.Next{Command: cmd("gate", f), Why: why})
	}
	if authors {
		out = append(out, contract.Next{Command: cmd("gate", f), NeedsHuman: true,
			Why: "to publish those commits with other authors, a person sets promote.author_email: noreply, or adds a promote.author_map entry (they edit the overlay config), then gate again"})
	}
	return out
}

// noteShadowRemote records the checkout's remote for the pair's shadow
// repo, for next to name in a push command.
func (a *app) noteShadowRemote(p workspace.Pair) {
	if a.env.ShadowRemote != nil && a.shadowRemote == "" {
		a.shadowRemote, _, _ = a.env.ShadowRemote(p.Shadow)
	}
}

// forcePush is the command that pushes a rewritten feature branch to the
// shadow repo, with the checkout's remote for it when known.
func (a *app) forcePush(f string) string {
	if a.shadowRemote != "" {
		return "`git push --force-with-lease " + a.shadowRemote + " " + f + "`"
	}
	return "`git push --force-with-lease <remote> " + f + "`, <remote> being the shadow repo's remote"
}

// historyFound is whether the problem was found in an older feature
// commit, one before the tip: a finding whose commits leave out the tip, or
// a marker error that names another commit. A finding the tip itself
// introduces or changes is in the final tree, so the plain fix applies.
// tip is "" when this run doesn't know the feature's commits.
func historyFound(ce *contract.Error, tip string) bool {
	if c, _ := ce.Details["commit"].(string); ce.Code == contract.MarkerInvalid && c != "" {
		return c != tip
	}
	fs, _ := ce.Details["findings"].([]leak.Finding)
	for _, f := range fs {
		if len(f.Commits) > 0 && !slices.Contains(f.Commits, tip) {
			return true
		}
	}
	return false
}

// featureTip is the feature's last commit: from the gate this run ran or
// reused, else the error's details.feature_head (apply), else "".
func (a *app) featureTip(ce *contract.Error) string {
	var g *engine.GateResult
	switch r := a.result.(type) {
	case *engine.GateResult:
		g = r
	case *engine.PromoteResult:
		g = r.Gate
	}
	if g != nil && len(g.Commits) > 0 {
		return g.Commits[len(g.Commits)-1].Source
	}
	tip, _ := ce.Details["feature_head"].(string)
	return tip
}

// tipFound is whether the feature's last commit, tip, introduced or changed
// a finding. A marker error naming no commit is in the final tree, so the
// tip holds it.
func tipFound(ce *contract.Error, tip string) bool {
	if ce.Code == contract.MarkerInvalid {
		c, _ := ce.Details["commit"].(string)
		return c == "" || c == tip
	}
	if tip == "" {
		return false
	}
	fs, _ := ce.Details["findings"].([]leak.Finding)
	for _, f := range fs {
		if slices.Contains(f.Commits, tip) {
			return true
		}
	}
	return false
}

// fixWhy is the why of the plain "fix it, then <then>" step. When the
// feature's last commit introduced the problem and the mode may publish
// each commit, a fix commit on top would leave it in a published commit
// and cost a round: amend the last commit instead.
func (a *app) fixWhy(ce *contract.Error, f, mode, then string) string {
	if f != "" && mode != config.ModeSquash && tipFound(ce, a.featureTip(ce)) {
		return "amend the feature's last commit to remove it (a fix commit on top would leave it in a commit that preserve and curated promotes publish), push with " +
			a.forcePush(f) + ", then " + then
	}
	return "after fixing the content on the feature branch in the shadow repo, " + then
}

// keptTexts are the --branch, --title and --body this promote was given
// (for apply, the plan's public branch), for a history promote to keep:
// all but the ones a finding is in.
func (a *app) keptTexts(ce *contract.Error, wheres map[string]bool) []string {
	var out []string
	switch a.command {
	case "promote":
		for _, k := range []struct{ flag, where string }{{"--branch", "branch"}, {"--title", "pr_title"}, {"--body", "pr_body"}} {
			if v, ok := flagValue(a.args, k.flag); ok && !wheres[k.where] {
				out = append(out, k.flag, v)
			}
		}
	case "apply":
		f, _ := ce.Details["feature"].(string)
		if b, _ := ce.Details["branch"].(string); b != "" && b != f && !wheres["branch"] {
			out = append(out, "--branch", b)
		}
	}
	return out
}

// keeps is whether keep (flag, value pairs) has flag.
func keeps(keep []string, flag string) bool {
	for i := 0; i+1 < len(keep); i += 2 {
		if keep[i] == flag {
			return true
		}
	}
	return false
}

// publisherFix is what to do when the publishing account's identity leaks:
// a person changes the leak rules or publishes from another account.
func (a *app) publisherFix() contract.Next {
	return contract.Next{Command: a.again(true), NeedsHuman: true,
		Why: "the publishing GitHub account's name or email matched a leak rule; a person changes leak_scan.block_patterns or publishes from another account"}
}

// publisherLeaks is whether a finding is in the publisher's identity: the
// committer always, and in squash mode the author too (the publisher
// authors the squash commit).
func publisherLeaks(wheres map[string]bool, mode string) bool {
	return wheres["committer"] || (wheres["author"] && (mode == config.ModeSquash || mode == ""))
}

// historyLeaks is whether a finding is in the feature's own commits: their
// authors outside squash, and their messages in preserve mode (curated
// messages come from the --curate file).
func historyLeaks(wheres map[string]bool, mode string) bool {
	if mode == config.ModeSquash || mode == "" {
		return false
	}
	return wheres["author"] || (mode != config.ModeCurated && wheres["commit_message"])
}

// textFix is the step for leaks in text the caller supplies: the flags to
// drop from the command and the inputs to ask for, one per leaking text.
func textFix(wheres map[string]bool, mode string) (drop, need []string, curateMsg bool) {
	add := func(input string, flags ...string) {
		need = append(need, input)
		drop = append(drop, flags...)
	}
	if wheres["commit_message"] {
		if mode == config.ModeCurated {
			add("--curate", "--curate")
			curateMsg = true
		} else {
			add("--message", "-m", "--message")
		}
	}
	if wheres["pr_title"] {
		add("--title", "--title")
	}
	if wheres["pr_body"] {
		add("--body", "--body")
	}
	if wheres["branch"] {
		add("--branch", "--branch")
	}
	return drop, need, curateMsg
}

// promoteGateLeaked is whether this promote stopped on the leak scan of the
// gate it ran, rather than on its own scan of what it would publish.
func (a *app) promoteGateLeaked() bool {
	r, ok := a.result.(*engine.PromoteResult)
	return ok && r.Gate != nil && r.Gate.Leak == feature.LeakFound && len(r.Findings) == 0
}

// replanArgs is the promote that makes a new plan for the feature f a plan
// was for, in mode, on the plan's public branch (error.details.branch) when
// keepBranch.
func replanArgs(ce *contract.Error, f, pub, mode string, keepBranch bool) []string {
	args := []string{"promote", f, "--mode", mode, "--plan", "--repo", pub}
	if branch, _ := ce.Details["branch"].(string); keepBranch && branch != "" && branch != f {
		args = append(args, "--branch", branch)
	}
	return args
}

// modeInputs are the flags a promote in mode needs from the caller.
func modeInputs(mode string) []string {
	switch mode {
	case config.ModeSquash:
		return []string{"--message"}
	case config.ModeCurated:
		return []string{"--curate"}
	}
	return nil
}

// leakNext is what to do about LEAK_FOUND or MARKER_INVALID: fix the
// content and run again, or, when the problem is in the feature's commits,
// the steps historyNexts gives; and, for waivable findings a person judges
// false positives, that person waives them at their console.
func (a *app) leakNext(ce *contract.Error) []contract.Next {
	f, _ := ce.Details["feature"].(string)
	mode, _ := ce.Details["mode"].(string)
	wheres := leakWheres(ce)
	files := wheres["tree"] || wheres["path"]
	older := historyFound(ce, a.featureTip(ce))
	// What a gate scans of a message or author is in a commit even at the tip.
	history := older || wheres["commit_message"] || wheres["author"]
	marker := ce.Code == contract.MarkerInvalid
	var out []contract.Next
	switch a.command {
	case "promote":
		drop, need, curateMsg := textFix(wheres, mode)
		switch {
		case a.promoteGateLeaked() || (marker && f != ""):
			// The gate this promote ran found it: fix it as after a gate.
			if f != "" && history {
				out = append(out, a.historyNexts(a.scoped, f, mode, wheres, marker, a.keptTexts(ce, wheres))...)
				break
			}
			out = append(out, contract.Next{Command: a.again(false), Why: a.fixWhy(ce, f, mode, "run this again")})
		case publisherLeaks(wheres, mode):
			out = append(out, a.publisherFix())
		case f != "" && (historyLeaks(wheres, mode) || (older && files)):
			out = append(out, a.historyNexts(a.scoped, f, mode, wheres, false, a.keptTexts(ce, wheres))...)
		case len(need) > 0:
			why := "a leak is in text this promote publishes: run it again with new text for " + strings.Join(need, ", ")
			if curateMsg {
				why += " (a --curate file whose messages don't leak)"
			}
			if files {
				why += ", after fixing the file findings on the feature branch in the shadow repo"
			}
			out = append(out, contract.Next{Command: withoutFlags(a.again(false), drop...), NeedsInput: need, Why: why})
		default:
			out = append(out, contract.Next{Command: a.again(false), Why: a.fixWhy(ce, f, mode, "run this again")})
		}
	case "apply":
		pub, _ := ce.Details["public"].(string)
		inPair := func(args ...string) []string { return a.gb(append(args, "--repo", pub)...) }
		switch {
		case f == "" || pub == "" || mode == "":
		case publisherLeaks(wheres, mode):
			out = append(out, a.publisherFix())
		case historyLeaks(wheres, mode) || (older && files):
			out = append(out, a.historyNexts(inPair, f, mode, wheres, false, a.keptTexts(ce, wheres))...)
		default:
			need := modeInputs(mode)
			for _, w := range []struct{ where, input string }{{"pr_title", "--title"}, {"pr_body", "--body"}, {"branch", "--branch"}} {
				if wheres[w.where] {
					need = append(need, w.input)
				}
			}
			out = append(out, contract.Next{Command: a.gb(replanArgs(ce, f, pub, mode, !wheres["branch"])...),
				Why: "fix what leaks (on the feature branch, or in the text), then make a new plan", NeedsInput: need})
		}
	case "gate", "waivers add":
		switch {
		case f != "" && history:
			out = append(out, a.historyNexts(a.scoped, f, mode, wheres, marker, nil)...)
		case f != "":
			out = append(out, contract.Next{Command: a.scoped("gate", f), Why: a.fixWhy(ce, f, mode, "gate again")})
		}
	}
	ids, _ := ce.Details["waivable"].([]string)
	if ce.Code == contract.LeakFound && f != "" && len(ids) > 0 {
		out = append(out, contract.Next{Command: a.scoped(append([]string{"waivers", "add", f}, ids...)...), NeedsHuman: true,
			NeedsInput: []string{"--reason"},
			Why:        "only if a person judges these findings false positives: they review them at their console and give a reason"})
	}
	return out
}
