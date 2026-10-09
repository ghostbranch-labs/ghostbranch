package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// behind is SHADOW_BEHIND when shadow main is a strict ancestor of public
// main. A diverged or missing shadow main says nothing here; sync reports
// it. A failure to compare the two is silent too: it's only a hint.
func (e *Engine) behind(ctx context.Context, pc *pairCtx) []contract.Warning {
	m, err := compareMains(ctx, pc.git)
	if err != nil {
		return nil
	}
	return m.warnings(pc.pair.Public)
}

// mains is shadow main against public main. Shadow or Public is "" when that
// branch is missing; Behind is how many commits shadow main is behind public
// main when it is a strict ancestor of it, else 0.
type mains struct {
	Shadow, Public string
	Behind         int
}

// compareMains reads both mains from the mirror and counts how far shadow
// main is behind, failing only when git does.
func compareMains(ctx context.Context, g *gitx.Git) (mains, error) {
	var m mains
	sm, ok1, err := g.Resolve(ctx, shadowRef("main"))
	if err != nil {
		return m, err
	}
	pm, ok2, err := g.Resolve(ctx, publicRef("main"))
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
		return m, fmt.Errorf("count the commits shadow main is behind: %w", err)
	}
	return m, nil
}

// warnings is SHADOW_BEHIND when shadow main is behind.
func (m mains) warnings(public string) []contract.Warning {
	if m.Behind == 0 {
		return nil
	}
	return []contract.Warning{{Code: contract.ShadowBehind,
		Message: fmt.Sprintf("shadow main is %d commit(s) behind public main; run ghostbranch sync --repo %s", m.Behind, public),
		Details: map[string]any{"behind": m.Behind, "shadow_main": m.Shadow, "public_main": m.Public}}}
}
