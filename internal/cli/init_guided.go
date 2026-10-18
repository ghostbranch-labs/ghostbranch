package cli

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
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
// sync flags. It works out the setup checklist first and, on a stop, writes
// nothing and returns the stop's code (with --plan too), with the checklist
// in data.setup. On success data.setup is the checklist worked out again.
func (a *app) plainInit(ctx context.Context, public string, f initFlags) error {
	a.ran = true
	in, installed := a.guardHere(public)
	guardOn := f.guard != nil && *f.guard
	if guardOn && !in {
		return guardFlagErr(public)
	}
	so := engine.SetupOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, InstallURL: a.installURL()}
	e, err := a.engine(ctx)
	if ce := contract.From(err); ce != nil && (ce.Code == contract.AuthMissing || ce.Code == contract.LoginExpired) {
		return a.loggedOutInit(ctx, so, in, installed, guardOn, ce)
	}
	if err != nil {
		return err
	}
	if _, err := e.InitPair(public, f.shadow); err != nil {
		return err
	}
	setup, err := a.setupCheck(ctx, e, so, in, installed, guardOn)
	if err != nil {
		return err
	}
	plainDoctorStep(setup)
	if stop := setup.FirstStop(); stop != nil {
		return a.setupStopped(e, so, setup, stop, installed)
	}
	res, w, err := e.Init(ctx, engine.InitOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, Schedule: schedule(f.sync)})
	if res != nil {
		a.pairFull = workspace.Pair{Public: res.Public, Shadow: res.Shadow}
		res.GuardInstalled = installed
		res.Setup = setup
	}
	if ce := contract.From(err); ce != nil && ce.Code == contract.AppCantSeeRepo && res != nil {
		res.Setup = shadowHiddenSetup(setup, ce)
		ce.Details["steps"] = res.Setup.Steps
	}
	if err == nil && res.Preview == nil && guardOn {
		gr, gerr := a.installGuard(ctx)
		if gerr != nil {
			return done(a, res, w, gerr)
		}
		res.GuardInstalled = gr.Status.Installed
	}
	if err == nil && res.Preview == nil {
		// The checklist again, now that init is done; the one from before
		// stands if GitHub can't be read.
		if s, serr := a.setupCheck(ctx, e, so, in, res.GuardInstalled, guardOn); serr == nil {
			plainDoctorStep(s)
			res.Setup = s
		}
	}
	return done(a, res, w, err)
}

// loggedOutInit is plain init with no token (AUTH_MISSING) or an expired
// login (LOGIN_EXPIRED): the checklist worked out without one, so an agent
// setting up from scratch sees every step, and the code with
// details.steps. Nothing is written.
func (a *app) loggedOutInit(ctx context.Context, so engine.SetupOptions, in, installed, guardOn bool, ce *contract.Error) error {
	ws, err := a.openWorkspace()
	if err != nil {
		return err
	}
	e := &engine.Engine{WS: ws, Now: a.env.Now}
	p, err := e.InitPair(so.Public, so.Shadow)
	if err != nil {
		return err
	}
	s, err := a.setupCheck(ctx, e, so, in, installed, guardOn)
	if err != nil {
		return err
	}
	plainDoctorStep(s)
	d := map[string]any{}
	for k, v := range ce.Details {
		d[k] = v
	}
	d["steps"] = s.Steps
	res := &engine.InitResult{Public: p.Public, Shadow: p.Shadow, MirroredBranches: []string{}, MirroredTags: []string{},
		GuardInstalled: installed, Setup: s}
	return done(a, res, nil, ce.WithDetails(d))
}

// plainDoctorStep makes s's doctor step true for plain init, which doesn't
// run doctor (guided init does): the caller runs it once init is done.
func plainDoctorStep(s *engine.Setup) {
	for i := range s.Steps {
		if s.Steps[i].ID == "doctor" {
			s.Steps[i].Status, s.Steps[i].Message = engine.SetupTodo, "Run ghostbranch doctor once init is done, to check the setup."
		}
	}
}

