package engine

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type InitOptions struct {
	Public string
	Shadow string // default: <public>-shadow
	Adopt  bool
}

type InitResult struct {
	Public           string   `json:"public"`
	Shadow           string   `json:"shadow"`
	CreatedShadow    bool     `json:"created_shadow"`
	Adopted          bool     `json:"adopted"`
	Resumed          bool     `json:"resumed"`
	MirroredBranches []string `json:"mirrored_branches"`
	MirroredTags     []string `json:"mirrored_tags"`
	OverlayCreated   bool     `json:"overlay_created"`
	DefaultBranchSet bool     `json:"default_branch_set"`
	DryRun           bool     `json:"dry_run"`
	Actions          []string `json:"actions"`
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
			summary = fmt.Sprintf("shadow %s ready (created=%v adopted=%v resumed=%v)", res.Shadow, res.CreatedShadow, res.Adopted, res.Resumed)
		}
		e.record("init", pair, "", "", summary, err)
	}()
	if _, _, err := forge.SplitName(opt.Public); err != nil {
		return nil, nil, contract.New(contract.BadFlag, "%v", err).WithField("public")
	}
	shadow := opt.Shadow
	if shadow == "" {
		shadow = opt.Public + "-shadow"
	}
	if _, _, err := forge.SplitName(shadow); err != nil {
		return nil, nil, contract.New(contract.BadFlag, "%v", err).WithField("shadow")
	}
	p := workspace.Pair{Public: opt.Public, Shadow: shadow}
	pair = &p
	res = &InitResult{Public: p.Public, Shadow: p.Shadow, MirroredBranches: []string{}, MirroredTags: []string{}, Actions: []string{}, DryRun: e.DryRun}

	pub, err := e.Forge.GetRepo(ctx, p.Public)
	if err != nil {
		return res, nil, forgeErr(err, "look up public repo %s", p.Public)
	}
	if pub.Private {
		warns = append(warns, contract.Warning{Code: contract.PublicNotPublic,
			Message: fmt.Sprintf("%s is private; Ghostbranch is meant for public repos", p.Public)})
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
		res.CreatedShadow = true
		if e.DryRun {
			res.Actions = []string{"create private repo " + p.Shadow, "mirror public branches and tags",
				"create the " + OverlayBranch + " overlay branch", "make " + OverlayBranch + " the default branch", "register the pair"}
			return res, warns, nil
		}
		e.progressf("creating private repo %s", p.Shadow)
		if _, err := e.Forge.CreatePrivateRepo(ctx, p.Shadow); err != nil {
			return res, warns, forgeErr(err, "create shadow repo %s", p.Shadow)
		}
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

	specs, leases, err := e.missingMirrorRefs(ctx, pc, res)
	if err != nil {
		return res, warns, err
	}
	_, hasOverlay, _ := g.Resolve(ctx, shadowRef(OverlayBranch))
	if e.DryRun {
		if len(specs) > 0 {
			res.Actions = append(res.Actions, fmt.Sprintf("mirror %d branches and %d tags", len(res.MirroredBranches), len(res.MirroredTags)))
		}
		if !hasOverlay {
			res.Actions = append(res.Actions, "create the "+OverlayBranch+" overlay branch")
		}
		if pc.shadow.DefaultBranch != OverlayBranch {
			res.Actions = append(res.Actions, "make "+OverlayBranch+" the default branch")
		}
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
	if e.WS.AddPair(p) {
		if err := e.WS.Save(); err != nil {
			return res, warns, internalErr(err, "save the workspace")
		}
	}
	return res, warns, nil
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
			if cfg, _, err := config.Parse(raw); err == nil && strings.EqualFold(cfg.Public, pc.pair.Public) {
				return nil
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
		if anc, err := g.IsAncestor(ctx, sha, pubSHA); err != nil || !anc {
			return contract.New(contract.ShadowNotEmpty, "%s branch %q has commits that are not in public", pc.pair.Shadow, b)
		}
	}
	return nil
}

// missingMirrorRefs lists public branches and tags absent from the shadow
// repo, main first, with must-not-exist leases.
func (e *Engine) missingMirrorRefs(ctx context.Context, pc *pairCtx, res *InitResult) ([]string, []gitx.Lease, error) {
	g := pc.git
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
		if _, ok := shHeads[shadowRef(b)]; !ok {
			add(pubHeads[publicRef(b)], "refs/heads/"+b)
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
	tree, err := g.MakeTree(ctx, []gitx.TreeEntry{
		{Mode: "100644", Type: "blob", SHA: cfgBlob, Path: ".ghostbranch.yml"},
		{Mode: "100644", Type: "blob", SHA: readme, Path: ".ghostbranch/README.md"},
	})
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
