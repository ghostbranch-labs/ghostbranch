package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
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

// OpenBaseIn runs openBase for op against the mirror at dir, for the
// engine_test package.
func OpenBaseIn(e *Engine, ctx context.Context, op, dir string) error {
	return e.openBase(ctx, &pairCtx{op: op, git: gitx.New(dir)})
}
