package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
)

type checkOutcome struct {
	status string // feature.ChecksPassed | ChecksFailed | ChecksNoCI | ChecksTimeout | ChecksPending
	failed []string
}

// Job event types, one per line of wait --json (schemas/v0/wait-event.schema.json).
const (
	EventWaiting = "waiting"
	EventCheck   = "check"
	EventSettled = "settled"
)

// JobEvent is one line of wait --json's NDJSON stream. Fields beyond the
// first four belong to one type and are omitted from the others.
type JobEvent struct {
	SchemaVersion string    `json:"schema_version"`
	Type          string    `json:"type"`
	Job           string    `json:"job"`
	At            time.Time `json:"at"`
	// waiting
	Candidate   string `json:"candidate,omitempty"`
	ShadowPR    int    `json:"shadow_pr,omitempty"`
	ShadowPRURL string `json:"shadow_pr_url,omitempty"`
	CITimeout   string `json:"ci_timeout,omitempty"`
	// NoWorkflows is true when the candidate has no workflow that runs on
	// its PR, so the wait for CI to start is CIAppearTimeout's short form.
	NoWorkflows     *bool  `json:"no_workflows,omitempty"`
	CIAppearTimeout string `json:"ci_appear_timeout,omitempty"`
	// check
	Name       string `json:"name,omitempty"`
	Status     string `json:"status,omitempty"`
	Conclusion string `json:"conclusion,omitempty"`
	// settled
	Outcome      string   `json:"outcome,omitempty"`
	FailedChecks []string `json:"failed_checks,omitempty"`
}

// waitOpts are a CI wait's knobs. start is when the gate started (budgets
// count from it; zero means now); deadline is wait --timeout (zero: none);
// job names the job in events and errors; emit receives events (nil: none).
type waitOpts struct {
	start, deadline time.Time
	job             string
	emit            func(JobEvent)
	step            progress.Step // step shows the wait (nil: none)
	// noWorkflows: the candidate has no workflow that runs on its PR, so
	// the wait for a first check is the short one; shadow names the repo
	// in the step.
	noWorkflows bool
	shadow      string
	// workflows are the workflow files that run on the PR (nil: unknown
	// or none); with each one's pull_request run finished and only GitHub
	// Actions checks, the gate settles after QuickSettle (settleWindow).
	workflows []string
}

// appearTimeout is how long the gate waits for a first check: the short
// ci_appear_timeout_no_workflows when no workflow runs on the PR, since
// only another CI service could still report. It is never longer than
// ci_appear_timeout.
func appearTimeout(cfg config.Gate, noWorkflows bool) time.Duration {
	if noWorkflows {
		return min(cfg.CIAppearTimeoutNoWorkflows, cfg.CIAppearTimeout)
	}
	return cfg.CIAppearTimeout
}

// startText is the CI step while no check has appeared: why the gate
// waits, and for how long. left is the time left to start.
func startText(cfg config.Gate, o waitOpts, left time.Duration) string {
	if o.noWorkflows {
		return fmt.Sprintf("Waiting up to %s for CI to start: %s has no workflow that runs on this PR (another CI service may still report)",
			shortDur(appearTimeout(cfg, true)), o.shadow)
	}
	if left >= time.Minute {
		left = left.Truncate(time.Minute)
	}
	return fmt.Sprintf("Waiting for CI to start (up to %s, %s left)", shortDur(cfg.CIAppearTimeout), shortDur(max(left, 0)))
}

// waitText is the CI step while the gate waits: CI to start (startLeft is
// the time left for that), the checks running, or every check done and
// the settle window running (settleLeft left of it).
func waitText(cfg config.Gate, o waitOpts, checks []forge.Check, done bool, startLeft, settleLeft time.Duration) string {
	switch {
	case len(checks) == 0:
		return startText(cfg, o, startLeft)
	case done && len(checks) == 1:
		return fmt.Sprintf("1 check done; waiting %s more in case more CI starts", shortDur(max(settleLeft, 0)))
	case done:
		return fmt.Sprintf("All %d checks done; waiting %s more in case more CI starts", len(checks), shortDur(max(settleLeft, 0)))
	}
	n := 0
	for _, c := range checks {
		if c.Status == "completed" {
			n++
		}
	}
	return fmt.Sprintf("Waiting for CI (%d of %d checks done)", n, len(checks))
}