// setupCheck is e.SetupCheck with the guard step for this directory, which
// the CLI knows (GuardState) and the engine doesn't.
func (a *app) setupCheck(ctx context.Context, e *engine.Engine, so engine.SetupOptions, in, installed, guardOn bool) (*engine.Setup, error) {
	s, err := e.SetupCheck(ctx, so)
	if err != nil || !in {
		return s, err
	}
	for i := range s.Steps {
		if s.Steps[i].ID != "guard" {
			continue
		}
		st := engine.SetupStep{ID: "guard", Status: engine.SetupSkipped, Details: map[string]any{},
			Message: "Pass --guard to install the push guard in this checkout."}
		switch {
		case installed:
			st.Status, st.Message = engine.SetupDone, "The push guard is installed in this checkout."
		case guardOn:
			st.Status, st.Message = engine.SetupTodo, "Ghostbranch installs the push guard in this checkout."
		}
		s.Steps[i] = st
	}
	return s, nil
}

// shadowHiddenSetup is the checklist for APP_CANT_SEE_REPO at create time:
// the shadow repo exists, and app_access is the stop.
func shadowHiddenSetup(s *engine.Setup, ce *contract.Error) *engine.Setup {
	out := &engine.Setup{Steps: append([]engine.SetupStep(nil), s.Steps...)}
	configure, _ := ce.Details["configure_url"].(string)
	repos, _ := ce.Details["repos"].([]string)
	for i, st := range out.Steps {
		switch st.ID {
		case "shadow":
			if len(repos) == 1 {
				out.Steps[i] = engine.SetupStep{ID: "shadow", Status: engine.SetupDone, Details: map[string]any{},
					Message: repos[0] + " exists, though the Ghostbranch app can't see it."}
			}
		case "app_access":
			out.Steps[i] = engine.SetupStep{ID: "app_access", Status: engine.SetupNeedsHuman, Message: ce.Message, URL: configure,
				Details: map[string]any{"configure_url": configure, "repos": ce.Details["repos"]}}
		}
	}
	return out
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

// ensureLogin offers a login when no token resolves, and reports whether
// it ran one.
func (a *app) ensureLogin(ctx context.Context, ask prompt.Asker) (bool, error) {
	_, _, err := auth.Resolve(ctx, a.authSource())
	ce := contract.From(err)
	if ce == nil {
		return false, nil
	}
	if ce.Code != contract.AuthMissing && ce.Code != contract.LoginExpired {
		return false, err
	}
	q := "No GitHub login found. Log in with GitHub now?"
	if ce.Code == contract.LoginExpired {
		q = "Your GitHub login has expired. Log in again now?"
	}
	yes, aerr := ask.YesNo(ctx, q, true)
	if aerr != nil {
		return false, aerr
	}
	if !yes {
		ask.Say("To use a token instead, pipe it in: ghostbranch login --with-token < token.txt, then run ghostbranch init again.")
		return false, contract.New(contract.AuthMissing, "init needs a GitHub login: run ghostbranch login (or ghostbranch login --with-token, with the token on stdin), then init again")
	}
	return true, a.loginInInit(ctx, ask)
}

// loginInInit is runLogin inside guided init, confirming the account.
func (a *app) loginInInit(ctx context.Context, ask prompt.Asker) error {
	l, err := a.runLogin(ctx, ask)
	if err != nil {
		return err
	}
	who := l.Account
	if who == "" {
		who = "an account GitHub didn't name"
	}
	ask.Say("Logged in to GitHub as " + who + " with the Ghostbranch GitHub App.")
	return nil
}

// sourceWords is auth.Resolve's token source in plain words.
func sourceWords(src string) string { return engine.TokenSourceWords(src) }

// tokenUser is the token's GitHub login, or "" when GitHub doesn't say.
func tokenUser(ctx context.Context, e *engine.Engine) string {
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return ""
	}
	return u.Login
}

