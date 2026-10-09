package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type FeatureStartResult struct {
	Name         string           `json:"name"`
	Branch       string           `json:"branch"`
	Base         string           `json:"base"`
	Checkout     [][]string       `json:"checkout"`      // argvs that get the branch into a checkout, in order
	CheckoutHint string           `json:"checkout_hint"` // Checkout as one shell line
	Preview      *preview.Preview `json:"preview"`
}

// SetCheckout sets Checkout and CheckoutHint, its one-line form.
func (r *FeatureStartResult) SetCheckout(argvs [][]string) {
	r.Checkout = argvs
	lines := make([]string, len(argvs))
	for i, a := range argvs {
		lines[i] = strings.Join(a, " ")
	}
	r.CheckoutHint = strings.Join(lines, " && ")
}

// FeatureStart creates a feature branch from shadow main and its metadata
// ref in one atomic push. It does not touch the user's checkout.
func (e *Engine) FeatureStart(ctx context.Context, p workspace.Pair, name string) (res *FeatureStartResult, warns []contract.Warning, err error) {
	defer func() { e.record("feature start", &p, name, "", "", err) }()
	if err := feature.ValidateName(name, "name"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature start", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	// A feature started from a stale shadow main would be based on old
	// code: refuse, before anything is created (--plan too).
	mn, err := compareMains(ctx, pc.git)
	if err != nil {
		return nil, warns, internalErr(err, "compare shadow main with public main")
	}
	if mn.Behind > 0 {
		return nil, warns, contract.New(contract.ShadowBehindError,
			"shadow main is %d commit(s) behind public main, so a feature started now would be based on old code; run ghostbranch sync --repo %s, then start the feature again", mn.Behind, p.Public).
			WithDetails(map[string]any{"public_main": mn.Public, "shadow_main": mn.Shadow})
	}
	if err := e.checkNameFree(ctx, pc, name); err != nil {
		return nil, warns, err
	}
	base, err := pc.git.MustResolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow main")
	}
	res = &FeatureStartResult{Name: name, Branch: name, Base: base}
	res.SetCheckout([][]string{{"git", "fetch", remoteShadow}, {"git", "switch", name}})
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"shadow_main": base})
		res.Preview.Add(preview.PushRef(p.Shadow, "refs/heads/"+name, "", base), preview.PushRef(p.Shadow, remoteMetaRef(name), "", ""))
		return res, warns, nil
	}
	m := feature.Meta{SchemaVersion: feature.SchemaVersion, Name: name, Branch: name, CreatedBy: e.Actor, CreatedAt: e.now(), Base: base}
	metaSHA, err := e.saveMeta(ctx, pc, m, "")
	if err != nil {
		return nil, warns, err
	}
	opt := gitx.PushOptions{Atomic: true, Leases: []gitx.Lease{{Ref: "refs/heads/" + name}, {Ref: remoteMetaRef(name)}}}
	if err := e.push(ctx, pc, remoteShadow, opt, base+":refs/heads/"+name, metaSHA+":"+remoteMetaRef(name)); err != nil {
		if contract.From(err).Code == contract.RefMoved {
			return nil, warns, contract.New(contract.BranchExists, "feature %q was just created elsewhere", name).WithField("name")
		}
		return nil, warns, err
	}
	warns = addWarnings(warns, e.updateMirror(ctx, pc.git, shadowRef(name), base)...)
	warns = addWarnings(warns, e.updateMirror(ctx, pc.git, metaRef(name), metaSHA)...)
	return res, warns, nil
}

// checkNameFree rejects names that exist, or would clash as a file/folder
// pair (auth vs auth/login), in the shadow repo, the public repo, or among
// features.
func (e *Engine) checkNameFree(ctx context.Context, pc *pairCtx, name string) error {
	spaces := []struct{ prefix, where string }{
		{"refs/remotes/shadow/", "shadow"},
		{"refs/remotes/public/", "public"},
		{"refs/shadow-meta/features/", "feature"},
	}
	for _, s := range spaces {
		refs, err := pc.git.Refs(ctx, strings.TrimSuffix(s.prefix, "/"))
		if err != nil {
			return internalErr(err, "list refs")
		}
		for ref := range refs {
			b := strings.TrimPrefix(ref, s.prefix)
			where := s.where
			if where == "feature" {
				where = "shadow"
			}
			var ce *contract.Error
			switch {
			case b == name:
				ce = contract.New(contract.BranchExists, "%q already exists in the %s repo", name, where)
			case strings.HasPrefix(name, b+"/") || strings.HasPrefix(b, name+"/"):
				ce = contract.New(contract.BranchExists, "%q clashes with the existing branch %q in the %s repo: git cannot have both a branch and a folder of that name", name, b, where)
			default:
				continue
			}
			return ce.WithField("name").
				WithDetails(map[string]any{"conflicts_with": b, "in": s.where})
		}
	}
	return nil
}
