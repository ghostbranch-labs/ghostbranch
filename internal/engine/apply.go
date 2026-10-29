package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type ApplyOptions struct {
	Plan string // plan ID or path to a plan .json
	Yes  bool
	// Archive hands the published features off after the publish (6.5):
	// the person picks Archive or Keep at the console, or, without one,
	// Archive applies. false keeps them without asking (R3).
	Archive bool
	// Lookup is the pair to look in when the plan file isn't on this
	// machine (--repo or the checkout's; nil when none resolves). A plan
	// that is here names its own pair.
	Lookup *workspace.Pair
	// LookupErr is why a --repo that was passed named no pair here. A plan
	// this machine lacks then fails with it (PAIR_NOT_FOUND, say), so the
	// ledger and the caller see the same code (re-review N4).
	LookupErr error
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
	// PRs is every PR the plan opened, bottom first (one for a plan of one
	// feature); StackNumber is the native GitHub stack, null when none.
	// PublicBranch, PublicPR and PRURL are the top PR's.
	PRs         []plan.StackPR `json:"prs"`
	StackNumber *int           `json:"stack_number"`
	// KeepsHistory: some PR publishes more than one commit in preserve or
	// curated mode, which "Squash and merge" would flatten (F-31).
	// PublicRepo is the plan's public repo. Both are for next.
	KeepsHistory bool   `json:"-"`
	PublicRepo   string `json:"-"`
	// AfterPublish is what happened to each published feature's shadow
	// branch: archived, kept, or incomplete (ARCHIVE_INCOMPLETE). Never
	// null. Archived lists the features archived, bottom first.
	AfterPublish map[string]string `json:"after_publish"`
	Archived     []string          `json:"-"`
	// Landed: the plan's updates had already landed, so this run only
	// recorded them and archived nothing (finishLanded, R14), though the
	// run that died may have been asked to. For next.
	Landed bool `json:"-"`
	// handOff are the features to archive once the publish succeeded;
	// moved is every shadow ref the archives created or deleted (ledger).
	handOff []string
	moved   map[string]string
}

// applyOpts are how apply runs: yes is --yes, archive is ApplyOptions.Archive.
type applyOpts struct{ yes, archive bool }

