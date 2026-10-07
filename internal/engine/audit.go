package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// SecretRef is one secret the workflows reference and the files that do.
type SecretRef struct {
	Name  string   `json:"name"`
	Files []string `json:"files"`
}

// AuditSecrets is what the audit knows about the shadow repo's secrets.
type AuditSecrets struct {
	Referenced   []SecretRef `json:"referenced"`
	Listed       bool        `json:"listed"`
	Missing      []string    `json:"missing"`
	Inherit      []string    `json:"inherit"`      // "<path>:<job>"
	Environments []string    `json:"environments"` // "<path>:<job>"
}

// AuditMinutes is whether the shadow repo's Actions minutes are billed.
type AuditMinutes struct {
	Billed        bool    `json:"billed"`
	ScheduledSync *string `json:"scheduled_sync"`
}

// AuditResult is the audit command's data.
type AuditResult struct {
	Public       string           `json:"public"`
	Shadow       string           `json:"shadow"`
	Guards       []workflow.Guard `json:"guards"`
	GuardMode    string           `json:"guard_mode"` // ci.rewrite_repo_guards
	Secrets      AuditSecrets     `json:"secrets"`
	PushTriggers []string         `json:"push_triggers"`
	Minutes      AuditMinutes     `json:"minutes"`
	Overlay      []ManagedFile    `json:"overlay"`
}

