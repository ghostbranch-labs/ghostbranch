package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type InitOptions struct {
	Public string
	Shadow string // default: <public>-shadow
	Adopt  bool
}

type InitResult struct {
	Public           string           `json:"public"`
	Shadow           string           `json:"shadow"`
	CreatedShadow    bool             `json:"created_shadow"`
	Adopted          bool             `json:"adopted"`
	Resumed          bool             `json:"resumed"`
	MirroredBranches []string         `json:"mirrored_branches"`
	MirroredTags     []string         `json:"mirrored_tags"`
	OverlayCreated   bool             `json:"overlay_created"`
	DefaultBranchSet bool             `json:"default_branch_set"`
	Audit            *AuditResult     `json:"audit"`
	Preview          *preview.Preview `json:"preview"`
	// Ready is true once Init returned without an error; human output says
	// "ready" only then. It is not part of the JSON result.
	Ready bool `json:"-"`
}

const overlayReadme = `# Ghostbranch overlay

This branch exists only in the shadow repo and is never published.

- .ghostbranch.yml configures Ghostbranch for this repo pair.
- .ghostbranch/ holds internal notes and docs.
- .github/workflows/ghostbranch-*.yml are private CI workflows.

Every file here is added on top of a feature when it is gated, and none of
them is ever promoted.
`

