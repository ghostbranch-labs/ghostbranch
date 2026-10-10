package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// SurveyForTest opens p read-only and surveys it, for the engine_test
// package.
func (e *Engine) SurveyForTest(ctx context.Context, p workspace.Pair, names ...string) ([]FeatureReport, []contract.Warning, error) {
	pc, err := e.open(ctx, "survey", p, false)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	return e.survey(ctx, pc, names...)
}

// AdoptCheckForTest opens p (fetching it into the mirror), runs between on
// the mirror's directory, then runs init's adopt check.
func (e *Engine) AdoptCheckForTest(ctx context.Context, p workspace.Pair, between func(mirror string)) error {
	pc, err := e.open(ctx, "init", p, false)
	if err != nil {
		return err
	}
	defer pc.unlock()
	between(e.WS.MirrorDir(p))
	return e.checkAdoptable(ctx, pc)
}

// ReparentChildrenForTest surveys p, lets edit change parent's record as
// read (not in the shadow repo), and re-parents parent's children as sync's
// close would, in a dry run of the writes.
func (e *Engine) ReparentChildrenForTest(ctx context.Context, p workspace.Pair, parent string, edit func(*feature.Meta)) error {
	pc, err := e.open(ctx, "sync", p, false)
	if err != nil {
		return err
	}
	defer pc.unlock()
	rs, _, err := e.survey(ctx, pc)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Name == parent && r.meta != nil {
			m := *r.meta
			edit(&m)
			r.meta = &m
			dry := e.DryRun
			e.DryRun = true
			defer func() { e.DryRun = dry }()
			_, err := e.reparentChildren(ctx, pc, r, childReports(rs, parent))
			return err
		}
	}
	return contract.New(contract.FeatureNotFound, "no feature %s", parent)
}
