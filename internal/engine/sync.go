package engine

import (
	"context"
	"fmt"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type SyncResult struct {
	Public           string `json:"public"`
	Shadow           string `json:"shadow"`
	Action           string `json:"action"`
	PublicMain       string `json:"public_main"`
	ShadowMainBefore string `json:"shadow_main_before"`
	ShadowMainAfter  string `json:"shadow_main_after"`
	DryRun           bool   `json:"dry_run"`
}

// Sync fast-forwards shadow main to public main. It never force-pushes: a
// diverged shadow main is reported and left alone.
func (e *Engine) Sync(ctx context.Context, p workspace.Pair) (res *SyncResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		if res != nil {
			summary = fmt.Sprintf("%s: %s -> %s", res.Action, short(res.ShadowMainBefore), short(res.ShadowMainAfter))
		}
		e.record("sync", &p, "", "", summary, err)
	}()
	pc, err := e.open(ctx, "sync", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	g := pc.git
	pm, err := g.MustResolve(ctx, publicRef("main"))
	if err != nil {
		return nil, nil, internalErr(err, "read public main")
	}
	sm, ok, err := g.Resolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, nil, internalErr(err, "read shadow main")
	}
	res = &SyncResult{Public: p.Public, Shadow: p.Shadow, PublicMain: pm, ShadowMainBefore: sm, ShadowMainAfter: sm, DryRun: e.DryRun}
	if ok && sm == pm {
		res.Action = "noop"
		return res, nil, nil
	}
	ff := !ok
	if ok {
		if ff, err = g.IsAncestor(ctx, sm, pm); err != nil {
			return res, nil, internalErr(err, "compare shadow and public main")
		}
	}
	if !ff {
		res.Action = "diverged"
		return res, nil, contract.New(contract.SyncDiverged,
			"shadow main %s has commits that are not in public main %s; nothing was pushed", short(sm), short(pm)).
			WithDetails(map[string]any{"shadow_main": sm, "public_main": pm})
	}
	res.Action = "fast_forward"
	if e.DryRun {
		return res, nil, nil
	}
	e.progressf("fast-forwarding shadow main %s -> %s", short(sm), short(pm))
	opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/main", Expect: sm}}}
	if err := e.push(ctx, pc, remoteShadow, opt, pm+":refs/heads/main"); err != nil {
		return res, nil, err
	}
	_ = g.UpdateRef(ctx, shadowRef("main"), pm)
	res.ShadowMainAfter = pm
	return res, nil, nil
}