// Apply publishes a saved plan if the repos still match it. Every step is
// safe to re-run, so an interrupted apply finishes on the next run.
func (e *Engine) Apply(ctx context.Context, opt ApplyOptions) (res *ApplyResult, warns []contract.Warning, err error) {
	var pair *workspace.Pair
	var pl plan.Plan
	defer func() {
		summary := ""
		if res != nil && err == nil {
			summary = publishSummary(res) + archivedSummary(res)
		}
		e.recordWith("apply", pair, pl.Feature, pl.ID, summary, err, func(ent *ledger.Entry) {
			if res != nil {
				ent.Approval = res.Approval
				ent.Waivers = findingIDs(res.Waived)
			}
			if res != nil && len(res.moved) > 0 {
				ent.Refs = res.moved
			}
		})
	}()
	if pl, _, err = plan.Load(e.WS.PlansDir(), opt.Plan); err != nil {
		if errors.Is(err, plan.ErrNotFound) && plan.ValidID(opt.Plan) {
			if opt.Lookup == nil && opt.LookupErr != nil {
				return nil, nil, opt.LookupErr
			}
			return nil, nil, e.planNotHere(ctx, opt.Plan, opt.Lookup)
		}
		return nil, nil, err
	}
	// The plan file is local and unsigned: re-check where it would publish.
	for _, target := range planTargets(pl) {
		if err := feature.ValidateName(target, "plan", "main"); err != nil {
			return nil, nil, err
		}
	}
	p, ok := e.WS.Find(pl.Pair.Public)
	if !ok {
		return nil, nil, contract.New(contract.PairNotFound, "plan %s is for %s, which isn't set up in workspace %q", pl.ID, pl.Pair.Public, e.WS.Name).
			WithField("plan").WithDetails(map[string]any{"public": pl.Pair.Public})
	}
	pair = &p
	pc, err := e.open(ctx, "apply", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	// Again with the pair's base branch, now it is known: GitHub's default
	// may be another branch (§ 4.6), so the default-branch check in
	// applyPlan doesn't cover it.
	for _, target := range planTargets(pl) {
		if err := feature.ValidateName(target, "plan", pc.base); err != nil {
			return nil, nil, err
		}
	}
	return e.apply(ctx, pc, pl, applyOpts{yes: opt.Yes, archive: opt.Archive})
}

// planTargets is every public branch a plan would publish to: its own
// target, then each chain entry's.
func planTargets(pl plan.Plan) []string {
	out := []string{pl.Publish.TargetBranch}
	for _, en := range pl.Chain {
		out = append(out, en.Publish.TargetBranch)
	}
	return out
}

// archivedSummary is the ledger's note of the features an apply archived.
func archivedSummary(res *ApplyResult) string {
	if res == nil || len(res.Archived) == 0 {
		return ""
	}
	return "; archived " + strings.Join(res.Archived, ", ")
}

// stale is the PLAN_STALE details: what moved, plus the feature, public
// repo, public branch and mode, and the PR texts the person typed, so the
// re-plan command can be named exactly and derives the other texts again.
func stale(pl plan.Plan, moved []string) map[string]any {
	d := map[string]any{"moved": moved, "feature": pl.Feature, "public": pl.Pair.Public, "branch": pl.Publish.TargetBranch, "mode": pl.Mode}
	if pl.Typed.Title {
		d["title"] = pl.Publish.PR.Title
	}
	if pl.Typed.Body {
		d["body"] = pl.Publish.PR.Body
	}
	return d
}

// replanDetails adds to a MARKER_INVALID, CURATE_CONFLICT or
// CURATE_TREE_MISMATCH the plan's feature, public repo, public branch and
// mode, so next can name the promote that makes a new plan.
func replanDetails(err error, pl plan.Plan) error {
	ce := contract.From(err)
	if ce == nil {
		return err
	}
	switch ce.Code {
	case contract.MarkerInvalid, contract.CurateConflict, contract.CurateTreeMismatch:
		if ce.Details == nil {
			ce.Details = map[string]any{}
		}
		ce.Details["feature"], ce.Details["public"], ce.Details["mode"] = pl.Feature, pl.Pair.Public, pl.Mode
		ce.Details["branch"] = pl.Publish.TargetBranch
	}
	return err
}

// earlierPlan names a saved plan of the same pair and feature that
// publishes exactly sha to branch: a publish that was interrupted after its
// push, which apply <that plan> resumes. It never names published, the plan
// the feature record says was published: plan files outlive their apply,
// and that publish is finished, not interrupted.
func (e *Engine) earlierPlan(pair workspace.Pair, feature, branch, sha, published string) string {
	// A plan file that can't be read is simply not named.
	files, _, err := planFiles(e.WS.PlansDir())
	if err != nil {
		return ""
	}
	for _, f := range files {
		// The files are local and unsigned: name only an ID apply can load.
		if !plan.ValidID(f.ID) || filepath.Base(f.Path) != f.ID+".json" || f.ID == published {
			continue
		}
		if f.Feature == feature && strings.EqualFold(f.Pair.Public, pair.Public) &&
			f.Publish.TargetBranch == branch && f.Publish.Commit == sha {
			return f.ID
		}
	}
	return ""
}

// branchExists refuses a publish to a public branch that is already at
// another commit. When earlier is set, that commit is an earlier plan's
// of the same feature, and the error names it.
func branchExists(branch, sha, earlier string) error {
	if earlier != "" {
		return contract.New(contract.BranchExists,
			"public branch %q already holds the commits of plan %s, an earlier promote of this feature; resume that publish with ghostbranch apply %s", branch, earlier, earlier).
			WithField("branch").WithDetails(map[string]any{"branch": branch, "sha": sha, "plan_id": earlier})
	}
	return contract.New(contract.BranchExists,
		"public branch %q already exists at another commit; promote under another name with --branch", branch).
		WithField("branch").WithDetails(map[string]any{"branch": branch, "sha": sha})
}

// publishedPlan is the plan m's record says was published, or "".
func publishedPlan(m feature.Meta) string {
	if m.Promoted == nil {
		return ""
	}
	return m.Promoted.PlanID
}

// previewPublish adds the public steps a publish of feature's P to branch
// still needs: the push unless the branch is already at P, and the PR
// unless one is open. A branch at another commit is refused, as the
// publish would be; published is the plan the feature's record says was
// published (see earlierPlan). base is the PR's base branch.
func (e *Engine) previewPublish(ctx context.Context, pc *pairCtx, pv *preview.Preview, feature, published, branch, base, P string) error {
	existing, exists, _ := pc.git.Resolve(ctx, publicRef(branch))
	if exists && existing != P {
		return branchExists(branch, existing, e.earlierPlan(pc.pair, feature, branch, existing, published))
	}
	if !exists {
		pv.Add(preview.PushRef(pc.pair.Public, "refs/heads/"+branch, "", P))
	}
	pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Public, branch, base)
	if err != nil {
		return forgeErr(err, "find the public PR")
	}
	if pr == nil {
		pv.Add(preview.OpenPR(pc.pair.Public, branch, base, false))
	}
	return nil
}

