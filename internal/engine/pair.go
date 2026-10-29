package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
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

func publicRef(branch string) string      { return "refs/remotes/public/" + branch }
func shadowRef(branch string) string      { return "refs/remotes/shadow/" + branch }
func metaRef(name string) string          { return "refs/shadow-meta/features/" + name }
func planRef(id string) string            { return "refs/shadow-meta/plans/" + id }
func remoteMetaRef(name string) string    { return "refs/ghostbranch/features/" + name }
func remotePlanRef(id string) string      { return "refs/ghostbranch/plans/" + id }
func archiveRef(name string) string       { return "refs/shadow-meta/archive/" + name }
func remoteArchiveRef(name string) string { return feature.ArchiveRef(name) }
func gateBranch(name string) string       { return "ghostbranch-gate/" + name }

// pairCtx is one operation's view of a pair: a fresh mirror and the lock.
type pairCtx struct {
	pair   workspace.Pair
	op     string
	opID   string
	git    *gitx.Git
	public forge.Repo
	shadow forge.Repo
	// base is the pair's base branch, the public line every command works
	// against: public_branch from the overlay config, main when the key is
	// missing. open sets it; it is "" only in doctor, when the overlay
	// config can't be read.
	base string
	// unlock releases the lock at the end of the operation; it is release.
	unlock func()
	// lockPath is the pair lock's file; empty in a dry run. held reports
	// whether this operation holds it now, and drop releases the hold (the
	// file stays).
	lockPath string
	held     bool
	drop     func()
}

// release drops the pair lock, early before a long CI wait or at the end
// of the operation. It never removes a lock it doesn't hold, so a lock
// another run took meanwhile is safe.
func (pc *pairCtx) release() {
	if pc.held {
		pc.drop()
		pc.held = false
	}
}

// sameRepo refuses a pair whose shadow repo is the public repo itself
// (GitHub names are case-insensitive).
func sameRepo(p workspace.Pair) error {
	if !strings.EqualFold(p.Public, p.Shadow) {
		return nil
	}
	return contract.New(contract.BadFlag, "the shadow repo must be a different repo from the public one (both are %s)", p.Public).WithField("shadow")
}

// open runs the shared preflight: refuse a shadow repo that is the public
// repo, look up both repos, refuse unless the shadow repo is private and not
// a fork, take the lock, fetch, and read the pair's base branch. Every
// command but init and doctor stops with BASE_BRANCH_CHANGED when the
// public repo no longer has that branch.
func (e *Engine) open(ctx context.Context, op string, p workspace.Pair, mutating bool) (*pairCtx, error) {
	if err := sameRepo(p); err != nil {
		return nil, err
	}
	st := e.startStep("Reading %s and %s from GitHub", p.Public, p.Shadow)
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
	pc := &pairCtx{pair: p, op: op, opID: opID, public: pub, shadow: sh}
	pc.unlock = pc.release
	if !e.DryRun {
		pc.lockPath = e.WS.LockPath(p)
	}
	if mutating && !e.DryRun {
		e.tightenState()
		if pc.drop, err = workspace.Lock(pc.lockPath, e.Now(), op); err != nil {
			return nil, err
		}
		pc.held = true
	}
	// The fetch can take minutes on a large repo: its own step, so the
	// command isn't silent under the "Reading" line meanwhile.
	st.Done("")
	st = e.startStep("Updating the local copy of %s and %s", p.Public, p.Shadow)
	if pc.git, err = e.mirror(ctx, p, pub, sh); err == nil {
		if mutating && !e.DryRun {
			e.privateMirror(ctx, pc.git)
		}
		err = e.fetch(ctx, pc.git)
	}
	if err != nil {
		pc.release()
		return nil, err
	}
	st.Done("")
	if err := e.openBase(ctx, pc); err != nil {
		pc.release()
		return nil, err
	}
	return pc, nil
}

// openBase sets pc.base and checks the public repo still has it. A config
// that can't be read stops every command but two: doctor reports it in its
// config check (pc.base stays ""), and init leaves an overlay it can't read
// to checkAdoptable, which refuses it before any write. Neither checks the
// branch here: init checks it itself once checkAdoptable has passed, and
// doctor reports a missing one.
func (e *Engine) openBase(ctx context.Context, pc *pairCtx) error {
	base, err := e.readBase(ctx, pc)
	switch ce := contract.From(err); {
	case err == nil:
		pc.base = base
	case pc.op == "doctor":
		return nil
	case pc.op == "init" && (ce.Code == contract.ConfigInvalid || ce.Code == contract.UpgradeNeeded):
		pc.base = defaultBase(pc.public)
	default:
		return err
	}
	if pc.op == "init" || pc.op == "doctor" {
		return nil
	}
	_, ok, err := pc.git.Resolve(ctx, publicRef(pc.base))
	if err != nil {
		return internalErr(err, "read public "+pc.base)
	}
	if !ok {
		return baseGone(pc)
	}
	return nil
}

