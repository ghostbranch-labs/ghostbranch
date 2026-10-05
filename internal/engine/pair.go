package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const (
	remotePublic = "public"
	remoteShadow = "shadow"
)

func publicRef(branch string) string   { return "refs/remotes/public/" + branch }
func shadowRef(branch string) string   { return "refs/remotes/shadow/" + branch }
func metaRef(name string) string       { return "refs/shadow-meta/features/" + name }
func planRef(id string) string         { return "refs/shadow-meta/plans/" + id } //nolint:unused // used by later tasks
func remoteMetaRef(name string) string { return "refs/ghostbranch/features/" + name }
func remotePlanRef(id string) string   { return "refs/ghostbranch/plans/" + id } //nolint:unused // used by later tasks
func gateBranch(name string) string    { return "ghostbranch-gate/" + name }

// pairCtx is one operation's view of a pair: a fresh mirror and the lock.
type pairCtx struct {
	pair   workspace.Pair
	op     string
	opID   string
	git    *gitx.Git
	public forge.Repo
	shadow forge.Repo
	unlock func()
}

// open runs the shared preflight: look up both repos, refuse unless the
// shadow repo is private and not a fork, take the lock, and fetch.
func (e *Engine) open(ctx context.Context, op string, p workspace.Pair, mutating bool) (*pairCtx, error) {
	pub, err := e.Forge.GetRepo(ctx, p.Public)
	if err != nil {
		return nil, forgeErr(err, "look up public repo %s", p.Public)
	}
	sh, err := e.Forge.GetRepo(ctx, p.Shadow)
	if err != nil {
		return nil, forgeErr(err, "look up shadow repo %s", p.Shadow)
	}
	if !sh.Private || sh.Fork {
		return nil, contract.New(contract.ShadowNotPrivate,
			"shadow repo %s must be private and not a fork; refusing to run", p.Shadow)
	}
	opID, err := e.newID(strings.ReplaceAll(op, " ", "-"))
	if err != nil {
		return nil, internalErr(err, "make operation id")
	}
	unlock := func() {}
	if mutating && !e.DryRun {
		if unlock, err = workspace.Lock(e.WS.LockPath(p), e.Now()); err != nil {
			return nil, err
		}
	}
	pc := &pairCtx{pair: p, op: op, opID: opID, public: pub, shadow: sh, unlock: unlock}
	if pc.git, err = e.mirror(ctx, p, pub, sh); err == nil {
		err = e.fetch(ctx, pc.git)
	}
	if err != nil {
		unlock()
		return nil, err
	}
	return pc, nil
}

func (e *Engine) mirror(ctx context.Context, p workspace.Pair, pub, sh forge.Repo) (*gitx.Git, error) {
	dir := e.WS.MirrorDir(p)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if _, err := gitx.Init(ctx, dir, true); err != nil {
			return nil, internalErr(err, "create the mirror")
		}
	}
	g := gitx.New(dir)
	g.Config = e.gitConfig()
	if err := g.AddRemote(ctx, remotePublic, pub.CloneURL); err != nil {
		return nil, internalErr(err, "configure the public remote")
	}
	if err := g.AddRemote(ctx, remoteShadow, sh.CloneURL); err != nil {
		return nil, internalErr(err, "configure the shadow remote")
	}
	return g, nil
}

func (e *Engine) fetch(ctx context.Context, g *gitx.Git) error {
	if err := g.Fetch(ctx, remotePublic, "+refs/heads/*:refs/remotes/public/*", "+refs/tags/*:refs/public-tags/*"); err != nil {
		return remoteErr(err, "fetch the public repo")
	}
	if err := g.Fetch(ctx, remoteShadow, "+refs/heads/*:refs/remotes/shadow/*",
		"+refs/tags/*:refs/shadow-tags/*", "+refs/ghostbranch/*:refs/shadow-meta/*"); err != nil {
		return remoteErr(err, "fetch the shadow repo")
	}
	return nil
}

// push is the only way the engine writes to a remote. Pushing to the public
// remote is refused outside promote and apply.
func (e *Engine) push(ctx context.Context, pc *pairCtx, remote string, opt gitx.PushOptions, refspecs ...string) error {
	if remote == remotePublic && pc.op != "promote" && pc.op != "apply" {
		return contract.New(contract.Internal, "refusing to push to the public repo from %q: only promote and apply may", pc.op)
	}
	if e.DryRun {
		return contract.New(contract.Internal, "refusing to push during --dry-run")
	}
	if e.OnPush != nil {
		e.OnPush(pc.op, remote, append([]string(nil), refspecs...))
	}
	err := pc.git.Push(ctx, remote, opt, refspecs...)
	var rej *gitx.RejectedError
	if errors.As(err, &rej) {
		return contract.Wrap(contract.RefMoved, err, "a ref changed on the %s repo while this ran; re-run the command", remote).
			WithDetails(map[string]any{"refs": rej.Refs})
	}
	if err != nil {
		return remoteErr(err, "push to the "+remote+" repo")
	}
	return nil
}