// sayAccount is the line naming the account and the token's source. After a
// login in this run (justLoggedIn) it says nothing when the login line
// already named the account.
func sayAccount(ask prompt.Asker, e *engine.Engine, user string, justLoggedIn bool) {
	if user != "" && justLoggedIn && e.TokenSource == "login" {
		return
	}
	if user == "" {
		ask.Say("Using a GitHub token from " + sourceWords(e.TokenSource) + "; GitHub didn't say which account it is.")
		return
	}
	ask.Say("Using GitHub account " + user + " (from " + sourceWords(e.TokenSource) + ")")
}

// relogin runs the device login inside init and builds the engine again,
// so every request after it uses the new token.
func (a *app) relogin(ctx context.Context, ask prompt.Asker) (*engine.Engine, string, error) {
	if err := a.loginInInit(ctx, ask); err != nil {
		return nil, "", err
	}
	e, err := a.engine(ctx)
	if err != nil {
		return nil, "", err
	}
	return e, tokenUser(ctx, e), nil
}

// askGhOrApp asks whether to use the gh CLI's login or log in with the app;
// app is the default.
func askGhOrApp(ctx context.Context, ask prompt.Asker, user string) (bool, error) {
	if user == "" {
		user = "an account GitHub didn't name"
	}
	q := "Found a GitHub CLI (gh) login as " + user + ". Use it, or log in with the Ghostbranch app instead? [app/gh]"
	for try := 1; ; try++ {
		c, err := ask.Text(ctx, q, "")
		if err != nil {
			return false, err
		}
		switch strings.ToLower(strings.TrimSpace(c)) {
		case "", "app":
			return true, nil
		case "gh":
			return false, nil
		}
		if try == prompt.MaxTries {
			return false, contract.New(contract.BadFlag, "no app or gh answer to %q after %d tries", q, prompt.MaxTries)
		}
		ask.Say("Answer app or gh.")
	}
}

// setupRechecks is how many times guided init checks a step a person does
// again: twice. A step still stopped after its second re-check stops init,
// so no step is asked a third time (anti-goal 2).
const setupRechecks = 2

// guidedSetup works out the setup checklist for so and shows it. A step a
// person does (needs_human) shows its link, waits for Enter, and the
// checklist is worked out again; a step still stopped after setupRechecks
// re-checks, or a blocked one, stops init with its code. The account step
// offers a login as the owner instead, once. The shadow step is no stop
// here: the shadow questions settle it. It returns the engine to go on
// with, which a login replaces, and the token's account.
func (a *app) guidedSetup(ctx context.Context, ask prompt.Asker, e *engine.Engine, so engine.SetupOptions, in, installed, guardOn bool, user string) (*engine.Engine, string, error) {
	rechecks := map[string]int{}
	relogged := false
	st := a.styler(a.env.StderrColor)
	for {
		s, err := a.setupCheck(ctx, e, so, in, installed, guardOn)
		if err != nil {
			return nil, "", err
		}
		stop := guidedStop(s)
		waits := stop != nil && stop.Status == engine.SetupNeedsHuman && stop.ID != "account" && rechecks[stop.ID] < setupRechecks
		var instr []string
		var enter string
		if waits {
			instr, enter = a.stopInstructions(ctx, e, so, stop)
		}
		var replace map[string][]string
		if instr != nil {
			replace = map[string][]string{stop.ID: instr}
		}
		for _, l := range checklistLines(s, st, replace) {
			ask.Say(l)
		}
		switch {
		case stop == nil:
			return e, user, nil
		case stop.Status == engine.SetupBlocked:
			return nil, "", a.setupStopped(e, so, s, stop, installed)
		case stop.ID == "account":
			owner, _ := stop.Details["owner"].(string)
			if relogged {
				ask.Say("The new login is still " + user + "'s, not " + owner + "'s.")
			}
			// GHOSTBRANCH_TOKEN wins over any login, so a login can't help.
			if relogged || e.TokenSource == "GHOSTBRANCH_TOKEN" {
				return nil, "", a.setupStopped(e, so, s, stop, installed)
			}
			yes, err := ask.YesNo(ctx, "Log in as "+owner+" with the Ghostbranch app now?", true)
			if err != nil {
				return nil, "", err
			}
			if !yes {
				return nil, "", a.setupStopped(e, so, s, stop, installed)
			}
			if e, user, err = a.relogin(ctx, ask); err != nil {
				return nil, "", err
			}
			sayAccount(ask, e, user, true)
			relogged = true
			continue
		case !waits:
			return nil, "", a.setupStopped(e, so, s, stop, installed)
		}
		if instr == nil {
			if stop.URL != "" {
				ask.Say(attention(st, "Link: "+stop.URL))
			}
			enter = "Press Enter once it's done."
		}
		if err := ask.Wait(ctx, enter); err != nil {
			return nil, "", err
		}
		rechecks[stop.ID]++
	}
}