// Init creates (or adopts) the shadow repo, mirrors public branches and
// tags into it, creates the overlay branch as its default branch, and
// registers the pair. Every step checks before acting, so re-running
// resumes a partial init.
func (e *Engine) Init(ctx context.Context, opt InitOptions) (res *InitResult, warns []contract.Warning, err error) {
	var pair *workspace.Pair
	defer func() {
		summary := ""
		if res != nil && err == nil {
			res.Ready = true
			summary = fmt.Sprintf("shadow %s ready (created=%v adopted=%v resumed=%v)", res.Shadow, res.CreatedShadow, res.Adopted, res.Resumed)
		}
		e.record("init", pair, "", "", summary, err)
	}()
	if _, _, err := forge.SplitName(opt.Public); err != nil {
		return nil, nil, contract.New(contract.BadFlag, "%v", err).WithField("public")
	}
	shadow := opt.Shadow
	// A registered pair keeps its shadow repo: init resumes it, and never
	// quietly sets up a second one.
	if reg, ok := e.WS.Find(opt.Public); ok {
		switch {
		case shadow == "":
			shadow = reg.Shadow
		case !strings.EqualFold(shadow, reg.Shadow):
			return nil, nil, contract.New(contract.BadFlag,
				"%s is registered here with the shadow repo %s, not %s. To switch shadow repos, run ghostbranch remove --repo %s, then ghostbranch init %s --shadow %s (with --adopt if that repo exists)",
				reg.Public, reg.Shadow, shadow, reg.Public, reg.Public, shadow).WithField("shadow").
				WithDetails(map[string]any{"public": reg.Public, "registered_shadow": reg.Shadow})
		}
	}
	if shadow == "" {
		shadow = opt.Public + "-shadow"
	}
	if _, _, err := forge.SplitName(shadow); err != nil {
		return nil, nil, contract.New(contract.BadFlag, "%v", err).WithField("shadow")
	}
	p := workspace.Pair{Public: opt.Public, Shadow: shadow}
	pair = &p
	res = &InitResult{Public: p.Public, Shadow: p.Shadow, MirroredBranches: []string{}, MirroredTags: []string{}}

	pub, err := e.Forge.GetRepo(ctx, p.Public)
	if err != nil {
		return res, nil, forgeErr(err, "look up public repo %s", p.Public)
	}
	if pub.Private {
		warns = append(warns, contract.Warning{Code: contract.PublicNotPublic,
			Message: fmt.Sprintf("%s is private; Ghostbranch is meant for public repos", p.Public)})
	}
	if err := e.checkPublicBranches(ctx, p, pub); err != nil {
		return res, warns, err
	}
	_, err = e.Forge.GetRepo(ctx, p.Shadow)
	exists := err == nil
	if err != nil && !errors.Is(err, forge.ErrNotFound) {
		return res, warns, forgeErr(err, "look up shadow repo %s", p.Shadow)
	}
	_, registered := e.WS.Find(p.Public)
	switch {
	case !exists && opt.Adopt:
		return res, warns, contract.New(contract.BadFlag, "shadow repo %s does not exist; drop --adopt to create it", p.Shadow).WithField("adopt")
	case exists && !opt.Adopt && !registered:
		return res, warns, contract.New(contract.BadFlag, "shadow repo %s already exists; pass --adopt to take it over", p.Shadow).WithField("adopt")
	}

	if !exists {
		if e.DryRun {
			res.CreatedShadow = true // what the run would do
			return res, warns, e.previewNewShadow(ctx, p, pub, registered, res)
		}
		e.progressf("creating private repo %s", p.Shadow)
		if _, err := e.Forge.CreatePrivateRepo(ctx, p.Shadow); err != nil {
			if errors.Is(err, forge.ErrForbidden) {
				return res, warns, shadowCreateDenied(p, opt.Shadow != "", err)
			}
			return res, warns, forgeErr(err, "create shadow repo %s", p.Shadow)
		}
		res.CreatedShadow = true
	} else {
		res.Adopted, res.Resumed = opt.Adopt, !opt.Adopt
	}

	pc, err := e.open(ctx, "init", p, true)
	if err != nil {
		return res, warns, err
	}
	defer pc.unlock()
	g := pc.git

	publicMain, ok, err := g.Resolve(ctx, publicRef("main"))
	if err != nil {
		return res, warns, internalErr(err, "read public main")
	}
	if !ok {
		return res, warns, contract.New(contract.ForgeError, "public repo %s has no main branch", p.Public)
	}
	entries, err := g.ListTree(ctx, publicMain)
	if err != nil {
		return res, warns, internalErr(err, "list public main")
	}
	set := overlay.NewSet()
	var clash []string
	for _, en := range entries {
		if set.Match(en.Path) {
			clash = append(clash, en.Path)
		}
	}
	if len(clash) > 0 {
		return res, warns, contract.New(contract.OverlayCollision,
			"public main already has paths Ghostbranch reserves for the overlay: %s", strings.Join(clash, ", ")).
			WithDetails(map[string]any{"paths": clash})
	}
	if err := e.checkAdoptable(ctx, pc); err != nil {
		return res, warns, err
	}

	specs, leases, err := e.missingMirrorRefs(ctx, g, res, opt.Adopt)
	if err != nil {
		return res, warns, err
	}
	_, hasOverlay, _ := g.Resolve(ctx, shadowRef(OverlayBranch))
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": publicMain})
		addMirrorOps(pv, p.Shadow, specs, leases)
		if !hasOverlay {
			pv.Add(preview.PushRef(p.Shadow, "refs/heads/"+OverlayBranch, "", ""))
		}
		if pc.shadow.DefaultBranch != OverlayBranch {
			pv.Add(preview.SetDefaultBranch(p.Shadow, OverlayBranch))
		}
		if !registered {
			pv.Add(preview.RegisterPair(p.Public, p.Shadow))
		}
		res.Preview = pv
		return res, warns, nil
	}
	if len(specs) > 0 {
		e.progressf("mirroring %d branches and %d tags into %s", len(res.MirroredBranches), len(res.MirroredTags), p.Shadow)
		if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Atomic: true, Leases: leases}, specs...); err != nil {
			return res, warns, err
		}
	}
	if !hasOverlay {
		if err := e.createOverlay(ctx, pc); err != nil {
			return res, warns, err
		}
		res.OverlayCreated = true
	}
	if pc.shadow.DefaultBranch != OverlayBranch {
		if err := e.Forge.SetDefaultBranch(ctx, p.Shadow, OverlayBranch); err != nil {
			return res, warns, forgeErr(err, "make %s the default branch of %s", OverlayBranch, p.Shadow)
		}
		res.DefaultBranchSet = true
	}
	if err := e.WS.Update(e.Now(), func(w *workspace.Workspace) bool { return w.AddPair(p) }); err != nil {
		return res, warns, updateErr(err, "save the workspace")
	}
	auditIncomplete := func(err error) {
		warns = append(warns, contract.Warning{Code: contract.AuditIncomplete,
			Message: "the CI audit could not run (" + contract.From(err).Message + "); run ghostbranch audit"})
	}
	if err := e.fetch(ctx, g); err != nil {
		auditIncomplete(err)
		return res, warns, nil
	}
	cfg, O, cw, cerr := e.readConfig(ctx, pc)
	warns = addWarnings(warns, cw...)
	if cerr != nil {
		auditIncomplete(cerr)
		return res, warns, nil
	}
	res.Audit, warns = e.auditInto(ctx, pc, cfg, O, warns)
	return res, warns, nil
}

