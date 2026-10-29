package engine

// A job is a gate whose CI wait another process can finish. The gate's
// start half builds, scans and pushes the candidate, opens the gate PR, and
// records a pending gate in the feature's metadata; that record is the job,
// and its id is gate/<feature>/<the candidate's first 12 hex>. The finish
// half waits on CI and records the outcome. Gate runs both halves, with the
// pair lock released during the wait; gate --detach runs only the start, and
// wait runs the finish later. The record is the truth: the finish writes
// only over the same pending job, leased on the commit it just read, and a
// finished record is never overwritten.

import (
	"context"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const (
	jobPrefix = "gate/"
	// finishLockWait is how long a finished CI wait waits for the pair lock
	// to record its result; finishLockPoll is how often it tries.
	finishLockWait = 2 * time.Minute
	finishLockPoll = 2 * time.Second
)

// JobRef is a parsed job id: gate/<feature>/<the candidate's first 12 hex>.
type JobRef struct{ Feature, Candidate string }

var jobCandidate = regexp.MustCompile(`^[0-9a-f]{12}$`)

func jobID(feature, candidate string) string { return jobPrefix + feature + "/" + short(candidate) }

// ParseJob parses a job id as gate --detach printed it.
func ParseJob(id string) (JobRef, error) {
	bad := contract.New(contract.BadFlag, "%q is not a job id: want gate/<feature>/<12 hex>, as gate --detach printed it", id).WithField("job")
	rest, ok := strings.CutPrefix(id, jobPrefix)
	i := strings.LastIndex(rest, "/")
	if !ok || i <= 0 {
		return JobRef{}, bad
	}
	f, c := rest[:i], rest[i+1:]
	if !jobCandidate.MatchString(c) || feature.ValidateName(f, "job", "main") != nil {
		return JobRef{}, bad
	}
	return JobRef{Feature: f, Candidate: c}, nil
}

func (j JobRef) matches(g *feature.Gate) bool {
	return g != nil && strings.HasPrefix(g.Candidate, j.Candidate)
}

// fetchMeta refreshes everything under the shadow repo's refs/ghostbranch/*
// (the feature metadata and the plan refs), pruned, so a caller
// mid-operation re-reads records. It never fetches the branches or public
// main, which stay where the operation read them.
func (e *Engine) fetchMeta(ctx context.Context, g *gitx.Git) error {
	if err := g.Fetch(ctx, remoteShadow, "+refs/ghostbranch/*:refs/shadow-meta/*"); err != nil {
		return remoteErr(err, "fetch the shadow repo's feature metadata")
	}
	return nil
}

// relock takes the pair lock back for a job's final write, retrying while
// another operation holds it, then re-reads the metadata. A caller that
// still holds the lock only re-reads.
func (e *Engine) relock(ctx context.Context, pc *pairCtx, job string) error {
	for waited := time.Duration(0); !pc.held && pc.lockPath != ""; waited += finishLockPoll {
		drop, err := workspace.Lock(pc.lockPath, e.Now(), pc.op)
		if err == nil {
			pc.drop, pc.held = drop, true
			break
		}
		if ce := contract.From(err); ce.Code != contract.Locked || waited >= finishLockWait {
			// Only a busy lock is worth retrying; any other lock error is a
			// plain error (owner, 2026-10-09). Either way the job is named.
			ce.Retryable = ce.Code == contract.Locked
			if ce.Details == nil {
				ce.Details = map[string]any{}
			}
			ce.Details["job"] = job
			return ce
		}
		if e.Sleep(ctx, finishLockPoll) != nil {
			return interruptedErr(job, "")
		}
	}
	return e.fetchMeta(ctx, pc.git)
}

// outcomeErr is the error a settled CI outcome stands for; nil when it
// passed. workflows lists the candidate's workflows that run on its PR
// (prWorkflows, read only for NO_CI); known is false when the candidate
// couldn't be read.
func outcomeErr(out checkOutcome, pr int, cfg config.Gate, shadow string, workflows func() (files []string, known bool)) error {
	switch out.status {
	case feature.ChecksFailed:
		return contract.New(contract.ChecksFailed, "checks failed on the gate candidate: %s", strings.Join(out.failed, ", ")).
			WithDetails(map[string]any{"failed": out.failed, "shadow_pr": pr})
	case feature.ChecksNoCI:
		wf, known := workflows()
		if known && len(wf) == 0 {
			return contract.New(contract.NoCI,
				"no checks appeared on shadow PR #%d within %s, and the gate candidate has no workflow under .github/workflows that runs on this PR: add CI to the public repo, or, if your CI is an external app, install it on %s",
				pr, shortDur(appearTimeout(cfg, true)), shadow).WithDetails(map[string]any{"reason": "no_workflow", "no_workflows": true, "shadow_pr": pr})
		}
		// An unreadable candidate gives no reason: which one holds is unknown.
		details := map[string]any{"shadow_pr": pr}
		if known {
			details["reason"], details["no_workflows"] = "actions_disabled_or_untriggered", false
		}
		return contract.New(contract.NoCI,
			"no checks appeared on shadow PR #%d within %s; check that Actions is enabled in %s and that the PR matches the workflows' pull_request filters", pr, shortDur(cfg.CIAppearTimeout), shadow).
			WithDetails(details)
	case feature.ChecksTimeout:
		return contract.New(contract.CITimeout, "checks did not finish within %s", cfg.CITimeout)
	}
	return nil
}

// prWorkflows lists the workflow files in candidate C that run on its gate
// PR, whose head is branch (workflow.File.RunsOnPR), leaving out
// Ghostbranch's own (ghostbranch-*.yml). A file that doesn't parse counts
// as running, so a doubt keeps the full wait. known is false when C can't
// be read. The CI wait and NO_CI's reason both use it, so they agree.
//
// Reading the candidate is a known approximation: GitHub runs a
// pull_request_target workflow from the base branch's copy (the shadow
// repo's main), not the PR's. The two
// differ only when the feature itself adds, drops or changes such a
// workflow, since the candidate starts from public main.
func prWorkflows(ctx context.Context, g *gitx.Git, names workflow.Names, C, branch string) (files []string, known bool) {
	es, err := g.ListTree(ctx, C)
	if err != nil {
		return nil, false
	}
	var paths, shas []string
	for _, en := range es {
		if en.Type == "blob" && workflow.IsWorkflowPath(en.Path) && !strings.HasPrefix(path.Base(en.Path), "ghostbranch-") {
			paths, shas = append(paths, en.Path), append(shas, en.SHA)
		}
	}
	blobs, err := g.ReadBlobs(ctx, shas)
	if err != nil {
		return nil, false
	}
	files = []string{}
	for i, p := range paths {
		if f, err := workflow.Parse(p, blobs[shas[i]], names); err != nil || f.RunsOnPR(branch) {
			files = append(files, p)
		}
	}
	return files, true
}

// finish is a job's second half: it waits on CI for the pending gate and
// records the outcome over that same pending record. A record another
// waiter already finished is kept and reported; a record a newer gate
// replaced is JOB_STALE and nothing is written. At o.deadline it returns
// res still pending.
func (e *Engine) finish(ctx context.Context, pc *pairCtx, name string, res *GateResult, pending feature.Gate, o waitOpts) (*GateResult, []contract.Warning, error) {
	cfg, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return res, warns, err
	}
	shadow, C, job := pc.pair.Shadow, pending.Candidate, jobID(name, pending.Candidate)
	url := ""
	if pr, err := e.Forge.GetPR(ctx, shadow, pending.ShadowPR); err == nil {
		url = pr.URL
	}
	e.notef(progress.Info, "CI runs on shadow PR #%d: %s (gate candidate %s)", pending.ShadowPR, url, short(C))
	wf, known := prWorkflows(ctx, pc.git, guardNames(pc), C, gateBranch(name))
	noWorkflows := known && len(wf) == 0
	o.job, o.noWorkflows, o.shadow = job, noWorkflows, shadow
	title := "Waiting for CI to start"
	if noWorkflows {
		title = startText(cfg.Gate, o, 0)
	}
	ci := e.startStep("%s", title)
	e.event(o, JobEvent{Type: EventWaiting, Candidate: C, ShadowPR: pending.ShadowPR, ShadowPRURL: url, CITimeout: cfg.Gate.CITimeout.String(),
		NoWorkflows: &noWorkflows, CIAppearTimeout: appearTimeout(cfg.Gate, noWorkflows).String()})
	out, err := e.waitChecks(ctx, shadow, C, cfg.Gate, waitOpts{start: pending.StartedAt, deadline: o.deadline, job: job, emit: o.emit, step: ci,
		noWorkflows: noWorkflows, shadow: shadow})
	if err != nil {
		return res, warns, err // the step stays running; the CLI marks it failed
	}
	// The text before each Fail is the step's news: the checklist's ✗ line
	// and plain mode's failed: line show it.
	switch out.status {
	case feature.ChecksPending:
		ci.Done("CI still running; the job stays pending")
	case feature.ChecksPassed:
		ci.Done("CI passed")
	case feature.ChecksFailed:
		ci.Update("CI failed: " + strings.Join(out.failed, ", "))
		ci.Fail()
	case feature.ChecksNoCI:
		ci.Update("No CI checks appeared")
		ci.Fail()
	default: // timeout
		ci.Update("CI didn't finish in time")
		ci.Fail()
	}
	if v, ok := e.Forge.(forge.CheckVisibility); ok && v.ChecksLimited(shadow) {
		warns = append(warns, contract.Warning{Code: contract.ChecksLimited,
			Message: "the token cannot read check runs, so only GitHub Actions workflows and commit statuses were considered; checks from other CI apps were not seen"})
	}
	if out.status == feature.ChecksPending {
		return res, warns, nil
	}
	res.Checks, res.FailedChecks = out.status, out.failed
	rw, err := e.recordFinish(ctx, pc, name, res, JobRef{Feature: name, Candidate: short(C)}, job)
	warns = addWarnings(warns, rw...)
	if err != nil {
		if ctx.Err() != nil {
			// A cancel stopped the write: the record is still pending, so the
			// job is named and wait can finish it.
			res.Checks, res.FailedChecks = feature.ChecksPending, []string{}
			return res, warns, interruptedErr(job, "")
		}
		switch contract.From(err).Code {
		case contract.JobStale, contract.JobNotFound, contract.FeatureNotFound, contract.Interrupted:
			return res, warns, err
		}
		if res.Checks == feature.ChecksPassed {
			// A green gate that isn't recorded can't be promoted. The record
			// is still pending, so the error names the job: wait records it
			// (or reports it, if the write landed after all).
			ce := contract.From(err)
			if ce.Details == nil {
				ce.Details = map[string]any{}
			}
			ce.Details["job"] = job
			return res, warns, ce
		}
		// The check outcome is what matters; don't replace it with a lesser error.
		warns = append(warns, contract.Warning{Code: contract.GateNotRecorded,
			Message: "the gate result could not be recorded in the feature metadata: " + contract.From(err).Message +
				"; run ghostbranch wait " + job + " to record it",
			Details: map[string]any{"job": job}})
	}
	return res, warns, outcomeErr(checkOutcome{status: res.Checks, failed: res.FailedChecks}, pending.ShadowPR, cfg.Gate, shadow,
		func() ([]string, bool) { return wf, known })
}

