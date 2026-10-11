package engine

import "context"

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
		head, ok, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
		if err != nil || !ok {
			return ""
		}
		fresh, err := e.gateStackFresh(ctx, pc, n, m)
		if err != nil {
			return ""
		}
		if !m.Gate.Passed() || !m.Gate.FreshFor(head, pm, O) || !fresh {
			return n
		}
	}
	return ""
}
