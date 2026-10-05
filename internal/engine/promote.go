package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type PromoteOptions struct {
	Feature  string
	Mode     string // only "squash" in this version
	Message  string
	Title    string
	Body     string
	Branch   string // public branch name; defaults to the feature name
	PlanOnly bool
	Yes      bool
}

type PromoteResult struct {
	Gate     *GateResult    `json:"gate"`
	Plan     *plan.Plan     `json:"plan"`
	PlanPath string         `json:"plan_path"`
	Findings []leak.Finding `json:"findings"`
	Applied  *ApplyResult   `json:"applied"`
	DryRun   bool           `json:"dry_run"`
}

const editorTemplate = "\n# Write the commit message for the public squash commit of feature %q.\n" +
	"# The first line becomes the PR title and the rest the PR body.\n" +
	"# Lines starting with '#' are ignored. An empty message aborts the promote.\n"

// Promote gates the feature (reusing a fresh green gate), builds the one
// public commit P, scans everything that would go public, saves a plan,
// and applies it once approved.
func (e *Engine) Promote(ctx context.Context, p workspace.Pair, opt PromoteOptions) (res *PromoteResult, warns []contract.Warning, err error) {
	planID := ""
	defer func() {
		summary := ""
		if res != nil && res.Applied != nil {
			summary = fmt.Sprintf("published %s as PR #%d", res.Applied.PublicBranch, res.Applied.PublicPR)
		} else if res != nil && res.Plan != nil {
			summary = "plan " + res.Plan.ID + " saved"
		}
		e.record("promote", &p, opt.Feature, planID, summary, err)
	}()
	if opt.Mode == "" {
		opt.Mode = "squash"
	}
	if opt.Mode != "squash" {
		return nil, nil, contract.New(contract.BadFlag, "--mode %s is not supported in this version; use squash", opt.Mode).WithField("mode")
	}
	res = &PromoteResult{Findings: []leak.Finding{}, DryRun: e.DryRun}
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

	// A fresh green gate is reused only if its candidate is in the mirror
	// (the gate branch may have been deleted by hand).
	candidateLocal := false
	if m.Gate != nil && m.Gate.Candidate != "" {
		_, candidateLocal, _ = g.Resolve(ctx, m.Gate.Candidate)
	}
	if m.Gate.Passed() && m.Gate.FreshFor(head, pm, O) && candidateLocal {
		res.Gate = &GateResult{Feature: opt.Feature, FeatureHead: head, PublicMain: pm, Overlay: O,
			Candidate: m.Gate.Candidate, PublishedTree: m.Gate.PublishedTree, Checks: m.Gate.Checks,
			FailedChecks: []string{}, Leak: m.Gate.Leak, Findings: []leak.Finding{}, Reused: true, DryRun: e.DryRun}
	} else {
		e.progressf("the gate for %s is missing or stale; running it", opt.Feature)
		gr, gw, err := e.gate(ctx, pc, opt.Feature)
		res.Gate = gr
		warns = append(warns, gw...)
		if err != nil {
			return res, warns, err
		}
	}
	C, T := res.Gate.Candidate, res.Gate.PublishedTree
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	R, err := g.MustResolve(ctx, C+"^")
	if err != nil {
		return res, warns, internalErr(err, "read the rebased feature")
	}
	T2, entries, err := publishedTree(ctx, g, R, set)
	if err != nil {
		return res, warns, err
	}
	if T2 != T {
		return res, warns, contract.New(contract.Internal, "the gated tree %s no longer matches its candidate (%s)", short(T), short(T2))
	}

	msg, err := e.message(opt)
	if err != nil {
		return res, warns, err
	}
	title, body := opt.Title, opt.Body
	if title == "" {
		t, b, _ := strings.Cut(msg, "\n")
		title = strings.TrimSpace(t)
		if body == "" {
			body = strings.TrimSpace(b)
		}
	}
	branch := opt.Branch
	if branch == "" {
		branch = opt.Feature
	}
	if err := feature.ValidateName(branch, "branch"); err != nil {
		return res, warns, err
	}

	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return res, warns, forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: u.Login, Email: forge.NoreplyEmail(u), When: e.now()}
	P, err := g.CommitTree(ctx, T, []string{pm}, msg, who, who)
	if err != nil {
		return res, warns, internalErr(err, "write the public commit")
	}
	if err := assertPublish(ctx, g, P, T, C, pm, set); err != nil {
		return res, warns, err
	}
	findings, version, err := e.scan(ctx, g, set, entries, publishTexts(msg, who.Name, who.Email, branch, title, body))
	if err != nil {
		return res, warns, err
	}
	if len(findings) > 0 {
		res.Findings = findings
		return res, warns, leakErr(findings)
	}

	if planID, err = plan.NewID(e.now(), e.Rand); err != nil {
		return res, warns, internalErr(err, "make a plan id")
	}
	pl := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: planID, Kind: "promote", Actor: e.Actor, CreatedAt: e.now(),
		Pair: plan.Pair{Public: p.Public, Shadow: p.Shadow}, Feature: opt.Feature, Mode: opt.Mode,
		Fingerprint: plan.Fingerprint{PublicMain: pm, FeatureHead: head, Overlay: O, GateCandidate: C},
		Publish: plan.Publish{Commit: P, Tree: T, Parent: pm,
			Author:       plan.Identity{Name: who.Name, Email: who.Email, Date: who.When},
			TargetBranch: branch, PR: plan.PR{Title: title, Body: body}},
		Leak:          plan.Leak{Findings: []map[string]any{}, ScannerVersion: version},
		RequiresHuman: cfg.Promote.RequireHuman}
	res.Plan = &pl
	if e.DryRun {
		return res, warns, nil
	}
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: remotePlanRef(planID)}}}, P+":"+remotePlanRef(planID)); err != nil {
		return res, warns, err
	}
	_ = g.UpdateRef(ctx, planRef(planID), P)
	if res.PlanPath, err = plan.Save(e.WS.PlansDir(), pl); err != nil {
		return res, warns, internalErr(err, "save the plan")
	}
	if opt.PlanOnly {
		return res, warns, nil
	}
	files, _ := g.DiffNames(ctx, pm, P)
	if err := e.approve(pl, files, opt.Yes); err != nil {
		return res, warns, err
	}
	ar, aw, err := e.apply(ctx, pc, pl)
	res.Applied = ar
	return res, append(warns, aw...), err
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