// recordFinish takes the lock back and records res's outcome over the
// pending job ref, re-reading once when someone else moved the metadata.
// A job another waiter finished sets res from its record instead.
func (e *Engine) recordFinish(ctx context.Context, pc *pairCtx, name string, res *GateResult, ref JobRef, job string) ([]contract.Warning, error) {
	if err := e.relock(ctx, pc, job); err != nil {
		return nil, err
	}
	for attempt := 0; ; attempt++ {
		m, sha, err := e.readMeta(ctx, pc, name)
		if err != nil {
			return nil, err
		}
		switch {
		case m.Gate == nil:
			return nil, contract.New(contract.JobNotFound, "feature %q has no gate record, so the job %s is gone; gate again", name, job).
				WithDetails(map[string]any{"job": job})
		case !ref.matches(m.Gate):
			return nil, staleErr(name, job, m.Gate, "; its result was not recorded")
		case m.Gate.Checks == feature.ChecksPending && m.Gate.ShadowPR == 0:
			// An older binary rewrote the pending record and dropped its
			// shadow PR: it is no job, and no other waiter finished it.
			return nil, contract.New(contract.JobNotFound,
				"feature %q's pending gate record has no shadow PR (an older ghostbranch rewrote it), so the job %s can't be recorded; gate it again", name, job).
				WithDetails(map[string]any{"feature": name, "job": job})
		case !m.Gate.Pending():
			// Another waiter recorded this job first: its record stands.
			res.Checks, res.FailedChecks = m.Gate.Checks, m.Gate.FailedChecks
			if res.FailedChecks == nil {
				res.FailedChecks = []string{}
			}
			return nil, nil
		}
		final := *m.Gate
		final.Checks, final.FailedChecks, final.At = res.Checks, res.FailedChecks, e.now()
		m.Gate = &final
		m.SchemaVersion = feature.SchemaVersion
		_, warns, err := e.updateMeta(ctx, pc, m, sha)
		if err == nil || attempt > 0 || contract.From(err).Code != contract.RefMoved {
			return warns, err
		}
		if err := e.fetchMeta(ctx, pc.git); err != nil {
			return nil, err
		}
	}
}

