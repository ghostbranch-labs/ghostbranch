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
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type GateResult struct {
	Feature       string          `json:"feature"`
	FeatureHead   string          `json:"feature_head"`
	PublicMain    string          `json:"public_main"`
	Overlay       string          `json:"overlay"`
	Candidate     string          `json:"candidate"`
	PublishedTree string          `json:"published_tree"`
	ScannedAs     string          `json:"scanned_as"` // the mode the leak scan covered: preserve scans every commit, squash and curated the final tree
	Commits       []CommitInfo    `json:"commits"`    // the replayed commits, oldest first; empty when the replay didn't finish
	Checks        string          `json:"checks"`
	FailedChecks  []string        `json:"failed_checks"`
	Leak          string          `json:"leak"`
	Findings      []leak.Finding  `json:"findings"`
	Waived        []WaivedFinding `json:"waived"`
	// RepoGuards is what the gate did with the public workflows' repo
	// guards in C; nil when no candidate was built.
	RepoGuards *workflow.GuardReport `json:"repo_guards"`
	// ShadowPR is the gate PR in the shadow repo; nil when none was opened
	// (the gate failed before CI, or it was a dry run or a reused gate).
	ShadowPR *int `json:"shadow_pr"`
	// Reused is set by promote when it reuses a fresh green gate instead of re-running it.
	Reused  bool             `json:"reused"`
	Preview *preview.Preview `json:"preview"`

	// mapping is the gate's rebased chain, for promote to publish from.
	mapping []feature.GateCommit `json:"-"`
}

// CommitInfo is one feature commit as the gate replayed it, for an agent
// drafting a --curate file.
type CommitInfo struct {
	Source  string   `json:"source"`
	Subject string   `json:"subject"`
	Author  string   `json:"author"` // "Name <email>" as committed
	Files   []string `json:"files"`  // paths it changes, overlay paths left out
}

// gateCommitInfos describes each rebased commit, oldest first: its subject,
// its author as committed, and the paths it changes from the commit before
// it (public main for the first), overlay paths left out.
func gateCommitInfos(ctx context.Context, g *gitx.Git, cfg config.Config, pm string, commits []feature.GateCommit) ([]CommitInfo, error) {
	infos := make([]CommitInfo, len(commits))
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	prev := pm
	for i, c := range commits {
		msg, err := g.CommitMessage(ctx, c.Rebased)
		if err != nil {
			return nil, internalErr(err, "read a commit message")
		}
		author, _, err := g.CommitIdentities(ctx, c.Rebased)
		if err != nil {
			return nil, internalErr(err, "read a commit author")
		}
		names, err := g.DiffNames(ctx, prev, c.Rebased)
		if err != nil {
			return nil, internalErr(err, "list a commit's files")
		}
		files := []string{}
		for _, n := range names {
			if !set.Match(n) {
				files = append(files, n)
			}
		}
		infos[i] = CommitInfo{Source: c.Source, Subject: subject(msg), Author: author, Files: files}
		prev = c.Rebased
	}
	return infos, nil
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
		e.recordWith("gate", &p, name, "", summary, err, func(ent *ledger.Entry) {
			if res != nil {
				ent.Waivers = findingIDs(res.Waived)
			}
		})
	}()
	pc, err := e.open(ctx, "gate", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	// A standalone gate scans for the repo's promote.default_mode.
	res, w, err := e.gate(ctx, pc, name, "")
	return res, addWarnings(e.behind(ctx, pc), w...), err
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
	commits []feature.GateCommit
	// scannedAs is the mode the scan covered: preserve scans every rebased
	// commit, squash and curated only the final tree.
	scannedAs string
	// infos describe commits; nil until the replay succeeds.
	infos []CommitInfo
}