// guidedStop is the first step guided init waits on or stops at: needs_human
// or blocked, other than the shadow step.
func guidedStop(s *engine.Setup) *engine.SetupStep {
	for i := range s.Steps {
		st := &s.Steps[i]
		if st.ID != "shadow" && (st.Status == engine.SetupNeedsHuman || st.Status == engine.SetupBlocked) {
			return st
		}
	}
	return nil
}

// stopInstructions is what a step the person does shows in place of its
// checklist line: the step as a "!" heading, what it is for, and the
// numbered steps, each in bold yellow, with the line Wait shows. It is nil
// for a step it has no instructions for.
func (a *app) stopInstructions(ctx context.Context, e *engine.Engine, so engine.SetupOptions, stop *engine.SetupStep) ([]string, string) {
	st := a.styler(a.env.StderrColor)
	var lines []string
	say := func(l string) { lines = append(lines, attention(st, l)) }
	switch stop.ID {
	case "app_installed":
		p, err := e.InitPair(so.Public, so.Shadow)
		if err != nil {
			return nil, ""
		}
		pubOwner, _, _ := forge.SplitName(p.Public)
		shadowOwner, _, _ := forge.SplitName(p.Shadow)
		_, shadowExists := lookupRepo(ctx, e, p.Shadow)
		accounts, _ := stop.Details["accounts"].([]map[string]any)
		first := true
		for _, acct := range accounts {
			name, _ := acct["account"].(string)
			if name == "" || acct["status"] != "missing" {
				continue
			}
			var pick []string
			if strings.EqualFold(name, pubOwner) {
				pick = append(pick, p.Public)
			}
			if strings.EqualFold(name, shadowOwner) && shadowExists {
				pick = append(pick, p.Shadow)
			}
			say("! Install the Ghostbranch app on " + name)
			if first {
				say("    Ghostbranch acts on GitHub through its app, and only on the repos you pick.")
			}
			say("    1. Open " + stop.URL)
			choose := "    2. Choose " + name + `, then "Only select repositories"`
			switch {
			case len(pick) > 0:
				choose += ", then pick " + strings.Join(pick, " and ")
			default:
				choose += ", then pick any one of your repos for now (GitHub needs one); init adds " + p.Shadow + " when it creates it"
			}
			say(choose)
			if !shadowExists && strings.EqualFold(name, shadowOwner) {
				if len(pick) > 0 {
					say("       (init creates " + p.Shadow + " next; the app gets it automatically)")
				}
			}
			say("    3. Click Install, then come back here")
			if isOrg(ctx, e, name, p.Public, p.Shadow) {
				say("    If you aren't an owner of " + name + ", GitHub sends an owner a request; setup continues once they approve.")
			}
			first = false
		}
		if lines == nil {
			return nil, ""
		}
		return lines, "Press Enter once it's installed."
	case "app_access":
		repos, _ := stop.Details["repos"].([]string)
		if len(repos) == 0 || stop.URL == "" {
			return nil, ""
		}
		list := strings.Join(repos, " and ")
		say("! The Ghostbranch app is installed, but can't see " + list)
		say("    1. Open " + stop.URL)
		say("    2. Under Repository access, tick " + list)
		say("    3. Click Save, then come back here")
		return lines, "Press Enter once it's saved."
	}
	return nil, ""
}

