package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
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
