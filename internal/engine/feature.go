package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type FeatureStartResult struct {
	Name     string `json:"name"`
	Branch   string `json:"branch"`
	Base     string `json:"base"`
	Checkout string `json:"checkout"`
	DryRun   bool   `json:"dry_run"`
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
	if err := e.checkNameFree(ctx, pc, name); err != nil {
		return nil, warns, err
	}
	base, err := pc.git.MustResolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow main")
	}
	res = &FeatureStartResult{Name: name, Branch: name, Base: base, Checkout: "git fetch && git switch " + name, DryRun: e.DryRun}
	if e.DryRun {
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
	_ = pc.git.UpdateRef(ctx, shadowRef(name), base)
	_ = pc.git.UpdateRef(ctx, metaRef(name), metaSHA)
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
