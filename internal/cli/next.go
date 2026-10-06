package cli

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
)

// gb is a ghostbranch argv, with --workspace when this run used one.
func (a *app) gb(args ...string) []string {
	out := append([]string{"ghostbranch"}, args...)
	if a.workspace != "" {
		out = append(out, "--workspace", a.workspace)
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

// again is this run's own argv, plus extra flags.
func (a *app) again(extra ...string) []string {
	return append(append([]string{"ghostbranch"}, a.args...), extra...)
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
			case reason == "grant_needs_yes":
				return []contract.Next{{Command: a.again("--yes"), Why: "a person allowed agents to publish here; repeat with --yes"}}
			case planID != "":
				return []contract.Next{a.applyNext(planID, false)}
			default:
				return []contract.Next{{Command: a.again(), NeedsHuman: true,
					Why: "a person must run this in their own terminal and type the code it shows"}}
			}
		case contract.PlanStale:
			f, _ := ce.Details["feature"].(string)
			pub, _ := ce.Details["public"].(string)
			if f != "" && pub != "" {
				return []contract.Next{{Command: a.gb("promote", f, "--mode", "squash", "--plan", "--repo", pub),
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
	return []contract.Next{{Command: []string{"ghostbranch", "guard", "install"}, NeedsHuman: true,
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
