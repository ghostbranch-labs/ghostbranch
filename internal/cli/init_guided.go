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

// publicQ asks for the public repo; intro opens every guided run, before the
// first question.
const publicQ = "Which public repo should get private branches? (owner/repo)"

var intro = []string{
	"Ghostbranch will set up private branches for a public GitHub repo.",
	"It asks a few questions. Press Enter to accept the suggestion in [brackets].",
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

// checkPublic looks the public repo up and returns its name and what GitHub
// says of it. When reask, a missing repo or a declined private one asks
// again.
func checkPublic(ctx context.Context, ask prompt.Asker, f forge.Forge, public string, reask bool) (string, forge.Repo, error) {
	for {
		pub, err := f.GetRepo(ctx, public)
		switch {
		case err == nil && !pub.Private:
			return public, pub, nil
		case err == nil:
			use, aerr := ask.YesNo(ctx, public+" is private, and Ghostbranch is meant for public repos. Use it anyway?", false)
			if aerr != nil {
				return "", forge.Repo{}, aerr
			}
			if use {
				return public, pub, nil
			}
			if !reask {
				return "", forge.Repo{}, contract.New(contract.BadFlag, "%s is private, and init stopped there", public).WithField("public")
			}
		case errors.Is(err, forge.ErrNotFound) && reask:
			ask.Say(public + " doesn't exist, or your login can't see it.")
		default:
			ce := contract.Wrap(contract.ForgeError, err, "look up public repo %s", public)
			ce.Retryable = errors.Is(err, forge.ErrTransient)
			return "", forge.Repo{}, ce
		}
		var aerr error
		if public, aerr = askRepo(ctx, ask, publicQ, ""); aerr != nil {
			return "", forge.Repo{}, aerr
		}
	}
}

func shadowOf(o engine.InitOptions) string {
	if o.Shadow != "" {
		return o.Shadow
	}
	return o.Public + "-shadow"
}

// isPublic reports, and says, that name is the public repo itself, which
// can't be its own shadow repo.
func isPublic(ask prompt.Asker, o engine.InitOptions, name string) bool {
	if !strings.EqualFold(name, o.Public) {
		return false
	}
	ask.Say(name + " is the public repo; the shadow repo must be a different repo from the public one.")
	return true
}

// askShadow settles the shadow repo, starting from name: create it, adopt
// it if it exists, or take another name (adopted when it exists).
func askShadow(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions, name string) (engine.InitOptions, error) {
	for {
		if isPublic(ask, o, name) {
			var aerr error
			if name, aerr = askRepo(ctx, ask, "Which repo should be the private shadow repo instead? (owner/repo; an existing one is adopted)", ""); aerr != nil {
				return o, aerr
			}
			continue
		}
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
			yes, aerr = ask.YesNo(ctx, "Create the private repo "+name+" to hold your private branches (the shadow repo)?", true)
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
		if name, aerr = askRepo(ctx, ask, "Which repo should be the private shadow repo instead? (owner/repo; an existing one is adopted)", ""); aerr != nil {
			return o, aerr
		}
	}
}

// setup is what guided init knows of where the shadow repo can live: the
// login, when the public repo is an organization's (the person's account is
// then the other place), and the app's install link when the token is a
// stored app login, which reaches only the accounts the app is installed on.
type setup struct {
	login   string // "" unless the public repo is an organization's and the login isn't its owner
	app     string // the GitHub App's URL name
	install string // its install link; "" unless the token is a stored app login
}

// shadowSetup reads the setup: the public repo's owner type and the login.
func (a *app) shadowSetup(ctx context.Context, e *engine.Engine, public string, pub forge.Repo) setup {
	s := setup{app: a.env.AppSlug}
	if s.app == "" {
		s.app = auth.AppSlug
	}
	if e.TokenSource == "login" && auth.Kind(e.Token) == "app" {
		s.install = "https://github.com/apps/" + s.app + "/installations/new"
	}
	if pub.OwnerType != forge.OwnerOrganization {
		return s
	}
	owner, _, _ := forge.SplitName(public)
	if u, err := e.Forge.CurrentUser(ctx); err == nil && u.Login != "" && !strings.EqualFold(u.Login, owner) {
		s.login = u.Login
	}
	return s
}

// personalShadow is <login>/<repo>-shadow.
func personalShadow(login, public string) string {
	_, name, _ := forge.SplitName(public)
	return login + "/" + name + "-shadow"
}

// appNeeded is the line saying the app must be installed on the person's
// account, or "" when the token isn't an app login.
func (s setup) appNeeded(lead string) string {
	if s.install == "" {
		return ""
	}
	return lead + "the " + s.app + " GitHub App must be installed on your account (" + s.login + "): " + s.install
}

// chooseShadow asks where the shadow repo of an organization's public repo
// lives (the org, the person's account, or a name given); for anyone
// else's public repo it is askShadow from <public>-shadow.
func chooseShadow(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions, s setup) (engine.InitOptions, error) {
	org := o.Public + "-shadow"
	if s.login == "" {
		return askShadow(ctx, ask, f, o, org)
	}
	personal := personalShadow(s.login, o.Public)
	ask.Say("Where should the private shadow repo live?")
	ask.Say("  1. " + org + "   (the org; teammates can use it)   [default]")
	ask.Say("  2. " + personal + "   (your account; only you)")
	if l := s.appNeeded("     For 2, "); l != "" {
		ask.Say(l)
	}
	ask.Say("  3. another repo name, or an existing repo to adopt")
	for {
		c, err := ask.Text(ctx, "Which one? (1, 2 or 3)", "1")
		if err != nil {
			return o, err
		}
		switch strings.TrimSpace(c) {
		case "1":
			return askShadow(ctx, ask, f, o, org)
		case "2":
			return askShadow(ctx, ask, f, o, personal)
		case "3":
			for {
				name, err := askRepo(ctx, ask, "Which repo should be the private shadow repo? (owner/repo; an existing one is adopted)", "")
				if err != nil {
					return o, err
				}
				if !isPublic(ask, o, name) {
					return askShadow(ctx, ask, f, o, name)
				}
			}
		}
		ask.Say("Answer 1, 2 or 3.")
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
// waits for Enter again. An app login sees only the repos of the accounts
// the app is installed on (and, there, those it was given), so with one the
// steps and the not-there line give the app's install link.
func createByHand(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions, s setup) (engine.InitOptions, error) {
	shadow := shadowOf(o)
	owner, _, _ := forge.SplitName(shadow)
	ask.Say("GitHub didn't let your login create " + shadow + ". Create it yourself:")
	ask.Say("  1. Open " + newRepoURL(shadow) + " and create it: private, with no README, license or .gitignore.")
	notYet := shadow + " isn't there yet, or your login can't see it."
	if s.install != "" {
		ask.Say("  2. Give Ghostbranch access to it: install the " + s.app + " GitHub App on " + owner + ", or add " + shadow + " to its repositories, at " + s.install)
		notYet = shadow + " isn't there yet, or the " + s.app + " GitHub App can't see it: install the app on " + owner + ", or add " + shadow + " to its repositories, at " + s.install
	} else {
		ask.Say("  2. Give Ghostbranch access to it: add it to the Ghostbranch GitHub App's repositories (or to your token's).")
	}
	for {
		if err := ask.Wait(ctx, "Press Enter once "+shadow+" exists."); err != nil {
			return o, err
		}
		sh, err := f.GetRepo(ctx, shadow)
		switch {
		case errors.Is(err, forge.ErrNotFound):
			ask.Say(notYet)
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

func createDenied(err error) bool {
	ce := contract.From(err)
	return ce != nil && ce.Code == contract.ShadowCreateDenied
}

// explainRefusal says which account refused to create shadow and why:
// GitHub's own message, when it sent one (nothing else from the response),
// then the likely causes for this case, each with its fix.
func explainRefusal(ask prompt.Asker, s setup, public, shadow string, err error) {
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(public)
	line := "GitHub refused to create " + shadow + " on " + owner + "."
	msg := forge.HostMessage(err)
	if msg != "" {
		line += ` GitHub said: "` + msg + `"`
	}
	ask.Say(line)
	var causes []string
	appMissing := s.install != "" && strings.HasPrefix(msg, "Resource not accessible by integration")
	switch {
	case appMissing:
		causes = append(causes, "the "+s.app+" GitHub App isn't installed on "+owner+": install it at "+s.install)
	case s.install == "":
		causes = append(causes, "your token may lack Administration: write (fine-grained) or the repo scope (classic) on "+owner+": ghostbranch doctor shows what's missing")
	}
	if s.login != "" && strings.EqualFold(owner, pubOwner) && !appMissing {
		access := "then give your token access to it"
		if s.install != "" {
			access = "then add it to the app"
		}
		causes = append(causes, owner+"'s settings may not let members create private repos: ask an org owner to create "+shadow+
			" ("+access+"), or choose your personal account")
	}
	switch len(causes) {
	case 0:
	case 1:
		ask.Say("Likely cause:")
	default:
		ask.Say("Likely causes:")
	}
	for _, c := range causes {
		ask.Say("  - " + c)
	}
}

// createRefused is the way on once GitHub refused to create the shadow
// repo: it says why, offers the person's account when offer is set (no
// --shadow, no registered pair) and the refusal was the organization's, and
// otherwise (or when that is refused too) waits for the repo made by hand.
func createRefused(ctx context.Context, ask prompt.Asker, e *engine.Engine, o engine.InitOptions, s setup, offer bool, refusal error) (*engine.InitResult, []contract.Warning, error) {
	shadow := shadowOf(o)
	explainRefusal(ask, s, o.Public, shadow, refusal)
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(o.Public)
	var w []contract.Warning
	if offer && s.login != "" && strings.EqualFold(owner, pubOwner) {
		personal := personalShadow(s.login, o.Public)
		if l := s.appNeeded("For that, "); l != "" {
			ask.Say(l)
		}
		use, err := ask.YesNo(ctx, "Use "+personal+" instead? It's on your account, so only you can use it.", true)
		if err != nil {
			return nil, nil, err
		}
		if use {
			p := o
			_, err := e.Forge.GetRepo(ctx, personal)
			switch {
			case err == nil: // it exists: the usual checks, then adopt
				if p, err = askShadow(ctx, ask, e.Forge, o, personal); err != nil {
					return nil, nil, err
				}
			case errors.Is(err, forge.ErrNotFound):
				p.Shadow, p.Adopt = personal, false
			default:
				ce := contract.Wrap(contract.ForgeError, err, "look up %s", personal)
				ce.Retryable = errors.Is(err, forge.ErrTransient)
				return nil, nil, ce
			}
			res, w2, err := e.Init(ctx, p)
			w = append(w, w2...)
			if !createDenied(err) {
				return res, w, err
			}
			explainRefusal(ask, s, p.Public, shadowOf(p), err)
			o = p
		}
	}
	o, err := createByHand(ctx, ask, e.Forge, o, s)
	if err != nil {
		return nil, w, err
	}
	res, w2, err := e.Init(ctx, o)
	return res, append(w, w2...), err
}

// guidedInit is spec § 4: ask what is missing, preview, run init, guard
// install and doctor, and end with the first-feature walkthrough. Nothing
// changes before "Make these changes now?", so Ctrl-C at any question
// leaves no trace.
func (a *app) guidedInit(ctx context.Context, args []string, f initFlags) error {
	a.ran = true
	ask, err := a.env.Asker()
	if err != nil {
		return contract.Wrap(contract.MissingInput, err,
			"init has questions and the console can't be opened; answer them with flags: <owner/repo>, --shadow or --adopt, --guard or --no-guard, --sync or --no-sync")
	}
	ask = pausingAsker{inner: ask, p: a.reporter()}
	for _, l := range intro {
		ask.Say(l)
	}
	public := ""
	if len(args) == 1 {
		public = args[0]
	} else if public, err = askRepo(ctx, ask, publicQ, originSlug(a.env.CwdRemotes)); err != nil {
		return err
	}
	if err := a.ensureLogin(ctx, ask); err != nil {
		return err
	}
	e, err := a.engine(ctx)
	if err != nil {
		return err
	}
	var pub forge.Repo
	if public, pub, err = checkPublic(ctx, ask, e.Forge, public, len(args) == 0); err != nil {
		return err
	}
	s := a.shadowSetup(ctx, e, public, pub)
	o := engine.InitOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, Schedule: schedule(f.sync)}
	reg, registered := e.WS.Find(public)
	switch {
	case registered:
		o.Shadow = reg.Shadow
		ask.Say(fmt.Sprintf("Resuming the setup of %s, whose shadow repo is %s.", reg.Public, reg.Shadow))
	case f.shadow == "" && !f.adopt:
		if o, err = chooseShadow(ctx, ask, e.Forge, o, s); err != nil {
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
	if ok, err := ask.YesNo(ctx, "Make these changes now?", true); err != nil {
		return err
	} else if !ok {
		return contract.New(contract.Interrupted, "init stopped at the summary; nothing was changed")
	}
	ask.Say("Setting up " + public + "…")
	res, w, err := e.Init(ctx, o)
	if createDenied(err) {
		var w2 []contract.Warning
		res, w2, err = createRefused(ctx, ask, e, o, s, f.shadow == "" && !registered, err)
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
