package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

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
	msg, err := e.message(opt)
	if err != nil {
		return nil, nil, err
	}
	title, body := prTexts(msg, opt.Title, opt.Body)
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

	// A fresh green gate is reused only while the shadow gate branch still
	// points at its candidate; apply checks that branch, so a deleted or moved
	// one would make every plan stale.
	tip, tipOK, _ := g.Resolve(ctx, shadowRef(gateBranch(opt.Feature)))
	candidateLive := m.Gate != nil && m.Gate.Candidate != "" && tipOK && tip == m.Gate.Candidate
	fresh := m.Gate.Passed() && m.Gate.FreshFor(head, pm, O) && candidateLive
	if fresh {
		// A gate recorded by a binary that stripped differently (an upgrade
		// that added internal markers) is stale: gate again rather than
		// publish a tree this binary would not build.
		R, err := g.MustResolve(ctx, m.Gate.Candidate+"^")
		if err != nil {
			fresh = false
		} else if T2, _, err := publishedTree(ctx, g, R, cfg); err != nil || T2 != m.Gate.PublishedTree {
			fresh = false
		}
	}
	switch {
	case fresh:
		res.Gate = &GateResult{Feature: opt.Feature, FeatureHead: head, PublicMain: pm, Overlay: O,
			Candidate: m.Gate.Candidate, PublishedTree: m.Gate.PublishedTree, Checks: m.Gate.Checks,
			FailedChecks: []string{}, Leak: m.Gate.Leak, Findings: []leak.Finding{}, Waived: []WaivedFinding{}, Reused: true}
	case e.DryRun:
		// Running the gate would push; say so and stop, rather than return
		// a plan whose checks never ran.
		res.Preview = preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O})
		res.Preview.Add(preview.RunGate(opt.Feature))
		return res, warns, nil
	default:
		e.progressf("the gate for %s is missing or stale; running it", opt.Feature)
		gr, gw, err := e.gate(ctx, pc, opt.Feature)
		res.Gate = gr
		warns = addWarnings(warns, gw...)
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
	T2, entries, err := publishedTree(ctx, g, R, cfg)
	if err != nil {
		return res, warns, err
	}
	if T2 != T {
		return res, warns, contract.New(contract.Internal, "the gated tree %s no longer matches its candidate (%s)", short(T), short(T2))
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
	ident := who.Name + " <" + who.Email + ">"
	rep, version, err := e.scan(ctx, g, cfg, entries, pm, publishTexts(msg, ident, ident, branch, title, body))
	if err != nil {
		return res, warns, err
	}
	blocking, waived, ww := e.splitFindings(p, rep.Findings)
	warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
	res.Waived = waived
	if len(blocking) > 0 {
		res.Findings = blocking
		return res, warns, leakErr(opt.Feature, blocking)
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
		Leak: plan.Leak{Findings: []map[string]any{}, Waived: planWaived(waived), Warnings: append([]contract.Warning{}, rep.Warnings...), ScannerVersion: version}}
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
	ar, aw, err := e.apply(ctx, pc, pl, opt.Yes)
	res.Applied = ar
	return res, append(warns, aw...), err
}

// ValidatePromoteOptions checks everything about a promote that needs no
// network, so a bad flag fails before any CI runs. It sets the default mode.
func ValidatePromoteOptions(opt *PromoteOptions) error {
	if opt.Mode == "" {
		opt.Mode = "squash"
	}
	if opt.Mode != "squash" {
		return contract.New(contract.BadFlag, "--mode %s is not supported in this version; use squash", opt.Mode).WithField("mode")
	}
	branch := opt.Branch
	if branch == "" {
		branch = opt.Feature
	}
	return feature.ValidateName(branch, "branch")
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

// publishTexts is every piece of text that goes public besides the tree.
// author and committer are "Name <email>".
func publishTexts(msg, author, committer, branch, title, body string) []leak.Text {
	return []leak.Text{
		{Where: "commit_message", Content: msg},
		{Where: "author", Content: author},
		{Where: "committer", Content: committer},
		{Where: "branch", Content: branch},
		{Where: "pr_title", Content: title},
		{Where: "pr_body", Content: body},
	}
}