// shadowCreateDenied is SHADOW_CREATE_DENIED with the steps that work with
// a token scoped to the two repos.
func shadowCreateDenied(p workspace.Pair, explicitShadow bool, err error) error {
	adopt := "ghostbranch init " + p.Public + " --adopt"
	if explicitShadow {
		adopt += " --shadow " + p.Shadow
	}
	steps := []string{
		fmt.Sprintf("Create an empty private repo %s (no README, license or .gitignore): on GitHub's new-repository page, or with `gh repo create %s --private`.", p.Shadow, p.Shadow),
		fmt.Sprintf("Add %s to the token's repository access, with the permissions in the token table of `ghostbranch guide`.", p.Shadow),
		"Run " + adopt + ".",
	}
	return contract.Wrap(contract.ShadowCreateDenied, err, "the token can't create %s; create it yourself and adopt it", p.Shadow).
		WithDetails(map[string]any{"public": p.Public, "shadow": p.Shadow, "explicit_shadow": explicitShadow, "steps": steps})
}

// checkAdoptable accepts an empty shadow repo, one that is already this
// pair's shadow, or a partial mirror whose only refs are public branches at
// commits public already contains. (GitHub never lets a repo's last
// default branch be deleted, so a reset shadow repo is never empty.)
func (e *Engine) checkAdoptable(ctx context.Context, pc *pairCtx) error {
	g := pc.git
	heads, err := g.Refs(ctx, "refs/remotes/shadow")
	if err != nil {
		return internalErr(err, "list shadow branches")
	}
	if O, ok := heads[shadowRef(OverlayBranch)]; ok {
		if raw, found, err := g.ShowFile(ctx, O, ".ghostbranch.yml"); err == nil && found {
			cfg, _, err := config.Parse(raw)
			if err == nil && strings.EqualFold(cfg.Public, pc.pair.Public) {
				return nil
			}
			// A key from a newer Ghostbranch is not unrelated content.
			if err != nil && contract.From(upgradeNeeded(err)).Code == contract.UpgradeNeeded {
				return upgradeNeeded(err)
			}
		}
		return contract.New(contract.ShadowNotEmpty, "%s has a %q branch that is not the overlay for %s", pc.pair.Shadow, OverlayBranch, pc.pair.Public)
	}
	if meta, _ := g.Refs(ctx, "refs/shadow-meta"); len(meta) > 0 {
		return contract.New(contract.ShadowNotEmpty, "%s has Ghostbranch refs but no overlay branch", pc.pair.Shadow)
	}
	for ref, sha := range heads {
		b := strings.TrimPrefix(ref, "refs/remotes/shadow/")
		pubSHA, ok, err := g.Resolve(ctx, publicRef(b))
		if err != nil {
			return internalErr(err, "read public branch")
		}
		if !ok {
			return contract.New(contract.ShadowNotEmpty, "%s has branch %q, which is not a public branch; adopt only an empty repo or a mirror", pc.pair.Shadow, b)
		}
		anc, err := g.IsAncestor(ctx, sha, pubSHA)
		if err != nil {
			return internalErr(err, "compare shadow branch "+b+" with public")
		}
		if !anc {
			return contract.New(contract.ShadowNotEmpty, "%s branch %q has commits that are not in public", pc.pair.Shadow, b)
		}
	}
	// Tags too: each must be a public tag, at the same object.
	shTags, err := g.Refs(ctx, "refs/shadow-tags")
	if err != nil {
		return internalErr(err, "list shadow tags")
	}
	pubTags, err := g.Refs(ctx, "refs/public-tags")
	if err != nil {
		return internalErr(err, "list public tags")
	}
	names := make([]string, 0, len(shTags))
	for ref := range shTags {
		names = append(names, strings.TrimPrefix(ref, "refs/shadow-tags/"))
	}
	sort.Strings(names)
	for _, t := range names {
		if pubTags["refs/public-tags/"+t] != shTags["refs/shadow-tags/"+t] {
			return contract.New(contract.ShadowNotEmpty, "%s has tag %q, which the public repo doesn't have at that object; adopt only an empty repo or a mirror", pc.pair.Shadow, t)
		}
	}
	return nil
}

