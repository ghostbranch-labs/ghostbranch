package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type GateResult struct {
	Feature       string         `json:"feature"`
	FeatureHead   string         `json:"feature_head"`
	PublicMain    string         `json:"public_main"`
	Overlay       string         `json:"overlay"`
	Candidate     string         `json:"candidate"`
	PublishedTree string         `json:"published_tree"`
	Checks        string         `json:"checks"`
	FailedChecks  []string       `json:"failed_checks"`
	Leak          string         `json:"leak"`
	Findings      []leak.Finding `json:"findings"`
	ShadowPR      int            `json:"shadow_pr"`
	// Reused is set by promote when it reuses a fresh green gate instead of re-running it.
	Reused  bool             `json:"reused"`
	Preview *preview.Preview `json:"preview"`
}

const gatePRBody = "Automated Ghostbranch gate candidate: the feature rebased onto public main, plus the overlay. " +
	"CI on this PR decides the gate. Do not merge; Ghostbranch never merges into shadow main."

// Gate rebases a feature onto public main, scans what would be published,
// runs CI on the candidate through a draft shadow PR, and records the
// result. It never touches the public repo.
func (e *Engine) Gate(ctx context.Context, p workspace.Pair, name string) (res *GateResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		if res != nil {
			summary = fmt.Sprintf("checks %s, leak %s, candidate %s", res.Checks, res.Leak, short(res.Candidate))
		}
		e.record("gate", &p, name, "", summary, err)
	}()
	pc, err := e.open(ctx, "gate", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	return e.gate(ctx, pc, name)
}

// built is what the gate would publish right now: the feature rebased onto
// public main (R), stripped (T) and scanned. Gate and waivers add share it.
type built struct {
	cfg     config.Config
	meta    feature.Meta
	metaSHA string
	head    string
	pm      string
	O       string
	R       string
	T       string
	entries []gitx.TreeEntry
	report  leak.Report
}

// build resolves the feature and public main, rebases, strips and scans.
// Once the feature and public main are resolved it returns them even when a
// later step fails, so callers can report what they ran against.
func (e *Engine) build(ctx context.Context, pc *pairCtx, name string) (*built, []contract.Warning, error) {
	g := pc.git
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	m, metaSHA, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, warns, err
	}
	head, ok, err := g.Resolve(ctx, shadowRef(m.Branch))
	if err != nil {
		return nil, warns, internalErr(err, "read the feature branch")
	}
	if !ok {
		return nil, warns, contract.New(contract.FeatureNotFound,
			"feature %q has metadata, but its branch %q is gone from %s", name, m.Branch, pc.pair.Shadow).WithField("feature")
	}
	pm, err := g.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read public main")
	}
	b := &built{cfg: cfg, meta: m, metaSHA: metaSHA, head: head, pm: pm, O: O}
	e.progressf("rebasing %s onto public main %s", name, short(pm))
	if b.R, err = e.rebase(ctx, pc, head, pm); err != nil {
		return b, warns, err
	}
	if b.T, b.entries, err = publishedTree(ctx, g, b.R, cfg); err != nil {
		if ce := contract.From(err); ce.Code == contract.MarkerInvalid {
			ce.Details["feature"] = name
		}
		return b, warns, err
	}
	e.progressf("scanning the published tree for leaks")
	if b.report, _, err = e.scan(ctx, g, cfg, b.entries, pm, nil); err != nil {
		return b, warns, err
	}
	return b, addWarnings(warns, b.report.Warnings...), nil
}