// staleErr is JOB_STALE for job, which the record g replaced; current_job
// names g when it is still pending, so the agent can wait on that instead.
func staleErr(name, job string, g *feature.Gate, what string) error {
	current := ""
	if g.Pending() {
		current = jobID(name, g.Candidate)
	}
	return contract.New(contract.JobStale, "a newer gate replaced the job %s%s", job, what).
		WithDetails(map[string]any{"job": job, "current_job": current})
}

// interrupted turns an error a cancel caused into INTERRUPTED. A pending
// job in res is named, so wait can finish it; otherwise the feature is. A
// settled outcome (a gate failure: failed or missing CI, a leak, a broken
// marker) is the answer, cancel or not, and passes through unchanged.
func interrupted(ctx context.Context, res *GateResult, name string, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	if c := contract.From(err).Code; c == contract.Interrupted || c.Exit() == contract.ExitGateFailed {
		return err
	}
	job := ""
	if res != nil && res.Job != nil && res.Checks == feature.ChecksPending {
		job = *res.Job
	}
	return interruptedErr(job, name)
}

// resultFromRecord is the GateResult a gate record stands for, as wait and
// a reused job report it, with the config's warnings, which a reused gate
// reports as a fresh one would. RepoGuards stays nil: the record doesn't
// keep it.
func (e *Engine) resultFromRecord(ctx context.Context, pc *pairCtx, name string, g feature.Gate) (*GateResult, []contract.Warning, error) {
	cfg, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	from := g.PublicMain
	if g.Base != "" {
		from = g.Base
	}
	infos, err := gateCommitInfos(ctx, pc.git, cfg, from, g.Commits)
	if err != nil {
		return nil, warns, err
	}
	failed := g.FailedChecks
	if failed == nil {
		failed = []string{}
	}
	res := &GateResult{Feature: name, FeatureHead: g.FeatureHead, PublicMain: g.PublicMain, Parent: g.Parent, Base: from, Overlay: g.Overlay,
		Candidate: g.Candidate, PublishedTree: g.PublishedTree, ScannedAs: g.ScannedAs, Commits: infos,
		Checks: g.Checks, FailedChecks: failed, Leak: g.Leak, Findings: []leak.Finding{}, Waived: []WaivedFinding{},
		mapping: g.Commits, Update: g.Update}
	if g.ShadowPR != 0 {
		pr, job := g.ShadowPR, jobID(name, g.Candidate)
		res.ShadowPR, res.Job = &pr, &job
	}
	return res, warns, nil
}

