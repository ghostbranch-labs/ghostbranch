package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// behind is SHADOW_BEHIND when shadow main is a strict ancestor of public
// main. A diverged or missing shadow main says nothing here; sync reports it.
func (e *Engine) behind(ctx context.Context, pc *pairCtx) []contract.Warning {
	g := pc.git
	sm, ok1, err1 := g.Resolve(ctx, shadowRef("main"))
	pm, ok2, err2 := g.Resolve(ctx, publicRef("main"))
	if err1 != nil || err2 != nil || !ok1 || !ok2 || sm == pm {
		return nil
	}
	if anc, err := g.IsAncestor(ctx, sm, pm); err != nil || !anc {
		return nil
	}
	out, err := g.Run(ctx, "rev-list", "--count", sm+".."+pm)
	n, cerr := strconv.Atoi(strings.TrimSpace(out))
	if err != nil || cerr != nil {
		return nil
	}
	return []contract.Warning{{Code: contract.ShadowBehind,
		Message: fmt.Sprintf("shadow main is %d commit(s) behind public main; run ghostbranch sync", n),
		Details: map[string]any{"behind": n, "shadow_main": sm, "public_main": pm}}}
}
