package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
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
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type PromoteOptions struct {
	Feature string
	// Features is a stack, bottom first: promote a b c. One feature is the
	// same as Feature. Feature is always Features[0] once validated.
	Features []string
	// Modes and Titles set one branch's history mode and PR title in a
	// stack promote (--mode-for, --title-for), keyed by feature.
	Modes    map[string]string
	Titles   map[string]string
	Mode     string // squash, preserve or curated; "" means promote.default_mode
	Message  string
	Title    string
	Body     string
	Branch   string       // public branch name; defaults to the feature name
	Curate   string       // path to the --curate file (curated mode)
	Groups   []plan.Group // the --curate file's groups, as read (ValidatePromoteOptions loads them)
	PlanOnly bool
	Yes      bool
	Detach   bool // gate and return the pending job without waiting on CI
}

type PromoteResult struct {
	Gate     *GateResult      `json:"gate"`
	Plan     *plan.Plan       `json:"plan"`
	PlanPath string           `json:"plan_path"`
	Findings []leak.Finding   `json:"findings"`
	Waived   []WaivedFinding  `json:"waived"`
	Applied  *ApplyResult     `json:"applied"`
	Preview  *preview.Preview `json:"preview"`
	// PRs is every PR the plan opens, bottom first, with numbers once
	// applied; StackNumber is the native GitHub stack, null when none.
	PRs         []plan.StackPR `json:"prs"`
	StackNumber *int           `json:"stack_number"`
}

const editorTemplate = "\n# Write the commit message for the public squash commit of feature %q.\n" +
	"# The first line becomes the PR title and the rest the PR body.\n" +
	"# Lines starting with '#' are ignored. An empty message aborts the promote.\n"

var errStaleGate = errors.New("the recorded gate's commit mapping does not end at its candidate")

