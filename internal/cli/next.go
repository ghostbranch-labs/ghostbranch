package cli

import (
	"cmp"
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
// hides the console they need, and --json, and names the pair with --repo
// (in place of any --repo given): they run it in their own terminal.
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
	// A relative --repo (".", say) names this checkout, which their
	// terminal may not be in, so it is replaced too.
	if forHuman && a.pairFull.Public != "" {
		out = append(withoutFlags(out, "--repo"), "--repo", a.pairFull.Public)
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
		case contract.PairNotFound:
			if pub, _ := ce.Details["public"].(string); pub != "" {
				return a.initNexts(pub)
			}
		case contract.DoctorFailed:
			return a.doctorNext()
		case contract.TokenPermissionMissing:
			return []contract.Next{{Command: a.scoped("doctor"),
				Why: "doctor lists every permission the token lacks, for each command; a person grants them, then runs this command again"}}
		case contract.LoginExpired:
			return []contract.Next{{Command: a.gb("login"), NeedsHuman: true,
				Why: "the stored GitHub login expired; a person logs in again at a terminal"}}
		case contract.LoginUnavailable:
			// no_keychain and keychain_error get no next: their fix (setting
			// GHOSTBRANCH_TOKEN or unlocking the keychain) isn't a command.
			switch r, _ := ce.Details["reason"].(string); r {
			case "corrupt":
				return []contract.Next{
					{Command: a.gb("logout"), Why: "remove the unreadable stored login"},
					{Command: a.gb("login"), NeedsHuman: true, Why: "then a person logs in again at a terminal"},
				}
			case "no_console", "denied", "expired_code", "refresh_failed", "refused":
				return []contract.Next{{Command: a.gb("login"), NeedsHuman: true,
					Why: "a person runs this in their own terminal and approves it on GitHub"}}
			case "no_client_id":
				return []contract.Next{{Command: a.gb("login", "--with-token"), NeedsHuman: true,
					Why: "pipe a GitHub token on stdin, for example: gh auth token | ghostbranch login --with-token"}}
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
			if fs := stringList(ce.Details["features"]); len(fs) > 1 {
				if pub, _ := ce.Details["public"].(string); pub != "" {
					return []contract.Next{{Command: a.gb(append(append([]string{"promote"}, fs...), "--plan", "--repo", pub)...),
						Why: "the stack moved since the plan was made; make a new plan for the whole stack, passing again any --mode, --mode-for and --title-for the old plan was made with (this command doesn't carry them)"}}
				}
			}
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
				{Command: a.startAgain(), Why: startAgainWhy(a.startFrom)},
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
			why := "nothing is pending; run it again"
			if a.command == "apply" {
				why = "apply resumes where it stopped; run it again"
			}
			return []contract.Next{{Command: a.again(false), Why: why}}
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
			who := "another ghostbranch command"
			if h, _ := ce.Details["held_by"].(map[string]any); h != nil {
				if text := workspace.HolderText(h); text != "" {
					who += " (" + text + ")"
				}
			}
			if a.personOnly() {
				// Only a person can finish it: say so, as its own next
				// does (review M7).
				return []contract.Next{{Command: a.again(true), NeedsHuman: true,
					Why: who + " holds the lock; wait a few seconds for it to end, then a person runs this again in their own terminal"}}
			}
			return []contract.Next{{Command: a.again(false), Why: who + " holds the lock; wait a few seconds for it to end, then run this again"}}
		case contract.FeatureMoved:
			if f, _ := ce.Details["feature"].(string); f != "" {
				why := "the feature changed while it was being closed and nothing was deleted; look at it again before closing it"
				if a.command == "feature fold" {
					why = "the feature or its parent changed while it was being folded, and nothing changed; look at it again before folding it"
				}
				return []contract.Next{{Command: a.scoped("feature", "show", f), Why: why}}
			}
		case contract.ForgeError:
			// feature close or fold deleted the refs, then couldn't close the gate PR.
			if r, ok := a.result.(*engine.FeatureCloseResult); ok && len(r.Deleted) > 0 {
				return []contract.Next{{Command: a.scoped("log", "--feature", r.Feature, "--op", "feature close"), NeedsHuman: true,
					Why: "the feature's refs are deleted and the ledger lists them with their SHAs; a person closes its gate draft PR in the shadow repo on GitHub (error.message names it)"}}
			}
			if r, ok := a.result.(*engine.FoldResult); ok && len(r.Deleted) > 0 {
				return []contract.Next{{Command: a.scoped("log", "--feature", r.Feature, "--op", "feature fold"), NeedsHuman: true,
					Why: "the fold landed: " + r.Parent + " holds the feature's commits, and the ledger lists its deleted refs with their SHAs; a person closes its gate draft PR in the shadow repo on GitHub (error.message names it)"}}
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
			case "title-for":
				fs := stringList(ce.Details["features"])
				if len(fs) == 0 {
					if f, _ := ce.Details["feature"].(string); f != "" {
						fs = []string{f}
					}
				}
				need := make([]string, len(fs))
				for i, f := range fs {
					need[i] = "--title-for " + f + "=<title>"
				}
				cmd, why := a.again(false), "write a PR title for each branch in needs_input: each has more than one commit, so no one commit names it"
				if stack := stringList(ce.Details["stack"]); len(stack) > 0 {
					// The open PRs above the listed feature need titles too:
					// list the whole line so --title-for may name them.
					args := append([]string{"promote"}, stack...)
					if mode, ok := flagValue(a.args, "--mode"); ok {
						args = append(args, "--mode", mode)
					}
					cmd = a.scoped(append(args, "--plan")...)
					why += "; list the whole stack so --title-for may name them, and pass again any --mode-for and --title-for you gave"
				}
				return []contract.Next{{Command: cmd, NeedsInput: need, Why: why}}
			}
		case contract.FoldNeedsRebase:
			f, _ := ce.Details["feature"].(string)
			parent, _ := ce.Details["parent"].(string)
			forked, _ := ce.Details["forked_at"].(string)
			if f != "" && parent != "" {
				return a.rebaseNexts(f, parent, forked, a.scoped("feature", "fold", f), "then fold it again: "+parent+" fast-forwards to it")
			}
		case contract.RebaseConflict:
			// A restack's stop names the command that carries on; a gate's
			// conflict has no next here.
			if again, ok := ce.Details["again"].([]string); ok {
				f, _ := ce.Details["feature"].(string)
				parent, _ := ce.Details["parent"].(string)
				up, _ := ce.Details["upstream"].(string)
				return a.rebaseNexts(f, parent, up, a.scoped(again...),
					"then run the restack again: it records "+f+" where you put it and goes on with the features above it")
			}
		case contract.HasChildren:
			// close or fold refused beside a record that can't be read.
			if bad, ok := ce.Details["unreadable"].([]string); ok && len(bad) > 0 {
				var out []contract.Next
				for _, b := range bad {
					out = append(out, contract.Next{Command: a.scoped("feature", "close", b), NeedsHuman: true,
						Why: "feature " + b + "'s record can't be read, so it may be stacked on this one: a person closes it at the console, or fixes its record"})
				}
				if f, _ := ce.Details["feature"].(string); f != "" {
					op, _ := ce.Details["op"].(string)
					out = append(out, contract.Next{Command: a.scoped("feature", op, f), Why: "then " + op + " " + f + " again"})
				}
				return out
			}
			kids, _ := ce.Details["children"].([]string)
			op, _ := ce.Details["op"].(string)
			var out []contract.Next
			for _, k := range kids {
				out = append(out,
					contract.Next{Command: a.scoped("feature", "fold", k), Why: "fold " + k + " into its parent: the parent fast-forwards to it and " + k + " closes"},
					contract.Next{Command: a.scoped("feature", op, k), NeedsHuman: op == "close", Why: "or " + op + " " + k + " too"},
					contract.Next{Command: a.scoped("feature", "restack", k, "--onto", "main"), Why: "or move " + k + " onto main, dropping its parent's commits"})
			}
			return out
		case contract.ParentRewritten:
			f, _ := ce.Details["feature"].(string)
			parent, _ := ce.Details["parent"].(string)
			if reason, _ := ce.Details["reason"].(string); reason == "parent_gone" && f != "" {
				return []contract.Next{{Command: a.scoped("feature", "restack", f, "--onto", "main"),
					Why: "its parent is gone; move it onto main, keeping only its own commits"}}
			}
			if parent == "main" && f != "" {
				// A feature moved onto main that was rebased by hand past
				// its old fork point.
				return []contract.Next{{Command: a.scoped("feature", "restack", f, "--onto", "main"),
					Why: "once " + f + " sits on main's tip, record it there, dropping its old fork point"}}
			}
			if parent != "" {
				return []contract.Next{
					{Command: a.scoped("feature", "restack", parent), Why: "restack the features above " + parent + " onto its tip, in the shadow repo; a conflict stops it with git steps"},
					{Command: a.again(false), Why: "then run this again"},
				}
			}
		case contract.NotAStack:
			// The bottom's published parent merged or moved: gating or
			// promoting again can't help until sync has recorded it. Never
			// "promote again", which would refuse the same way.
			reason, _ := ce.Details["reason"].(string)
			parent, _ := ce.Details["parent"].(string)
			switch {
			case reason == "parent_moved" && ce.Details["public_main_moved"] == true && parent != "":
				f, _ := ce.Details["feature"].(string)
				return []contract.Next{
					{Command: a.scoped("promote", parent, "--plan"),
						Why: "public main moved since " + parent + " was published: promote " + parent + " first, which updates its PR onto the new main; then gate and promote " + cmp.Or(f, "this feature")},
					{Command: a.scoped("sync"),
						Why: "or wait for " + parent + "'s public PR to merge: sync then closes " + parent + " and moves its children onto main; gate and promote them then"},
				}
			case reason == "parent_merged", reason == "parent_moved":
				return []contract.Next{{Command: a.scoped("sync"), Why: "sync records what changed on the public side; then restack and gate what moved"}}
			case reason == "stale_gate":
				if f, _ := ce.Details["feature"].(string); f != "" {
					if m, _ := ce.Details["update_mode"].(string); m != "" {
						// The bottom's gate built its PR's update in another
						// mode, and gate builds in promote.default_mode: gating
						// again would loop. Its own promote gates it in m.
						// One step: the branches above can be promoted only once
						// that update is applied, so they are named in why, not
						// offered as a command to run now.
						why := f + "'s gate built its PR's update in promote.default_mode, which a " + m + " update can't use; promoting " + f + " on its own gates it in " + m + " mode and plans its update"
						fs := stringList(ce.Details["features"])
						if i := slices.Index(fs, f); i >= 0 && i < len(fs)-1 {
							why += "; once that plan is applied, promote " + strings.Join(fs[i+1:], " ") + " --mode " + m + " --plan"
						}
						return []contract.Next{{Command: a.scoped("promote", f, "--mode", m, "--plan"), Why: why}}
					}
					why := "its gate is missing or stale; gate it, then each branch above it, from the bottom up, then promote the stack again"
					fs := stringList(ce.Details["features"])
					if i := slices.Index(fs, f); i >= 0 && i < len(fs)-1 {
						why = "its gate is missing or stale; gate it, then the branches above it from the bottom up (" + strings.Join(fs[i+1:], ", ") +
							"), each gate on the one below, then promote the stack again"
						if ce.Details["updating"] == true {
							// Their PRs are open: one plan updates them together.
							why = "its gate is missing or stale; gate it, then the branches above it from the bottom up (" + strings.Join(fs[i+1:], ", ") +
								"), each gate on the one below, then promote again: the plan updates them all"
						}
					}
					return []contract.Next{{Command: a.scoped("gate", f), Why: why}}
				}
			case reason == "parent_stale":
				if parent != "" {
					return []contract.Next{
						{Command: a.scoped("feature", "restack", parent), Why: "the branches above " + parent + " still sit on its old commits; restack them onto its tip (a conflict stops it with git steps)"},
						{Command: a.again(false), Why: "then gate each restacked branch from the bottom (next says which), and promote again"},
					}
				}
			case reason == "open_above":
				// f is published (both refusals are raised only then): no
				// plan can update its PR while another line's PRs stay open
				// on it, so promoting any one line is refused naming the
				// others. A person closes or merges them first.
				f, _ := ce.Details["feature"].(string)
				kids := stringList(ce.Details["children"])
				if f != "" && len(kids) > 0 {
					return []contract.Next{
						{Command: a.scoped("status"), NeedsHuman: true,
							Why: f + " has open PRs stacked on it that one plan can't update together (" + strings.Join(kids, ", ") +
								"): a person closes or merges on GitHub the PRs of every line on " + f + " but the one to update, as an update of " + f + " would leave them on its old commits"},
						{Command: a.scoped("promote", f, "--plan"), Why: "then plan the update of " + f + " and the one line of open PRs left above it"},
					}
				}
			}
			// The list could be a stack in another order, or with the
			// branches it left out: promote that one.
			if want := stringList(ce.Details["want"]); len(want) > 0 {
				return []contract.Next{{Command: a.scoped(append(append([]string{"promote"}, want...), "--plan")...),
					Why: "promote the stack from its bottom, each branch right after its parent"}}
			}
		case contract.PRChangedOnGitHub:
			// Commits someone made on the public PR: never overwritten.
			// feature pull brings them in; when it can't by itself, the
			// person merges them and runs it again.
			f, _ := ce.Details["feature"].(string)
			if f == "" {
				break
			}
			switch reason, _ := ce.Details["reason"].(string); reason {
			case "unrecorded_update":
				// Ghostbranch's own update landed but was never recorded:
				// apply of its plan finds the push landed and writes the
				// record. Never feature pull, which would adopt our commits.
				if id, _ := ce.Details["plan"].(string); id != "" {
					nx := a.applyNext(id, true)
					nx.Why = "the update of " + f + "'s PR that plan " + id + " pushed was never recorded; apply finds the push landed and finishes the record (on the machine that made the plan). " + nx.Why
					return []contract.Next{nx, {Command: a.scoped("promote", f, "--plan"), Why: "then plan the next update of " + f}}
				}
			case "adopted_missing":
				// Commits feature pull adopted from GitHub that the feature
				// no longer has: a preserve update would drop them.
				branch, _ := ce.Details["branch"].(string)
				remote := a.shadowRemote
				if remote == "" {
					remote = "shadow"
				}
				pick := append([]string{"git", "cherry-pick"}, foreignSHAs(ce.Details["foreign"])...)
				return []contract.Next{
					{Command: []string{"git", "fetch", "origin", branch}, Why: "fetch the PR's branch from the public repo in your checkout, so its commits are there (origin is its remote there)"},
					{Command: []string{"git", "switch", f}, Why: "switch to the feature branch"},
					{Command: pick, Why: "bring back the commits made on GitHub that " + f + " no longer has (error.details.foreign); you resolve any conflict, Ghostbranch never does"},
					{Command: []string{"git", "push", remote, f}, Why: "push the feature branch to the shadow repo, never to origin"},
					{Command: a.scoped("gate", f), Why: "then gate it, and promote again: the update keeps their commits"},
				}
			}
			if reason, _ := ce.Details["reason"].(string); reason != "" && a.command == "feature pull" {
				branch, _ := ce.Details["branch"].(string)
				remote := a.shadowRemote
				if remote == "" {
					remote = "shadow"
				}
				return []contract.Next{
					{Command: []string{"git", "fetch", "origin", branch}, Why: "fetch the PR's branch from the public repo in your checkout (origin is its remote there)"},
					{Command: []string{"git", "switch", f}, Why: "switch to the feature branch"},
					{Command: []string{"git", "merge", "origin/" + branch}, Why: "merge the PR's commits into it; you resolve any conflict, Ghostbranch never does"},
					{Command: []string{"git", "push", remote, f}, Why: "push the merged feature branch to the shadow repo, never to origin"},
					{Command: a.scoped("feature", "pull", f), Why: "then pull again: it records the PR's commits as adopted"},
				}
			}
			return []contract.Next{
				{Command: a.scoped("feature", "pull", f), Why: "someone pushed commits to " + f + "'s public PR on GitHub (error.details.foreign); bring them into the feature branch: an update never overwrites them"},
				{Command: a.scoped("gate", f), Why: "then gate it, and promote again: the update keeps their commits"},
			}
		case contract.PlanNotFound:
			pub, _ := ce.Details["public"].(string)
			onPair := func(args ...string) []string {
				if pub != "" {
					args = append(args, "--repo", pub)
				}
				return a.gb(args...)
			}
			if f, _ := ce.Details["feature"].(string); f != "" {
				return []contract.Next{{Command: onPair("promote", f, "--plan"),
					Why: "plan files stay on the machine that made them: plan " + f + " again here (a person approves it), or apply the plan on the machine that made it"}}
			}
			if pub == "" {
				// No pair was found to look in (several pairs, none, or
				// not a checkout): status needs one too (review M2).
				return []contract.Next{{Command: a.gb("status"), NeedsInput: []string{"--repo"},
					Why: "pass --repo <owner/repo> to see that pair's features, then plan the one you meant again with promote <feature> --plan; apply <plan> --repo <owner/repo> looks the plan up in that pair's shadow repo"}}
			}
			return []contract.Next{{Command: onPair("status"), Why: "see the features, then plan the one you meant again with promote <feature> --plan"}}
		case contract.NoOpenPR:
			f, _ := ce.Details["feature"].(string)
			if f == "" {
				break
			}
			switch reason, _ := ce.Details["reason"].(string); reason {
			case "not_published":
				return []contract.Next{{Command: a.scoped("promote", f, "--plan"), Why: "nothing of " + f + " is public yet; publish it first"}}
			case "merged":
				return []contract.Next{{Command: a.scoped("sync"), Why: "sync closes the merged feature"}}
			case "closed", "branch_gone":
				return []contract.Next{{Command: a.scoped("feature", "show", f),
					Why: "a person decides: promote " + f + " again to open a new PR, or close the feature"}}
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
		// A sync whose push failed still surveyed the features: they still
		// need what they need, and a shadow branch that moved under the
		// push syncs again.
		if r, ok := a.result.(*engine.SyncResult); ok && a.command == "sync" {
			if ce.Code == contract.RefMoved {
				return a.featureNexts([]contract.Next{{Command: a.scoped("sync"), Why: "a shadow branch moved during the push; sync again"}}, r.Attention)
			}
			return a.featureNexts(nil, r.Attention)
		}
		return nil
	}
	out := a.syncNext()
	switch r := a.result.(type) {
	case *engine.FeatureStartResult:
		if r.Preview == nil {
			if first := r.GateFirst(); first != "" {
				out = append(out, contract.Next{Command: a.scoped("gate", first),
					Why: first + ", below " + r.Name + " in its stack, has no fresh green gate; gate it first (then each branch above it, bottom up), as " + r.Name + "'s gate builds on it"})
			}
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
			rd := r.Ready
			// promoteOne is the feature's own promote, on its own.
			promoteOne := func(why string) {
				mode := r.ScannedAs
				need := modeInputs(mode)
				if mode != config.ModeSquash && len(r.Commits) > 1 {
					need = append(need, "--title")
				}
				out = append(out, contract.Next{Command: a.scoped("promote", r.Feature, "--mode", mode, "--plan"), Why: why, NeedsInput: need})
			}
			switch {
			case rd.GateFirst != "":
				out = append(out, contract.Next{Command: a.scoped("gate", rd.GateFirst),
					Why: rd.GateFirst + " below has no fresh gate; gate it first, then the branches above it"})
			case len(r.Children) > 0 || len(r.Chain) > 1:
				for _, c := range rd.StaleAbove {
					out = append(out, contract.Next{Command: a.scoped("gate", c),
						Why: "the gate passed; gate " + c + " next: it is stacked on this one, and its gate builds on this gate"})
				}
				if len(rd.Levels) > 1 {
					args := append(append([]string{"promote"}, rd.Levels...), "--mode", r.ScannedAs, "--plan")
					var need []string
					if r.ScannedAs == config.ModeCurated {
						need = append(need, "--curate")
					}
					for _, f := range rd.TitleFor {
						need = append(need, "--title-for "+f+"=<title>")
					}
					out = append(out, contract.Next{Command: a.scoped(args...), NeedsInput: need,
						Why: "every gate from the bottom up to " + rd.Levels[len(rd.Levels)-1] + " is fresh; plan the publish of the stack: one PR per branch, each on the branch below, one approval for all"})
				} else if len(rd.StaleAbove) == 0 {
					// Review I1: the stack forks above this feature, and every
					// line on it is fresh. One plan publishes one line, so
					// publish this feature on its own first; next never ends
					// empty after a passing gate.
					promoteOne("the gate passed, and the stack forks above " + r.Feature + " (" + strings.Join(r.Children, ", ") +
						"); plan the publish of " + r.Feature + " on its own, then promote each line above it")
				}
			default:
				// A feature on its own, or one on a published parent.
				promoteOne("the gate passed; plan the publish")
			}
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
		case r.Applied != nil && r.Applied.Preview == nil:
			out = append(out, a.mergedThenSync(r.Applied))
		}
	case *engine.ApplyResult:
		for _, w := range a.warnings {
			// A landed update was recorded, but its typed PR texts were not
			// approved: setting them takes a new, approved plan.
			if f, _ := w.Details["feature"].(string); w.Code == contract.PRTextNotSet && f != "" {
				out = append(out, contract.Next{Command: a.scoped("promote", f, "--plan"), NeedsInput: []string{"--title", "--body"},
					Why: "the PR texts plan " + r.PlanID + " types were not set (no approval); plan them again with --title and/or --body, and have a person approve the plan"})
			}
		}
		if r.Preview == nil && r.PublicPR != 0 {
			out = append(out, a.mergedThenSync(r))
		}
	case *engine.FoldResult:
		if r.Preview != nil {
			out = append(out, contract.Next{Command: a.scoped("feature", "fold", r.Feature), Why: "the preview lists what folding changes; run it to fold"})
		} else {
			out = append(out, contract.Next{Command: a.scoped("gate", r.Parent), Why: r.Parent + " now holds " + r.Feature + "'s commits; gate it"})
		}
	case *engine.RestackResult:
		args := []string{"feature", "restack", r.Feature}
		if r.Onto != "" {
			args = append(args, "--onto", r.Onto)
		}
		switch {
		case r.Preview != nil:
			out = append(out, contract.Next{Command: a.scoped(args...), Why: "the preview lists the branches restacking rewrites; run it to restack"})
		case len(r.Restacked) > 0:
			first := cmp.Or(r.GateFirst(), r.Restacked[0].Feature)
			why := "the restacked branches were rewritten; gate them again from the bottom (and in each checkout: git fetch, then git reset --keep <remote>/<branch>)"
			if first != r.Restacked[0].Feature {
				why = first + ", below the restacked branches, has no fresh green gate, and their gates build on it: gate it first, then the restacked branches from the bottom (and in each checkout: git fetch, then git reset --keep <remote>/<branch>)"
			}
			out = append(out, contract.Next{Command: a.scoped("gate", first), Why: why})
		}
	case *engine.PullResult:
		switch {
		case r.Preview != nil:
			out = append(out, contract.Next{Command: a.scoped("feature", "pull", r.Feature), Why: "the preview lists what pulling changes; run it to pull"})
		case len(r.Adopted) > 0:
			if r.From != r.To {
				// The pull rewrote the feature branch in the shadow repo: the
				// checkout follows it, and the branches above move onto it.
				remote := cmp.Or(a.shadowRemote, "shadow")
				out = append(out,
					contract.Next{Command: []string{"git", "fetch", remote}, Why: "in your checkout: fetch the feature branch pull rewrote in the shadow repo"},
					contract.Next{Command: []string{"git", "reset", "--keep", remote + "/" + r.Feature},
						Why: "with " + r.Feature + " checked out, move it to the pulled branch (--keep refuses rather than lose uncommitted changes)"})
				if len(r.Children) > 0 {
					out = append(out, contract.Next{Command: a.scoped("feature", "restack", r.Feature),
						Why: strings.Join(r.Children, ", ") + " still sit on " + r.Feature + "'s old commits (parent_stale); restack them onto its new head (a conflict stops it with git steps)"})
				}
			}
			out = append(out, contract.Next{Command: a.scoped("gate", r.Feature),
				Why: "the PR's commits are in the feature branch now; gate it, then promote: the update keeps them"})
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

// mergedThenSync is the step after a publish or an update: a maintainer
// merges on GitHub, then sync closes what merged. A native stack merges
// whole, from its bottom PR's page (as observed, R11) or through
// merge-async on its top PR; plain chained PRs merge one by one, bottom
// first. A history worth keeping must not be squashed (F-31); a single
// PR's text is unchanged (decision 5).
func (a *app) mergedThenSync(r *engine.ApplyResult) contract.Next {
	why := "a maintainer reviews and merges PR #" + strconv.Itoa(r.PublicPR) + " on GitHub; then sync closes the feature"
	switch {
	case len(r.PRs) > 1 && r.StackNumber != nil:
		why = "A maintainer merges the stack on GitHub: on the bottom PR's page (#" + strconv.Itoa(r.PRs[0].Number) + "), the merge button merges the whole stack."
		if r.KeepsHistory {
			why += ` Choose "Create a merge commit": "Squash and merge" turns each PR into one commit and drops the history this stack was published with.`
		}
		pub := cmp.Or(r.PublicRepo, a.pairFull.Public)
		why += " From a terminal, `gh pr merge` can't merge a stack; the API route is merge-async on the top PR: `gh api -X PUT repos/" + pub + "/pulls/" +
			strconv.Itoa(r.PublicPR) + "/merge-async -f merge_method=merge`. Then sync closes the merged features and moves any branch above onto main."
	case len(r.PRs) > 1:
		how := ""
		if r.KeepsHistory {
			how = ", with a merge commit or a rebase, not a squash"
		}
		why = "a maintainer reviews and merges the PRs on GitHub from the bottom up (#" + strconv.Itoa(r.PRs[0].Number) + " first, each into main" + how +
			"); then sync closes each merged feature and moves the branches above it onto main"
	}
	return contract.Next{Command: a.scoped("sync"), Why: why}
}

// personOnly is whether this command is one only a person can finish: it
// asks for a console or an approval code (agents allow, waivers add,
// login, and apply without --yes).
func (a *app) personOnly() bool {
	switch a.command {
	case "agents allow", "waivers add", "login":
		return true
	case "apply":
		return !slices.ContainsFunc(a.args, func(arg string) bool { return arg == "--yes" || arg == "--yes=true" })
	}
	return false
}

// initNexts sets up pub on this machine: adopt its shadow repo when it
// exists (another machine set it up, or another GHOSTBRANCH_HOME), else
// create one. Both are dry runs. PAIR_NOT_FOUND and doctor's failing pair
// check (F-27) give exactly these.
func (a *app) initNexts(pub string) []contract.Next {
	return []contract.Next{
		{Command: a.gb("init", pub, "--adopt", "--plan", "--json"),
			Why: "its shadow repo already exists (set up on another machine, or in another GHOSTBRANCH_HOME): adopt it — this only registers the pair here and checks the shadow repo. If the shadow repo has a non-default name, adopt it with --shadow <owner/name> instead"},
		{Command: a.gb("init", pub, "--plan", "--json"),
			Why: "no shadow repo yet: create one"},
	}
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
	if a.startFrom != "" {
		args = append(args, "--from", a.startFrom)
	}
	if a.dryRun {
		args = append(args, "--plan")
	}
	return a.scoped(args...)
}

// startAgainWhy is the reason for starting the feature again after a sync.
func startAgainWhy(from string) string {
	if from == "" || from == feature.MainParent {
		return "then start the feature again, from the synced shadow main"
	}
	return "then start the feature again on " + from
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
// must close, the ones to gate again, and the stacks to restack (one
// restack per moved parent). Abandoned features need nothing.
func (a *app) featureNexts(out []contract.Next, rs []engine.FeatureReport) []contract.Next {
	newer, intoParent := a.newerRecords(), a.mergedIntoParent()
	var merged []string
	var rest []contract.Next
	restacks := map[string]bool{}
	for _, r := range rs {
		closeCmd := a.scoped("feature", "close", r.Name)
		if newer[r.Name] && (r.State == feature.StateMerged || (r.State == feature.StateBranchGone && r.PRState == "merged")) {
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its PR merged, but a newer Ghostbranch wrote its record, so this binary won't close it unattended: upgrade ghostbranch on this machine and sync, or a person closes it at the console"})
			continue
		}
		if base, ok := intoParent[r.Name]; ok {
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its PR merged into " + base + "'s public branch, not main, so sync left it open: a person checks that branch holds its changes, then closes it"})
			continue
		}
		if engine.ClosableWithoutConsole(r) {
			merged = append(merged, r.Name)
			continue
		}
		if r.PR != nil && r.PR.Foreign > 0 {
			rest = append(rest, contract.Next{Command: a.scoped("feature", "pull", r.Name),
				Why: "someone pushed commits to its public PR on GitHub; bring them into the feature branch (an update never overwrites them), then gate and promote"})
			continue
		}
		switch r.State {
		case feature.StateUnreadable:
			rest = append(rest, contract.Next{Command: closeCmd, NeedsHuman: true,
				Why: "its record couldn't be read; run feature show " + r.Name + " for the error: run the command again if it was a passing failure, and if the record is corrupt a person closes it at the console"})
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
		case feature.StateParentStale:
			// Several branches on one moved parent share its restack.
			cmd := a.scoped("feature", "restack", r.Parent)
			why := "the branch below it, " + r.Parent + ", moved since it was started or restacked: restack the branches above " + r.Parent + ", then gate them again"
			if r.Parent == feature.MainParent {
				cmd = a.scoped("feature", "restack", r.Name, "--onto", "main")
				why = "the branch it stood on merged: move its own commits onto main, then gate it again"
			}
			if k := shellJoin(cmd); !restacks[k] {
				restacks[k] = true
				rest = append(rest, contract.Next{Command: cmd, Why: why})
			}
		case feature.StatePublicPRNeedsUpdate:
			rest = append(rest, contract.Next{Command: a.scoped("feature", "restack", r.Name, "--onto", "main"),
				Why: "the PR below it merged by squash or rebase, and its open public PR still carries that PR's old commits: restack it onto main, gate it, then promote it (the update replaces its PR's branch)"})
		case feature.StatePRBehind:
			switch {
			case r.Gate != nil && r.Gate.Job != nil:
				rest = append(rest, contract.Next{Command: a.scoped("wait", *r.Gate.Job),
					Why: "its gate is waiting on CI; finish it, then promote: the update goes to its open PR"})
			case r.Gate != nil && r.Gate.Fresh && r.Gate.Checks == feature.ChecksPassed && r.Gate.Leak == feature.LeakClean:
				rest = append(rest, contract.Next{Command: a.scoped("promote", r.Name, "--plan"),
					Why: "it has commits its open PR doesn't, and a green gate: plan the update of its PR (new commits are added; a changed history replaces the branch, leased)"})
			default:
				rest = append(rest, contract.Next{Command: a.scoped("gate", r.Name),
					Why: "it has commits its open PR doesn't; gate them, then promote to update the PR"})
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

// mergedIntoParent maps each feature sync left open because its PR merged
// into the public branch below it, not main, to that branch: the
// FEATURE_NOT_CLOSED warnings with reason merged_into_parent_branch.
func (a *app) mergedIntoParent() map[string]string {
	out := map[string]string{}
	for _, w := range a.warnings {
		f, _ := w.Details["feature"].(string)
		if w.Code == contract.FeatureNotClosed && w.Details["reason"] == "merged_into_parent_branch" && f != "" {
			base, _ := w.Details["base"].(string)
			out[f] = base
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
	"--limit": true, "--from": true, "--onto": true, "--mode-for": true, "--title-for": true,
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

// rebaseNexts are the git steps that rebase branch onto onto by hand in the
// person's checkout, then the ghostbranch command that carries on. The
// remote is this checkout's remote for the shadow repo, "shadow" when
// unknown, as feature start's checkout steps name it.
func (a *app) rebaseNexts(branch, onto, upstream string, then []string, thenWhy string) []contract.Next {
	remote := a.shadowRemote
	if remote == "" {
		remote = "shadow"
	}
	whys := []string{
		"fetch the shadow repo in your checkout (" + remote + " is its remote there)",
		"switch to " + branch,
		"rebase " + branch + "'s own commits onto " + onto + "; you resolve any conflict, Ghostbranch never does",
		"push the rebased branch, leased so it never overwrites someone else's push",
	}
	var out []contract.Next
	for i, s := range engine.RebaseSteps(remote, branch, onto, upstream) {
		out = append(out, contract.Next{Command: s, Why: whys[i]})
	}
	return append(out, contract.Next{Command: then, Why: thenWhy})
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

// stringList reads a details value that is a list of strings, as the
// engine sets it ([]string) or as JSON decodes it ([]any).
func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// foreignSHAs is the sha of each commit in a PUBLIC_BRANCH_CHANGED's
// details.foreign list ({sha, author, subject}), in order.
func foreignSHAs(v any) []string {
	var out []string
	add := func(m map[string]any) {
		if s, ok := m["sha"].(string); ok && s != "" {
			out = append(out, s)
		}
	}
	switch l := v.(type) {
	case []map[string]any:
		for _, m := range l {
			add(m)
		}
	case []any:
		for _, x := range l {
			if m, ok := x.(map[string]any); ok {
				add(m)
			}
		}
	}
	return out
}