// build resolves the feature and public main, rebases, strips and scans as
// mode would publish: preserve scans every rebased commit, squash and
// curated the final tree (promote scans the exact chain it publishes in
// every mode). An empty mode means promote.default_mode. waiving is for
// waivers add, which only looks up tree findings: it leaves author
// identities out of the scan, so an unmapped author can't hide a waivable
// finding. Once the feature and public main are resolved it returns them
// even when a later step fails, so callers can report what they ran
// against.
func (e *Engine) build(ctx context.Context, pc *pairCtx, name, mode string, waiving bool) (*built, []contract.Warning, error) {
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
	if mode == "" {
		mode = cfg.Promote.DefaultMode
	}
	b := &built{cfg: cfg, meta: m, metaSHA: metaSHA, head: head, pm: pm, O: O, scannedAs: mode}
	e.progressf("replaying %s onto public main %s", name, short(pm))
	sources, err := g.SourceCommits(ctx, pm, head)
	if err != nil {
		return b, warns, internalErr(err, "list the feature's commits")
	}
	rebased, err := e.replay(ctx, pc, sources, pm)
	if err != nil {
		var ce *gitx.ConflictError
		if errors.As(err, &ce) {
			return b, warns, contract.New(contract.RebaseConflict,
				"the feature does not rebase cleanly onto public main; commit %s stopped on conflicts in: %s", short(ce.Commit), strings.Join(ce.Files, ", ")).
				WithDetails(map[string]any{"files": ce.Files, "commit": ce.Commit})
		}
		return b, warns, contract.From(err)
	}
	b.commits = make([]feature.GateCommit, len(sources))
	b.R = pm
	for i := range sources {
		b.commits[i] = feature.GateCommit{Source: sources[i], Rebased: rebased[i]}
		b.R = rebased[i]
	}
	infos, err := gateCommitInfos(ctx, g, cfg, pm, b.commits)
	if err != nil {
		return b, warns, err
	}
	b.infos = infos
	if b.T, b.entries, err = publishedTree(ctx, g, b.R, cfg); err != nil {
		return b, warns, namedMarkerErr(err, name)
	}
	e.progressf("scanning what %s would publish for leaks", b.scannedAs)
	if b.scannedAs == config.ModePreserve {
		// Stripping each older commit can hit a broken marker the tip no
		// longer has.
		b.report, err = e.scanHistory(ctx, g, cfg, b, !waiving)
	} else {
		b.report, _, err = e.scan(ctx, g, cfg, b.entries, pm, nil)
	}
	if err != nil {
		return b, warns, namedMarkerErr(err, name)
	}
	return b, addWarnings(warns, b.report.Warnings...), nil
}

// namedMarkerErr names the feature on a MARKER_INVALID error, so the agent
// is told to gate it again once the marker is fixed. Other errors pass
// through unchanged.
func namedMarkerErr(err error, name string) error {
	if ce := contract.From(err); ce != nil && ce.Code == contract.MarkerInvalid {
		if ce.Details == nil {
			ce.Details = map[string]any{}
		}
		ce.Details["feature"] = name
	}
	return err
}

// scanHistory scans every rebased commit as preserve would publish it: each
// one's stripped tree, its message, and, under keep or map, its author (so
// an author with no mapping is AUTHOR_UNMAPPED here). Only a preserve gate
// runs it; squash and curated gates scan the final tree, and promote scans
// whatever chain it builds.
func (e *Engine) scanHistory(ctx context.Context, g *gitx.Git, cfg config.Config, b *built, withAuthors bool) (leak.Report, error) {
	links, _, err := deriveChain(ctx, g, cfg, chainInput{mode: config.ModePreserve, pm: b.pm, T: b.T, tEntries: b.entries, commits: b.commits}, nil)
	if err != nil && contract.From(err).Code == contract.NothingToPublish {
		return leak.Report{Findings: []leak.Finding{}, Warnings: []contract.Warning{}}, nil
	}
	if err != nil {
		return leak.Report{}, err
	}
	var authors []gitx.Identity
	if withAuthors && cfg.Promote.AuthorEmail != config.AuthorNoreply {
		if authors, err = chainAuthors(cfg.Promote, config.ModePreserve, links, gitx.Identity{}); err != nil {
			return leak.Report{}, err
		}
	}
	steps := make([]scanStep, len(links))
	for i, l := range links {
		texts := []leak.Text{{Where: "commit_message", Content: l.message}}
		if authors != nil {
			texts = append(texts, leak.Text{Where: "author", Content: identityText(authors[i])})
		}
		steps[i] = scanStep{sources: l.sources, entries: l.entries, texts: texts}
	}
	rep, _, err := e.scanChain(ctx, g, cfg, b.pm, steps, nil)
	return rep, err
}

