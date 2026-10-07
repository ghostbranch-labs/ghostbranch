package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

// ManagedFile is one Ghostbranch-managed overlay path and its state.
type ManagedFile struct {
	Path   string         `json:"path"`
	Status managed.Status `json:"status"`
}

func managedInput(cfg config.Config) managed.Input {
	return managed.Input{Public: cfg.Public, Schedule: cfg.Sync.Schedule, Branches: cfg.Sync.Branches}
}

func isManaged(path string) bool {
	for _, p := range managed.Paths() {
		if p == path {
			return true
		}
	}
	return false
}

// managedFiles classifies every managed path on overlay commit O.
func (e *Engine) managedFiles(ctx context.Context, g *gitx.Git, O string, cfg config.Config) ([]ManagedFile, error) {
	in := managedInput(cfg)
	out := []ManagedFile{}
	for _, p := range managed.Paths() {
		cur, ok, err := g.ShowFile(ctx, O, p)
		if err != nil {
			return nil, internalErr(err, "read "+p+" on the overlay branch")
		}
		out = append(out, ManagedFile{Path: p, Status: managed.StatusOf(p, cur, ok, in)})
	}
	return out, nil
}

// outdatedWarnings is OVERLAY_OUTDATED when any managed file needs upgrade.
func outdatedWarnings(files []ManagedFile) []contract.Warning {
	var bad []string
	for _, f := range files {
		switch f.Status {
		case managed.Missing, managed.Outdated, managed.Modified:
			bad = append(bad, f.Path+" ("+string(f.Status)+")")
		}
	}
	if len(bad) == 0 {
		return nil
	}
	return []contract.Warning{{Code: contract.OverlayOutdated,
		Message: "managed overlay files need ghostbranch upgrade: " + strings.Join(bad, ", "),
		Details: map[string]any{"files": bad}}}
}

func guardWhere(g workflow.Guard) string {
	where := g.Path + " job " + g.Job
	if g.Step != nil {
		where += " step " + *g.Step
	}
	return where
}

// guardWarnings turns guards into warnings for ci.rewrite_repo_guards mode.
// The gate (audit=false) is silent in off mode; the audit always reports
// what it can't rewrite.
func guardWarnings(mode string, guards []workflow.Guard, audit bool) []contract.Warning {
	var out []contract.Warning
	for _, g := range guards {
		d := map[string]any{"path": g.Path, "job": g.Job, "step": g.Step, "expr": g.Expr}
		switch {
		case !g.Rewritable && (audit || mode != "off"):
			out = append(out, contract.Warning{Code: contract.RepoGuardUnrewritable, Details: d,
				Message: "the guard in " + guardWhere(g) + " can't be rewritten, so that job skips in the shadow repo and CI does not test it: " + g.Expr})
		case g.Rewritable && mode == "warn":
			out = append(out, contract.Warning{Code: contract.RepoGuard, Details: d,
				Message: guardWhere(g) + " is guarded to the public repo, so it skips in the shadow repo: " + g.Expr})
		}
	}
	return out
}
