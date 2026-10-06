package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type ApplyOptions struct {
	Plan string // plan ID or path to a plan .json
	Yes  bool
}

type ApplyResult struct {
	PlanID       string           `json:"plan_id"`
	PublicBranch string           `json:"public_branch"`
	PublicPR     int              `json:"public_pr"`
	PRURL        string           `json:"pr_url"`
	Resumed      bool             `json:"resumed"`
	Approval     *ledger.Approval `json:"approval"`
	Preview      *preview.Preview `json:"preview"`
}

// Apply publishes a saved plan if the repos still match it. Every step is
// safe to re-run, so an interrupted apply finishes on the next run.
func (e *Engine) Apply(ctx context.Context, opt ApplyOptions) (res *ApplyResult, warns []contract.Warning, err error) {
	var pair *workspace.Pair
	var pl plan.Plan
	defer func() {
		summary := ""
		if res != nil && err == nil {
			summary = fmt.Sprintf("published %s as PR #%d", res.PublicBranch, res.PublicPR)
		}
		e.recordWith("apply", pair, pl.Feature, pl.ID, summary, err, func(ent *ledger.Entry) {
			if res != nil {
				ent.Approval = res.Approval
			}
		})
	}()
	if pl, _, err = plan.Load(e.WS.PlansDir(), opt.Plan); err != nil {
		return nil, nil, err
	}
	// The plan file is local and unsigned: re-check where it would publish.
	if err := feature.ValidateName(pl.Publish.TargetBranch, "plan"); err != nil {
		return nil, nil, err
	}
	p, ok := e.WS.Find(pl.Pair.Public)
	if !ok {
		return nil, nil, contract.New(contract.PairAmbiguous, "plan %s is for %s, which is not in workspace %q", pl.ID, pl.Pair.Public, e.WS.Name).WithField("plan")
	}
	pair = &p
	pc, err := e.open(ctx, "apply", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	return e.apply(ctx, pc, pl, opt.Yes)
}

// stale is the PLAN_STALE details: what moved, plus the feature and public
// repo so the re-plan command can be named exactly.
func stale(pl plan.Plan, moved []string) map[string]any {
	return map[string]any{"moved": moved, "feature": pl.Feature, "public": pl.Pair.Public}
}

func (e *Engine) apply(ctx context.Context, pc *pairCtx, pl plan.Plan, yes bool) (*ApplyResult, []contract.Warning, error) {
	g := pc.git
	if def := pc.public.DefaultBranch; def != "" && strings.EqualFold(pl.Publish.TargetBranch, def) {
		return nil, nil, contract.New(contract.NameInvalid, "plan %s would publish to the public default branch %q", pl.ID, def).WithField("plan")
	}
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	m, metaSHA, err := e.readMeta(ctx, pc, pl.Feature)
	if err != nil {
		return nil, warns, err
	}
	head, _, _ := g.Resolve(ctx, shadowRef(m.Branch))
	pm, err := g.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read public main")
	}
	gateC, _, _ := g.Resolve(ctx, shadowRef(gateBranch(pl.Feature)))
	live := plan.Fingerprint{PublicMain: pm, FeatureHead: head, Overlay: O, GateCandidate: gateC}
	if moved := pl.Fingerprint.Moved(live); len(moved) > 0 {
		return nil, warns, contract.New(contract.PlanStale,
			"the repos changed since plan %s was made (%s); make a new plan with ghostbranch promote", pl.ID, strings.Join(moved, ", ")).
			WithDetails(stale(pl, moved))
	}
	// The plan only stands while the recorded gate is green, fresh, and for
	// the very candidate the plan was made from.
	if !m.Gate.Passed() || m.Gate.Candidate != gateC || !m.Gate.FreshFor(head, pm, O) {
		return nil, warns, contract.New(contract.PlanStale,
			"the recorded gate for %q is no longer green for plan %s's candidate; run ghostbranch gate, then make a new plan", pl.Feature, pl.ID).
			WithDetails(stale(pl, []string{"gate"}))
	}
	P, ok, _ := g.Resolve(ctx, planRef(pl.ID))
	if !ok || P != pl.Publish.Commit {
		return nil, warns, contract.New(contract.PlanStale, "plan %s's commit is missing from the shadow repo", pl.ID).
			WithDetails(stale(pl, []string{"publish.commit"}))
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	if err := assertPublish(ctx, g, P, pl.Publish.Tree, pl.Fingerprint.GateCandidate, pm, set); err != nil {
		return nil, warns, err
	}
	entries, err := g.ListTree(ctx, P)
	if err != nil {
		return nil, warns, internalErr(err, "list the published tree")
	}
	msg, err := g.CommitMessage(ctx, P)
	if err != nil {
		return nil, warns, internalErr(err, "read the commit message")
	}
	findings, _, err := e.scan(ctx, g, set, entries, publishTexts(msg, pl.Publish.Author.Name,
		pl.Publish.Author.Email, pl.Publish.TargetBranch, pl.Publish.PR.Title, pl.Publish.PR.Body))
	if err != nil {
		return nil, warns, err
	}
	if len(findings) > 0 {
		return nil, warns, leakErr(findings)
	}

	branch := pl.Publish.TargetBranch
	res := &ApplyResult{PlanID: pl.ID, PublicBranch: branch}
	existing, exists, _ := g.Resolve(ctx, publicRef(branch))
	if exists && existing != P {
		return res, warns, contract.New(contract.BranchExists,
			"public branch %q already exists at another commit; promote under another name with --branch", branch).
			WithField("branch").WithDetails(map[string]any{"branch": branch, "sha": existing})
	}
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O, "gate_candidate": gateC})
		if !exists {
			pv.Add(preview.PushRef(pc.pair.Public, "refs/heads/"+branch, "", P))
		}
		pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Public, branch, "main")
		if err != nil {
			return res, warns, forgeErr(err, "find the public PR")
		}
		if pr == nil {
			pv.Add(preview.OpenPR(pc.pair.Public, branch, "main", false))
		}
		res.Preview = pv
		return res, warns, nil
	}
	files, err := g.DiffNames(ctx, pl.Publish.Parent, P)
	if err != nil {
		return nil, warns, internalErr(err, "list the files going public")
	}
	if res.Approval, err = e.authorize(ctx, pc.pair, pl.ID, planSummary(pl, files), yes); err != nil {
		return nil, warns, err
	}
	if !exists {
		e.progressf("publishing %s to %s:%s", short(P), pc.pair.Public, branch)
		opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + branch}}}
		if err := e.push(ctx, pc, remotePublic, opt, P+":refs/heads/"+branch); err != nil {
			if contract.From(err).Code == contract.RefMoved {
				return res, warns, contract.New(contract.BranchExists, "public branch %q appeared while publishing", branch).WithField("branch")
			}
			return res, warns, err
		}
	} else {
		res.Resumed = true
	}
	pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Public, branch, "main")
	if err != nil {
		return res, warns, forgeErr(err, "find the public PR")
	}
	if pr == nil {
		created, err := e.Forge.CreatePR(ctx, pc.pair.Public, forge.NewPR{Title: pl.Publish.PR.Title, Body: pl.Publish.PR.Body, Head: branch, Base: "main"})
		if err != nil {
			return res, warns, forgeErr(err, "open the public PR")
		}
		pr = &created
	} else {
		res.Resumed = true
	}
	res.PublicPR, res.PRURL = pr.Number, pr.URL
	m.Promoted = &feature.Promoted{PublicPR: pr.Number, PlanID: pl.ID}
	if err := e.updateMeta(ctx, pc, m, metaSHA); err != nil {
		return res, warns, err
	}
	return res, warns, nil
}