func (e *Engine) gate(ctx context.Context, pc *pairCtx, name string) (*GateResult, []contract.Warning, error) {
	g := pc.git
	b, warns, err := e.build(ctx, pc, name)
	if b == nil {
		return nil, warns, err
	}
	cfg, m, metaSHA, head, pm, O := b.cfg, b.meta, b.metaSHA, b.head, b.pm, b.O
	res := &GateResult{Feature: name, FeatureHead: head, PublicMain: pm, Overlay: O,
		Checks: feature.ChecksNotRun, FailedChecks: []string{}, Leak: feature.LeakClean, Findings: []leak.Finding{}}
	if err != nil {
		return res, warns, err
	}
	C, err := e.candidate(ctx, g, b.R, b.entries, O, name)
	if err != nil {
		return res, warns, err
	}
	res.Candidate, res.PublishedTree = C, b.T
	findings := b.report.Findings
	if len(findings) > 0 {
		res.Leak, res.Findings = feature.LeakFound, findings
		if !e.DryRun {
			// The leak is the result that matters: a failure to record it
			// must not replace LEAK_FOUND with a lesser error.
			if err := e.recordGate(ctx, pc, m, metaSHA, res); err != nil {
				warns = append(warns, contract.Warning{Code: contract.GateNotRecorded,
					Message: "the gate result could not be recorded in the feature metadata: " + contract.From(err).Message})
			}
		}
		return res, warns, leakErr(findings)
	}
	if e.DryRun {
		old, _, _ := g.Resolve(ctx, shadowRef(gateBranch(name)))
		pv := preview.New(map[string]string{"public_main": pm, "feature_head": head, "overlay": O})
		pv.Add(preview.PushRef(pc.pair.Shadow, "refs/heads/"+gateBranch(name), old, C))
		pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Shadow, gateBranch(name), "main")
		if err != nil {
			return res, warns, forgeErr(err, "find the gate PR")
		}
		if pr == nil {
			pv.Add(preview.OpenPR(pc.pair.Shadow, gateBranch(name), "main", true))
		}
		pv.Add(preview.WaitChecks(pc.pair.Shadow, C), preview.PushRef(pc.pair.Shadow, remoteMetaRef(name), metaSHA, ""))
		res.Preview = pv
		return res, warns, nil
	}

	gateRef := "refs/heads/" + gateBranch(name)
	old, _, _ := g.Resolve(ctx, shadowRef(gateBranch(name)))
	e.progressf("pushing gate candidate %s", short(C))
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: gateRef, Expect: old}}}, C+":"+gateRef); err != nil {
		return res, warns, err
	}
	_ = g.UpdateRef(ctx, shadowRef(gateBranch(name)), C)
	pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Shadow, gateBranch(name), "main")
	if err != nil {
		return res, warns, forgeErr(err, "find the gate PR")
	}
	if pr == nil {
		created, err := e.Forge.CreatePR(ctx, pc.pair.Shadow, forge.NewPR{Title: "ghostbranch gate: " + name,
			Body: gatePRBody, Head: gateBranch(name), Base: "main", Draft: true})
		if err != nil {
			return res, warns, forgeErr(err, "open the gate PR")
		}
		pr = &created
	}
	res.ShadowPR = pr.Number

	e.progressf("waiting for CI on %s (shadow PR #%d)", short(C), pr.Number)
	out, err := e.waitChecks(ctx, pc.pair.Shadow, C, cfg.Gate)
	if err != nil {
		return res, warns, err
	}
	if v, ok := e.Forge.(forge.CheckVisibility); ok && v.ChecksLimited(pc.pair.Shadow) {
		warns = append(warns, contract.Warning{Code: contract.ChecksLimited,
			Message: "the token cannot read check runs, so only GitHub Actions workflows and commit statuses were considered; checks from other CI apps were not seen"})
	}
	res.Checks, res.FailedChecks = out.status, out.failed
	if err := e.recordGate(ctx, pc, m, metaSHA, res); err != nil {
		return res, warns, err
	}
	switch out.status {
	case feature.ChecksFailed:
		return res, warns, contract.New(contract.ChecksFailed, "checks failed on the gate candidate: %s", strings.Join(out.failed, ", ")).
			WithDetails(map[string]any{"failed": out.failed, "shadow_pr": pr.Number})
	case feature.ChecksNoCI:
		return res, warns, contract.New(contract.NoCI,
			"no checks appeared on shadow PR #%d within %s; check that Actions is enabled in %s", pr.Number, cfg.Gate.CIAppearTimeout, pc.pair.Shadow)
	case feature.ChecksTimeout:
		return res, warns, contract.New(contract.CITimeout, "checks did not finish within %s", cfg.Gate.CITimeout)
	}
	return res, warns, nil
}

func (e *Engine) recordGate(ctx context.Context, pc *pairCtx, m feature.Meta, metaSHA string, res *GateResult) error {
	m.Gate = &feature.Gate{FeatureHead: res.FeatureHead, PublicMain: res.PublicMain, Overlay: res.Overlay,
		Candidate: res.Candidate, PublishedTree: res.PublishedTree, Checks: res.Checks, Leak: res.Leak, At: e.now()}
	return e.updateMeta(ctx, pc, m, metaSHA)
}

// rebase replays head onto onto in a throwaway worktree and returns R. The
// worktree is always removed; a failure to remove it is reported unless
// the rebase itself already failed.
func (e *Engine) rebase(ctx context.Context, pc *pairCtx, head, onto string) (R string, err error) {
	dir := e.WS.WorkDir(pc.pair, pc.opID)
	wt, err := pc.git.WorktreeAdd(ctx, dir, head)
	if err != nil {
		return "", internalErr(err, "create a worktree")
	}
	defer func() {
		if rerr := pc.git.WorktreeRemove(ctx, dir); rerr != nil && err == nil {
			R, err = "", internalErr(rerr, "remove the rebase worktree")
		}
	}()
	if err := wt.Rebase(ctx, onto, e.bot()); err != nil {
		var ce *gitx.ConflictError
		if errors.As(err, &ce) {
			return "", contract.New(contract.RebaseConflict,
				"the feature does not rebase cleanly onto public main; resolve conflicts in: %s", strings.Join(ce.Files, ", ")).
				WithDetails(map[string]any{"files": ce.Files})
		}
		return "", internalErr(err, "rebase")
	}
	R, err = wt.MustResolve(ctx, "HEAD")
	if err != nil {
		return "", internalErr(err, "read the rebased head")
	}
	return R, nil
}
