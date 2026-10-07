package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type SyncResult struct {
	Public           string           `json:"public"`
	Shadow           string           `json:"shadow"`
	Action           string           `json:"action"`
	PublicMain       string           `json:"public_main"`
	ShadowMainBefore string           `json:"shadow_main_before"`
	ShadowMainAfter  string           `json:"shadow_main_after"`
	Branches         []SyncBranch     `json:"branches"`
	Preview          *preview.Preview `json:"preview"`
}

// SyncBranch is what sync did to one branch in sync.branches. From is ""
// when the shadow repo had no such branch.
type SyncBranch struct {
	Name   string `json:"name"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"` // up_to_date | created | fast_forwarded | diverged
}

// Sync fast-forwards each branch in sync.branches in the shadow repo to the
// public branch, creating it when missing. It never force-pushes: a diverged
// branch is reported and left alone while the others still fast-forward.
// The top-level main fields describe main.
func (e *Engine) Sync(ctx context.Context, p workspace.Pair) (res *SyncResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		if res != nil {
			parts := make([]string, 0, len(res.Branches))
			for _, b := range res.Branches {
				parts = append(parts, b.Name+" "+b.Status)
			}
			summary = strings.Join(parts, ", ")
		}
		e.record("sync", &p, "", "", summary, err)
	}()
	pc, err := e.open(ctx, "sync", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	cfg, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	g := pc.git
	pubs := map[string]string{}
	for _, b := range cfg.Sync.Branches {
		pb, ok, err := g.Resolve(ctx, publicRef(b))
		if err != nil {
			return nil, warns, internalErr(err, "read public "+b)
		}
		if !ok {
			return nil, warns, contract.New(contract.ConfigInvalid,
				"sync.branches names %q, but the public repo has no such branch; nothing was pushed", b).WithField("sync.branches")
		}
		pubs[b] = pb
	}
	pm := pubs["main"]
	sm, _, err := g.Resolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow main")
	}
	res = &SyncResult{Public: p.Public, Shadow: p.Shadow, PublicMain: pm, ShadowMainBefore: sm, ShadowMainAfter: sm, Branches: []SyncBranch{}}
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"public_main": pm, "shadow_main": sm})
	}
	var diverged []string
	for _, b := range cfg.Sync.Branches {
		pb := pubs[b]
		sb, ok, err := g.Resolve(ctx, shadowRef(b))
		if err != nil {
			return res, warns, internalErr(err, "read shadow "+b)
		}
		br := SyncBranch{Name: b, From: sb, To: sb}
		switch {
		case ok && sb == pb:
			br.Status = "up_to_date"
		case ok:
			ff, err := g.IsAncestor(ctx, sb, pb)
			if err != nil {
				return res, warns, internalErr(err, "compare shadow and public "+b)
			}
			if !ff {
				br.Status = "diverged"
				diverged = append(diverged, b)
				break
			}
			br.Status = "fast_forwarded"
		default:
			br.Status = "created"
		}
		if br.Status == "fast_forwarded" || br.Status == "created" {
			if e.DryRun {
				res.Preview.Add(preview.PushRef(p.Shadow, "refs/heads/"+b, sb, pb))
			} else {
				e.progressf("fast-forwarding shadow %s %s -> %s", b, short(sb), short(pb))
				opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + b, Expect: sb}}}
				if err := e.push(ctx, pc, remoteShadow, opt, pb+":refs/heads/"+b); err != nil {
					return res, warns, err
				}
				_ = g.UpdateRef(ctx, shadowRef(b), pb)
				br.To = pb
			}
		}
		if b == "main" {
			res.Action = syncAction[br.Status]
			res.ShadowMainAfter = br.To
		}
		res.Branches = append(res.Branches, br)
	}
	if len(diverged) > 0 {
		pronoun := "them"
		if len(diverged) == 1 {
			pronoun = "it"
		}
		return res, warns, contract.New(contract.SyncDiverged,
			"shadow %s has commits that are not in public; nothing was pushed for %s", strings.Join(diverged, ", "), pronoun).
			WithDetails(map[string]any{"branches": diverged, "shadow_main": sm, "public_main": pm})
	}
	return res, warns, nil
}

// syncAction is the top-level action for main's per-branch status.
var syncAction = map[string]string{"up_to_date": "noop", "fast_forwarded": "fast_forward", "created": "fast_forward", "diverged": "diverged"}
