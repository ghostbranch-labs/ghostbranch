package engine

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
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
	Waived       []WaivedFinding  `json:"waived"`
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
				ent.Waivers = findingIDs(res.Waived)
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

// stale is the PLAN_STALE details: what moved, plus the feature, public
// repo, public branch, mode and PR texts so the re-plan command can be
// named exactly.
func stale(pl plan.Plan, moved []string) map[string]any {
	return map[string]any{"moved": moved, "feature": pl.Feature, "public": pl.Pair.Public, "branch": pl.Publish.TargetBranch,
		"mode": pl.Mode, "title": pl.Publish.PR.Title, "body": pl.Publish.PR.Body}
}

// branchExists refuses a publish to a public branch that is already at
// another commit.
func branchExists(branch, sha string) error {
	return contract.New(contract.BranchExists,
		"public branch %q already exists at another commit; promote under another name with --branch", branch).
		WithField("branch").WithDetails(map[string]any{"branch": branch, "sha": sha})
}

// previewPublish adds the public steps a publish of P to branch still
// needs: the push unless the branch is already at P, and the PR unless one
// is open. A branch at another commit is refused, as the publish would be.
func (e *Engine) previewPublish(ctx context.Context, pc *pairCtx, pv *preview.Preview, branch, P string) error {
	existing, exists, _ := pc.git.Resolve(ctx, publicRef(branch))
	if exists && existing != P {
		return branchExists(branch, existing)
	}
	if !exists {
		pv.Add(preview.PushRef(pc.pair.Public, "refs/heads/"+branch, "", P))
	}
	pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Public, branch, "main")
	if err != nil {
		return forgeErr(err, "find the public PR")
	}
	if pr == nil {
		pv.Add(preview.OpenPR(pc.pair.Public, branch, "main", false))
	}
	return nil
}

