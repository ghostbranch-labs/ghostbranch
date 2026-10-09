package engine

import (
	"context"
	"sort"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
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
}

func (e *Engine) event(o waitOpts, ev JobEvent) {
	if o.emit == nil {
		return
	}
	ev.SchemaVersion, ev.Job, ev.At = contract.SchemaVersion, o.job, e.now()
	o.emit(ev)
}

// interruptedErr is a cancelled run. A job still pending is named, so wait
// can finish it; otherwise the feature is, so the gate can run again.
func interruptedErr(job, feature string) error {
	if job != "" {
		return contract.New(contract.Interrupted, "interrupted while waiting for CI; the job %s is still pending: finish it with ghostbranch wait", job).
			WithDetails(map[string]any{"job": job})
	}
	return contract.New(contract.Interrupted, "interrupted before the gate's CI started; nothing is pending, so gate again").
		WithDetails(map[string]any{"feature": feature})
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
		seen = seen || len(checks) > 0
		for _, c := range checks {
			if p, ok := prev[c.Name]; !ok || p != c {
				e.event(o, JobEvent{Type: EventCheck, Name: c.Name, Status: c.Status, Conclusion: c.Conclusion})
				prev[c.Name] = c
			}
		}
		done, failed := summarize(checks)
		var out *checkOutcome
		switch {
		case !seen && now.Sub(start) >= cfg.CIAppearTimeout:
			out = &checkOutcome{status: feature.ChecksNoCI, failed: []string{}}
		// At ci_timeout, checks that have all completed report their outcome
		// even if the settle window has not elapsed: there is no more time
		// to wait for late checks, and the completed result is the truth.
		case done && (now.Sub(stableSince) >= e.SettleWindow || now.Sub(start) >= cfg.CITimeout):
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
		if err := e.Sleep(ctx, e.PollInterval); err != nil {
			return checkOutcome{}, interruptedErr(o.job, "")
		}
	}
}
