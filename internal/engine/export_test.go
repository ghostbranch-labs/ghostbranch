package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// BaseOf opens p read-only, as status does, and returns the pair's base
// branch, for the engine_test package.
func BaseOf(e *Engine, ctx context.Context, p workspace.Pair) (string, error) {
	pc, err := e.open(ctx, "status", p, false)
	if err != nil {
		return "", err
	}
	defer pc.unlock()
	return pc.base, nil
}