// checkPublicBranches refuses a public repo with a branch named ghostbranch
// (or under ghostbranch/): mirroring it would collide with the overlay
// branch. It reads the public repo's branches with ls-remote, before
// anything is created or pushed.
func (e *Engine) checkPublicBranches(ctx context.Context, p workspace.Pair, pub forge.Repo) error {
	// ls-remote needs no repo. Run it in a new empty directory, with repo
	// discovery stopped at its parent, so no repo's config applies while the
	// token is sent.
	dir, err := os.MkdirTemp("", "ghostbranch-ls-remote-")
	if err != nil {
		return internalErr(err, "make a scratch directory")
	}
	defer func() { _ = os.RemoveAll(dir) }()
	g := gitx.New(dir)
	g.Config = e.gitConfig()
	g.Env = []string{"GIT_CEILING_DIRECTORIES=" + filepath.Dir(dir)}
	out, err := g.Run(ctx, "ls-remote", "--heads", pub.CloneURL)
	if err != nil {
		return remoteErr(err, "list the public repo's branches")
	}
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		b := strings.TrimPrefix(ref, "refs/heads/")
		if ok && (b == OverlayBranch || strings.HasPrefix(b, OverlayBranch+"/")) {
			return contract.New(contract.NameInvalid,
				"public repo %s has a branch %q, but Ghostbranch reserves that name for the shadow repo's overlay branch, so it can't mirror it; rename or delete that branch in the public repo, then run init again",
				p.Public, b).WithField("public").WithDetails(map[string]any{"public": p.Public, "branch": b})
		}
	}
	return nil
}

// missingMirrorRefs lists public branches and tags absent from the shadow
// repo, main first, with must-not-exist leases. With ffMain (an adopt), a
// shadow main that is behind public main is fast-forwarded too, leased on
// its old head, and listed in res.MirroredBranches.
func (e *Engine) missingMirrorRefs(ctx context.Context, g *gitx.Git, res *InitResult, ffMain bool) ([]string, []gitx.Lease, error) {
	get := func(prefix string) (map[string]string, error) { return g.Refs(ctx, prefix) }
	pubHeads, err := get("refs/remotes/public")
	if err != nil {
		return nil, nil, internalErr(err, "list public branches")
	}
	shHeads, _ := get("refs/remotes/shadow")
	pubTags, _ := get("refs/public-tags")
	shTags, _ := get("refs/shadow-tags")

	var specs []string
	var leases []gitx.Lease
	add := func(sha, dst string) {
		specs = append(specs, sha+":"+dst)
		leases = append(leases, gitx.Lease{Ref: dst})
	}
	branches := make([]string, 0, len(pubHeads))
	for ref := range pubHeads {
		branches = append(branches, strings.TrimPrefix(ref, "refs/remotes/public/"))
	}
	sort.Slice(branches, func(i, j int) bool {
		if (branches[i] == "main") != (branches[j] == "main") {
			return branches[i] == "main"
		}
		return branches[i] < branches[j]
	})
	for _, b := range branches {
		pb := pubHeads[publicRef(b)]
		sb, ok := shHeads[shadowRef(b)]
		if !ok {
			add(pb, "refs/heads/"+b)
			res.MirroredBranches = append(res.MirroredBranches, b)
			continue
		}
		if b != "main" || !ffMain || sb == pb {
			continue
		}
		anc, err := g.IsAncestor(ctx, sb, pb)
		if err != nil {
			return nil, nil, internalErr(err, "compare shadow main with public main")
		}
		if anc {
			specs = append(specs, pb+":refs/heads/main")
			leases = append(leases, gitx.Lease{Ref: "refs/heads/main", Expect: sb})
			res.MirroredBranches = append(res.MirroredBranches, b)
		}
	}
	tags := make([]string, 0, len(pubTags))
	for ref := range pubTags {
		tags = append(tags, strings.TrimPrefix(ref, "refs/public-tags/"))
	}
	sort.Strings(tags)
	for _, t := range tags {
		if _, ok := shTags["refs/shadow-tags/"+t]; !ok {
			add(pubTags["refs/public-tags/"+t], "refs/tags/"+t)
			res.MirroredTags = append(res.MirroredTags, t)
		}
	}
	return specs, leases, nil
}

