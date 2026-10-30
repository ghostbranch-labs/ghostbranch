package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// behind is SHADOW_BEHIND when the shadow repo's copy of the base branch
// is a strict ancestor of public's. A diverged or missing shadow copy says
// nothing here; sync reports it. A failure to compare the two is silent
// too: it's only a hint.
func (e *Engine) behind(ctx context.Context, pc *pairCtx) []contract.Warning {
	m, err := compareMains(ctx, pc.git, pc.base)
	if err != nil {
		return nil
	}
	return m.warnings(pc.pair.Public)
}

// mains is the shadow repo's copy of the base branch against public's (the
// "mains" of a main pair). Base is the branch's name. Shadow or Public is ""
// when that copy is missing; Behind is how many commits the shadow copy is
// behind public's when it is a strict ancestor of it, else 0.
type mains struct {
	Base           string
	Shadow, Public string
	Behind         int
}

// compareMains reads both copies of the base branch from the mirror and
// counts how far the shadow copy is behind, failing only when git does.
func compareMains(ctx context.Context, g *gitx.Git, base string) (mains, error) {
	m := mains{Base: base}
	sm, ok1, err := g.Resolve(ctx, shadowRef(base))
	if err != nil {
		return m, err
	}
	pm, ok2, err := g.Resolve(ctx, publicRef(base))
	if err != nil {
		return m, err
	}
	m.Shadow, m.Public = sm, pm
	if !ok1 || !ok2 || sm == pm {
		return m, nil
	}
	anc, err := g.IsAncestor(ctx, sm, pm)
	if err != nil || !anc {
		return m, err
	}
	out, err := g.Run(ctx, "rev-list", "--count", sm+".."+pm)
	if err != nil {
		return m, err
	}
	if m.Behind, err = strconv.Atoi(strings.TrimSpace(out)); err != nil {
		return m, fmt.Errorf("count the commits shadow %s is behind: %w", base, err)
	}
	return m, nil
}

// warnings is SHADOW_BEHIND when the shadow copy is behind.
func (m mains) warnings(public string) []contract.Warning {
	if m.Behind == 0 {
		return nil
	}
	return []contract.Warning{{Code: contract.ShadowBehind,
		Message: fmt.Sprintf("shadow %s is %d commit(s) behind public %s; run ghostbranch sync --repo %s", m.Base, m.Behind, m.Base, public),
		Details: map[string]any{"behind": m.Behind, "shadow_main": m.Shadow, "public_main": m.Public}}}
}