// Promote gates the feature (reusing a fresh green gate), builds the chain
// of public commits its history mode publishes, scans everything that would
// go public, saves a plan, and applies it once approved.
func (e *Engine) Promote(ctx context.Context, p workspace.Pair, opt PromoteOptions) (res *PromoteResult, warns []contract.Warning, err error) {
	planID := ""
	defer func() {
		summary := ""
		if res != nil && res.Applied != nil && err == nil {
			summary = publishSummary(res.Applied)
		} else if res != nil && res.Plan != nil {
			summary = "plan " + res.Plan.ID + " saved"
		}
		e.recordWith("promote", &p, opt.Feature, planID, summary, err, func(ent *ledger.Entry) {
			if res != nil {
				ent.Waivers = findingIDs(res.Waived)
			}
			if res != nil && res.Applied != nil {
				ent.Approval = res.Applied.Approval
			}
		})
	}()
	// Registered after the ledger's defer, so it runs first and the ledger
	// records INTERRUPTED.
	defer func() {
		var g *GateResult
		if res != nil {
			g = res.Gate
		}
		err = interrupted(ctx, g, opt.Feature, err)
		// Once the plan is saved, apply resumes the publish wherever it
		// stopped (an apply that began returns its partial result too).
		if res != nil && res.PlanPath != "" && contract.From(err) != nil && contract.From(err).Code == contract.Interrupted {
			id := res.Plan.ID
			err = contract.New(contract.Interrupted,
				"interrupted after plan %s was saved; resume the publish with ghostbranch apply %s (promoting again would make new commits)", id, id).
				WithDetails(map[string]any{"plan_id": id, "feature": opt.Feature})
		}
	}()
	if err := ValidatePromoteOptions(&opt); err != nil {
		return nil, nil, err
	}
	branch := opt.Branch
	if branch == "" {
		branch = opt.Feature
	}
	res = &PromoteResult{Findings: []leak.Finding{}, Waived: []WaivedFinding{}, PRs: []plan.StackPR{}}
	pc, err := e.open(ctx, "promote", p, true)
	if err != nil {
		return res, nil, err
	}
	defer pc.unlock()
	g := pc.git

	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return res, warns, err
	}
	mode := opt.Mode
	if mode == "" {
		mode = cfg.Promote.DefaultMode
	}
	if mode != config.ModeSquash && strings.TrimSpace(opt.Message) != "" {
		return res, warns, messageNotSquash(mode)
	}
	if len(cfg.Promote.AuthorMap) > 0 && cfg.Promote.AuthorEmail != config.AuthorMap {
		warns = addWarnings(warns, contract.Warning{Code: contract.ConfigKeyIgnored,
			Message: fmt.Sprintf("promote.author_map is set but not used: promote.author_email is %s; set author_email: map to use it, or remove the map",
				cfg.Promote.AuthorEmail),
			Details: map[string]any{"key": "promote.author_map"}})
	}
	// A list of several features, one stacked on another feature, or one
	// with open PRs stacked on it is a stack: one plan for every PR, each on
	// the branch below; an open PR above updates with it.
	above, err := e.openAbove(ctx, pc, opt.Features)
	if err != nil {
		return res, warns, err
	}
	if len(opt.Features) > 1 || e.stacked(ctx, pc, opt.Feature) || len(above) > 0 {
		var sw []contract.Warning
		planID, sw, err = e.promoteStack(ctx, pc, cfg, O, opt, above, res)
		return res, addWarnings(warns, sw...), err
	}
	if err := curateFlags(mode, opt.Curate); err != nil {
		return res, warns, err
	}
	if mode == config.ModeCurated && opt.Groups == nil {
		if opt.Groups, err = LoadCurate(opt.Curate); err != nil {
			return res, warns, err
		}
	}
	var msg string
	if mode == config.ModeSquash {
		if msg, err = e.message(opt); err != nil {
			return res, warns, err
		}
	}
	warns = addWarnings(warns, e.behind(ctx, pc)...)
	m, _, err := e.readMeta(ctx, pc, opt.Feature)
	if err != nil {
		return res, warns, err
	}
	head, ok, _ := g.Resolve(ctx, shadowRef(m.Branch))
	if !ok {
		return res, warns, contract.New(contract.FeatureNotFound, "feature branch %q is gone from %s", m.Branch, p.Shadow).WithField("feature")
	}
	pm, err := g.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return res, warns, internalErr(err, "read public main")
	}
	// A feature whose PR is open is updated, not published again: read the
	// PR now. Commits someone pushed to it stop the promote before any CI
	// runs, and the update pushes to the PR's own branch.
	upd, err := e.prHead(ctx, pc, &m)
	if err != nil {
		return res, warns, err
	}
	if upd != nil {
		if upd.changed() {
			return res, warns, prChangedErr(opt.Feature, upd)
		}
		if err := updateFlags(opt, upd); err != nil {
			return res, warns, err
		}
		branch = upd.Branch
	}
	// A preserved history of several commits has no one message to take the
	// PR title from: ask for it before any CI runs.
	// The feature's own commits, as its gate counts them: forked_at..head
	// for one moved onto main that keeps its fork point.
	// An update keeps the PR's title unless one is typed.
	if upd == nil && mode == config.ModePreserve && strings.TrimSpace(opt.Title) == "" {
		sources, err := ownSources(ctx, g, opt.Feature, m, head, pm)
		if err != nil {
			return res, warns, err
		}
		if len(sources) > 1 {
			return res, warns, contract.New(contract.MissingInput, "this feature has %d commits, so its PR needs a title: pass --title", len(sources)).WithField("title")
		}
	}
	// A bad --curate file fails here, before any CI runs.
	if mode == config.ModeCurated {
		if upd == nil && len(opt.Groups) > 1 && strings.TrimSpace(opt.Title) == "" {
			return res, warns, contract.New(contract.MissingInput, "the --curate file has %d groups, so the PR needs a title: pass --title", len(opt.Groups)).WithField("title")
		}
		sources, err := ownSources(ctx, g, opt.Feature, m, head, pm)
		if err != nil {
			return res, warns, err
		}
		if _, err := ResolveGroups(opt.Groups, sources); err != nil {
			return res, warns, err
		}
	}

	names := guardNames(pc)
	// A gate is reused only for the update it built, in the style that
	// update needs now (gateFitsUpdate); an append gate's chain starts on
	// the PR's head, not on public main.
	reusable := func(m feature.Meta) (bool, string, []gitx.TreeEntry, workflow.GuardReport, string, error) {
		onto := pm
		if m.Gate != nil && m.Gate.Update != nil && m.Gate.Update.Style == plan.StyleAppend {
			onto = m.Gate.Update.Head
		}
		ok, T2, entries, rep := e.reusableGate(ctx, pc, cfg, m, opt.Feature, head, pm, onto, O, names)
		if !ok {
			return false, T2, entries, rep, onto, nil
		}
		fits, _, err := e.updateFits(ctx, pc, m.Gate, m, mode)
		return fits, T2, entries, rep, onto, err
	}
	fresh, T2, entries, guardRep, onto, err := reusable(m)
	if err != nil {
		return res, warns, err
	}
	var commits []feature.GateCommit
	// A pending gate job for exactly these inputs is waited on, not gated
	// again; --detach returns it at once. A dry run never reuses one:
	// finishing it would wait on CI and write.
	var waited *GateResult
	if !fresh && !e.DryRun {
		pg, err := e.pendingFor(ctx, pc, opt.Feature, mode)
		if err != nil {
			return res, warns, err
		}
		if pg != nil {
			gr, rw, err := e.resultFromRecord(ctx, pc, opt.Feature, *pg)
			warns = addWarnings(warns, rw...)
			if err != nil {
				return res, warns, err
			}
			if opt.Detach {
				res.Gate = gr
				return res, warns, nil
			}
			gr, gw, err := e.finish(ctx, pc, opt.Feature, gr, *pg, waitOpts{})
			warns = addWarnings(warns, gw...)
			if err != nil {
				res.Gate = gr
				return res, warns, err
			}
			waited = gr
			// The finish wrote the outcome: re-read it, and reuse a green gate
			// through the fresh case, which recomputes the guard report.
			if m, _, err = e.readMeta(ctx, pc, opt.Feature); err != nil {
				return res, warns, err
			}
			if fresh, T2, entries, guardRep, onto, err = reusable(m); err != nil {
				return res, warns, err
			}
		}
	}
	switch {
	case fresh:
		infos, err := gateCommitInfos(ctx, g, cfg, onto, m.Gate.Commits)
		if err != nil {
			return res, warns, err
		}
		res.Gate = &GateResult{Feature: opt.Feature, FeatureHead: head, PublicMain: pm, Parent: m.Gate.Parent, Base: cmp.Or(m.Gate.Base, pm), Overlay: O,
			Candidate: m.Gate.Candidate, PublishedTree: m.Gate.PublishedTree, ScannedAs: m.Gate.ScannedAs, Commits: infos, Checks: m.Gate.Checks,
			FailedChecks: []string{}, Leak: m.Gate.Leak, Findings: []leak.Finding{}, Waived: []WaivedFinding{},
			RepoGuards: &guardRep, Reused: true, Update: m.Gate.Update}
		if waited != nil {
			// This promote waited on the gate job: keep its shadow PR and id.
			res.Gate.ShadowPR, res.Gate.Job = waited.ShadowPR, waited.Job
		}
		commits = m.Gate.Commits
	case e.DryRun:
		// Running the gate would push; say so and stop, rather than return
		// a plan whose checks never ran.
		res.Preview = preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O})
		res.Preview.Add(preview.RunGate(opt.Feature))
		return res, warns, nil
	default:
		e.notef(progress.Info, "The gate for %s is missing or stale, so it runs first", opt.Feature)
		// The internal gate scans for the mode this promote publishes.
		gr, gw, err := e.gate(ctx, pc, opt.Feature, mode, gateHow{detach: opt.Detach})
		res.Gate = gr
		warns = addWarnings(warns, gw...)
		if err != nil {
			return res, warns, err
		}
		if gr.Checks == feature.ChecksPending {
			return res, warns, nil // detached: the job is the result
		}
		R, err := g.MustResolve(ctx, gr.Candidate+"^")
		if err != nil {
			return res, warns, internalErr(err, "read the rebased feature")
		}
		if T2, entries, err = publishedTree(ctx, g, R, cfg); err != nil {
			return res, warns, err
		}
		guardRep = *gr.RepoGuards
		commits = gr.mapping
	}
	C, T := res.Gate.Candidate, res.Gate.PublishedTree
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	if T2 != T {
		return res, warns, contract.New(contract.Internal, "the gated tree %s no longer matches its candidate (%s)", short(T), short(T2))
	}

	// The gate's chain is the feature as it was gated: resolve the groups
	// against it (the same commits unless the feature moved).
	var groups []plan.Group
	if mode == config.ModeCurated {
		if groups, err = ResolveGroups(opt.Groups, sourcesOf(commits)); err != nil {
			return res, warns, err
		}
	}
	// The gate built the update this promote saw, or neither is one.
	gu := res.Gate.Update
	if (gu == nil) != (upd == nil) || (gu != nil && (gu.PR != upd.PR || gu.Head != upd.Head || gu.Base != upd.Base)) {
		return res, warns, contract.New(contract.RefMoved, "feature %s's public PR changed while this promote ran; run it again", opt.Feature).WithField("feature")
	}
	pubParent := pm
	in := chainInput{mode: mode, pm: pm, T: T, tEntries: entries, commits: commits, message: msg, groups: groups}
	if gu != nil && gu.Style == plan.StyleAppend {
		// An append publishes only the new commits, on the PR's head H.
		pubParent, in.onto = gu.Head, gu.Head
		if in.baseTree, err = g.TreeOf(ctx, gu.Head); err != nil {
			return res, warns, internalErr(err, "read the public PR's head")
		}
	}
	links, dropped, err := deriveChain(ctx, g, cfg, in,
		func(cs []string, onto string) ([]string, error) { return e.replay(ctx, pc, cs, onto) })
	if err != nil {
		ce := contract.From(err)
		if ce.Code == contract.MarkerInvalid {
			err = namedMarkerErr(err, opt.Feature)
			contract.From(err).Details["mode"] = mode
		}
		if gu != nil && gu.Style == plan.StyleAppend && ce.Code == contract.NothingToPublish {
			return res, warns, contract.New(contract.NothingToPublish,
				"public PR #%d already holds every change of feature %s; there is nothing to update", gu.PR, opt.Feature).
				WithDetails(map[string]any{"feature": opt.Feature, "public_pr": gu.PR, "dropped_empty": ce.Details["dropped_empty"]})
		}
		return res, warns, err
	}
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return res, warns, forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: u.Login, Email: forge.NoreplyEmail(u), When: e.now()}
	if gu != nil {
		// Commits adopted from the PR keep their authors: as they are in
		// preserve, as Co-authored-by trailers on a squash commit.
		switch mode {
		case config.ModePreserve:
			if err := keepAdoptedAuthors(ctx, g, links, adoptedOf(m)); err != nil {
				return res, warns, err
			}
		case config.ModeSquash:
			if len(links) == 1 {
				if links[0].message, err = withCoAuthors(ctx, g, links[0].message, adoptedOf(m), who); err != nil {
					return res, warns, err
				}
			}
		}
	}
	authors, err := chainAuthors(cfg.Promote, mode, links, who)
	if err != nil {
		return res, warns, err
	}
	chain, err := writeChain(ctx, g, links, authors, who, pubParent)
	if err != nil {
		return res, warns, err
	}
	P := chain[len(chain)-1].Commit
	if err := assertChain(ctx, g, chain, T, C, pubParent, set, names); err != nil {
		return res, warns, err
	}
	// A preserve replace keeps every commit feature pull adopted from GitHub,
	// or stops: the approval summary lists those squash or curated fold.
	if gu != nil && gu.Style == plan.StyleReplace && mode == config.ModePreserve {
		missing, err := missingAdopted(ctx, g, m.Promoted.Adopted, commitSHAs(chain))
		if err != nil {
			return res, warns, err
		}
		if len(missing) > 0 {
			return res, warns, adoptedMissingErr(opt.Feature, branch, gu.PR, gu.Head, missing)
		}
	}

	var title, body string
	switch {
	case gu != nil:
		// An update keeps the PR's title and body: only what is typed is
		// planned, and apply edits just that.
		title, body = strings.TrimSpace(opt.Title), strings.TrimSpace(opt.Body)
	case mode == config.ModeSquash:
		title, body = prTexts(msg, opt.Title, opt.Body)
	case len(links) == 1:
		title, body = prTexts(links[0].message, opt.Title, opt.Body)
	default:
		title, body = strings.TrimSpace(opt.Title), strings.TrimSpace(opt.Body)
		if body == "" {
			lines := make([]string, len(chain))
			for i, c := range chain {
				lines[i] = "- " + c.Subject
			}
			body = strings.Join(lines, "\n")
		}
	}
	if title == "" && gu == nil {
		return res, warns, contract.New(contract.MissingInput, "the PR needs a title: pass --title").WithField("title")
	}

	texts := []leak.Text{{Where: "branch", Content: branch}, {Where: "pr_title", Content: title}, {Where: "pr_body", Content: body}}
	var rep leak.Report
	var version string
	if gu != nil {
		// An update scans what isn't public yet, from the PR's head H: the
		// new commits (append) or the whole new history against H
		// (replace), each with its message, author and committer as written
		// (under author_email: map, the mapped identities).
		rep, version, err = e.updateScan(ctx, pc, cfg, gu.Head, P, scanOpts{identities: true, sources: commitSources(chain, mode), texts: texts})
	} else {
		steps := make([]scanStep, len(links))
		for i, l := range links {
			var sources []string
			if mode != config.ModeSquash {
				sources = l.sources
			}
			steps[i] = scanStep{sources: sources, entries: l.entries, texts: []leak.Text{
				{Where: "commit_message", Content: l.message},
				{Where: "author", Content: identityText(authors[i])},
				{Where: "committer", Content: identityText(who)}}}
		}
		rep, version, err = e.scanChain(ctx, g, cfg, pm, steps, texts)
	}
	if err != nil {
		return res, warns, err
	}
	blocking, waived, ww := e.splitFindings(p, rep.Findings)
	warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
	res.Waived = waived
	if len(blocking) > 0 {
		res.Findings = blocking
		ce := contract.From(leakErr(opt.Feature, blocking))
		ce.Details["mode"] = mode
		return res, warns, ce
	}

	if planID, err = plan.NewID(e.now(), e.Rand); err != nil {
		return res, warns, internalErr(err, "make a plan id")
	}
	pub := plan.Publish{Commit: P, Tree: T, Parent: pubParent, TargetBranch: branch, PR: plan.PR{Title: title, Body: body},
		Commits: chain, DroppedEmpty: dropped}
	prs := []plan.StackPR{{Feature: opt.Feature, Base: "main"}}
	if gu != nil {
		pub.Update, pub.Style, pub.ReplaceReason, pub.ExpectedHead = true, gu.Style, planReason(gu.Style, gu.Reason, forcedReason(upd, &m, mode, false)), gu.Head
		if gu.Style == plan.StyleReplace {
			pub.Restores = upd.Restores // the published commits GitHub lost, which this replace puts back
		}
		prs = []plan.StackPR{{Feature: opt.Feature, Base: gu.Base, Number: gu.PR, Update: true, Style: gu.Style}}
	}
	pl := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: planID, Kind: "promote", Actor: e.Actor, CreatedAt: e.now(),
		Pair: plan.Pair{Public: p.Public, Shadow: p.Shadow}, Feature: opt.Feature, Mode: mode, Curate: groups,
		Fingerprint: plan.Fingerprint{PublicMain: pm, FeatureHead: head, Overlay: O, GateCandidate: C},
		Publish:     pub,
		Typed:       plan.Typed{Title: strings.TrimSpace(opt.Title) != "", Body: strings.TrimSpace(opt.Body) != ""},
		Leak:        plan.Leak{Findings: []map[string]any{}, Waived: planWaived(waived), Warnings: append([]contract.Warning{}, rep.Warnings...), ScannerVersion: version},
		CI:          plan.CI{RepoGuards: guardRep}, PRs: prs}
	res.Plan, res.PRs = &pl, pl.PRs
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O, "gate_candidate": C})
		pv.Add(preview.PushRef(p.Shadow, remotePlanRef(planID), "", P),
			preview.WriteFile(filepath.Join(e.WS.PlansDir(), planID+".json")))
		if opt.PlanOnly {
			// promote --plan stores the plan and stops; nothing public.
			res.Preview = pv
			return res, warns, nil
		}
		if gu != nil {
			e.previewUpdate(ctx, pc, pv, branch, gu.Head, P, gu.PR, pl.Typed)
		} else if err := e.previewPublish(ctx, pc, pv, opt.Feature, publishedPlan(m), branch, "main", P); err != nil {
			return res, warns, err
		}
		res.Preview = pv
		return res, warns, nil
	}
	if res.PlanPath, err = plan.Save(e.WS.PlansDir(), pl); err != nil {
		return res, warns, internalErr(err, "save the plan")
	}
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: remotePlanRef(planID)}}}, P+":"+remotePlanRef(planID)); err != nil {
		_ = os.Remove(res.PlanPath) // a plan whose commits never reached the shadow repo can't be applied
		res.PlanPath = ""
		return res, warns, err
	}
	warns = addWarnings(warns, e.updateMirror(ctx, g, planRef(planID), P)...)
	if opt.PlanOnly {
		return res, warns, nil
	}
	ar, aw, err := e.apply(ctx, pc, pl, opt.Yes)
	res.Applied = ar
	if ar != nil {
		res.PRs, res.StackNumber = ar.PRs, ar.StackNumber
	}
	return res, addWarnings(warns, aw...), err
}