// apply publishes pl (applyPlan) and notes what next needs to advise the
// merge: whether a squash would flatten the history, and the public repo.
// After a successful publish it hands the features off when the choice
// was Archive (handOff), and reports each one's after_publish; a dry run
// reports what would happen.
func (e *Engine) apply(ctx context.Context, pc *pairCtx, pl plan.Plan, ao applyOpts) (*ApplyResult, []contract.Warning, error) {
	res, warns, err := e.applyPlan(ctx, pc, pl, ao)
	if res == nil {
		return res, warns, err
	}
	res.KeepsHistory, res.PublicRepo = keepsHistory(pl), pl.Pair.Public
	res.AfterPublish = map[string]string{}
	names := []string{pl.Feature}
	if len(pl.Chain) > 0 {
		names = chainNames(pl)
	}
	for _, n := range names {
		res.AfterPublish[n] = AfterKept
	}
	switch {
	case res.Preview != nil:
		for _, n := range res.handOff {
			res.AfterPublish[n] = AfterArchived
		}
	case err == nil:
		warns = addWarnings(warns, e.handOff(ctx, pc, res, res.handOff)...)
	}
	return res, warns, err
}

// keepsHistory reports whether some PR of pl holds more than one commit
// in preserve or curated mode. An append adds its commits on top of the
// open PR's own, so one appended commit already makes two (review M2).
func keepsHistory(pl plan.Plan) bool {
	entries := pl.Chain
	if len(entries) == 0 {
		entries = []plan.Entry{{Mode: pl.Mode, Publish: pl.Publish}}
	}
	for _, en := range entries {
		n := len(en.Publish.Commits)
		if en.Publish.Update && en.Publish.Style == plan.StyleAppend && n > 0 {
			n++ // the PR's head, and whatever came before it
		}
		if (en.Mode == config.ModePreserve || en.Mode == config.ModeCurated) && n > 1 {
			return true
		}
	}
	return false
}

