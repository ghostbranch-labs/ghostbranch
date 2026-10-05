package engine

import (
	"context"
	"sort"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

type checkOutcome struct {
	status string // feature.ChecksPassed | ChecksFailed | ChecksNoCI | ChecksTimeout
	failed []string
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
// have completed, and none has appeared for SettleWindow.
func (e *Engine) waitChecks(ctx context.Context, repo, sha string, cfg config.Gate) (checkOutcome, error) {
	start := e.Now()
	var stableSince time.Time
	last := -1
	for {
		if err := ctx.Err(); err != nil {
			return checkOutcome{}, err
		}
		checks, err := e.Forge.ListChecks(ctx, repo, sha)
		if err != nil {
			return checkOutcome{}, forgeErr(err, "list checks on %s", short(sha))
		}
		now := e.Now()
		if len(checks) != last {
			last, stableSince = len(checks), now
		}
		done, failed := summarize(checks)
		switch {
		case len(checks) == 0 && now.Sub(start) >= cfg.CIAppearTimeout:
			return checkOutcome{status: feature.ChecksNoCI, failed: []string{}}, nil
		// At ci_timeout, checks that have all completed report their outcome
		// even if the settle window has not elapsed: there is no more time
		// to wait for late checks, and the completed result is the truth.
		case done && (now.Sub(stableSince) >= e.SettleWindow || now.Sub(start) >= cfg.CITimeout):
			if len(failed) > 0 {
				return checkOutcome{status: feature.ChecksFailed, failed: failed}, nil
			}
			return checkOutcome{status: feature.ChecksPassed, failed: []string{}}, nil
		case now.Sub(start) >= cfg.CITimeout:
			return checkOutcome{status: feature.ChecksTimeout, failed: []string{}}, nil
		}
		e.Sleep(e.PollInterval)
	}
}
