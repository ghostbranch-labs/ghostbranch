package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// Schedule is sync.schedule for a new overlay: "" or config.ScheduleOff
	// leaves scheduled sync off, DefaultSyncSchedule turns it on. An
	// overlay that already exists keeps its config: init refuses with
	// BAD_FLAG, before pushing anything, if its schedule differs.
	Schedule string
}

// DefaultSyncSchedule is init --sync's cron: every 6 hours, at minute 23.
const DefaultSyncSchedule = "23 */6 * * *"

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
	// GuardInstalled: the push guard is installed in this checkout when
	// init finishes (installed by this run, or before it).
	GuardInstalled bool `json:"guard_installed"`
	// Doctor is doctor's summary after a guided init; nil otherwise.
	Doctor *DoctorSummary `json:"doctor"`
	// Setup is the setup checklist: worked out before init acts, and
	// again once it is done.
	Setup *Setup `json:"setup"`
	// Guided and DoctorChecks are for human output only.
	Guided       bool          `json:"-"`
	DoctorChecks []DoctorCheck `json:"-"`
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
	p, err := e.initPair(opt.Public, opt.Shadow)
	if err != nil {
		return nil, nil, err
	}
	if s := opt.Schedule; s != "" && s != config.ScheduleOff && s != DefaultSyncSchedule {
		return nil, nil, contract.New(contract.BadFlag, "init's schedule must be %q or %q, not %q", config.ScheduleOff, DefaultSyncSchedule, s).WithField("sync")
	}
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
		st := e.startStep("Creating the private repo %s", p.Shadow)
		if _, err := e.Forge.CreatePrivateRepo(ctx, p.Shadow); err != nil {
			st.Fail() // guided init asks next; the step mustn't spin or end as ✓
			if errors.Is(err, forge.ErrForbidden) {
				return res, warns, shadowCreateDenied(p, opt.Shadow != "", err)
			}
			if errors.Is(err, forge.ErrRepoExists) && e.AppLogin {
				return res, warns, e.shadowHidden(ctx, p)
			}
			return res, warns, forgeErr(err, "create shadow repo %s", p.Shadow)
		}
		st.Done(fmt.Sprintf("Created the private repo %s", p.Shadow))
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
	if hasOverlay && opt.Schedule != "" {
		if err := e.checkSchedule(ctx, pc, opt.Schedule); err != nil {
			return res, warns, err
		}
	}
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
		nb, nt := len(res.MirroredBranches), len(res.MirroredTags)
		st := e.startStep("Mirroring %d branch(es) and %d tag(s) into %s", nb, nt, p.Shadow)
		if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Atomic: true, Leases: leases}, specs...); err != nil {
			return res, warns, err
		}
		st.Done(fmt.Sprintf("Mirrored %d branch(es) and %d tag(s) into %s", nb, nt, p.Shadow))
	}
	if !hasOverlay {
		if err := e.createOverlay(ctx, pc, opt.Schedule); err != nil {
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
	st := e.startStep("Reading %s and %s again for the CI audit", p.Public, p.Shadow)
	if err := e.fetch(ctx, g); err != nil {
		st.Fail()
		auditIncomplete(err)
		return res, warns, nil
	}
	st.Done("")
	cfg, O, cw, cerr := e.readConfig(ctx, pc)
	warns = addWarnings(warns, cw...)
	if cerr != nil {
		auditIncomplete(cerr)
		return res, warns, nil
	}
	res.Audit, warns = e.auditInto(ctx, pc, cfg, O, warns)
	return res, warns, nil
}

// InitPair is the pair init sets up for public and the --shadow given.
func (e *Engine) InitPair(public, shadow string) (workspace.Pair, error) {
	return e.initPair(public, shadow)
}

// initPair is the pair init sets up for public: the shadow named, else the
// registered pair's, else <public>-shadow. A registered pair keeps its
// shadow repo: init resumes it, and never quietly sets up a second one.
func (e *Engine) initPair(public, shadow string) (workspace.Pair, error) {
	if _, _, err := forge.SplitName(public); err != nil {
		return workspace.Pair{}, contract.New(contract.BadFlag, "%v", err).WithField("public")
	}
	if reg, ok := e.WS.Find(public); ok {
		switch {
		case shadow == "":
			shadow = reg.Shadow
		case !strings.EqualFold(shadow, reg.Shadow):
			return workspace.Pair{}, contract.New(contract.BadFlag,
				"%s is registered here with the shadow repo %s, not %s. To switch shadow repos, run ghostbranch remove --repo %s, then ghostbranch init %s --shadow %s (with --adopt if that repo exists)",
				reg.Public, reg.Shadow, shadow, reg.Public, reg.Public, shadow).WithField("shadow").
				WithDetails(map[string]any{"public": reg.Public, "registered_shadow": reg.Shadow})
		}
	}
	if shadow == "" {
		shadow = public + "-shadow"
	}
	if _, _, err := forge.SplitName(shadow); err != nil {
		return workspace.Pair{}, contract.New(contract.BadFlag, "%v", err).WithField("shadow")
	}
	p := workspace.Pair{Public: public, Shadow: shadow}
	return p, sameRepo(p)
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

// shadowHidden is APP_CANT_SEE_REPO for an app login whose create of p's
// shadow repo GitHub refused because the name is taken, though GetRepo
// couldn't find it: the repo exists, and the Ghostbranch app can't see it.
// details.configure_url is the app's install on the shadow's account ("" when
// it can't be listed).
func (e *Engine) shadowHidden(ctx context.Context, p workspace.Pair) error {
	owner, _, _ := forge.SplitName(p.Shadow)
	configure := ""
	if in := e.installer(); in != nil {
		if insts, lerr := in.UserInstallations(ctx); lerr == nil {
			if inst, ok := installFor(insts, owner); ok {
				configure = inst.ConfigureURL()
			}
		}
	}
	msg := fmt.Sprintf("%s already exists, but the Ghostbranch app can't see it: tick %s in the Ghostbranch app's repositories on %s", p.Shadow, p.Shadow, owner)
	if configure != "" {
		msg += ", at " + configure
	}
	return contract.New(contract.AppCantSeeRepo, "%s.", msg).
		WithDetails(map[string]any{"configure_url": configure, "repos": []string{p.Shadow}})
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

// checkPublicBranches refuses a public repo without a main branch (an empty
// repo: there is nothing to mirror), or with a branch named ghostbranch
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
	var branches []string
	for _, line := range strings.Split(out, "\n") {
		_, ref, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		b := strings.TrimPrefix(ref, "refs/heads/")
		if b == OverlayBranch || strings.HasPrefix(b, OverlayBranch+"/") {
			return contract.New(contract.NameInvalid,
				"public repo %s has a branch %q, but Ghostbranch reserves that name for the shadow repo's overlay branch, so it can't mirror it; rename or delete that branch in the public repo, then run init again",
				p.Public, b).WithField("public").WithDetails(map[string]any{"public": p.Public, "branch": b})
		}
		branches = append(branches, b)
	}
	switch {
	case len(branches) == 0:
		return contract.New(contract.BadFlag,
			"%s has no commits on main yet: push a first commit (a README is enough), then run ghostbranch init again", p.Public).
			WithField("public").WithDetails(map[string]any{"public": p.Public, "branch": "main"})
	case !slices.Contains(branches, "main"):
		return contract.New(contract.BadFlag,
			"%s has no main branch (it has %s): Ghostbranch mirrors main, so create it in the public repo, then run ghostbranch init again",
			p.Public, strings.Join(branches, ", ")).WithField("public").WithDetails(map[string]any{"public": p.Public, "branch": "main"})
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

// checkSchedule refuses an init --sync or --no-sync that the existing
// overlay's config contradicts: init never rewrites a config. A config it
// can't read is left to the audit's warning, as before.
func (e *Engine) checkSchedule(ctx context.Context, pc *pairCtx, want string) error {
	cfg, _, _, err := e.readConfig(ctx, pc)
	if err != nil || cfg.Sync.Schedule == want {
		return nil
	}
	flag := "--no-sync"
	if want != config.ScheduleOff {
		flag = "--sync"
	}
	return contract.New(contract.BadFlag,
		"%s already has an overlay whose sync.schedule is %q, and init doesn't change it: drop %s, or edit sync.schedule in .ghostbranch.yml on the %s branch and run ghostbranch upgrade",
		pc.pair.Shadow, cfg.Sync.Schedule, flag, OverlayBranch).WithField("sync")
}

func (e *Engine) createOverlay(ctx context.Context, pc *pairCtx, schedule string) error {
	g := pc.git
	sched := schedule
	if sched == "" {
		sched = config.ScheduleOff
	}
	cfgBlob, err := g.HashObject(ctx, config.RenderSchedule(pc.pair.Public, pc.pair.Shadow, "main", sched))
	if err != nil {
		return internalErr(err, "write .ghostbranch.yml")
	}
	readme, err := g.HashObject(ctx, []byte(overlayReadme))
	if err != nil {
		return internalErr(err, "write the overlay README")
	}
	// The managed files the new config wants: the sync workflow when the
	// schedule is on.
	def := config.Default()
	files := managed.Render(managed.Input{Public: pc.pair.Public, Schedule: sched, Branches: def.Sync.Branches})
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
	c, err := g.CommitTree(ctx, tree, nil, "ghostbranch: initialize overlay", who, e.bot())
	if err != nil {
		return internalErr(err, "write the overlay commit")
	}
	st := e.startStep("Creating the overlay branch %s", OverlayBranch)
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + OverlayBranch}}},
		c+":refs/heads/"+OverlayBranch); err != nil {
		return err
	}
	st.Done(fmt.Sprintf("Created the overlay branch %s", OverlayBranch))
	return nil
}
