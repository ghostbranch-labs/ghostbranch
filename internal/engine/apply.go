package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type ApplyOptions struct {
	Plan string // plan ID or path to a plan .json
	Yes  bool
}

type ApplyResult struct {
	PlanID       string `json:"plan_id"`
	PublicBranch string `json:"public_branch"`
	PublicPR     int    `json:"public_pr"`
	PRURL        string `json:"pr_url"`
	Resumed      bool   `json:"resumed"`
	DryRun       bool   `json:"dry_run"`
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
		e.record("apply", pair, pl.Feature, pl.ID, summary, err)
	}()
	if pl, _, err = plan.Load(e.WS.PlansDir(), opt.Plan); err != nil {
		return nil, nil, err
	}
	p, ok := e.WS.Find(pl.Pair.Public)
	if !ok {
		return nil, nil, contract.New(contract.PairAmbiguous, "plan %s is for %s, which is not in workspace %q", pl.ID, pl.Pair.Public, e.WS.Name).WithField("plan")
	}
	pair = &p
	if pl.RequiresHuman && IsAgent(e.Actor) {
		return nil, nil, e.approve(pl, nil, opt.Yes)
	}
	pc, err := e.open(ctx, "apply", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	files, err := pc.git.DiffNames(ctx, pl.Publish.Parent, pl.Publish.Commit)
	if err != nil {
		return nil, nil, internalErr(err, "list the files going public")
	}
	if err := e.approve(pl, files, opt.Yes); err != nil {
		return nil, nil, err
	}
	return e.apply(ctx, pc, pl)
}

func (e *Engine) apply(ctx context.Context, pc *pairCtx, pl plan.Plan) (*ApplyResult, []contract.Warning, error) {
	g := pc.git
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
			WithDetails(map[string]any{"moved": moved})
	}
	// The plan only stands while the recorded gate is green, fresh, and for
	// the very candidate the plan was made from.
	if !m.Gate.Passed() || m.Gate.Candidate != gateC || !m.Gate.FreshFor(head, pm, O) {
		return nil, warns, contract.New(contract.PlanStale,
			"the recorded gate for %q is no longer green for plan %s's candidate; run ghostbranch gate, then make a new plan", pl.Feature, pl.ID).
			WithDetails(map[string]any{"moved": []string{"gate"}})
	}
	P, ok, _ := g.Resolve(ctx, planRef(pl.ID))
	if !ok || P != pl.Publish.Commit {
		return nil, warns, contract.New(contract.PlanStale, "plan %s's commit is missing from the shadow repo", pl.ID).
			WithDetails(map[string]any{"moved": []string{"publish.commit"}})
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	if err := assertPublish(ctx, g, P, pl.Publish.Tree, pl.Fingerprint.GateCandidate, pm, set); err != nil {
		return nil, warns, err
	}
	entries, err := g.ListTree(ctx, P)
	if err != nil {
		return nil, warns, internalErr(err, "list the published tree")
	}
	msg, err := g.Run(ctx, "log", "-1", "--format=%B", P)
	if err != nil {
		return nil, warns, internalErr(err, "read the commit message")
	}
	findings, _, err := e.scan(ctx, g, set, entries, publishTexts(strings.TrimSpace(msg), pl.Publish.Author.Name,
		pl.Publish.Author.Email, pl.Publish.TargetBranch, pl.Publish.PR.Title, pl.Publish.PR.Body))
	if err != nil {
		return nil, warns, err
	}
	if len(findings) > 0 {
		return nil, warns, leakErr(findings)
	}

	branch := pl.Publish.TargetBranch
	res := &ApplyResult{PlanID: pl.ID, PublicBranch: branch, DryRun: e.DryRun}
	if e.DryRun {
		return res, warns, nil
	}
	existing, exists, _ := g.Resolve(ctx, publicRef(branch))
	switch {
	case !exists:
		e.progressf("publishing %s to %s:%s", short(P), pc.pair.Public, branch)
		opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + branch}}}
		if err := e.push(ctx, pc, remotePublic, opt, P+":refs/heads/"+branch); err != nil {
			if contract.From(err).Code == contract.RefMoved {
				return res, warns, contract.New(contract.BranchExists, "public branch %q appeared while publishing", branch).WithField("branch")
			}
			return res, warns, err
		}
	case existing == P:
		res.Resumed = true
	default:
		return res, warns, contract.New(contract.BranchExists,
			"public branch %q already exists at another commit; promote under another name with --branch", branch).
			WithField("branch").WithDetails(map[string]any{"branch": branch, "sha": existing})
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