// addMirrorOps previews specs; leases[i] guards specs[i], and its Expect
// is the ref's current value ("" for a new ref).
func addMirrorOps(pv *preview.Preview, shadow string, specs []string, leases []gitx.Lease) {
	for i, s := range specs {
		sha, dst, _ := strings.Cut(s, ":")
		pv.Add(preview.PushRef(shadow, dst, leases[i].Expect, sha))
	}
}

// previewNewShadow lists what init would do when the shadow repo does not
// exist yet. Only the public repo is fetched, into the local mirror.
func (e *Engine) previewNewShadow(ctx context.Context, p workspace.Pair, pub forge.Repo, registered bool, res *InitResult) error {
	dir := e.WS.MirrorDir(p)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		if _, err := gitx.Init(ctx, dir, true); err != nil {
			return internalErr(err, "create the mirror")
		}
	}
	g := gitx.New(dir)
	g.Config = e.gitConfig()
	if err := g.AddRemote(ctx, remotePublic, pub.CloneURL); err != nil {
		return internalErr(err, "configure the public remote")
	}
	if err := g.Fetch(ctx, remotePublic, "+refs/heads/*:refs/remotes/public/*", "+refs/tags/*:refs/public-tags/*"); err != nil {
		return remoteErr(err, "fetch the public repo")
	}
	pm, _, _ := g.Resolve(ctx, publicRef("main"))
	specs, leases, err := e.missingMirrorRefs(ctx, g, res, false)
	if err != nil {
		return err
	}
	pv := preview.New(map[string]string{"public_main": pm})
	pv.Add(preview.CreateRepo(p.Shadow))
	addMirrorOps(pv, p.Shadow, specs, leases)
	pv.Add(preview.PushRef(p.Shadow, "refs/heads/"+OverlayBranch, "", ""), preview.SetDefaultBranch(p.Shadow, OverlayBranch))
	if !registered {
		pv.Add(preview.RegisterPair(p.Public, p.Shadow))
	}
	res.Preview = pv
	return nil
}

func (e *Engine) createOverlay(ctx context.Context, pc *pairCtx) error {
	g := pc.git
	cfgBlob, err := g.HashObject(ctx, config.Render(pc.pair.Public, pc.pair.Shadow))
	if err != nil {
		return internalErr(err, "write .ghostbranch.yml")
	}
	readme, err := g.HashObject(ctx, []byte(overlayReadme))
	if err != nil {
		return internalErr(err, "write the overlay README")
	}
	// The managed files the default config wants: none while sync.schedule
	// defaults to off, but this keeps init in step if a default changes.
	def := config.Default()
	files := managed.Render(managed.Input{Public: pc.pair.Public, Schedule: def.Sync.Schedule, Branches: def.Sync.Branches})
	entries := []gitx.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: cfgBlob, Path: ".ghostbranch.yml"},
		{Mode: "100644", Type: "blob", SHA: readme, Path: ".ghostbranch/README.md"},
	}
	for _, path := range managed.Paths() {
		body, ok := files[path]
		if !ok {
			continue
		}
		sha, err := g.HashObject(ctx, body)
		if err != nil {
			return internalErr(err, "write "+path)
		}
		entries = append(entries, gitx.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Path: path})
	}
	tree, err := g.MakeTree(ctx, entries)
	if err != nil {
		return internalErr(err, "write the overlay tree")
	}
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: u.Login, Email: forge.NoreplyEmail(u), When: e.now()}
	c, err := g.CommitTree(ctx, tree, nil, "ghostbranch: initialize overlay", who, who)
	if err != nil {
		return internalErr(err, "write the overlay commit")
	}
	e.progressf("creating the %s overlay branch", OverlayBranch)
	return e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + OverlayBranch}}},
		c+":refs/heads/"+OverlayBranch)
}