// readConfig reads .ghostbranch.yml from the overlay branch, never from a
// feature, and checks the overlay branch holds only overlay paths. It
// returns the config and the overlay commit O.
func (e *Engine) readConfig(ctx context.Context, pc *pairCtx) (config.Config, string, []contract.Warning, error) {
	g := pc.git
	O, ok, err := g.Resolve(ctx, shadowRef(OverlayBranch))
	if err != nil {
		return config.Config{}, "", nil, internalErr(err, "read the overlay branch")
	}
	if !ok {
		return config.Config{}, "", nil, contract.New(contract.ConfigInvalid,
			"shadow repo %s has no %q overlay branch; run ghostbranch init", pc.pair.Shadow, OverlayBranch)
	}
	raw, ok, err := g.ShowFile(ctx, O, ".ghostbranch.yml")
	if err != nil || !ok {
		return config.Config{}, "", nil, contract.Wrap(contract.ConfigInvalid, err, "the overlay branch has no .ghostbranch.yml")
	}
	cfg, warns, err := config.Parse(raw)
	if err != nil {
		return config.Config{}, "", warns, err
	}
	if !strings.EqualFold(cfg.Public, pc.pair.Public) || !strings.EqualFold(cfg.Shadow, pc.pair.Shadow) {
		return config.Config{}, "", warns, contract.New(contract.ConfigInvalid,
			".ghostbranch.yml names %s -> %s, but this pair is %s -> %s", cfg.Public, cfg.Shadow, pc.pair.Public, pc.pair.Shadow)
	}
	entries, err := g.ListTree(ctx, O)
	if err != nil {
		return config.Config{}, "", warns, internalErr(err, "list the overlay branch")
	}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	var bad []string
	for _, en := range entries {
		if !set.Match(en.Path) {
			bad = append(bad, en.Path)
		}
	}
	if len(bad) > 0 {
		return config.Config{}, "", warns, contract.New(contract.ConfigInvalid,
			"the overlay branch may only hold overlay paths; found %s", strings.Join(bad, ", ")).
			WithDetails(map[string]any{"paths": bad})
	}
	return cfg, O, warns, nil
}

// readMeta returns a feature's metadata and the metadata commit's SHA.
func (e *Engine) readMeta(ctx context.Context, pc *pairCtx, name string) (feature.Meta, string, error) {
	sha, ok, err := pc.git.Resolve(ctx, metaRef(name))
	if err != nil {
		return feature.Meta{}, "", internalErr(err, "read feature metadata")
	}
	if !ok {
		return feature.Meta{}, "", contract.New(contract.FeatureNotFound,
			"no feature %q in %s; start one with ghostbranch feature start", name, pc.pair.Shadow).WithField("feature")
	}
	b, ok, err := pc.git.ShowFile(ctx, sha, feature.MetaFile)
	if err != nil || !ok {
		return feature.Meta{}, "", contract.Wrap(contract.Internal, err, "feature %q has no %s", name, feature.MetaFile)
	}
	m, err := feature.Decode(b)
	if err != nil {
		return feature.Meta{}, "", internalErr(err, "parse "+feature.MetaFile)
	}
	return m, sha, nil
}

// saveMeta writes a metadata commit (parent "" for a new feature) without pushing it.
func (e *Engine) saveMeta(ctx context.Context, pc *pairCtx, m feature.Meta, parent string) (string, error) {
	g := pc.git
	blob, err := g.HashObject(ctx, feature.Encode(m))
	if err != nil {
		return "", internalErr(err, "write feature.json")
	}
	tree, err := g.MakeTree(ctx, []gitx.TreeEntry{{Mode: "100644", Type: "blob", SHA: blob, Path: feature.MetaFile}})
	if err != nil {
		return "", internalErr(err, "write the metadata tree")
	}
	var parents []string
	if parent != "" {
		parents = []string{parent}
	}
	who := e.bot()
	sha, err := g.CommitTree(ctx, tree, parents, "ghostbranch: update feature "+m.Name, who, who)
	if err != nil {
		return "", internalErr(err, "write the metadata commit")
	}
	return sha, nil
}

// updateMeta saves and pushes new metadata, leased on the previous commit.
func (e *Engine) updateMeta(ctx context.Context, pc *pairCtx, m feature.Meta, parent string) error {
	sha, err := e.saveMeta(ctx, pc, m, parent)
	if err != nil {
		return err
	}
	opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: remoteMetaRef(m.Name), Expect: parent}}}
	if err := e.push(ctx, pc, remoteShadow, opt, sha+":"+remoteMetaRef(m.Name)); err != nil {
		return err
	}
	if err := pc.git.UpdateRef(ctx, metaRef(m.Name), sha); err != nil {
		return internalErr(err, "update the local metadata ref")
	}
	return nil
}