// pendingFor returns the feature's gate record when it is a pending job
// for exactly the inputs a gate in mode would run on now: the feature
// head, public main, the overlay, the scanned mode, and the candidate
// still on the gate branch. Otherwise it returns nil, and the gate runs.
func (e *Engine) pendingFor(ctx context.Context, pc *pairCtx, name, mode string) (*feature.Gate, error) {
	g := pc.git
	cfg, O, _, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, err
	}
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, err
	}
	if !m.Gate.Pending() {
		return nil, nil
	}
	head, ok, err := g.Resolve(ctx, shadowRef(m.Branch))
	if err != nil {
		return nil, internalErr(err, "read the feature branch")
	}
	if !ok {
		return nil, nil // the gate reports the missing branch
	}
	pm, err := g.MustResolve(ctx, publicRef(pc.base))
	if err != nil {
		return nil, internalErr(err, "read public "+pc.base)
	}
	tip, _, err := g.Resolve(ctx, shadowRef(gateBranch(name)))
	if err != nil {
		return nil, internalErr(err, "read the gate branch")
	}
	if mode == "" {
		mode = cfg.Promote.DefaultMode
	}
	if m.Gate.FreshFor(head, pm, O) && m.Gate.ScannedAs == mode && tip == m.Gate.Candidate {
		// An update job is this gate only while it builds an update the
		// open PR can take now (gateFitsUpdate): the same PR, still open on
		// the same base, its branch at the head the job built on. An append
		// job stops standing once a replace is forced (PRs opened above it, a
		// restack flagged since); a replace job stands for any forced
		// replace. A first-publish job stands only while no PR is open.
		if m.Gate.Update != nil || (m.Promoted != nil && parentFeature(m) == "") {
			fits, _, err := e.updateFits(ctx, pc, m.Gate, m, mode)
			if err != nil {
				return nil, err
			}
			if !fits {
				return nil, nil
			}
		}
		// A job built on a stack that has moved since is not this gate.
		st, err := e.stackState(ctx, pc, name, m)
		if err != nil {
			return nil, err
		}
		if !st.broken && m.Gate.StackFreshFor(parentFeature(m), forkOf(m), st.heads) {
			return m.Gate, nil
		}
	}
	return nil, nil
}

