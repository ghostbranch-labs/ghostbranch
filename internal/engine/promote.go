package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type PromoteOptions struct {
	Feature  string
	Mode     string // squash, preserve or curated; "" means promote.default_mode
	Message  string
	Title    string
	Body     string
	Branch   string       // public branch name; defaults to the feature name
	Curate   string       // path to the --curate file (curated mode)
	Groups   []plan.Group // the --curate file's groups, as read (ValidatePromoteOptions loads them)
	PlanOnly bool
	Yes      bool
}

type PromoteResult struct {
	Gate     *GateResult      `json:"gate"`
	Plan     *plan.Plan       `json:"plan"`
	PlanPath string           `json:"plan_path"`
	Findings []leak.Finding   `json:"findings"`
	Waived   []WaivedFinding  `json:"waived"`
	Applied  *ApplyResult     `json:"applied"`
	Preview  *preview.Preview `json:"preview"`
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
		if res != nil && res.Applied != nil {
			summary = fmt.Sprintf("published %s as PR #%d", res.Applied.PublicBranch, res.Applied.PublicPR)
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
	if err := ValidatePromoteOptions(&opt); err != nil {
		return nil, nil, err
	}
	branch := opt.Branch
	if branch == "" {
		branch = opt.Feature
	}
	res = &PromoteResult{Findings: []leak.Finding{}, Waived: []WaivedFinding{}}
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
	if err := curateFlags(mode, opt.Curate); err != nil {
		return res, warns, err
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
	// A preserved history of several commits has no one message to take the
	// PR title from: ask for it before any CI runs.
	if mode == config.ModePreserve && strings.TrimSpace(opt.Title) == "" {
		sources, err := g.SourceCommits(ctx, pm, head)
		if err != nil {
			return res, warns, internalErr(err, "list the feature's commits")
		}
		if len(sources) > 1 {
			return res, warns, contract.New(contract.MissingInput, "this feature has %d commits, so its PR needs a title: pass --title", len(sources)).WithField("title")
		}
	}
	// A bad --curate file fails here, before any CI runs.
	if mode == config.ModeCurated {
		if len(opt.Groups) > 1 && strings.TrimSpace(opt.Title) == "" {
			return res, warns, contract.New(contract.MissingInput, "the --curate file has %d groups, so the PR needs a title: pass --title", len(opt.Groups)).WithField("title")
		}
		sources, err := g.SourceCommits(ctx, pm, head)
		if err != nil {
			return res, warns, internalErr(err, "list the feature's commits")
		}
		if _, err := ResolveGroups(opt.Groups, sources); err != nil {
			return res, warns, err
		}
	}

	// A fresh green gate is reused only while the shadow gate branch still
	// points at its candidate; apply checks that branch, so a deleted or moved
	// one would make every plan stale.
	tip, tipOK, _ := g.Resolve(ctx, shadowRef(gateBranch(opt.Feature)))
	candidateLive := m.Gate != nil && m.Gate.Candidate != "" && tipOK && tip == m.Gate.Candidate
	// A gate recorded without the mode its scan covered is not reused, so
	// the result never claims a scan that did not happen.
	fresh := m.Gate.Passed() && m.Gate.FreshFor(head, pm, O) && candidateLive && m.Gate.Commits != nil && m.Gate.ScannedAs != ""
	names := guardNames(pc)
	var R, T2 string
	var entries []gitx.TreeEntry
	var commits []feature.GateCommit
	var guardRep workflow.GuardReport
	if fresh {
		// A gate recorded by a binary that built its candidate differently
		// is stale: one that stripped differently (an upgrade that added
		// internal markers), or one that did not rewrite repo guards (so its
		// guarded jobs skipped). Gate again rather than publish a tree this
		// binary would not build, or reuse CI that did not run those jobs.
		fresh = false
		var rerr error
		if R, rerr = g.MustResolve(ctx, m.Gate.Candidate+"^"); rerr == nil {
			if n := len(m.Gate.Commits); (n == 0 && R != pm) || (n > 0 && m.Gate.Commits[n-1].Rebased != R) {
				rerr = errStaleGate
			}
		}
		if rerr == nil {
			if T2, entries, rerr = publishedTree(ctx, g, R, cfg); rerr == nil && T2 == m.Gate.PublishedTree {
				want, rep, _, werr := e.candidateTree(ctx, g, R, entries, O, names, cfg.CI.RewriteRepoGuards)
				have, herr := g.TreeOf(ctx, m.Gate.Candidate)
				fresh, guardRep = werr == nil && herr == nil && want == have, rep
			}
		}
	}
	switch {
	case fresh:
		infos, err := gateCommitInfos(ctx, g, cfg, pm, m.Gate.Commits)
		if err != nil {
			return res, warns, err
		}
		res.Gate = &GateResult{Feature: opt.Feature, FeatureHead: head, PublicMain: pm, Overlay: O,
			Candidate: m.Gate.Candidate, PublishedTree: m.Gate.PublishedTree, ScannedAs: m.Gate.ScannedAs, Commits: infos, Checks: m.Gate.Checks,
			FailedChecks: []string{}, Leak: m.Gate.Leak, Findings: []leak.Finding{}, Waived: []WaivedFinding{},
			RepoGuards: &guardRep, Reused: true}
		commits = m.Gate.Commits
	case e.DryRun:
		// Running the gate would push; say so and stop, rather than return
		// a plan whose checks never ran.
		res.Preview = preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O})
		res.Preview.Add(preview.RunGate(opt.Feature))
		return res, warns, nil
	default:
		e.progressf("the gate for %s is missing or stale; running it", opt.Feature)
		// The internal gate scans for the mode this promote publishes.
		gr, gw, err := e.gate(ctx, pc, opt.Feature, mode)
		res.Gate = gr
		warns = addWarnings(warns, gw...)
		if err != nil {
			return res, warns, err
		}
		if R, err = g.MustResolve(ctx, gr.Candidate+"^"); err != nil {
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
	links, dropped, err := deriveChain(ctx, g, cfg, chainInput{mode: mode, pm: pm, T: T, tEntries: entries, commits: commits, message: msg, groups: groups},
		func(cs []string, onto string) ([]string, error) { return e.replay(ctx, pc, cs, onto) })
	if err != nil {
		return res, warns, err
	}
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return res, warns, forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: u.Login, Email: forge.NoreplyEmail(u), When: e.now()}
	authors, err := chainAuthors(cfg.Promote, mode, links, who)
	if err != nil {
		return res, warns, err
	}
	chain, err := writeChain(ctx, g, links, authors, who, pm)
	if err != nil {
		return res, warns, err
	}
	P := chain[len(chain)-1].Commit
	if err := assertChain(ctx, g, chain, T, C, pm, set, names); err != nil {
		return res, warns, err
	}

	var title, body string
	switch {
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
	if title == "" {
		return res, warns, contract.New(contract.MissingInput, "the PR needs a title: pass --title").WithField("title")
	}

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
	rep, version, err := e.scanChain(ctx, g, cfg, pm, steps, []leak.Text{
		{Where: "branch", Content: branch}, {Where: "pr_title", Content: title}, {Where: "pr_body", Content: body}})
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
	pl := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: planID, Kind: "promote", Actor: e.Actor, CreatedAt: e.now(),
		Pair: plan.Pair{Public: p.Public, Shadow: p.Shadow}, Feature: opt.Feature, Mode: mode, Curate: groups,
		Fingerprint: plan.Fingerprint{PublicMain: pm, FeatureHead: head, Overlay: O, GateCandidate: C},
		Publish: plan.Publish{Commit: P, Tree: T, Parent: pm, TargetBranch: branch, PR: plan.PR{Title: title, Body: body},
			Commits: chain, DroppedEmpty: dropped},
		Leak: plan.Leak{Findings: []map[string]any{}, Waived: planWaived(waived), Warnings: append([]contract.Warning{}, rep.Warnings...), ScannerVersion: version},
		CI:   plan.CI{RepoGuards: guardRep}}
	res.Plan = &pl
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O, "gate_candidate": C})
		pv.Add(preview.PushRef(p.Shadow, remotePlanRef(planID), "", P))
		if opt.PlanOnly {
			// promote --plan stores the plan and stops; nothing public.
			pv.Add(preview.WriteFile(filepath.Join(e.WS.PlansDir(), planID+".json")))
			res.Preview = pv
			return res, warns, nil
		}
		if err := e.previewPublish(ctx, pc, pv, branch, P); err != nil {
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
	_ = g.UpdateRef(ctx, planRef(planID), P)
	if opt.PlanOnly {
		return res, warns, nil
	}
	ar, aw, err := e.apply(ctx, pc, pl, opt.Yes)
	res.Applied = ar
	return res, addWarnings(warns, aw...), err
}

// ValidatePromoteOptions checks everything about a promote that needs no
// network, so a bad flag fails before any CI runs. An empty Mode means
// promote.default_mode, which is checked once the config is read.
func ValidatePromoteOptions(opt *PromoteOptions) error {
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
	if opt.Curate != "" {
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
