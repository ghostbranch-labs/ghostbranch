package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// initFlags are init's answers given as flags. guard and sync are nil when
// neither --guard/--no-guard nor --sync/--no-sync was given.
type initFlags struct {
	shadow string
	adopt  bool
	yes    bool
	guard  *bool
	sync   *bool
}

// onOff reads a --x/--no-x pair.
func onOff(name string, on, off bool) (*bool, error) {
	switch {
	case on && off:
		return nil, contract.New(contract.BadFlag, "--%s and --no-%s contradict each other; pass one", name, name).WithField(name)
	case on:
		t := true
		return &t, nil
	case off:
		f := false
		return &f, nil
	}
	return nil, nil
}

// schedule is InitOptions.Schedule for a sync answer.
func schedule(on *bool) string {
	switch {
	case on == nil:
		return ""
	case *on:
		return engine.DefaultSyncSchedule
	}
	return config.ScheduleOff
}

// wantsGuide is spec § 4's rule: a person at a usable console, no --json,
// --non-interactive or --yes, and an answer missing.
func (a *app) wantsGuide(args []string, f initFlags) bool {
	if !a.env.ConsoleUsable || a.jsonOut || a.nonInteractive || f.yes || a.env.Asker == nil {
		return false
	}
	return len(args) == 0 || (f.shadow == "" && !f.adopt) || f.guard == nil || f.sync == nil
}

// guardHere is whether this directory is a clone of public, and whether
// the guard is installed there.
func (a *app) guardHere(public string) (in, installed bool) {
	if a.env.GuardState == nil {
		return false, false
	}
	return a.env.GuardState(public)
}

func guardFlagErr(public string) error {
	return contract.New(contract.BadFlag,
		"--guard installs the push guard in this checkout, but this directory isn't a clone of %s; drop --guard, and run ghostbranch guard install in your clone", public).WithField("guard")
}

// plainInit is init without questions: today's init, plus the guard and
// sync flags.
func (a *app) plainInit(ctx context.Context, public string, f initFlags) error {
	a.ran = true
	in, installed := a.guardHere(public)
	guardOn := f.guard != nil && *f.guard
	if guardOn && !in {
		return guardFlagErr(public)
	}
	e, err := a.engine(ctx)
	if err != nil {
		return err
	}
	res, w, err := e.Init(ctx, engine.InitOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, Schedule: schedule(f.sync)})
	if res != nil {
		a.pairFull = workspace.Pair{Public: res.Public, Shadow: res.Shadow}
		res.GuardInstalled = installed
	}
	if err == nil && res.Preview == nil && guardOn {
		gr, gerr := a.installGuard(ctx)
		if gerr != nil {
			return done(a, res, w, gerr)
		}
		res.GuardInstalled = gr.Status.Installed
	}
	return done(a, res, w, err)
}

// originSlug is owner/repo from the checkout's origin remote, or "".
func originSlug(remotes func() []workspace.Remote) string {
	if remotes == nil {
		return ""
	}
	for _, r := range remotes() {
		if r.Name == "origin" {
			if s, ok := workspace.ParseRemoteURL(r.URL); ok {
				return s
			}
		}
	}
	return ""
}

// askRepo asks for owner/repo until it gets one; a pasted GitHub URL works too.
func askRepo(ctx context.Context, ask prompt.Asker, question, def string) (string, error) {
	for {
		s, err := ask.Text(ctx, question, def)
		if err != nil {
			return "", err
		}
		s = strings.TrimSpace(s)
		if slug, ok := workspace.ParseRemoteURL(s); ok {
			s = slug
		}
		if _, _, err := forge.SplitName(s); err == nil {
			return s, nil
		}
		ask.Say(fmt.Sprintf("%q isn't owner/repo.", s))
	}
}

// ensureLogin offers a login when no token resolves.
func (a *app) ensureLogin(ctx context.Context, ask prompt.Asker) error {
	_, _, err := auth.Resolve(ctx, a.authSource())
	ce := contract.From(err)
	if ce == nil {
		return nil
	}
	if ce.Code != contract.AuthMissing && ce.Code != contract.LoginExpired {
		return err
	}
	q := "No GitHub login found. Log in with GitHub now?"
	if ce.Code == contract.LoginExpired {
		q = "Your GitHub login has expired. Log in again now?"
	}
	yes, aerr := ask.YesNo(ctx, q, true)
	if aerr != nil {
		return aerr
	}
	if !yes {
		ask.Say("To use a token instead, pipe it in: ghostbranch login --with-token < token.txt, then run ghostbranch init again.")
		return contract.New(contract.AuthMissing, "init needs a GitHub login: run ghostbranch login (or ghostbranch login --with-token, with the token on stdin), then init again")
	}
	_, err = a.runLogin(ctx, ask)
	return err
}