// WaitOptions are wait's flags.
type WaitOptions struct {
	// Timeout returns the job still pending after this long; zero waits
	// until CI settles or the gate's own CI budget runs out.
	Timeout time.Duration
	// Events receives the job's events as they happen; nil for none.
	Events func(JobEvent)
}

// Wait finishes a gate job: it waits on CI for the pending gate the job
// names and records the outcome over it. A job already finished returns
// its recorded result at once, writing nothing. It never touches the
// public repo.
func (e *Engine) Wait(ctx context.Context, p workspace.Pair, job string, opt WaitOptions) (res *GateResult, warns []contract.Warning, err error) {
	var ref JobRef
	defer func() {
		summary := ""
		if res != nil {
			summary = "checks " + res.Checks
		}
		e.record("wait", &p, ref.Feature, "", summary, err)
	}()
	if ref, err = ParseJob(job); err != nil {
		return nil, nil, err
	}
	defer func() {
		if res == nil && err != nil && ctx.Err() != nil && contract.From(err).Code != contract.Interrupted {
			// Nothing was read, so nothing changed: the job is as it was.
			err = contract.New(contract.Interrupted, "interrupted before the job was read; run ghostbranch wait again").
				WithDetails(map[string]any{"job": job})
			return
		}
		err = interrupted(ctx, res, ref.Feature, err)
	}()
	pc, err := e.open(ctx, "wait", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	defer func() {
		if res != nil {
			e.stackView(ctx, pc, res)
		}
	}()
	m, _, err := e.readMeta(ctx, pc, ref.Feature)
	if err != nil {
		return nil, nil, err
	}
	switch {
	case m.Gate == nil || (!m.Gate.Pending() && m.Gate.ShadowPR == 0):
		return nil, nil, contract.New(contract.JobNotFound,
			"feature %q has no gate job to wait on: it was never gated, or its last gate stopped before CI; gate it", ref.Feature).
			WithField("job").WithDetails(map[string]any{"feature": ref.Feature})
	case !ref.matches(m.Gate):
		return nil, nil, staleErr(ref.Feature, job, m.Gate, "")
	}
	var rw []contract.Warning
	if res, rw, err = e.resultFromRecord(ctx, pc, ref.Feature, *m.Gate); err != nil {
		return nil, rw, err
	}
	warns = addWarnings(warns, rw...)
	if !m.Gate.Pending() {
		cfg, _, _, err := e.readConfig(ctx, pc)
		if err != nil {
			return res, warns, err
		}
		return res, warns, outcomeErr(checkOutcome{status: res.Checks, failed: res.FailedChecks}, m.Gate.ShadowPR, cfg.Gate, pc.pair.Shadow,
			func() ([]string, bool) {
				return prWorkflows(ctx, pc.git, guardNames(pc), m.Gate.Candidate, gateBranch(ref.Feature))
			})
	}
	var deadline time.Time
	if opt.Timeout > 0 {
		deadline = e.Now().Add(opt.Timeout)
	}
	var fw []contract.Warning
	res, fw, err = e.finish(ctx, pc, ref.Feature, res, *m.Gate, waitOpts{deadline: deadline, emit: opt.Events})
	return res, addWarnings(warns, fw...), err
}