// baseGone is BASE_BRANCH_CHANGED: the public repo no longer has pc.base,
// the branch the pair was set up on.
func baseGone(pc *pairCtx) error {
	return contract.New(contract.BaseBranchChanged,
		"%s no longer has the branch %s this pair was set up on (its default branch is now %s); Ghostbranch doesn't follow a renamed default branch yet. See ghostbranch explain BASE_BRANCH_CHANGED.",
		pc.pair.Public, pc.base, pc.public.DefaultBranch).
		WithDetails(map[string]any{"recorded": pc.base, "current": pc.public.DefaultBranch})
}

// readBase reads the pair's base branch from the overlay's .ghostbranch.yml:
// public_branch, or main when the key is missing. Before init has made the
// overlay, it is the public repo's default branch for init and doctor (which
// reports the missing overlay itself); every other command stops there with
// readConfig's error, so none acts on a branch the pair never recorded. A
// config that doesn't parse is an error, never a guess: guessing main would
// act on the wrong branch.
func (e *Engine) readBase(ctx context.Context, pc *pairCtx) (string, error) {
	O, ok, err := pc.git.Resolve(ctx, shadowRef(OverlayBranch))
	if err != nil {
		return "", internalErr(err, "read the overlay branch")
	}
	if !ok {
		if pc.op != "init" && pc.op != "doctor" {
			return "", noOverlay(pc)
		}
		return defaultBase(pc.public), nil
	}
	raw, ok, err := pc.git.ShowFile(ctx, O, ".ghostbranch.yml")
	if err != nil || !ok {
		return "", contract.Wrap(contract.ConfigInvalid, err, "the overlay branch has no .ghostbranch.yml")
	}
	cfg, _, err := config.Parse(raw)
	if err != nil {
		return "", upgradeNeeded(err)
	}
	return cfg.Base(), nil
}

// defaultBase is the public repo's default branch, or main when GitHub
// didn't say.
func defaultBase(pub forge.Repo) string {
	return cmp.Or(pub.DefaultBranch, config.Config{}.Base())
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
	if err := g.ProtectAttributes(ctx); err != nil {
		return nil, internalErr(err, "protect the mirror from in-tree attributes")
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
	var refused *gitx.RefusedError
	if errors.As(err, &refused) {
		msg := fmt.Sprintf("the %s repo refused the push: %s", remote, refused.Reason)
		// GitHub's scope refusal: "refusing to allow ... to create or update
		// workflow ... without workflow scope". A ruleset's "Required
		// workflows" is not a token permission.
		if r := strings.ToLower(refused.Reason); strings.Contains(r, "refusing to allow") && strings.Contains(r, "workflow") {
			msg += "; the token needs Workflows write (fine-grained) or the workflow scope (gh/OAuth)"
		}
		details := map[string]any{"remote": remote, "refs": refused.Refs, "reason": refused.Reason}
		if text, extra, ok := explainGH007(ctx, pc.git, refspecs, refused, pc.public.FullName); ok {
			msg = text
			for k, v := range extra {
				details[k] = v
			}
		}
		ce := contract.New(contract.PushRefused, "%s", msg).WithDetails(details)
		ce.Err = err
		return ce
	}
	var perm *gitx.PermissionError
	if remote == remotePublic && errors.As(err, &perm) {
		src := e.TokenSource
		if src == "" {
			src = "the configured token"
		}
		return contract.Wrap(contract.TokenCannotPublish, err,
			"the token from %s cannot push to the public repo; publish from a terminal whose token can write to it", src).
			WithField("token").WithDetails(map[string]any{"remote": remote, "token_source": src})
	}
	if err != nil {
		return remoteErr(err, "push to the "+remote+" repo")
	}
	return nil
}

// upgradeNeeded turns config.Parse's unknown-key error into UPGRADE_NEEDED:
// a key this binary doesn't know most likely comes from a newer Ghostbranch.
// Any other error passes through.
func upgradeNeeded(err error) error {
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.ConfigInvalid {
		return err
	}
	key, _ := ce.Details["unknown_key"].(string)
	if key == "" {
		return err
	}
	out := contract.New(contract.UpgradeNeeded,
		".ghostbranch.yml on the overlay branch has the key %q, which this ghostbranch doesn't know: the pair was probably set up by a newer Ghostbranch; upgrade the ghostbranch binary, or fix it if it's a typo", key).
		WithField(key).WithDetails(map[string]any{"unknown_key": key})
	out.Err = err
	return out
}

// updateMirror moves the mirror's copy of ref to sha after a push to the
// shadow repo. The push is what counts: a failure is a MIRROR_NOT_UPDATED
// warning, and the next fetch corrects the mirror.
func (e *Engine) updateMirror(ctx context.Context, g *gitx.Git, ref, sha string) []contract.Warning {
	if err := g.UpdateRef(ctx, ref, sha); err != nil {
		return []contract.Warning{mirrorNotUpdated("couldn't update the local mirror's "+ref, err, map[string]any{"ref": ref, "sha": sha})}
	}
	return nil
}

// deleteMirror is updateMirror for a ref the push deleted.
func (e *Engine) deleteMirror(ctx context.Context, g *gitx.Git, ref string) []contract.Warning {
	if err := g.DeleteRef(ctx, ref); err != nil {
		return []contract.Warning{mirrorNotUpdated("couldn't delete the local mirror's "+ref, err, map[string]any{"ref": ref})}
	}
	return nil
}

// removeLocal deletes a local file whose shadow-repo counterpart a push
// deleted (a plan file). A file already gone is fine.
func removeLocal(path string) []contract.Warning {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return []contract.Warning{mirrorNotUpdated("couldn't remove the local file "+path, err, map[string]any{"path": path})}
	}
	return nil
}