// checkPublic looks the public repo up. When reask, a missing repo or a
// declined private one asks again.
func checkPublic(ctx context.Context, ask prompt.Asker, f forge.Forge, public string, reask bool) (string, error) {
	for {
		pub, err := f.GetRepo(ctx, public)
		switch {
		case err == nil && !pub.Private:
			return public, nil
		case err == nil:
			use, aerr := ask.YesNo(ctx, public+" is private, and Ghostbranch is meant for public repos. Use it anyway?", false)
			if aerr != nil {
				return "", aerr
			}
			if use {
				return public, nil
			}
			if !reask {
				return "", contract.New(contract.BadFlag, "%s is private, and init stopped there", public).WithField("public")
			}
		case errors.Is(err, forge.ErrNotFound) && reask:
			ask.Say(public + " doesn't exist, or your login can't see it.")
		default:
			ce := contract.Wrap(contract.ForgeError, err, "look up public repo %s", public)
			ce.Retryable = errors.Is(err, forge.ErrTransient)
			return "", ce
		}
		var aerr error
		if public, aerr = askRepo(ctx, ask, "Public repo (owner/repo)", ""); aerr != nil {
			return "", aerr
		}
	}
}

func shadowOf(o engine.InitOptions) string {
	if o.Shadow != "" {
		return o.Shadow
	}
	return o.Public + "-shadow"
}

// askShadow settles the shadow repo: create <public>-shadow, adopt it if it
// exists, or take another name (adopted when it exists).
func askShadow(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions) (engine.InitOptions, error) {
	name := o.Public + "-shadow"
	for {
		sh, err := f.GetRepo(ctx, name)
		var yes bool
		var aerr error
		switch {
		case err == nil && sh.Fork:
			ask.Say(name + " is a fork; a shadow repo can't be one.")
		case err == nil && !sh.Private:
			ask.Say(name + " is public; a shadow repo must be private.")
		case err == nil:
			yes, aerr = ask.YesNo(ctx, fmt.Sprintf("%s already exists. Adopt it as the shadow repo? It must be private and empty, or a mirror of %s.", name, o.Public), true)
			if aerr == nil && yes {
				o.Shadow, o.Adopt = name, true
				return o, nil
			}
		case errors.Is(err, forge.ErrNotFound):
			yes, aerr = ask.YesNo(ctx, "Create private repo "+name+"?", true)
			if aerr == nil && yes {
				o.Shadow = name
				return o, nil
			}
		default:
			ce := contract.Wrap(contract.ForgeError, err, "look up %s", name)
			ce.Retryable = errors.Is(err, forge.ErrTransient)
			return o, ce
		}
		if aerr != nil {
			return o, aerr
		}
		if name, aerr = askRepo(ctx, ask, "Shadow repo to use instead (owner/repo; an existing repo is adopted)", ""); aerr != nil {
			return o, aerr
		}
	}
}

// overlayExists: init makes the overlay branch the shadow repo's default
// branch right after creating it.
func overlayExists(ctx context.Context, f forge.Forge, o engine.InitOptions) bool {
	sh, err := f.GetRepo(ctx, shadowOf(o))
	return err == nil && sh.DefaultBranch == engine.OverlayBranch
}

func newRepoURL(full string) string {
	owner, name, _ := forge.SplitName(full)
	return "https://github.com/new?owner=" + url.QueryEscape(owner) + "&name=" + url.QueryEscape(name) + "&visibility=private"
}

// createByHand is the way on when GitHub won't let the login create the
// shadow repo: the person creates it, presses Enter, and init adopts it
// once it exists, is private and isn't a fork. A lookup GitHub didn't answer
// waits for Enter again.
func createByHand(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions) (engine.InitOptions, error) {
	shadow := shadowOf(o)
	ask.Say("GitHub didn't let your login create " + shadow + ". Create it yourself:")
	ask.Say("  1. Open " + newRepoURL(shadow) + " and create it: private, with no README, license or .gitignore.")
	ask.Say("  2. Give Ghostbranch access to it: add it to the Ghostbranch GitHub App's repositories (or to your token's).")
	for {
		if err := ask.Wait(ctx, "Press Enter once "+shadow+" exists."); err != nil {
			return o, err
		}
		sh, err := f.GetRepo(ctx, shadow)
		switch {
		case errors.Is(err, forge.ErrNotFound):
			ask.Say(shadow + " isn't there yet, or your login can't see it.")
		case errors.Is(err, forge.ErrTransient):
			ask.Say("couldn't reach GitHub; press Enter to try again")
		case err != nil:
			// Not transient (that case waits again), so not retryable.
			return o, contract.Wrap(contract.ForgeError, err, "look up %s", shadow)
		case sh.Fork:
			ask.Say(shadow + " is a fork; a shadow repo can't be one.")
		case !sh.Private:
			ask.Say(shadow + " is public; make it private in its settings.")
		default:
			o.Shadow, o.Adopt = shadow, true
			return o, nil
		}
	}
}