// stacked reports whether name's record names a feature as its parent. A
// record that can't be read is not: the one-feature path reports why.
func (e *Engine) stacked(ctx context.Context, pc *pairCtx, name string) bool {
	m, _, _, err := e.readMetaAny(ctx, pc, name)
	return err == nil && stackParent(m) != "main"
}

// publishSummary is the ledger's line for a publish.
func publishSummary(r *ApplyResult) string {
	if len(r.PRs) == 1 && r.PRs[0].Update {
		if r.PRs[0].Style == plan.StyleAppend {
			return fmt.Sprintf("updated PR #%d on %s", r.PublicPR, r.PublicBranch)
		}
		return fmt.Sprintf("replaced PR #%d's branch %s", r.PublicPR, r.PublicBranch)
	}
	if len(r.PRs) <= 1 {
		return fmt.Sprintf("published %s as PR #%d", r.PublicBranch, r.PublicPR)
	}
	parts := make([]string, len(r.PRs))
	for i, p := range r.PRs {
		parts[i] = fmt.Sprintf("%s as PR #%d", p.Feature, p.Number)
	}
	s := "published " + strings.Join(parts, ", ")
	if r.StackNumber != nil {
		s += fmt.Sprintf(" (stack #%d)", *r.StackNumber)
	}
	return s
}