// shortDur is d to the second without zero tails: 10m, 1h, 1m30s, 45s.
func shortDur(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func (e *Engine) event(o waitOpts, ev JobEvent) {
	if o.emit == nil {
		return
	}
	ev.SchemaVersion, ev.Job, ev.At = contract.SchemaVersion, o.job, e.now()
	o.emit(ev)
}

// interruptedErr is a cancelled run. A job still pending is named, so wait
// can finish it; otherwise the feature is, so the command can run again.
func interruptedErr(job, feature string) error {
	if job != "" {
		return contract.New(contract.Interrupted, "interrupted while waiting for CI; the job %s is still pending: finish it with ghostbranch wait", job).
			WithDetails(map[string]any{"job": job})
	}
	return contract.New(contract.Interrupted, "interrupted before any gate job was recorded; nothing is pending, so run the command again").
		WithDetails(map[string]any{"feature": feature})
}

// settleWindow is how long the check list must stay unchanged, with every
// check done, before the gate trusts it. It is QuickSettle only for the
// complete picture: every check comes from GitHub Actions, the token can
// see other apps' checks, and each workflow file that runs on the PR
// (workflows) has a finished pull_request or pull_request_target run on
// sha. A push run never counts: with on: [push, pull_request] and the push
// guard, the gate's push makes a run whose jobs are all skipped, which
// finishes before the pull_request run has started. Anything less is
// SettleWindow, which leaves time for another CI's first check to appear.
// It asks the forge for runs only once every check is done.
func (e *Engine) settleWindow(ctx context.Context, repo, sha string, checks []forge.Check, done bool, workflows []string) time.Duration {
	if !done || e.QuickSettle <= 0 || e.QuickSettle >= e.SettleWindow || len(workflows) == 0 || len(checks) == 0 {
		return e.SettleWindow
	}
	if v, ok := e.Forge.(forge.CheckVisibility); ok && v.ChecksLimited(repo) {
		return e.SettleWindow
	}
	for _, c := range checks {
		if !c.Actions {
			return e.SettleWindow
		}
	}
	lister, ok := e.Forge.(forge.RunLister)
	if !ok {
		return e.SettleWindow
	}
	runs, err := lister.RunsForSHA(ctx, repo, sha)
	if err != nil {
		return e.SettleWindow
	}
	finished := map[string]bool{}
	for _, r := range runs {
		// GitHub records a pull_request_target run against the base
		// branch's commit, so a workflow that runs only on it never matches
		// here and keeps the full window: slower, never early.
		if (r.Event == "pull_request" || r.Event == "pull_request_target") && r.Status == "completed" {
			finished[r.Path] = true
		}
	}
	for _, w := range workflows {
		if !finished[w] {
			return e.SettleWindow
		}
	}
	return e.QuickSettle
}

// summarize reports whether every check has completed, and which failed.
// success, skipped and neutral pass; any other conclusion fails.
func summarize(checks []forge.Check) (bool, []string) {
	done := len(checks) > 0
	failed := []string{}
	for _, c := range checks {
		if c.Status != "completed" {
			done = false
			continue
		}
		switch c.Conclusion {
		case "success", "skipped", "neutral":
		default:
			failed = append(failed, c.Name)
		}
	}
	sort.Strings(failed)
	return done, failed
}

// waitChecks polls until checks on sha settle: at least one exists, all
// have completed, and none has appeared for SettleWindow. Budgets count
// from o.start. Once any check has been seen, the outcome is never no_ci.
// At o.deadline it returns pending; on cancel, INTERRUPTED.
func (e *Engine) waitChecks(ctx context.Context, repo, sha string, cfg config.Gate, o waitOpts) (checkOutcome, error) {
	start := o.start
	if start.IsZero() {
		start = e.Now()
	}
	var stableSince time.Time
	last, seen := -1, false
	prev := map[string]forge.Check{}
	for {
		if ctx.Err() != nil {
			return checkOutcome{}, interruptedErr(o.job, "")
		}
		checks, err := e.Forge.ListChecks(ctx, repo, sha)
		if err != nil {
			if ctx.Err() != nil {
				return checkOutcome{}, interruptedErr(o.job, "")
			}
			return checkOutcome{}, forgeErr(err, "list checks on %s", short(sha))
		}
		now := e.Now()
		if len(checks) != last {
			last, stableSince = len(checks), now
		}
		done, failed := summarize(checks)
		settle := e.settleWindow(ctx, repo, sha, checks, done, o.workflows)
		seen = seen || len(checks) > 0
		for _, c := range checks {
			// Only what the event carries counts as a change: two checks may
			// share a name (a workflow's push and pull_request runs).
			if p, ok := prev[c.Name]; !ok || p.Status != c.Status || p.Conclusion != c.Conclusion {
				e.event(o, JobEvent{Type: EventCheck, Name: c.Name, Status: c.Status, Conclusion: c.Conclusion})
				prev[c.Name] = c
			}
		}
		var out *checkOutcome
		switch {
		case !seen && now.Sub(start) >= appearTimeout(cfg, o.noWorkflows):
			out = &checkOutcome{status: feature.ChecksNoCI, failed: []string{}}
		// At ci_timeout, checks that have all completed report their outcome
		// even if the settle window has not elapsed: there is no more time
		// to wait for late checks, and the completed result is the truth.
		case done && (now.Sub(stableSince) >= settle || now.Sub(start) >= cfg.CITimeout):
			out = &checkOutcome{status: feature.ChecksPassed, failed: []string{}}
			if len(failed) > 0 {
				out = &checkOutcome{status: feature.ChecksFailed, failed: failed}
			}
		case now.Sub(start) >= cfg.CITimeout:
			out = &checkOutcome{status: feature.ChecksTimeout, failed: []string{}}
		case !o.deadline.IsZero() && !now.Before(o.deadline):
			return checkOutcome{status: feature.ChecksPending, failed: []string{}}, nil
		}
		if out != nil {
			e.event(o, JobEvent{Type: EventSettled, Outcome: out.status, FailedChecks: out.failed})
			return *out, nil
		}
		if o.step != nil {
			o.step.Update(waitText(cfg, o, checks, done, appearTimeout(cfg, o.noWorkflows)-now.Sub(start), settle-now.Sub(stableSince)))
		}
		d := e.PollInterval
		if !o.deadline.IsZero() {
			// Never sleep past wait --timeout. The deadline case above has
			// returned unless it is still ahead.
			if left := o.deadline.Sub(now); left < d {
				d = left
			}
		}
		if err := e.Sleep(ctx, d); err != nil {
			return checkOutcome{}, interruptedErr(o.job, "")
		}
	}
}