func mirrorNotUpdated(what string, err error, details map[string]any) contract.Warning {
	return contract.Warning{Code: contract.MirrorNotUpdated,
		Message: what + " (the shadow repo has the change; the next command's fetch corrects the mirror): " + err.Error(),
		Details: details}
}

// noOverlay is CONFIG_INVALID for a shadow repo with no overlay branch.
func noOverlay(pc *pairCtx) error {
	return contract.New(contract.ConfigInvalid,
		"shadow repo %s has no %q overlay branch; run ghostbranch init", pc.pair.Shadow, OverlayBranch)
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
		return config.Config{}, "", nil, noOverlay(pc)
	}
	raw, ok, err := g.ShowFile(ctx, O, ".ghostbranch.yml")
	if err != nil || !ok {
		return config.Config{}, "", nil, contract.Wrap(contract.ConfigInvalid, err, "the overlay branch has no .ghostbranch.yml")
	}
	cfg, warns, err := config.Parse(raw)
	if err != nil {
		return config.Config{}, "", warns, upgradeNeeded(err)
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

// unreadableRecord marks a feature record that couldn't be parsed, so a
// console close can still delete the feature.
type unreadableRecord struct {
	feature string
	err     error
}

func (u *unreadableRecord) Error() string { return "feature " + u.feature + ": " + u.err.Error() }
func (u *unreadableRecord) Unwrap() error { return u.err }

// readMetaAny reads a feature's record and the metadata commit's SHA.
// newer reports a record written by a newer Ghostbranch: what this binary
// can read of it is returned, and callers that would write it must refuse
// (readMeta does). A record that doesn't parse, or a metadata commit with
// no record, is an error wrapping *unreadableRecord.
func (e *Engine) readMetaAny(ctx context.Context, pc *pairCtx, name string) (feature.Meta, string, bool, error) {
	sha, ok, err := pc.git.Resolve(ctx, metaRef(name))
	if err != nil {
		return feature.Meta{}, "", false, internalErr(err, "read feature metadata")
	}
	if !ok {
		return feature.Meta{}, "", false, contract.New(contract.FeatureNotFound,
			"no feature %q in %s; start one with ghostbranch feature start", name, pc.pair.Shadow).WithField("feature")
	}
	b, ok, err := pc.git.ShowFile(ctx, sha, feature.MetaFile)
	if err != nil {
		return feature.Meta{}, "", false, contract.Wrap(contract.Internal, err, "read feature %q's %s", name, feature.MetaFile)
	}
	if !ok {
		return feature.Meta{}, "", false, internalErr(&unreadableRecord{feature: name, err: errors.New("the metadata commit has no " + feature.MetaFile)},
			"read "+feature.MetaFile)
	}
	m, err := feature.Decode(b)
	if err != nil {
		return feature.Meta{}, "", false, internalErr(&unreadableRecord{feature: name, err: err}, "parse "+feature.MetaFile)
	}
	return m, sha, contract.Newer(m.SchemaVersion), nil
}

// readMeta is readMetaAny for a caller that may write the record: a record
// from a newer Ghostbranch is UPGRADE_NEEDED, as this binary would drop
// what it doesn't know.
func (e *Engine) readMeta(ctx context.Context, pc *pairCtx, name string) (feature.Meta, string, error) {
	m, sha, newer, err := e.readMetaAny(ctx, pc, name)
	if err == nil && newer {
		err = contract.NewerRecord("feature", m.SchemaVersion).WithField("feature")
	}
	return m, sha, err
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

// updateMeta saves and pushes new metadata, leased on the previous commit,
// and returns the new metadata commit.
func (e *Engine) updateMeta(ctx context.Context, pc *pairCtx, m feature.Meta, parent string) (string, []contract.Warning, error) {
	sha, err := e.saveMeta(ctx, pc, m, parent)
	if err != nil {
		return "", nil, err
	}
	opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: remoteMetaRef(m.Name), Expect: parent}}}
	if err := e.push(ctx, pc, remoteShadow, opt, sha+":"+remoteMetaRef(m.Name)); err != nil {
		return "", nil, err
	}
	// The shadow repo has the metadata: a stale mirror is only a warning.
	return sha, e.updateMirror(ctx, pc.git, metaRef(m.Name), sha), nil
}
