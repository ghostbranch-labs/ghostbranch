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
	"regexp"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
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
	if !jobCandidate.MatchString(c) || feature.ValidateName(f, "job") != nil {
		return JobRef{}, bad
	}
	return JobRef{Feature: f, Candidate: c}, nil
}

func (j JobRef) matches(g *feature.Gate) bool {
	return g != nil && strings.HasPrefix(g.Candidate, j.Candidate)
}

// fetchMeta refreshes the mirror's feature metadata only, so a caller
// mid-operation re-reads records without moving public main under itself.
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
		drop, err := workspace.Lock(pc.lockPath, e.Now())
		if err == nil {
			pc.drop, pc.held = drop, true
			break
		}
		if ce := contract.From(err); ce.Code != contract.Locked || waited >= finishLockWait {
			ce.Retryable = true
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

// outcomeErr is the error a settled CI outcome stands for; nil when it passed.
func outcomeErr(out checkOutcome, pr int, cfg config.Gate, shadow string) error {
	switch out.status {
	case feature.ChecksFailed:
		return contract.New(contract.ChecksFailed, "checks failed on the gate candidate: %s", strings.Join(out.failed, ", ")).
			WithDetails(map[string]any{"failed": out.failed, "shadow_pr": pr})
	case feature.ChecksNoCI:
		return contract.New(contract.NoCI,
			"no checks appeared on shadow PR #%d within %s; check that Actions is enabled in %s", pr, cfg.CIAppearTimeout, shadow)
	case feature.ChecksTimeout:
		return contract.New(contract.CITimeout, "checks did not finish within %s", cfg.CITimeout)
	}
	return nil
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
	e.progressf("waiting for CI on %s (shadow PR #%d: %s)", short(C), pending.ShadowPR, url)
	o.job = job
	e.event(o, JobEvent{Type: EventWaiting, Candidate: C, ShadowPR: pending.ShadowPR, ShadowPRURL: url, CITimeout: cfg.Gate.CITimeout.String()})
	out, err := e.waitChecks(ctx, shadow, C, cfg.Gate, waitOpts{start: pending.StartedAt, deadline: o.deadline, job: job, emit: o.emit})
	if err != nil {
		return res, warns, err
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
		switch contract.From(err).Code {
		case contract.JobStale, contract.JobNotFound, contract.FeatureNotFound, contract.Interrupted:
			return res, warns, err
		}
		if res.Checks == feature.ChecksPassed {
			return res, warns, err // a green gate that isn't recorded can't be promoted
		}
		// The check outcome is what matters; don't replace it with a lesser error.
		warns = append(warns, contract.Warning{Code: contract.GateNotRecorded,
			Message: "the gate result could not be recorded in the feature metadata: " + contract.From(err).Message})
	}
	return res, warns, outcomeErr(checkOutcome{status: res.Checks, failed: res.FailedChecks}, pending.ShadowPR, cfg.Gate, shadow)
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
			current := ""
			if m.Gate.Pending() {
				current = jobID(name, m.Gate.Candidate)
			}
			return nil, contract.New(contract.JobStale, "a newer gate replaced the job %s; its result was not recorded", job).
				WithDetails(map[string]any{"job": job, "current_job": current})
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
		warns, err := e.updateMeta(ctx, pc, m, sha)
		if err == nil || attempt > 0 || contract.From(err).Code != contract.RefMoved {
			return warns, err
		}
		if err := e.fetchMeta(ctx, pc.git); err != nil {
			return nil, err
		}
	}
}