// lookupRepo is the repo GitHub names, if it exists.
func lookupRepo(ctx context.Context, e *engine.Engine, name string) (forge.Repo, bool) {
	r, err := e.Forge.GetRepo(ctx, name)
	return r, err == nil
}

// isOrg reports whether account is an organization, going by the repos
// (public, shadow) it owns that GitHub names.
func isOrg(ctx context.Context, e *engine.Engine, account string, repos ...string) bool {
	for _, name := range repos {
		if owner, _, _ := forge.SplitName(name); !strings.EqualFold(owner, account) {
			continue
		}
		if r, ok := lookupRepo(ctx, e, name); ok && r.OwnerType != "" {
			return r.OwnerType == forge.OwnerOrganization
		}
	}
	return false
}

// setupStopped ends init at a setup stop, before any write: stop's code,
// with the checklist in data.setup.
func (a *app) setupStopped(e *engine.Engine, so engine.SetupOptions, s *engine.Setup, stop *engine.SetupStep, installed bool) error {
	p, err := e.InitPair(so.Public, so.Shadow)
	if err != nil {
		return err
	}
	res := &engine.InitResult{Public: p.Public, Shadow: p.Shadow, MirroredBranches: []string{}, MirroredTags: []string{},
		GuardInstalled: installed, Setup: s}
	return done(a, res, nil, s.StopError(stop))
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
	user    string // the token's account; "" when GitHub didn't say
	pubOrg  bool   // the public repo is an organization's
	source  string // auth.Resolve's source of the token
}

// appSlug is the GitHub App's URL name.
func (a *app) appSlug() string {
	if a.env.AppSlug != "" {
		return a.env.AppSlug
	}
	return auth.AppSlug
}

// installURL is the GitHub App's install page.
func (a *app) installURL() string {
	return "https://github.com/apps/" + a.appSlug() + "/installations/new"
}

