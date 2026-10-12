package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Overlay statuses in a StatusResult.
const (
	overlayCurrent  = "current"
	overlayOutdated = "outdated"
)

// StatusResult is the pair's health and every feature, abandoned included.
type StatusResult struct {
	Public        string          `json:"public"`
	Shadow        string          `json:"shadow"`
	PublicMain    string          `json:"public_main"`
	ShadowMain    string          `json:"shadow_main"` // "" when shadow main is missing
	ShadowBehind  bool            `json:"shadow_behind"`
	Overlay       string          `json:"overlay"`        // overlay commit
	OverlayStatus string          `json:"overlay_status"` // current | outdated (a managed file needs upgrade)
	ScheduledSync bool            `json:"scheduled_sync"`
	Features      []FeatureReport `json:"features"` // never null; abandoned included
}

// FeatureListResult is the features table.
type FeatureListResult struct {
	Features []FeatureReport `json:"features"` // never null; abandoned only with all
}

// FeatureShowResult is one feature in full.
type FeatureShowResult struct {
	Feature FeatureReport  `json:"feature"`
	Commits []CommitInfo   `json:"commits"` // the feature's own commits (since its fork point, else since public main), oldest first; [] when the branch is gone
	Ledger  []ledger.Entry `json:"ledger"`  // this pair's entries for this feature, oldest first
}

// Status reports the pair's health (shadow main behind, the managed
// overlay files, scheduled sync) and surveys every feature. Like the other
// pair commands it warns SHADOW_BEHIND and OVERLAY_OUTDATED. It only reads:
// no push and no ledger entry.
func (e *Engine) Status(ctx context.Context, p workspace.Pair) (*StatusResult, []contract.Warning, error) {
	pc, err := e.open(ctx, "status", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	g := pc.git
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	m, err := compareMains(ctx, g)
	if err != nil {
		return nil, warns, internalErr(err, "compare shadow main with public main")
	}
	if m.Public == "" {
		return nil, warns, internalErr(fmt.Errorf("git: %s does not exist", publicRef("main")), "read public main")
	}
	files, err := e.managedFiles(ctx, g, O, cfg)
	if err != nil {
		return nil, warns, err
	}
	res := &StatusResult{Public: pc.pair.Public, Shadow: pc.pair.Shadow, PublicMain: m.Public, ShadowMain: m.Shadow,
		ShadowBehind: m.Behind > 0, Overlay: O, OverlayStatus: overlayStatus(files),
		ScheduledSync: cfg.Sync.Schedule != config.ScheduleOff}
	warns = addWarnings(warns, m.warnings(pc.pair.Public)...)
	warns = addWarnings(warns, outdatedWarnings(files)...)
	rs, sw, err := e.surveyShown(ctx, pc)
	warns = addWarnings(warns, sw...)
	if err != nil {
		return nil, warns, err
	}
	res.Features = rs
	return res, warns, nil
}

// overlayStatus is outdated when any managed file needs upgrade, as
// OVERLAY_OUTDATED counts it.
func overlayStatus(files []ManagedFile) string {
	for _, f := range files {
		if needsUpgrade(f.Status) {
			return overlayOutdated
		}
	}
	return overlayCurrent
}

// FeatureList surveys the pair's features, leaving out abandoned ones
// unless all. It only reads.
func (e *Engine) FeatureList(ctx context.Context, p workspace.Pair, all bool) (*FeatureListResult, []contract.Warning, error) {
	pc, err := e.open(ctx, "feature list", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	_, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	rs, sw, err := e.surveyShown(ctx, pc)
	warns = addWarnings(warns, sw...)
	if err != nil {
		return nil, warns, err
	}
	res := &FeatureListResult{Features: []FeatureReport{}}
	for _, r := range rs {
		if all || !r.Abandoned {
			res.Features = append(res.Features, r)
		}
	}
	return res, warns, nil
}

// FeatureShow reports one feature in full: its survey report, its own
// commits (since its fork point, or since public main) and its ledger
// entries. It only reads. A feature whose record can't be read returns the
// read error.
func (e *Engine) FeatureShow(ctx context.Context, p workspace.Pair, name string) (*FeatureShowResult, []contract.Warning, error) {
	pc, err := e.open(ctx, "feature show", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	g := pc.git
	cfg, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	rs, sw, err := e.surveyShown(ctx, pc, name)
	warns = addWarnings(warns, sw...)
	if err != nil {
		return nil, warns, err
	}
	if len(rs) != 1 {
		return nil, warns, contract.New(contract.FeatureNotFound,
			"no feature %q in %s; list them with ghostbranch feature list --all", name, pc.pair.Shadow).WithField("feature")
	}
	if rs[0].err != nil {
		return nil, warns, rs[0].err
	}
	res := &FeatureShowResult{Feature: rs[0], Commits: []CommitInfo{}, Ledger: []ledger.Entry{}}
	if head := res.Feature.Head; head != "" {
		pm, err := g.MustResolve(ctx, publicRef("main"))
		if err != nil {
			return nil, warns, internalErr(err, "read public main")
		}
		// Only its own commits: forked_at..head when it records a fork point
		// (stacked, or moved onto main off a parent), else every commit
		// public main lacks. A lost fork point, or one that can't be
		// checked (an unknown commit), falls back to the latter.
		var sources []string
		var own bool
		if m := res.Feature.meta; m != nil {
			if sources, own, err = ownCommits(ctx, g, *m, head, pm); err != nil {
				own = false
			}
		}
		if !own {
			if sources, err = g.SourceCommits(ctx, pm, head); err != nil {
				return nil, warns, internalErr(err, "list the feature's commits")
			}
		}
		if res.Commits, err = sourceCommitInfos(ctx, g, cfg, pm, sources); err != nil {
			return nil, warns, err
		}
	}
	all, err := ledger.Read(e.WS.LedgerPath())
	if err != nil {
		return nil, warns, internalErr(err, "read the ledger "+e.WS.LedgerPath())
	}
	for _, ent := range all {
		if strings.EqualFold(ent.Pair, pc.pair.Public) && ent.Feature == name {
			res.Ledger = append(res.Ledger, ent)
		}
	}
	return res, warns, nil
}

// sourceCommitInfos describes feature commits as they are on the branch,
// oldest first: each one's files are what it changes from its parent, so
// public commits the branch doesn't have yet aren't counted. A commit
// without a parent is compared with public main.
func sourceCommitInfos(ctx context.Context, g *gitx.Git, cfg config.Config, pm string, sources []string) ([]CommitInfo, error) {
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	infos := make([]CommitInfo, len(sources))
	for i, s := range sources {
		parent, ok, err := g.Resolve(ctx, s+"^")
		if err != nil {
			return nil, internalErr(err, "read a commit's parent")
		}
		if !ok {
			parent = pm
		}
		if infos[i], err = commitInfo(ctx, g, set, parent, s); err != nil {
			return nil, err
		}
	}
	return infos, nil
}