// gate builds and scans for mode ("" for promote.default_mode), then runs
// CI on the candidate and records the result.
func (e *Engine) gate(ctx context.Context, pc *pairCtx, name, mode string) (*GateResult, []contract.Warning, error) {
	g := pc.git
	b, warns, err := e.build(ctx, pc, name, mode, false)
	if b == nil {
		return nil, warns, err
	}
	cfg, m, metaSHA, head, pm, O := b.cfg, b.meta, b.metaSHA, b.head, b.pm, b.O
	infos := b.infos
	if infos == nil {
		infos = []CommitInfo{}
	}
	res := &GateResult{Feature: name, FeatureHead: head, PublicMain: pm, Overlay: O, ScannedAs: b.scannedAs, Commits: infos,
		Checks: feature.ChecksNotRun, FailedChecks: []string{}, Leak: feature.LeakNotRun, Findings: []leak.Finding{}, Waived: []WaivedFinding{}}
	if err != nil {
		return res, warns, err
	}
	res.mapping = b.commits
	res.Leak = feature.LeakClean
	C, guardRep, guards, err := e.candidate(ctx, g, b.R, b.entries, O, name, guardNames(pc), cfg.CI.RewriteRepoGuards)
	if err != nil {
		return res, warns, err
	}
	res.RepoGuards = &guardRep
	warns = addWarnings(warns, guardWarnings(cfg.CI.RewriteRepoGuards, guards, false)...)
	if files, err := e.managedFiles(ctx, g, O, cfg); err == nil {
		warns = addWarnings(warns, outdatedWarnings(files)...)
	}
	res.Candidate, res.PublishedTree = C, b.T
	blocking, waived, ww := e.splitFindings(pc.pair, b.report.Findings)
	warns = addWarnings(warns, ww...)
	res.Waived = waived
	if len(blocking) > 0 {
		res.Leak, res.Findings = feature.LeakFound, blocking
		if !e.DryRun {
			// The leak is the result that matters: a failure to record it
			// must not replace LEAK_FOUND with a lesser error.
			if err := e.recordGate(ctx, pc, m, metaSHA, res, b.commits); err != nil {
				warns = append(warns, contract.Warning{Code: contract.GateNotRecorded,
					Message: "the gate result could not be recorded in the feature metadata: " + contract.From(err).Message})
			}
		}
		ce := contract.From(leakErr(name, blocking))
		ce.Details["mode"] = b.scannedAs
		return res, warns, ce
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
	res.ShadowPR = &pr.Number

	e.progressf("waiting for CI on %s (shadow PR #%d: %s)", short(C), pr.Number, pr.URL)
	out, err := e.waitChecks(ctx, pc.pair.Shadow, C, cfg.Gate)
	if err != nil {
		return res, warns, err
	}
	if v, ok := e.Forge.(forge.CheckVisibility); ok && v.ChecksLimited(pc.pair.Shadow) {
		warns = append(warns, contract.Warning{Code: contract.ChecksLimited,
			Message: "the token cannot read check runs, so only GitHub Actions workflows and commit statuses were considered; checks from other CI apps were not seen"})
	}
	res.Checks, res.FailedChecks = out.status, out.failed
	if err := e.recordGate(ctx, pc, m, metaSHA, res, b.commits); err != nil {
		if out.status == feature.ChecksPassed {
			return res, warns, err // a green gate that isn't recorded can't be promoted
		}
		// The check outcome is what matters; don't replace it with a lesser error.
		warns = append(warns, contract.Warning{Code: contract.GateNotRecorded,
			Message: "the gate result could not be recorded in the feature metadata: " + contract.From(err).Message})
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

// guardNames are the repo names the guard rewrite uses. The gate, promote,
// apply and the audit all take them from GitHub's canonical names for the
// pair, never from the config or the names as typed locally, so the rewrite
// the gate made and the one assertChain recomputes are byte for byte the
// same, even for a plan made on another machine.
func guardNames(pc *pairCtx) workflow.Names {
	n := workflow.Names{Public: pc.public.FullName, Shadow: pc.shadow.FullName}
	if n.Public == "" || n.Shadow == "" {
		n = workflow.Names{Public: pc.pair.Public, Shadow: pc.pair.Shadow}
	}
	return n
}

func (e *Engine) recordGate(ctx context.Context, pc *pairCtx, m feature.Meta, metaSHA string, res *GateResult, commits []feature.GateCommit) error {
	m.SchemaVersion = feature.SchemaVersion
	m.Gate = &feature.Gate{FeatureHead: res.FeatureHead, PublicMain: res.PublicMain, Overlay: res.Overlay,
		Candidate: res.Candidate, PublishedTree: res.PublishedTree, ScannedAs: res.ScannedAs, Commits: commits, Checks: res.Checks, Leak: res.Leak, At: e.now()}
	return e.updateMeta(ctx, pc, m, metaSHA)
}

// replay cherry-picks commits onto onto in a throwaway worktree and returns
// the new SHAs, one per commit. The worktree is always removed; a failure
// to remove it is reported unless the replay itself already failed. A
// conflict comes back as *gitx.ConflictError for the caller to name.
func (e *Engine) replay(ctx context.Context, pc *pairCtx, commits []string, onto string) (out []string, err error) {
	dir := e.WS.WorkDir(pc.pair, pc.opID)
	wt, err := pc.git.WorktreeAdd(ctx, dir, onto)
	if err != nil {
		return nil, internalErr(err, "create a worktree")
	}
	defer func() {
		if rerr := pc.git.WorktreeRemove(ctx, dir); rerr != nil && err == nil {
			out, err = nil, internalErr(rerr, "remove the replay worktree")
		}
	}()
	return wt.CherryPick(ctx, commits, e.bot())
}
