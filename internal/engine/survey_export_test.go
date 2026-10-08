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

// ClosableWithoutConsole exposes closableWithoutConsole to engine_test.
var ClosableWithoutConsole = closableWithoutConsole
