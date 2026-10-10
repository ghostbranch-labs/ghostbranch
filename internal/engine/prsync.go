package engine

import (
	"context"
	"fmt"
	"strconv"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// PRSync is how a published feature's open public PR compares with the
// feature: Behind counts the commits on the private branch the PR doesn't
// have (gate and promote bring them). Foreign counts the commits on the
// PR's branch Ghostbranch didn't publish; it is always 0 until the survey
// learns to detect them.
type PRSync struct {
	Behind  int `json:"behind"`
	Foreign int `json:"foreign"`
}

// prSyncOf compares an open published feature with its PR. head is the
// feature branch's head ("" when it is gone), published the head it was
// published from (publishedHead; "" when unknown), pm public main, and
// below the heads of the features under it. A count that can't be worked
// out is 0, with a PR_STATE_UNKNOWN warning: the survey only reports, and
// the next one tries again.
func (e *Engine) prSyncOf(ctx context.Context, pc *pairCtx, m feature.Meta, head, published, pm string, below map[string]string) (*PRSync, []contract.Warning) {
	ps := &PRSync{}
	var warns []contract.Warning
	if head != "" && head != published {
		n, err := behindCount(ctx, pc.git, head, published, pm, below)
		if err != nil {
			warns = append(warns, contract.Warning{Code: contract.PRStateUnknown,
				Message: fmt.Sprintf("couldn't count the commits feature %s's public PR #%d doesn't have yet (%v); it counts 0 until the next survey", m.Name, m.Promoted.PublicPR, err),
				Details: map[string]any{"feature": m.Name, "public_pr": m.Promoted.PublicPR}})
		}
		ps.Behind = n
	}
	return ps, warns
}

// behindCount counts the commits on head that are in none of: published
// (the head the feature was published from), public main pm, and the
// heads of the features below it. That is what the feature added since it
// was published, whatever rebases happened since: main's commits and the
// parents' commits never count. When published isn't known, or isn't in
// this machine's mirror (it was rewritten away on another machine), every
// commit of the feature's own counts.
func behindCount(ctx context.Context, g *gitx.Git, head, published, pm string, below map[string]string) (int, error) {
	args := []string{"rev-list", "--count", head, "^" + pm}
	if published != "" {
		_, ok, err := g.Resolve(ctx, published)
		if err != nil {
			return 0, err
		}
		if ok {
			args = append(args, "^"+published)
		}
	}
	for _, h := range below {
		args = append(args, "^"+h)
	}
	out, err := g.Run(ctx, args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(out)
}