// shadowSetup reads the setup: the public repo's owner type and the
// token's account, user.
func (a *app) shadowSetup(e *engine.Engine, public string, pub forge.Repo, user string) setup {
	s := setup{app: a.appSlug(), user: user, pubOrg: pub.OwnerType == forge.OwnerOrganization, source: e.TokenSource}
	if e.AppLogin {
		s.install = a.installURL()
	}
	if !s.pubOrg {
		return s
	}
	owner, _, _ := forge.SplitName(public)
	if user != "" && !strings.EqualFold(user, owner) {
		s.login = user
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
// once it exists, is private and isn't a fork. A check that fails (a lookup
// GitHub didn't answer too) says why and waits for Enter again, once: after
// setupRechecks checks it stops with what is still wrong. An app login sees
// only the repos of the accounts the app is installed on (and, there, those
// it was given), so with one the steps and the not-there line give the
// app's install link.
func createByHand(ctx context.Context, ask prompt.Asker, f forge.Forge, o engine.InitOptions, s setup) (engine.InitOptions, error) {
	shadow := shadowOf(o)
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(o.Public)
	same := strings.EqualFold(owner, pubOwner) // one install, or one token, can hold both repos
	ask.Say("GitHub didn't let your login create " + shadow + ". Create it yourself:")
	ask.Say("  1. Open " + newRepoURL(shadow) + " and create it: private, with no README, license or .gitignore.")
	// GitHub answers 404 both for a repo that doesn't exist and for one
	// the app can't see, so with an app login both causes are said.
	notYet := []string{shadow + " isn't there yet, or your login can't see it."}
	switch {
	case s.install != "" && same:
		ask.Say("  2. Give Ghostbranch access to it: install the " + s.app + " GitHub App on " + owner + " and pick both " + o.Public + " and " + shadow +
			" (publishing needs " + o.Public + "), or add them to its repositories, at " + s.install)
		notYet = []string{"- If you haven't created " + shadow + " yet: create it at " + newRepoURL(shadow) + ".",
			"- If you have: the Ghostbranch app can't see it yet. Add " + shadow + " (and " + o.Public + ") to the app's repositories at " + s.install + "."}
	case s.install != "":
		ask.Say("  2. Give Ghostbranch access to it: install the " + s.app + " GitHub App on " + owner + " and pick " + shadow + ", or add it to its repositories, at " + s.install)
		notYet = []string{"- If you haven't created " + shadow + " yet: create it at " + newRepoURL(shadow) + ".",
			"- If you have: the Ghostbranch app can't see it yet. Add " + shadow + " to the app's repositories at " + s.install + "."}
	default:
		ask.Say("  2. Give your token access to it: " + tokenAccess(o.Public, shadow, s.source))
	}
	for check := 1; ; check++ {
		if err := ask.Wait(ctx, "Press Enter once "+shadow+" exists."); err != nil {
			return o, err
		}
		sh, err := f.GetRepo(ctx, shadow)
		var wrong []string
		switch {
		case errors.Is(err, forge.ErrNotFound):
			wrong = notYet
		case errors.Is(err, forge.ErrTransient):
			wrong = []string{"couldn't reach GitHub to check " + shadow + "."}
		case err != nil:
			// Not transient (that case waits again), so not retryable.
			return o, contract.Wrap(contract.ForgeError, err, "look up %s", shadow)
		case sh.Fork:
			wrong = []string{shadow + " is a fork; a shadow repo can't be one."}
		case !sh.Private:
			wrong = []string{shadow + " is public; make it private in its settings."}
		default:
			o.Shadow, o.Adopt = shadow, true
			return o, nil
		}
		if check == setupRechecks {
			// No third ask (anti-goal 2): stop with what is still wrong.
			return o, handShadowStopped(o.Public, shadow, wrong)
		}
		if errors.Is(err, forge.ErrTransient) {
			wrong = []string{"couldn't reach GitHub; press Enter to try again"}
		}
		for _, l := range wrong {
			ask.Say(l)
		}
	}
}

// handShadowStopped is SHADOW_CREATE_DENIED once a hand-made shadow repo
// still isn't right after its second check: what is still wrong, and the
// adopt that finishes the setup once it is fixed.
func handShadowStopped(public, shadow string, wrong []string) error {
	adopt := "ghostbranch init " + public + " --adopt --shadow " + shadow
	wrong = slices.Clone(wrong)
	for i := range wrong {
		wrong[i] = strings.TrimPrefix(wrong[i], "- ")
	}
	steps := append(slices.Clone(wrong), "Run "+adopt+".")
	return contract.New(contract.ShadowCreateDenied, "%s was checked twice and still isn't ready: %s Once that is fixed, run %s",
		shadow, strings.Join(wrong, " "), adopt).
		WithDetails(map[string]any{"public": public, "shadow": shadow, "explicit_shadow": true, "steps": steps})
}

// tokenAccess is how a token (not the app) reaches a shadow repo made by
// hand: add it to the token, or make a token from the pre-filled link,
// which can't pick the repos itself. A new token goes where this run's came
// from: while GHOSTBRANCH_TOKEN is set, a stored login is never read.
func tokenAccess(public, shadow, source string) string {
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(public)
	add := shadow + " (and " + public + ")"
	if !strings.EqualFold(owner, pubOwner) {
		add = shadow
	}
	link, repos := forge.PairTokenLink(public, shadow)
	store := "store it with ghostbranch login --with-token"
	if source == "GHOSTBRANCH_TOKEN" {
		store = "set the new token as GHOSTBRANCH_TOKEN"
	}
	return "add " + add + " to its repositories, or make a fine-grained token at " + link +
		` (choose "Only select repositories", then pick ` + repos + `), then ` + store + ` and run ghostbranch init again.`
}

func createDenied(err error) bool {
	ce := contract.From(err)
	return ce != nil && ce.Code == contract.ShadowCreateDenied
}

// appMissing is whether GitHub refused an app login because the app isn't
// installed on the account.
func (s setup) appMissing(err error) bool {
	return s.install != "" && strings.HasPrefix(forge.HostMessage(err), "Resource not accessible by integration")
}

// explainRefusal says which account refused to create shadow and why:
// GitHub's own message, when it sent one (nothing else from the response),
// then the likely causes for this case, each with its fix. With retry, an
// app that isn't installed is the install step instead, and init then
// waits for Enter and tries again.
func explainRefusal(ask prompt.Asker, s setup, public, shadow string, err error, retry bool) {
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(public)
	line := "GitHub refused to create " + shadow + " on " + owner + "."
	msg := forge.HostMessage(err)
	if msg != "" {
		line += ` GitHub said: "` + msg + `"`
	}
	ask.Say(line)
	appMissing := s.appMissing(err)
	if appMissing && retry {
		pick := ""
		if strings.EqualFold(owner, pubOwner) {
			pick = " and pick " + public
		}
		ask.Say("The Ghostbranch app isn't installed on " + owner + ". Install it at " + s.install + pick + ", then press Enter: init tries creating " + shadow + " again.")
		return
	}
	// The token's account is another personal account than the owner's
	// (a registered pair, which the account step lets through): no
	// permission makes that work, a login as the owner does.
	mismatch := s.install == "" && s.user != "" && !s.pubOrg && strings.EqualFold(owner, pubOwner) && !strings.EqualFold(owner, s.user)
	var causes []string
	switch {
	case appMissing:
		causes = append(causes, "the "+s.app+" GitHub App isn't installed on "+owner+": install it at "+s.install)
	case mismatch && s.source == "GHOSTBRANCH_TOKEN":
		causes = append(causes, "your token is "+s.user+"'s, so it can't create repos on "+owner+": set GHOSTBRANCH_TOKEN to a token of "+owner+"'s (a login can't take effect while it is set), then ghostbranch init again")
	case mismatch:
		causes = append(causes, "your token is "+s.user+"'s, so it can't create repos on "+owner+": run ghostbranch login and approve it as "+owner+" (the Ghostbranch app), then ghostbranch init again")
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

// installRetries is how many times init tries the creation again after the
// app's install step: once. A second refusal, for any reason, goes on to
// the next way (the person's account for an org, else making the repo by
// hand).
const installRetries = 1

// installThenRetry explains refusal, the refused creation of o's shadow.
// When the app isn't installed there, it shows the install step, waits for
// Enter and runs the same init again, up to installRetries times. It
// returns the last attempt: a refusal it couldn't get past (explained) or
// init's result.
func installThenRetry(ctx context.Context, ask prompt.Asker, e *engine.Engine, o engine.InitOptions, s setup, refusal error) (*engine.InitResult, []contract.Warning, error) {
	var w []contract.Warning
	for try := 0; ; try++ {
		retry := s.appMissing(refusal) && try < installRetries
		explainRefusal(ask, s, o.Public, shadowOf(o), refusal, retry)
		if !retry {
			return nil, w, refusal
		}
		if err := ask.Wait(ctx, "Press Enter once it's installed."); err != nil {
			return nil, w, err
		}
		res, w2, err := e.Init(ctx, o)
		w = append(w, w2...)
		if !createDenied(err) {
			return res, w, err
		}
		refusal = err
	}
}

// createRefused is the way on once GitHub refused to create the shadow
// repo: it says why (and, when the app isn't installed, waits for its
// install and tries again), offers the person's account when offer is set
// (no --shadow, no registered pair) and the refusal was the organization's,
// and otherwise (or when that is refused too) waits for the repo made by
// hand.
func createRefused(ctx context.Context, ask prompt.Asker, e *engine.Engine, o engine.InitOptions, s setup, offer bool, refusal error) (*engine.InitResult, []contract.Warning, error) {
	res, w, err := installThenRetry(ctx, ask, e, o, s, refusal)
	if !createDenied(err) {
		return res, w, err
	}
	shadow := shadowOf(o)
	owner, _, _ := forge.SplitName(shadow)
	pubOwner, _, _ := forge.SplitName(o.Public)
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
			res, w2, err = installThenRetry(ctx, ask, e, p, s, err)
			w = append(w, w2...)
			if !createDenied(err) {
				return res, w, err
			}
			o = p
		}
	}
	o, err = createByHand(ctx, ask, e.Forge, o, s)
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
	ask = pausingAsker{inner: prompt.Styled(ask, a.styler(a.env.StderrColor).Bold, func(t string) string { return attention(a.styler(a.env.StderrColor), t) }), p: a.reporter()}
	for _, l := range intro {
		ask.Say(l)
	}
	public := ""
	if len(args) == 1 {
		public = args[0]
	} else if public, err = askRepo(ctx, ask, publicQ, originSlug(a.env.CwdRemotes)); err != nil {
		return err
	}
	loggedIn, err := a.ensureLogin(ctx, ask)
	if err != nil {
		return err
	}
	e, err := a.engine(ctx)
	if err != nil {
		return err
	}
	user := tokenUser(ctx, e)
	// gh's login may be another account's: it is used only when the person
	// says so (spike R0 finding 5). Resolve's order is unchanged.
	if e.TokenSource == "gh auth token" && !loggedIn {
		useApp, err := askGhOrApp(ctx, ask, user)
		if err != nil {
			return err
		}
		if useApp {
			ask.Say("To stop Ghostbranch using gh's login, add GHOSTBRANCH_NO_GH=1 to your shell profile.")
			if e, user, err = a.relogin(ctx, ask); err != nil {
				return err
			}
			loggedIn = true
		}
	}
	sayAccount(ask, e, user, loggedIn)
	var pub forge.Repo
	if public, pub, err = checkPublic(ctx, ask, e.Forge, public, len(args) == 0); err != nil {
		return err
	}
	reg, registered := e.WS.Find(public)
	in, installed := a.guardHere(public)
	// The checklist, before any write: the app's install comes before the
	// shadow repo is created.
	so := engine.SetupOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, InstallURL: a.installURL()}
	flagGuard := f.guard != nil && *f.guard
	if e, user, err = a.guidedSetup(ctx, ask, e, so, in, installed, flagGuard, user); err != nil {
		return err
	}
	checked := f.shadow
	if checked == "" && registered {
		checked = reg.Shadow
	}
	checked = shadowOf(engine.InitOptions{Public: public, Shadow: checked})
	s := a.shadowSetup(e, public, pub, user)
	o := engine.InitOptions{Public: public, Shadow: f.shadow, Adopt: f.adopt, Schedule: schedule(f.sync)}
	switch {
	case registered:
		o.Shadow = reg.Shadow
		ask.Say(fmt.Sprintf("Resuming the setup of %s, whose shadow repo is %s.", reg.Public, reg.Shadow))
	case f.shadow == "" && !f.adopt:
		if o, err = chooseShadow(ctx, ask, e.Forge, o, s); err != nil {
			return err
		}
		// Another shadow repo (the person's account, say) may need the app
		// on another account: check again.
		if !strings.EqualFold(shadowOf(o), checked) {
			so.Shadow, so.Adopt = o.Shadow, o.Adopt
			if e, user, err = a.guidedSetup(ctx, ask, e, so, in, installed, flagGuard, user); err != nil {
				return err
			}
			s = a.shadowSetup(e, public, pub, user)
		}
	}
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
	nothing := len(lines) == 0
	// Nothing to confirm when the preview is empty and no guard or sync
	// line follows; only then is "(nothing to change)" true of the block.
	skipConfirm := nothing && !guardOn && o.Schedule != engine.DefaultSyncSchedule
	if skipConfirm {
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
	if skipConfirm {
		ask.Say("Nothing to change: " + pv.Public + " is already set up with " + pv.Shadow + ".")
	} else if ok, err := ask.YesNo(ctx, "Make these changes now?", true); err != nil {
		return err
	} else if !ok {
		return contract.New(contract.Interrupted, "init stopped at the summary; nothing was changed")
	}
	if !skipConfirm {
		ask.Say("Setting up " + public + "…")
	}
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