func (e *Engine) apply(ctx context.Context, pc *pairCtx, pl plan.Plan, yes bool) (*ApplyResult, []contract.Warning, error) {
	g := pc.git
	if pl.SchemaVersion != plan.SchemaVersion {
		return nil, nil, contract.New(contract.PlanStale,
			"plan %s was made by an older Ghostbranch (schema %s); make a new plan with ghostbranch promote", pl.ID, pl.SchemaVersion).
			WithDetails(stale(pl, []string{"schema_version"}))
	}
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
	// the very candidate the plan was made from, and records the chain it
	// rebased (a gate from an older binary does not).
	gateStale := contract.New(contract.PlanStale,
		"the recorded gate for %q is no longer green for plan %s's candidate; run ghostbranch gate, then make a new plan", pl.Feature, pl.ID).
		WithDetails(stale(pl, []string{"gate"}))
	if !m.Gate.Passed() || m.Gate.Commits == nil || m.Gate.Candidate != gateC || !m.Gate.FreshFor(head, pm, O) {
		return nil, warns, gateStale
	}
	// The recorded chain must run from public main to the candidate's
	// parent, one parent at a time.
	prev := pm
	for _, c := range m.Gate.Commits {
		if parent, err := g.MustResolve(ctx, c.Rebased+"^"); err != nil || parent != prev {
			return nil, warns, gateStale
		}
		prev = c.Rebased
	}
	if R, err := g.MustResolve(ctx, gateC+"^"); err != nil || R != prev {
		return nil, warns, gateStale
	}
	P, ok, _ := g.Resolve(ctx, planRef(pl.ID))
	if !ok || P != pl.Publish.Commit {
		return nil, warns, contract.New(contract.PlanStale, "plan %s's commit is missing from the shadow repo", pl.ID).
			WithDetails(stale(pl, []string{"publish.commit"}))
	}
	// P matching C proves nothing if C itself kept internal sections (a plan
	// from a binary that did not strip, or a forged one): derive the
	// published tree again from the candidate's parent.
	T2, tEntries, err := publishedTree(ctx, g, gateC+"^", cfg)
	if err != nil {
		if ce := contract.From(err); ce.Code == contract.MarkerInvalid {
			ce.Details["feature"] = pl.Feature
		}
		return nil, warns, err
	}
	if T2 != pl.Publish.Tree {
		return nil, warns, contract.New(contract.PlanStale,
			"plan %s was not built with this version's strip of internal sections; make a new plan with ghostbranch promote", pl.ID).
			WithDetails(stale(pl, []string{"publish.tree"}))
	}
	// The plan file is local and unsigned: derive the chain again from the
	// shadow repo's objects, and refuse a plan that names any other.
	links, dropped, err := deriveChain(ctx, g, cfg, chainInput{mode: pl.Mode, pm: pm, T: pl.Publish.Tree, tEntries: tEntries,
		commits: m.Gate.Commits, groups: pl.Curate}, nil)
	if err != nil {
		return nil, warns, err
	}
	chainStale := contract.New(contract.PlanStale,
		"plan %s's public commits are not the ones its feature's history gives; make a new plan with ghostbranch promote", pl.ID).
		WithDetails(stale(pl, []string{"publish.commits"}))
	commits := pl.Publish.Commits
	if len(links) != len(commits) || !slices.Equal(dropped, pl.Publish.DroppedEmpty) {
		return nil, warns, chainStale
	}
	for i, l := range links {
		if l.tree != commits[i].Tree || !reflect.DeepEqual(l.sources, commits[i].Sources) {
			return nil, warns, chainStale
		}
	}
	// The plan ref is the chain's tip: walk back from it.
	at := P
	for i := len(commits) - 1; i >= 0; i-- {
		if at != commits[i].Commit {
			return nil, warns, chainStale
		}
		if i > 0 {
			if at, err = g.MustResolve(ctx, at+"^"); err != nil {
				return nil, warns, chainStale
			}
		}
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	if err := assertChain(ctx, g, commits, pl.Publish.Tree, gateC, pm, set, guardNames(pc)); err != nil {
		return nil, warns, err
	}
	// Scan what is stored in the commits, not what the plan file says.
	steps := make([]scanStep, len(commits))
	authors := make([]string, len(commits))
	shown := append([]plan.Commit(nil), commits...)
	for i, c := range commits {
		msg, err := g.CommitMessage(ctx, c.Commit)
		if err != nil {
			return nil, warns, internalErr(err, "read a public commit's message")
		}
		author, committer, err := g.CommitIdentities(ctx, c.Commit)
		if err != nil {
			return nil, warns, internalErr(err, "read a public commit's author and committer")
		}
		var sources []string
		if pl.Mode != config.ModeSquash {
			sources = links[i].sources
		}
		steps[i] = scanStep{sources: sources, entries: links[i].entries, texts: []leak.Text{
			{Where: "commit_message", Content: msg},
			{Where: "author", Content: author},
			{Where: "committer", Content: committer}}}
		authors[i] = author
		shown[i].Subject = subject(msg)
	}
	rep, _, err := e.scanChain(ctx, g, cfg, pm, steps, []leak.Text{
		{Where: "branch", Content: pl.Publish.TargetBranch}, {Where: "pr_title", Content: pl.Publish.PR.Title},
		{Where: "pr_body", Content: pl.Publish.PR.Body}})
	if err != nil {
		return nil, warns, err
	}
	blocking, waived, ww := e.splitFindings(pc.pair, rep.Findings)
	warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
	if len(blocking) > 0 {
		ce := contract.From(leakErr(pl.Feature, blocking))
		ce.Details["mode"], ce.Details["public"], ce.Details["branch"] = pl.Mode, pl.Pair.Public, pl.Publish.TargetBranch
		return nil, warns, ce
	}

	branch := pl.Publish.TargetBranch
	res := &ApplyResult{PlanID: pl.ID, PublicBranch: branch, Waived: waived}
	existing, exists, _ := g.Resolve(ctx, publicRef(branch))
	if exists && existing != P {
		return res, warns, branchExists(branch, existing)
	}
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O, "gate_candidate": gateC})
		if err := e.previewPublish(ctx, pc, pv, branch, P); err != nil {
			return res, warns, err
		}
		res.Preview = pv
		return res, warns, nil
	}
	files, err := g.DiffNames(ctx, pl.Publish.Parent, P)
	if err != nil {
		return nil, warns, internalErr(err, "list the files going public")
	}
	summary := pl
	summary.Publish.Commits = shown // subjects as stored in the commits
	if res.Approval, err = e.authorize(ctx, pc.pair, pl.ID, planSummary(summary, files, authors, waived, rep.Warnings), yes); err != nil {
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
	m.SchemaVersion = feature.SchemaVersion
	m.Promoted = &feature.Promoted{PublicPR: pr.Number, PlanID: pl.ID}
	if err := e.updateMeta(ctx, pc, m, metaSHA); err != nil {
		return res, warns, err
	}
	return res, warns, nil
}