// reusableGate reports whether the recorded gate can be reused as is, with
// the published tree T2 and its entries, and the repo guard report it
// rebuilt on the way. A fresh green gate is reused only
// while the shadow gate branch still points at its candidate; apply checks
// that branch, so a deleted or moved one would make every plan stale. The
// gate must have built on the stack as it is now (gateStackFresh), and its
// rebased chain must end at the candidate's parent, starting from onto when
// it is empty: public main, or for a stacked feature its gate's Base.
func (e *Engine) reusableGate(ctx context.Context, pc *pairCtx, cfg config.Config, m feature.Meta, name, head, pm, onto, O string, names workflow.Names) (ok bool, T2 string, entries []gitx.TreeEntry, rep workflow.GuardReport) {
	g := pc.git
	tip, tipOK, _ := g.Resolve(ctx, shadowRef(gateBranch(name)))
	candidateLive := m.Gate != nil && m.Gate.Candidate != "" && tipOK && tip == m.Gate.Candidate
	// A gate recorded without the mode its scan covered is not reused, so
	// the result never claims a scan that did not happen.
	ok = m.Gate.Passed() && m.Gate.FreshFor(head, pm, O) && candidateLive && m.Gate.Commits != nil && m.Gate.ScannedAs != ""
	if ok {
		// A stack read that fails is not fresh: gating again reports it.
		stackFresh, err := e.gateStackFresh(ctx, pc, name, m)
		ok = err == nil && stackFresh
	}
	if ok {
		// A gate recorded by a binary that built its candidate differently
		// is stale: one that stripped differently (an upgrade that added
		// internal markers), or one that did not rewrite repo guards (so its
		// guarded jobs skipped). Gate again rather than publish a tree this
		// binary would not build, or reuse CI that did not run those jobs.
		ok = false
		var R string
		var rerr error
		if R, rerr = g.MustResolve(ctx, m.Gate.Candidate+"^"); rerr == nil {
			if n := len(m.Gate.Commits); (n == 0 && R != onto) || (n > 0 && m.Gate.Commits[n-1].Rebased != R) {
				rerr = errStaleGate
			}
		}
		if rerr == nil {
			if T2, entries, rerr = publishedTree(ctx, g, R, cfg); rerr == nil && T2 == m.Gate.PublishedTree {
				want, r, _, werr := e.candidateTree(ctx, g, R, entries, O, names, cfg.CI.RewriteRepoGuards)
				have, herr := g.TreeOf(ctx, m.Gate.Candidate)
				ok, rep = werr == nil && herr == nil && want == have, r
			}
		}
	}
	return ok, T2, entries, rep
}