func (e *Engine) applyPlan(ctx context.Context, pc *pairCtx, pl plan.Plan, ao applyOpts) (*ApplyResult, []contract.Warning, error) {
	g := pc.git
	if pl.SchemaVersion != plan.SchemaVersion {
		return nil, nil, contract.New(contract.PlanStale,
			"plan %s was made by an older Ghostbranch (schema %s); make a new plan with ghostbranch promote", pl.ID, pl.SchemaVersion).
			WithDetails(stale(pl, []string{"schema_version"}))
	}
	if def := pc.public.DefaultBranch; def != "" && strings.EqualFold(pl.Publish.TargetBranch, def) {
		return nil, nil, contract.New(contract.NameInvalid, "plan %s would publish to the public default branch %q", pl.ID, def).WithField("plan")
	}
	if len(pl.Chain) > 0 {
		return e.applyStack(ctx, pc, pl, ao)
	}
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	m, metaSHA, err := e.readMeta(ctx, pc, pl.Feature)
	if err != nil {
		return nil, warns, err
	}
	if ok, err := e.landedUpdate(ctx, pc, pl, pl.Publish, m, 0); err != nil || ok {
		if err != nil {
			return nil, warns, err
		}
		return e.finishLanded(ctx, pc, cfg, pl, ao.yes, warns) // never archives (R14)
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
	// An update plan stands only on the gate that built that very update.
	if err := updateStands(m, pl); err != nil {
		return nil, warns, err
	}
	P, links, err := e.verifyChain(ctx, pc, pl, m, cfg, live)
	if err != nil {
		return nil, warns, err
	}
	// Scan what is stored in the commits, not what the plan file says.
	steps, authors, shown, err := storedSteps(ctx, g, pl, links)
	if err != nil {
		return nil, warns, err
	}
	var rep leak.Report
	if pl.Publish.Update {
		// An update scans what isn't public yet, from the PR's head H, with
		// each commit's message, author and committer as stored.
		rep, _, err = e.updateScan(ctx, pc, cfg, pl.Publish.ExpectedHead, P,
			scanOpts{identities: true, sources: commitSources(pl.Publish.Commits, pl.Mode), texts: planTexts(pl)})
	} else {
		rep, _, err = e.scanChain(ctx, g, cfg, pm, steps, planTexts(pl))
	}
	if err != nil {
		return nil, warns, err
	}
	blocking, waived, ww := e.splitFindings(pc.pair, rep.Findings)
	warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
	if len(blocking) > 0 {
		ce := contract.From(leakErr(pl.Feature, blocking))
		ce.Details["mode"], ce.Details["public"], ce.Details["branch"] = pl.Mode, pl.Pair.Public, pl.Publish.TargetBranch
		ce.Details["feature_head"] = pl.Fingerprint.FeatureHead // verified equal to the live head
		return nil, warns, ce
	}

	branch := pl.Publish.TargetBranch
	prs := []plan.StackPR{{Feature: pl.Feature, Base: "main"}}
	if pl.Publish.Update {
		prs = []plan.StackPR{pl.PRs[0]} // updateStands checked there is exactly one
	}
	res := &ApplyResult{PlanID: pl.ID, PublicBranch: branch, Waived: waived, PRs: prs}
	existing, exists, _ := g.Resolve(ctx, publicRef(branch))
	landed := false
	if pl.Publish.Update {
		// The open PR's branch is where the plan saw it (H), or already at P.
		if landed, err = e.updateLive(ctx, pc, pl, m, existing, exists, P); err != nil {
			return res, warns, err
		}
	} else if exists && existing != P {
		return res, warns, branchExists(branch, existing, e.earlierPlan(pc.pair, pl.Feature, branch, existing, publishedPlan(m)))
	}
	var ids []string
	if !pl.Publish.Update {
		if ids, err = g.PatchIDs(ctx, commitSHAs(pl.Publish.Commits)); err != nil {
			return res, warns, internalErr(err, "compute the published commits' patch-ids")
		}
	}
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O, "gate_candidate": gateC})
		if pl.Publish.Update {
			e.previewUpdate(ctx, pc, pv, branch, pl.Publish.ExpectedHead, P, pl.PRs[0].Number, pl.Typed)
		} else if err := e.previewPublish(ctx, pc, pv, pl.Feature, publishedPlan(m), branch, "main", P); err != nil {
			return res, warns, err
		}
		ch, err := e.handOffChoice(ctx, pc, []string{pl.Feature}, ao.archive)
		if err != nil {
			return res, warns, err
		}
		if ch != nil && archives(ch, ch.Default) {
			res.handOff = []string{pl.Feature}
			if err := e.previewArchive(ctx, pc, pv, pl.Feature); err != nil {
				return res, warns, err
			}
		}
		res.Preview = pv
		return res, warns, nil
	}
	from := pm
	if pl.Publish.Update {
		from = pl.Publish.ExpectedHead // the summary lists what the PR's branch changes by
	}
	files, err := g.DiffNames(ctx, from, P)
	if err != nil {
		return nil, warns, internalErr(err, "list the files going public")
	}
	summary := pl
	summary.Publish.Commits = shown // subjects as stored in the commits
	summary.Publish.Parent = pm     // the verified public main
	if pl.Publish.Style == plan.StyleAppend {
		summary.Publish.Parent = pl.Publish.ExpectedHead // the chain's verified parent
	}
	folded, err := e.foldedAdopted(ctx, pc, pl.Feature, m, pl.Mode, pl.Publish, pl.PRs[0].Number)
	if err != nil {
		return res, warns, err
	}
	ch, err := e.handOffChoice(ctx, pc, []string{pl.Feature}, ao.archive)
	if err != nil {
		return nil, warns, err
	}
	appr, pick, err := e.authorizeChoosing(ctx, pc.pair, pl.ID, planSummary(summary, files, authors, waived, rep.Warnings, folded), ao.yes, ch)
	if err != nil {
		return nil, warns, err
	}
	res.Approval = appr
	if archives(ch, pick) {
		res.handOff = []string{pl.Feature}
	}
	if pl.Publish.Update {
		if landed {
			res.Resumed = true
		} else if err := e.pushUpdate(ctx, pc, pl, P); err != nil {
			return res, warns, err
		}
		pr, mw, err := e.recordUpdate(ctx, pc, pl, P)
		warns = addWarnings(warns, mw...)
		if err != nil {
			return res, warns, err
		}
		res.PublicPR, res.PRURL = pr.Number, pr.URL
		res.PRs[0].Number, res.PRs[0].URL = pr.Number, pr.URL
		if err := e.editUpdated(ctx, pc, pl); err != nil {
			return res, warns, err
		}
		return res, warns, nil
	}
	if !exists {
		st := e.startStep("Publishing %s to %s (branch %s)", pl.Feature, pc.pair.Public, branch)
		e.notef(progress.Info, "commit %s → %s:%s", short(P), pc.pair.Public, branch)
		opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + branch}}}
		if err := e.push(ctx, pc, remotePublic, opt, P+":refs/heads/"+branch); err != nil {
			if contract.From(err).Code == contract.RefMoved {
				return res, warns, contract.New(contract.BranchExists, "public branch %q appeared while publishing", branch).WithField("branch")
			}
			return res, warns, err
		}
		st.Done(fmt.Sprintf("Published %s to %s (branch %s)", pl.Feature, pc.pair.Public, branch))
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
	res.PRs[0].Number, res.PRs[0].URL = pr.Number, pr.URL
	m.SchemaVersion = feature.SchemaVersion
	m.Promoted = &feature.Promoted{PublicPR: pr.Number, PlanID: pl.ID, FeatureHead: pl.Fingerprint.FeatureHead,
		Branch: branch, Base: "main", PublishedHead: P, PatchIDs: ids, Stack: stackOfRecord(m, pr.Number)}
	_, mw, err := e.updateMeta(ctx, pc, m, metaSHA)
	warns = addWarnings(warns, mw...)
	if err != nil {
		return res, warns, err
	}
	return res, warns, nil
}