// publishTexts is every piece of text that goes public besides the tree.
func publishTexts(msg, name, email, branch, title, body string) []leak.Text {
	return []leak.Text{
		{Where: "commit_message", Content: msg},
		{Where: "author", Content: name + " <" + email + ">"},
		{Where: "branch", Content: branch},
		{Where: "pr_title", Content: title},
		{Where: "pr_body", Content: body},
	}
}

// approve decides whether a plan may be applied now. Agents never approve
// plans that require a human.
func (e *Engine) approve(pl plan.Plan, files []string, yes bool) error {
	needs := func(why string) error {
		return contract.New(contract.ApprovalRequired, "%s; plan %s is saved: run `ghostbranch apply %s --yes` to publish it", why, pl.ID, pl.ID).
			WithDetails(map[string]any{"plan_id": pl.ID})
	}
	if pl.RequiresHuman && IsAgent(e.Actor) {
		return needs("this plan needs a human to approve it")
	}
	if yes {
		return nil
	}
	if e.Prompt == nil {
		return needs("confirmation is needed and there is no terminal to ask in")
	}
	ok, err := e.Prompt(planSummary(pl, files))
	if err != nil {
		return contract.Wrap(contract.MissingInput, err, "read the confirmation")
	}
	if !ok {
		return needs("not confirmed")
	}
	return nil
}

// planSummary is the "what goes public" view shown before confirming.
func planSummary(pl plan.Plan, files []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "About to publish to %s:\n", pl.Pair.Public)
	fmt.Fprintf(&b, "  branch   %s\n", pl.Publish.TargetBranch)
	fmt.Fprintf(&b, "  commit   %s (parent %s)\n", short(pl.Publish.Commit), short(pl.Publish.Parent))
	fmt.Fprintf(&b, "  author   %s <%s>\n", pl.Publish.Author.Name, pl.Publish.Author.Email)
	fmt.Fprintf(&b, "  PR title %s\n", pl.Publish.PR.Title)
	fmt.Fprintf(&b, "  files    %d changed\n", len(files))
	for i, f := range files {
		if i == 20 {
			fmt.Fprintf(&b, "           … and %d more\n", len(files)-20)
			break
		}
		fmt.Fprintf(&b, "           %s\n", f)
	}
	fmt.Fprintf(&b, "Plan %s", pl.ID)
	return b.String()
}