// ValidatePromoteOptions checks everything about a promote that needs no
// network, so a bad flag fails before any CI runs. An empty Mode means
// promote.default_mode, which is checked once the config is read.
func ValidatePromoteOptions(opt *PromoteOptions) error {
	if len(opt.Features) == 0 && opt.Feature != "" {
		opt.Features = []string{opt.Feature}
	}
	if len(opt.Features) > 0 {
		opt.Feature = opt.Features[0]
	}
	for _, f := range opt.Features {
		if err := feature.ValidateName(f, "feature"); err != nil {
			return err
		}
	}
	// Map keys in order, so the error names the same feature every run.
	for _, fl := range []struct {
		flag string
		m    map[string]string
	}{{"mode-for", opt.Modes}, {"title-for", opt.Titles}} {
		for _, f := range slices.Sorted(maps.Keys(fl.m)) {
			if !slices.Contains(opt.Features, f) {
				return contract.New(contract.BadFlag, "--%s names %s, which this promote doesn't list", fl.flag, f).WithField(fl.flag)
			}
		}
	}
	for _, f := range slices.Sorted(maps.Keys(opt.Modes)) {
		switch m := opt.Modes[f]; {
		case m == config.ModeSquash || m == config.ModePreserve:
		case m == config.ModeCurated && len(opt.Features) == 1:
		case m == config.ModeCurated:
			return contract.New(contract.BadFlag, "--mode-for %s=curated: a curated branch is promoted on its own, on top of the published branch below it", f).WithField("mode-for")
		default:
			return contract.New(contract.BadFlag, "--mode-for %s=%s: use squash or preserve", f, m).WithField("mode-for")
		}
	}
	if len(opt.Features) == 1 {
		// One feature: its --mode-for and --title-for are --mode and --title.
		// Both at once would leave which one wins to the code path, so it
		// is refused.
		if m, ok := opt.Modes[opt.Feature]; ok {
			if opt.Mode != "" {
				return contract.New(contract.BadFlag, "--mode and --mode-for %s both set %s's mode: pass one", opt.Feature, opt.Feature).WithField("mode-for")
			}
			opt.Mode = m
		}
		if t, ok := opt.Titles[opt.Feature]; ok {
			if strings.TrimSpace(opt.Title) != "" {
				return contract.New(contract.BadFlag, "--title and --title-for %s both set %s's PR title: pass one", opt.Feature, opt.Feature).WithField("title-for")
			}
			opt.Title = t
		}
	}
	if len(opt.Features) > 1 {
		for _, fl := range []struct {
			set        bool
			name, what string
		}{
			{opt.Branch != "", "branch", "each branch of a stack publishes under its own name"},
			{strings.TrimSpace(opt.Title) != "", "title", "set each PR's title with --title-for <feature>=<title>"},
			{strings.TrimSpace(opt.Body) != "", "body", "each PR's body lists its commits"},
			{strings.TrimSpace(opt.Message) != "", "message", "a squashed branch's commit message is its --title-for title, or its one commit's message"},
			{opt.Curate != "" || opt.Mode == config.ModeCurated, "curate", "a curated branch is promoted on its own, on top of the published branch below it"},
			{opt.Detach, "detach", "a stack promote runs no gate: gate each branch, bottom first, then promote"},
		} {
			if fl.set {
				return contract.New(contract.BadFlag, "--%s takes one feature; with several, %s", fl.name, fl.what).WithField(fl.name)
			}
		}
	}
	switch opt.Mode {
	case "", config.ModeSquash, config.ModePreserve, config.ModeCurated:
	default:
		return contract.New(contract.BadFlag, "--mode %s: use squash, preserve or curated", opt.Mode).WithField("mode")
	}
	if opt.Mode != "" && opt.Mode != config.ModeSquash && strings.TrimSpace(opt.Message) != "" {
		return messageNotSquash(opt.Mode)
	}
	if opt.Mode != "" {
		if err := curateFlags(opt.Mode, opt.Curate); err != nil {
			return err
		}
	}
	// The file is read only once the mode is known to be curated. With no
	// --mode, promote.default_mode decides, and Promote reads the file after
	// the config.
	if opt.Mode == config.ModeCurated && opt.Groups == nil {
		gs, err := LoadCurate(opt.Curate)
		if err != nil {
			return err
		}
		opt.Groups = gs
	}
	branch := opt.Branch
	if branch == "" {
		branch = opt.Feature
	}
	return feature.ValidateName(branch, "branch")
}

