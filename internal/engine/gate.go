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
	// Job names the gate's CI wait, for wait; nil when no candidate was
	// pushed (a leak, a conflict, a dry run, or a reused gate).
	Job *string `json:"job"`
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
		info, err := commitInfo(ctx, g, set, prev, c.Rebased)
		if err != nil {
			return nil, err
		}
		info.Source = c.Source
		infos[i] = info
		prev = c.Rebased
	}
	return infos, nil
}

// commitInfo describes commit sha: its subject, its author as committed, and
// the paths it changes from commit from, overlay paths left out. Source is
// sha; the caller sets it when sha is a rebased copy.
func commitInfo(ctx context.Context, g *gitx.Git, set overlay.Set, from, sha string) (CommitInfo, error) {
	msg, err := g.CommitMessage(ctx, sha)
	if err != nil {
		return CommitInfo{}, internalErr(err, "read a commit message")
	}
	author, _, err := g.CommitIdentities(ctx, sha)
	if err != nil {
		return CommitInfo{}, internalErr(err, "read a commit author")
	}
	names, err := g.DiffNames(ctx, from, sha)
	if err != nil {
		return CommitInfo{}, internalErr(err, "list a commit's files")
	}
	files := []string{}
	for _, n := range names {
		if !set.Match(n) {
			files = append(files, n)
		}
	}
	return CommitInfo{Source: sha, Subject: subject(msg), Author: author, Files: files}, nil
}

const gatePRBody = "Automated Ghostbranch gate candidate: the feature rebased onto public main, plus the overlay. " +
	"CI on this PR decides the gate. Do not merge; Ghostbranch never merges into shadow main."

// Gate rebases a feature onto public main, scans what would be published,
// runs CI on the candidate through a draft shadow PR, and records the
// result. The pair lock is free while CI runs. It never touches the public
// repo.
func (e *Engine) Gate(ctx context.Context, p workspace.Pair, name string) (*GateResult, []contract.Warning, error) {
	return e.runGate(ctx, p, name, false)
}

// GateDetached is gate --detach: it records the gate as a pending job and
// returns without waiting on CI; wait finishes the job.
func (e *Engine) GateDetached(ctx context.Context, p workspace.Pair, name string) (*GateResult, []contract.Warning, error) {
	return e.runGate(ctx, p, name, true)
}

// runGate is the body Gate and GateDetached share.
func (e *Engine) runGate(ctx context.Context, p workspace.Pair, name string, detach bool) (res *GateResult, warns []contract.Warning, err error) {
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
	defer func() { err = interrupted(ctx, res, name, err) }()
	pc, err := e.open(ctx, "gate", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	// A pending job for exactly these inputs is reused, not gated again. A
	// dry run never reuses one: finishing it would wait on CI and write.
	var pending *feature.Gate
	if !e.DryRun {
		if pending, err = e.pendingFor(ctx, pc, name, ""); err != nil {
			return nil, nil, err
		}
	}
	var w []contract.Warning
	if pending != nil {
		if res, err = e.resultFromRecord(ctx, pc, name, *pending); err != nil {
			return nil, nil, err
		}
		if !detach {
			pc.release()
			res, w, err = e.finish(ctx, pc, name, res, *pending, waitOpts{})
		}
	} else {
		// A standalone gate scans for the repo's promote.default_mode.
		res, w, err = e.gate(ctx, pc, name, "", gateHow{detach: detach, release: !detach})
	}
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

// gateHow is how a gate runs past its start: detach returns once the
// pending job is recorded; release frees the pair lock during the CI wait.
// Promote does neither: it holds the lock from gate to publish.
type gateHow struct{ detach, release bool }

// gate builds and scans for mode ("" for promote.default_mode), pushes the
// candidate and records it as a pending job, then (unless detached) waits
// on CI and records the result.
func (e *Engine) gate(ctx context.Context, pc *pairCtx, name, mode string, how gateHow) (*GateResult, []contract.Warning, error) {
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
			rw, err := e.writeGate(ctx, pc, m, metaSHA, e.gateRecord(res, b.commits))
			warns = addWarnings(warns, rw...)
			if err != nil {
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
		// The pending record, the wait, then the final record over it.
		pv.Add(preview.PushRef(pc.pair.Shadow, remoteMetaRef(name), metaSHA, ""), preview.WaitChecks(pc.pair.Shadow, C),
			preview.PushRef(pc.pair.Shadow, remoteMetaRef(name), "", ""))
		res.Preview = pv
		return res, warns, nil
	}

	gateRef := "refs/heads/" + gateBranch(name)
	old, _, _ := g.Resolve(ctx, shadowRef(gateBranch(name)))
	e.progressf("pushing gate candidate %s", short(C))
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: gateRef, Expect: old}}}, C+":"+gateRef); err != nil {
		return res, warns, err
	}
	warns = addWarnings(warns, e.updateMirror(ctx, g, shadowRef(gateBranch(name)), C)...)
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
	res.ShadowPR, res.Checks = &pr.Number, feature.ChecksPending
	job := jobID(name, C)
	res.Job = &job
	pending := e.gateRecord(res, b.commits)
	pending.ShadowPR, pending.StartedAt = pr.Number, e.now()
	pending.At = pending.StartedAt
	rw, err := e.writeGate(ctx, pc, m, metaSHA, pending)
	warns = addWarnings(warns, rw...)
	if err != nil {
		res.Job = nil // no job was recorded, so there is nothing to wait on
		return res, warns, err
	}
	if how.detach {
		return res, warns, nil
	}
	if how.release {
		pc.release()
	}
	r, w, err := e.finish(ctx, pc, name, res, pending, waitOpts{})
	return r, addWarnings(warns, w...), err
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

// gateRecord is the metadata record of res's gate, stamped now.
func (e *Engine) gateRecord(res *GateResult, commits []feature.GateCommit) feature.Gate {
	return feature.Gate{FeatureHead: res.FeatureHead, PublicMain: res.PublicMain, Overlay: res.Overlay,
		Candidate: res.Candidate, PublishedTree: res.PublishedTree, ScannedAs: res.ScannedAs, Commits: commits,
		Checks: res.Checks, FailedChecks: res.FailedChecks, Leak: res.Leak, At: e.now()}
}

// writeGate records g as the feature's gate, leased on metaSHA.
func (e *Engine) writeGate(ctx context.Context, pc *pairCtx, m feature.Meta, metaSHA string, g feature.Gate) ([]contract.Warning, error) {
	m.SchemaVersion = feature.SchemaVersion
	m.Gate = &g
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