// guidedInit is spec § 4: ask what is missing, preview, run init, guard
// install and doctor, and end with the first-feature walkthrough. Nothing
// changes before "Proceed?", so Ctrl-C at any question leaves no trace.
func (a *app) guidedInit(ctx context.Context, args []string, f initFlags) error {
	a.ran = true
	ask, err := a.env.Asker()
	if err != nil {
		return contract.Wrap(contract.MissingInput, err,
			"init has questions and the console can't be opened; answer them with flags: <owner/repo>, --shadow or --adopt, --guard or --no-guard, --sync or --no-sync")
	}
	public := ""
	if len(args) == 1 {
		public = args[0]
	} else if public, err = askRepo(ctx, ask, "Public repo (owner/repo)", originSlug(a.env.CwdRemotes)); err != nil {
		return err
	}
	if err := a.ensureLogin(ctx, ask); err != nil {
		return err
	}
	e, err := a.engine(ctx)
	if err != nil {
		return err
	}
	if public, err = checkPublic(ctx, ask, e.Forge, public, len(args) == 0); err != nil {
		return err
	}
	o := engine.InitOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, Schedule: schedule(f.sync)}
	reg, registered := e.WS.Find(public)
	switch {
	case registered:
		o.Shadow = reg.Shadow
		ask.Say(fmt.Sprintf("Resuming the setup of %s, whose shadow repo is %s.", reg.Public, reg.Shadow))
	case f.shadow == "" && !f.adopt:
		if o, err = askShadow(ctx, ask, e.Forge, o); err != nil {
			return err
		}
	}
	in, installed := a.guardHere(public)
	guardOn := false
	switch {
	case f.guard != nil:
		if guardOn = *f.guard; guardOn && !in {
			return guardFlagErr(public)
		}
	case in && !installed:
		if guardOn, err = ask.YesNo(ctx, "Install the push guard in this checkout? It refuses pushes of feature work to the public repo.", true); err != nil {
			return err
		}
	}
	if f.sync == nil && !registered && !overlayExists(ctx, e.Forge, o) {
		on, err := ask.YesNo(ctx, "Turn on scheduled sync? A workflow in the shadow repo then syncs it every 6 hours (about 120 Actions minutes a month).", false)
		if err != nil {
			return err
		}
		o.Schedule = schedule(&on)
	}
	// The summary is the --plan preview.
	e.DryRun = true
	pv, _, err := e.Init(ctx, o)
	e.DryRun = a.dryRun
	if err != nil {
		return done(a, pv, nil, err)
	}
	ask.Say("This will:")
	lines := []string{}
	if pv.Preview != nil {
		lines = pv.Preview.Describe()
	}
	if len(lines) == 0 {
		ask.Say("  (nothing to change)")
	}
	for _, l := range lines {
		ask.Say("  - " + l)
	}
	if guardOn {
		ask.Say("  - install the push guard in this checkout")
	}
	if o.Schedule == engine.DefaultSyncSchedule {
		ask.Say("  - turn on scheduled sync (" + engine.DefaultSyncSchedule + ")")
	}
	a.pairFull = workspace.Pair{Public: pv.Public, Shadow: pv.Shadow}
	if a.dryRun {
		return done(a, pv, nil, nil)
	}
	if ok, err := ask.YesNo(ctx, "Proceed?", true); err != nil {
		return err
	} else if !ok {
		return contract.New(contract.Interrupted, "init stopped at the summary; nothing was changed")
	}
	ask.Say("Setting up " + public + "…")
	res, w, err := e.Init(ctx, o)
	if ce := contract.From(err); ce != nil && ce.Code == contract.ShadowCreateDenied {
		if o, err = createByHand(ctx, ask, e.Forge, o); err != nil {
			return err
		}
		var w2 []contract.Warning
		res, w2, err = e.Init(ctx, o)
		w = append(w, w2...)
	}
	if res != nil {
		a.pairFull = workspace.Pair{Public: res.Public, Shadow: res.Shadow}
	}
	if err != nil {
		return done(a, res, w, err)
	}
	res.Guided, res.GuardInstalled = true, installed
	if guardOn {
		ask.Say("Installing the push guard…")
		if gr, gerr := a.installGuard(ctx); gerr != nil {
			ask.Say("The push guard couldn't be installed: " + contract.From(gerr).Message)
		} else {
			res.GuardInstalled = gr.Status.Installed
		}
	}
	ask.Say("Checking the setup (ghostbranch doctor)…")
	if dr, derr := a.doctorRun(ctx, res.Public); derr == nil {
		s := dr.Summary()
		res.Doctor, res.DoctorChecks = &s, dr.Checks
	} else {
		ask.Say("doctor couldn't run: " + contract.From(derr).Message)
	}
	return done(a, res, w, nil)
}