// curateFlags checks --curate against the resolved mode: curated mode
// needs the file, and no other mode takes one.
func curateFlags(mode, curate string) error {
	switch {
	case curate != "" && mode != config.ModeCurated:
		return contract.New(contract.BadFlag, "--curate applies only to --mode curated").WithField("curate")
	case mode == config.ModeCurated && curate == "":
		return contract.New(contract.MissingInput, "curated mode needs a --curate file").WithField("curate")
	}
	return nil
}

// messageNotSquash is BAD_FLAG for -m in a mode other than squash.
func messageNotSquash(mode string) error {
	return contract.New(contract.BadFlag,
		"-m sets the squash commit's message, but this promote runs in %s mode; pass --mode squash, or drop -m (use --title and --body for the PR)", mode).
		WithField("message")
}

// prTexts derives the PR title and body: an explicit --title or --body wins,
// and whatever is missing comes from the message. Both are trimmed.
func prTexts(msg, title, body string) (string, string) {
	t, b, _ := strings.Cut(msg, "\n")
	if title = strings.TrimSpace(title); title == "" {
		title = strings.TrimSpace(t)
	}
	if body = strings.TrimSpace(body); body == "" {
		body = strings.TrimSpace(b)
	}
	return title, body
}

// message returns the squash commit message from -m, or from the editor on
// a TTY. Comment lines are dropped; an empty message is MISSING_INPUT.
func (e *Engine) message(opt PromoteOptions) (string, error) {
	msg := strings.TrimSpace(opt.Message)
	if msg != "" {
		return msg, nil
	}
	missing := contract.New(contract.MissingInput, "a commit message is required: pass -m/--message").WithField("message")
	if e.Editor == nil {
		return "", missing
	}
	edited, err := e.Editor(fmt.Sprintf(editorTemplate, opt.Feature))
	if err != nil {
		return "", contract.From(err)
	}
	var lines []string
	for _, line := range strings.Split(strings.ReplaceAll(edited, "\r\n", "\n"), "\n") {
		if !strings.HasPrefix(line, "#") {
			lines = append(lines, strings.TrimRight(line, " \t"))
		}
	}
	if msg = strings.TrimSpace(strings.Join(lines, "\n")); msg == "" {
		return "", missing
	}
	return msg, nil
}
