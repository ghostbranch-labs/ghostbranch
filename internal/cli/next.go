package cli

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
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
// hides the console they need, and --json: they run it in their own
// terminal.
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
	if ws := a.usedWorkspace(); !hasWS && ws != "default" {
		out = append(out, "--workspace", ws)
	}
	return out
}

// next is spec section 9.3: what to run now, exactly as given. Flags the
// caller must supply are in NeedsInput; nothing here is ever a placeholder.
func (a *app) next(err error) []contract.Next {
	if ce := contract.From(err); ce != nil {
		switch ce.Code {
		case contract.ApprovalRequired:
			reason, _ := ce.Details["reason"].(string)
			planID, _ := ce.Details["plan_id"].(string)
			switch {
			case reason == "grant_needs_yes" && planID != "":
				// The plan is saved: publish it rather than build it again.
				return []contract.Next{{Command: a.gb("apply", planID, "--yes"), Why: "a person allowed agents to publish here; publish the saved plan with --yes"}}
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
		case contract.PlanStale:
			f, _ := ce.Details["feature"].(string)
			pub, _ := ce.Details["public"].(string)
			branch, _ := ce.Details["branch"].(string)
			if f != "" && pub != "" {
				args := []string{"promote", f, "--mode", "squash", "--plan", "--repo", pub}
				if branch != "" && branch != f {
					args = append(args, "--branch", branch)
				}
				return []contract.Next{{Command: a.gb(args...),
					Why: "the repos moved since the plan was made; make a new plan", NeedsInput: []string{"--message"}}}
			}
		}
		return nil
	}
	var out []contract.Next
	switch r := a.result.(type) {
	case *engine.FeatureStartResult:
		if r.Preview == nil {
			out = append(out, contract.Next{Command: a.scoped("gate", r.Name), Why: "push work to the feature branch in the shadow repo, then gate it"})
		}
	case *engine.GateResult:
		if r.Preview == nil && r.Checks == feature.ChecksPassed && r.Leak == feature.LeakClean {
			out = append(out, contract.Next{Command: a.scoped("promote", r.Feature, "--mode", "squash", "--plan"),
				Why: "the gate passed; plan the publish", NeedsInput: []string{"--message"}})
		}
	case *engine.PromoteResult:
		if r.Preview == nil && r.Applied == nil && r.Plan != nil && r.PlanPath != "" {
			out = append(out, a.applyNext(r.Plan.ID, true))
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
	return out
}

// applyNext publishes plan id: under a grant when one is in force (and
// checkGrant is set), otherwise by a person at a console.
func (a *app) applyNext(id string, checkGrant bool) contract.Next {
	if checkGrant && a.eng != nil && a.eng.Grants != nil && a.pairFull.Public != "" {
		if _, ok, err := a.eng.Grants.Valid(a.pairFull.Public, a.pairFull.Shadow, a.env.Now()); err == nil && ok {
			return contract.Next{Command: a.gb("apply", id, "--yes"), Why: "a person allowed agents to publish here; publish the plan"}
		}
	}
	return contract.Next{Command: a.gb("apply", id), NeedsHuman: true,
		Why: "publish the plan: a person runs this in their own terminal and types the code it shows (or allows agents first with ghostbranch agents allow)"}
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

// textWheres are the findings in text promote publishes rather than in the
// tree: fixed by new text, never on the feature branch.
var textWheres = map[string]bool{"commit_message": true, "author": true, "committer": true, "branch": true, "pr_title": true, "pr_body": true}

func typedTextLeaks(ce *contract.Error) bool {
	fs, _ := ce.Details["findings"].([]leak.Finding)
	for _, f := range fs {
		if textWheres[f.Where] {
			return true
		}
	}
	return false
}

// withoutTexts drops -m/--message, --title and --body and their values, so
// the caller supplies new text rather than repeat the leak.
func withoutTexts(argv []string) []string {
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "-m" || arg == "--message" || arg == "--title" || arg == "--body":
			i++ // and its value
		case strings.HasPrefix(arg, "--message=") || strings.HasPrefix(arg, "--title=") || strings.HasPrefix(arg, "--body="),
			strings.HasPrefix(arg, "-m") && !strings.HasPrefix(arg, "--"):
		default:
			out = append(out, arg)
		}
	}
	return out
}

// leakNext is what to do about LEAK_FOUND or MARKER_INVALID: fix the
// content and run again; and, for waivable findings a person judges false
// positives, that person waives them at their console.
func (a *app) leakNext(ce *contract.Error) []contract.Next {
	f, _ := ce.Details["feature"].(string)
	var out []contract.Next
	switch a.command {
	case "promote":
		if typedTextLeaks(ce) {
			out = append(out, contract.Next{Command: withoutTexts(a.again(false)), NeedsInput: []string{"--message"},
				Why: "a leak is in text this promote publishes: run it again with a new commit message, title or body (and fix any file findings on the feature branch in the shadow repo first)"})
			break
		}
		out = append(out, contract.Next{Command: a.again(false), Why: "after fixing the content on the feature branch in the shadow repo, run this again"})
	case "gate", "waivers add":
		if f != "" {
			out = append(out, contract.Next{Command: a.scoped("gate", f), Why: "after fixing the content on the feature branch in the shadow repo, gate again"})
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
