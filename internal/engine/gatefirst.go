package engine

import (
	"context"

	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

// firstToGate is the lowest feature, from the bottom of name's stack up to
// name itself, whose gate isn't green and fresh: for its head, public main,
// the overlay and the stack below it. It is the one next gates first, as
// every gate above builds on it. It is "" when every one is fresh, or when
// a record or branch can't be read (next then names the feature it knows).
func (e *Engine) firstToGate(ctx context.Context, pc *pairCtx, name, pm, O string) string {
	var chain []string
	seen := map[string]bool{}
	for n := name; n != "" && !seen[n]; {
		seen[n] = true
		chain = append([]string{n}, chain...)
		m, _, _, err := e.readMetaAny(ctx, pc, n)
		if err != nil {
			return ""
		}
		n = parentFeature(m)
	}
	for _, n := range chain {
		m, _, _, err := e.readMetaAny(ctx, pc, n)
		if err != nil {
			return ""
		}
		fresh, known := e.gateFresh(ctx, pc, n, m, pm, O)
		if !known {
			return ""
		}
		if !fresh {
			return n
		}
	}
	return ""
}

// gateFresh is the one test of a level's own gate that next and firstToGate
// share: green, for the branch's head, public main and the overlay, and
// built on the stack below as it is now (gateStackFresh). resolveStack
// applies it too and adds the tree link and reusableGate. known is false
// when the branch or the stack below can't be read.
func (e *Engine) gateFresh(ctx context.Context, pc *pairCtx, name string, m feature.Meta, pm, O string) (fresh, known bool) {
	head, ok, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
	if err != nil || !ok {
		return false, false
	}
	stackFresh, err := e.gateStackFresh(ctx, pc, name, m)
	if err != nil {
		return false, false
	}
	return m.Gate.Passed() && m.Gate.FreshFor(head, pm, O) && stackFresh, true
}