// verifyChain is verifyEntry for a plan of one feature. Its chain sits on
// public main, or, for an append to an open PR, on the PR's head H, where
// the gate built it (updateStands checked that this is the gate's).
func (e *Engine) verifyChain(ctx context.Context, pc *pairCtx, pl plan.Plan, m feature.Meta, cfg config.Config, live plan.Fingerprint) (string, []link, error) {
	tip, _, err := pc.git.Resolve(ctx, planRef(pl.ID))
	if err != nil {
		return "", nil, internalErr(err, "read plan "+pl.ID+"'s commit")
	}
	onto := live.PublicMain
	if pl.Publish.Update && pl.Publish.Style == plan.StyleAppend {
		onto = pl.Publish.ExpectedHead
	}
	return e.verifyEntry(ctx, pc, pl, m, cfg, live, onto, onto, tip, false)
}

// verifyEntry checks that plan pl (a plan of one feature, or one branch of
// a stack plan seen as one: entryPlan) still stands on the shadow repo's
// objects, not on what the file says:
//   - the recorded gate is green, fresh and for the live gate candidate;
//   - its rebased chain runs from onto (public main, or the commit a
//     stacked gate replayed its own commits onto) to the candidate's parent;
//   - the plan ref holds the plan's commit (planTip, "" when the ref is
//     missing), or, for a branch below the top of a stack (below), a
//     commit above it;
//   - the published tree is what this binary's strip gives;
//   - the curate groups fit the mode and the gated commits;
//   - the chain derived again from the gate is the plan's, commit by commit,
//     on pubParent, the public commit below it.
//
// It returns the plan's commit P and the derived links, whose sources and
// entries the scan uses.
func (e *Engine) verifyEntry(ctx context.Context, pc *pairCtx, pl plan.Plan, m feature.Meta, cfg config.Config, live plan.Fingerprint,
	onto, pubParent, planTip string, below bool) (string, []link, error) {
	g := pc.git
	pm, gateC := live.PublicMain, live.GateCandidate
	// The plan only stands while the recorded gate is green, fresh, and for
	// the very candidate the plan was made from, and records the chain it
	// rebased (a gate from an older binary does not).
	gateStale := contract.New(contract.PlanStale,
		"the recorded gate for %q is no longer green for plan %s's candidate; run ghostbranch gate, then make a new plan", pl.Feature, pl.ID).
		WithDetails(stale(pl, []string{"gate"}))
	if !m.Gate.Passed() || m.Gate.Commits == nil || m.Gate.Candidate != gateC || !m.Gate.FreshFor(live.FeatureHead, pm, live.Overlay) {
		return "", nil, gateStale
	}
	// The recorded chain must run from onto to the candidate's parent, one
	// parent at a time.
	prev := onto
	for _, c := range m.Gate.Commits {
		if parent, err := g.MustResolve(ctx, c.Rebased+"^"); err != nil || parent != prev {
			return "", nil, gateStale
		}
		prev = c.Rebased
	}
	if R, err := g.MustResolve(ctx, gateC+"^"); err != nil || R != prev {
		return "", nil, gateStale
	}
	P := pl.Publish.Commit
	inPlan := planTip != "" && planTip == P
	if !inPlan && below && planTip != "" {
		// A lower branch of a stack plan: its tip is under the plan ref's.
		var err error
		if inPlan, err = g.IsAncestor(ctx, P, planTip); err != nil {
			return "", nil, internalErr(err, "compare plan "+pl.ID+"'s commits")
		}
	}
	if !inPlan {
		return "", nil, contract.New(contract.PlanStale, "plan %s's commit is missing from the shadow repo", pl.ID).
			WithDetails(stale(pl, []string{"publish.commit"}))
	}
	// P matching C proves nothing if C itself kept internal sections (a plan
	// from a binary that did not strip, or a forged one): derive the
	// published tree again from the candidate's parent.
	T2, tEntries, err := publishedTree(ctx, g, gateC+"^", cfg)
	if err != nil {
		return "", nil, replanDetails(err, pl)
	}
	if T2 != pl.Publish.Tree {
		return "", nil, contract.New(contract.PlanStale,
			"plan %s was not built with this version's strip of internal sections; make a new plan with ghostbranch promote", pl.ID).
			WithDetails(stale(pl, []string{"publish.tree"}))
	}
	// The plan file is local and unsigned: derive the chain again from the
	// shadow repo's objects, and refuse a plan that names any other.
	// A curated plan replays its groups again; any other plan has none.
	curateStale := contract.New(contract.PlanStale,
		"plan %s's curate groups do not fit its mode %s; make a new plan with ghostbranch promote", pl.ID, pl.Mode).
		WithDetails(stale(pl, []string{"curate"}))
	if (pl.Mode == config.ModeCurated) != (pl.Curate != nil) {
		return "", nil, curateStale
	}
	for _, gr := range pl.Curate {
		if len(gr.From) == 0 {
			return "", nil, curateStale
		}
	}
	// The groups name each gated commit exactly once, by its full SHA.
	if pl.Curate != nil {
		if resolved, err := ResolveGroups(pl.Curate, sourcesOf(m.Gate.Commits)); err != nil || !reflect.DeepEqual(resolved, pl.Curate) {
			return "", nil, curateStale
		}
	}
	// A chain on the public commit below it (a stacked branch) starts from
	// that commit's tree, and curated groups replay onto onto.
	baseTree := ""
	if pubParent != pm {
		if baseTree, err = g.TreeOf(ctx, pubParent); err != nil {
			return "", nil, internalErr(err, "read the public commit below "+pl.Feature)
		}
	}
	links, dropped, err := deriveChain(ctx, g, cfg, chainInput{mode: pl.Mode, pm: pm, baseTree: baseTree, onto: onto, T: pl.Publish.Tree, tEntries: tEntries,
		commits: m.Gate.Commits, groups: pl.Curate},
		func(cs []string, onto string) ([]string, error) { return e.replay(ctx, pc, cs, onto) })
	if err != nil {
		return "", nil, replanDetails(err, pl)
	}
	chainStale := contract.New(contract.PlanStale,
		"plan %s's public commits are not the ones its feature's history gives; make a new plan with ghostbranch promote", pl.ID).
		WithDetails(stale(pl, []string{"publish.commits"}))
	commits := pl.Publish.Commits
	if len(links) != len(commits) || !slices.Equal(dropped, pl.Publish.DroppedEmpty) {
		return "", nil, chainStale
	}
	for i, l := range links {
		if l.tree != commits[i].Tree || !reflect.DeepEqual(l.sources, commits[i].Sources) {
			return "", nil, chainStale
		}
	}
	// The chain publishes on pubParent; the approval's file list is
	// measured from there, so the plan file's parent must be it.
	if pl.Publish.Parent != pubParent {
		what := "public main"
		if pubParent != pm {
			what = "the public commit below it"
		}
		return "", nil, contract.New(contract.PlanStale,
			"plan %s names parent %s, not %s %s; make a new plan with ghostbranch promote", pl.ID, short(pl.Publish.Parent), what, short(pubParent)).
			WithDetails(stale(pl, []string{"publish.parent"}))
	}
	// The plan ref is the chain's tip: walk back from it.
	at := P
	for i := len(commits) - 1; i >= 0; i-- {
		if at != commits[i].Commit {
			return "", nil, chainStale
		}
		if i > 0 {
			if at, err = g.MustResolve(ctx, at+"^"); err != nil {
				return "", nil, chainStale
			}
		}
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	if err := assertChain(ctx, g, commits, pl.Publish.Tree, gateC, pubParent, set, guardNames(pc)); err != nil {
		return "", nil, err
	}
	return P, links, nil
}

// storedSteps is what a plan's public commits hold, read from the commits
// themselves, not the plan file: the scan step of each, each one's author,
// and the commits with their subjects as stored.
func storedSteps(ctx context.Context, g *gitx.Git, pl plan.Plan, links []link) ([]scanStep, []string, []plan.Commit, error) {
	commits := pl.Publish.Commits
	steps := make([]scanStep, len(commits))
	authors := make([]string, len(commits))
	shown := append([]plan.Commit(nil), commits...)
	for i, c := range commits {
		msg, err := g.CommitMessage(ctx, c.Commit)
		if err != nil {
			return nil, nil, nil, internalErr(err, "read a public commit's message")
		}
		author, committer, err := g.CommitIdentities(ctx, c.Commit)
		if err != nil {
			return nil, nil, nil, internalErr(err, "read a public commit's author and committer")
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
	return steps, authors, shown, nil
}

// planTexts are the texts a plan publishes beside its commits.
func planTexts(pl plan.Plan) []leak.Text {
	return []leak.Text{{Where: "branch", Content: pl.Publish.TargetBranch}, {Where: "pr_title", Content: pl.Publish.PR.Title},
		{Where: "pr_body", Content: pl.Publish.PR.Body}}
}

func commitSHAs(cs []plan.Commit) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Commit
	}
	return out
}

// stackOfRecord is the native stack m's publish record names for PR pr, 0
// when none: a resumed publish of the same PR keeps it.
func stackOfRecord(m feature.Meta, pr int) int {
	if m.Promoted == nil || m.Promoted.PublicPR != pr {
		return 0
	}
	return m.Promoted.Stack
}
