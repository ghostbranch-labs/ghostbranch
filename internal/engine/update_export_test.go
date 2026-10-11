package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// PRUpdateView is what prHead read of a feature's open PR, for the
// engine_test package.
type PRUpdateView struct {
	PR       int
	Head     string
	Base     string
	Public   []string
	Foreign  []string
	Restores []string
}

// PRUpdateForTest opens p read-only and reads name's open PR as an update
// would (prHead); nil when it has none.
func (e *Engine) PRUpdateForTest(ctx context.Context, p workspace.Pair, name string) (*PRUpdateView, error) {
	pc, err := e.open(ctx, "gate", p, false)
	if err != nil {
		return nil, err
	}
	defer pc.unlock()
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, err
	}
	u, err := e.prHead(ctx, pc, &m)
	if err != nil || u == nil {
		return nil, err
	}
	v := &PRUpdateView{PR: u.PR, Head: u.Head, Base: u.Base, Public: u.Public, Restores: u.Restores}
	for _, f := range u.Foreign {
		v.Foreign = append(v.Foreign, f.SHA)
	}
	return v, nil
}