// Audit reports what keeps the shadow repo's CI from matching public CI.
// It changes nothing and is not ledgered.
func (e *Engine) Audit(ctx context.Context, p workspace.Pair) (*AuditResult, []contract.Warning, error) {
	pc, err := e.open(ctx, "audit", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	warns = addWarnings(warns, e.behind(ctx, pc)...)
	res, aw, err := e.audit(ctx, pc, cfg, O)
	return res, addWarnings(warns, aw...), err
}

// auditInto runs the audit after init or upgrade; a failure is a warning.
func (e *Engine) auditInto(ctx context.Context, pc *pairCtx, cfg config.Config, O string, warns []contract.Warning) (*AuditResult, []contract.Warning) {
	res, aw, err := e.audit(ctx, pc, cfg, O)
	if err != nil {
		return nil, append(warns, contract.Warning{Code: contract.AuditIncomplete,
			Message: "the CI audit could not run (" + contract.From(err).Message + "); run ghostbranch audit"})
	}
	return res, addWarnings(warns, aw...)
}

func (e *Engine) audit(ctx context.Context, pc *pairCtx, cfg config.Config, O string) (*AuditResult, []contract.Warning, error) {
	g := pc.git
	names := guardNames(pc)
	base, ok, err := g.Resolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, nil, internalErr(err, "read shadow main")
	}
	if !ok {
		if base, err = g.MustResolve(ctx, publicRef("main")); err != nil {
			return nil, nil, internalErr(err, "read public main")
		}
	}
	mainEntries, err := g.ListTree(ctx, base)
	if err != nil {
		return nil, nil, internalErr(err, "list shadow main")
	}
	oEntries, err := g.ListTree(ctx, O)
	if err != nil {
		return nil, nil, internalErr(err, "list the overlay branch")
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	type wf struct {
		path, sha string
		public    bool
	}
	var files []wf
	var shas []string
	for _, en := range mainEntries {
		if en.Type == "blob" && workflow.IsWorkflowPath(en.Path) && !set.Match(en.Path) {
			files, shas = append(files, wf{en.Path, en.SHA, true}), append(shas, en.SHA)
		}
	}
	for _, en := range oEntries {
		if en.Type == "blob" && workflow.IsWorkflowPath(en.Path) && !isManaged(en.Path) {
			files, shas = append(files, wf{en.Path, en.SHA, false}), append(shas, en.SHA)
		}
	}
	blobs, err := g.ReadBlobs(ctx, shas)
	if err != nil {
		return nil, nil, internalErr(err, "read workflow files")
	}
	res := &AuditResult{Public: pc.pair.Public, Shadow: pc.pair.Shadow, Guards: []workflow.Guard{}, GuardMode: cfg.CI.RewriteRepoGuards,
		Secrets:      AuditSecrets{Referenced: []SecretRef{}, Missing: []string{}, Inherit: []string{}, Environments: []string{}},
		PushTriggers: []string{}, Minutes: AuditMinutes{Billed: pc.shadow.Private}}
	if cfg.Sync.Schedule != config.ScheduleOff {
		s := cfg.Sync.Schedule
		res.Minutes.ScheduledSync = &s
	}
	if res.Overlay, err = e.managedFiles(ctx, g, O, cfg); err != nil {
		return nil, nil, err
	}
	refs := map[string][]string{}
	for _, f := range files {
		src := blobs[f.sha]
		parsed, err := workflow.Parse(f.path, src, names)
		if err != nil {
			continue // GitHub reports invalid workflow files itself
		}
		if f.public {
			if _, guards, err := workflow.Rewrite(f.path, src, names); err == nil {
				res.Guards = append(res.Guards, guards...)
			}
			if parsed.PushTriggered(cfg.Sync.Branches) {
				res.PushTriggers = append(res.PushTriggers, f.path)
			}
		}
		for _, s := range parsed.Secrets {
			refs[s] = append(refs[s], f.path)
		}
		for _, j := range parsed.Inherit {
			res.Secrets.Inherit = append(res.Secrets.Inherit, f.path+":"+j)
		}
		for _, j := range parsed.Environments {
			res.Secrets.Environments = append(res.Secrets.Environments, f.path+":"+j)
		}
	}
	for name, paths := range refs {
		res.Secrets.Referenced = append(res.Secrets.Referenced, SecretRef{Name: name, Files: paths})
	}
	sort.Slice(res.Secrets.Referenced, func(i, j int) bool { return res.Secrets.Referenced[i].Name < res.Secrets.Referenced[j].Name })

	warns := guardWarnings(cfg.CI.RewriteRepoGuards, res.Guards, true)
	if len(refs) > 0 {
		have, err := e.Forge.SecretNames(ctx, pc.pair.Shadow)
		switch {
		case errors.Is(err, forge.ErrForbidden):
			warns = append(warns, contract.Warning{Code: contract.SecretsUnlisted,
				Message: fmt.Sprintf("the token can't list %s's secret names; check that these exist there: %s", pc.pair.Shadow, strings.Join(sortedKeys(refs), ", "))})
		case err != nil:
			return nil, nil, forgeErr(err, "list the secrets of %s", pc.pair.Shadow)
		default:
			res.Secrets.Listed = true
			present := map[string]bool{}
			for _, n := range have {
				present[strings.ToUpper(n)] = true
			}
			for _, n := range sortedKeys(refs) {
				if !present[n] {
					res.Secrets.Missing = append(res.Secrets.Missing, n)
				}
			}
			if len(res.Secrets.Missing) > 0 {
				warns = append(warns, contract.Warning{Code: contract.SecretsMissing,
					Message: fmt.Sprintf("workflows reference secrets %s doesn't have: %s", pc.pair.Shadow, strings.Join(res.Secrets.Missing, ", ")),
					Details: map[string]any{"names": res.Secrets.Missing}})
			}
		}
	}
	installed := false
	for _, f := range res.Overlay {
		if f.Path == managed.SyncPath && f.Status != managed.Missing && cfg.Sync.Schedule != config.ScheduleOff {
			installed = true
		}
	}
	if installed && len(res.PushTriggers) > 0 {
		warns = append(warns, contract.Warning{Code: contract.PushTriggersOnSync,
			Message: "a scheduled sync that uses GHOSTBRANCH_SYNC_TOKEN starts these workflows in the shadow repo: " + strings.Join(res.PushTriggers, ", "),
			Details: map[string]any{"paths": res.PushTriggers}})
	}
	if res.Minutes.Billed {
		msg := pc.pair.Shadow + " is private, so its Actions minutes are billed"
		if res.Minutes.ScheduledSync != nil {
			msg += "; scheduled sync (" + *res.Minutes.ScheduledSync + ") bills at least one minute a run, about 120 minutes a month at every 6 hours"
		}
		warns = append(warns, contract.Warning{Code: contract.ActionsMinutesBilled, Message: msg})
	}
	return res, addWarnings(warns, outdatedWarnings(res.Overlay)...), nil
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
