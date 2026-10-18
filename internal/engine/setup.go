package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
)

// The statuses of a setup step.
const (
	SetupDone       = "done"
	SetupNeedsHuman = "needs_human"
	SetupBlocked    = "blocked"
	SetupTodo       = "todo" // Ghostbranch will do it
	SetupSkipped    = "skipped"
)

// SetupStepIDs are the setup checklist's steps, in order. The order and
// the ids are part of the contract.
var SetupStepIDs = []string{"login", "account", "app_installed", "shadow", "app_access", "pair", "guard", "doctor"}

// SetupStep is one step of the setup checklist.
type SetupStep struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Message string         `json:"message"`
	URL     string         `json:"url"`     // "" when none
	Details map[string]any `json:"details"` // never nil
}

// Setup is the setup checklist for one pair: every step of SetupStepIDs.
type Setup struct {
	Steps []SetupStep `json:"steps"`
}

// SetupOptions name the pair setup is for, as init's flags do. Checkout is
// the checkout the guard goes in ("" for none); InstallURL is the
// Ghostbranch app's install page.
type SetupOptions struct {
	Public, Shadow string
	Adopt          bool
	Checkout       string
	InstallURL     string
}

// setupLookup is what SetupCheck read from GitHub.
type setupLookup struct {
	user         string
	pub          forge.Repo
	pubFound     bool
	shadowRepo   forge.Repo
	shadowExists bool
	insts        []forge.Installation
	instsErr     error // ErrForbidden from listing the installations
}

// SetupCheck works out the setup checklist for opt's pair. It is
// read-only: GETs only, no write to GitHub, git or the workspace. Steps
// after the first stop are still worked out where they can be; a step that
// can't be known until an earlier one is done is todo.
func (e *Engine) SetupCheck(ctx context.Context, opt SetupOptions) (*Setup, error) {
	p, err := e.initPair(opt.Public, opt.Shadow)
	if err != nil {
		return nil, err
	}
	_, registered := e.WS.Find(p.Public)
	appSteps := e.AppLogin && e.installer() != nil
	steps := map[string]SetupStep{}
	set := func(id, status, url string, details map[string]any, format string, args ...any) {
		if details == nil {
			details = map[string]any{}
		}
		steps[id] = SetupStep{ID: id, Status: status, Message: fmt.Sprintf(format, args...), URL: url, Details: details}
	}

	if registered {
		set("pair", SetupDone, "", nil, "%s is registered with %s.", p.Public, p.Shadow)
	} else {
		set("pair", SetupTodo, "", nil, "Ghostbranch registers %s with %s.", p.Public, p.Shadow)
	}
	e.setupGuard(ctx, opt.Checkout, set)
	set("doctor", SetupTodo, "", nil, "Ghostbranch runs doctor once setup is done.")

	if e.Token == "" || e.Forge == nil {
		set("login", SetupNeedsHuman, "", nil, "log in with ghostbranch login")
		set("account", SetupTodo, "", nil, "Ghostbranch checks the token's account once you log in.")
		set("shadow", SetupTodo, "", nil, "Ghostbranch checks %s once you log in.", p.Shadow)
		// ghostbranch login is an app login, so the app steps come once
		// the person has logged in.
		for _, id := range []string{"app_installed", "app_access"} {
			set(id, SetupTodo, "", nil, "Ghostbranch checks the Ghostbranch app once you log in.")
		}
		return ordered(steps), nil
	}
	var lk setupLookup
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return nil, forgeErr(err, "read the token's GitHub account")
	}
	lk.user = u.Login
	set("login", SetupDone, "", map[string]any{"source": e.TokenSource, "account": lk.user},
		"Logged in as %s (the token from %s).", lk.user, e.TokenSource)
	switch r, err := e.Forge.GetRepo(ctx, p.Public); {
	case err == nil:
		lk.pub, lk.pubFound = r, true
	case errors.Is(err, forge.ErrNotFound) && appSteps:
		// The app may be what can't see it: app_access says so.
	default:
		return nil, forgeErr(err, "look up public repo %s", p.Public)
	}
	switch r, err := e.Forge.GetRepo(ctx, p.Shadow); {
	case err == nil:
		lk.shadowRepo, lk.shadowExists = r, true
	case !errors.Is(err, forge.ErrNotFound):
		return nil, forgeErr(err, "look up shadow repo %s", p.Shadow)
	}
	if appSteps {
		lk.insts, err = e.installer().UserInstallations(ctx)
		if err != nil {
			if !errors.Is(err, forge.ErrForbidden) {
				return nil, forgeErr(err, "list the Ghostbranch app's installations")
			}
			lk.instsErr = err
		}
	}

	pubOwner, name, _ := forge.SplitName(p.Public)
	shadowOwner, _, _ := forge.SplitName(p.Shadow)
	ownerType := func(account string) string {
		switch {
		case strings.EqualFold(account, pubOwner) && lk.pub.OwnerType != "":
			return lk.pub.OwnerType
		case strings.EqualFold(account, shadowOwner) && lk.shadowRepo.OwnerType != "":
			return lk.shadowRepo.OwnerType
		}
		if inst, ok := installFor(lk.insts, account); ok && inst.AccountType != "" {
			return inst.AccountType
		}
		return forge.OwnerUser
	}

	// account: a personal account's repos are created by that account
	// only. An org checks membership itself when the shadow is created. A
	// registered pair or --adopt creates no repo, so any account the person
	// set up with is fine (as v0.1.x: a collaborator's pair keeps working).
	owner := ""
	switch {
	case registered || opt.Adopt:
	case ownerType(pubOwner) != forge.OwnerOrganization:
		if !strings.EqualFold(pubOwner, lk.user) && !strings.EqualFold(shadowOwner, lk.user) {
			owner = pubOwner
		}
	case lk.shadowExists && lk.shadowRepo.OwnerType == forge.OwnerUser &&
		!strings.EqualFold(shadowOwner, pubOwner) && !strings.EqualFold(shadowOwner, lk.user):
		owner = shadowOwner
	}
	acct := map[string]any{"token_account": lk.user, "owner": pubOwner}
	if owner == "" {
		set("account", SetupDone, "", acct, "The token is %s's, which can set up %s.", lk.user, p.Shadow)
	} else {
		acct["owner"] = owner
		yours := lk.user + "/" + name + "-shadow"
		acct["shadow_on_your_account"] = yours
		fix := "log in as " + owner + " with ghostbranch login"
		if e.TokenSource == "GHOSTBRANCH_TOKEN" {
			fix = "set GHOSTBRANCH_TOKEN to a token of " + owner + "'s"
		}
		set("account", SetupNeedsHuman, "", acct,
			"The token is %s's, so it can't set up %s on %s: %s, or put the shadow on your account with --shadow %s.",
			lk.user, p.Shadow, owner, fix, yours)
	}

	switch {
	case lk.shadowExists && (opt.Adopt || registered):
		set("shadow", SetupDone, "", nil, "%s exists.", p.Shadow)
	case lk.shadowExists:
		set("shadow", SetupNeedsHuman, "", nil, "%s already exists; pass --adopt to take it over.", p.Shadow)
	case opt.Adopt:
		set("shadow", SetupNeedsHuman, "", nil, "%s does not exist; drop --adopt to create it.", p.Shadow)
	default:
		set("shadow", SetupTodo, "", nil, "Ghostbranch creates the private repo %s.", p.Shadow)
	}

	if !appSteps {
		for _, id := range []string{"app_installed", "app_access"} {
			set(id, SetupSkipped, "", nil, "Only an app login can see where the Ghostbranch app is installed.")
		}
		return ordered(steps), nil
	}
	if lk.instsErr != nil {
		set("app_installed", SetupBlocked, "", nil,
			"GitHub refused to list the Ghostbranch app's installations; log in again with ghostbranch login.")
		set("app_access", SetupTodo, "", nil, "Ghostbranch checks the app can see %s once it can list the app's installations.", p.Public)
		return ordered(steps), nil
	}

	// app_installed: on the public repo's owner, and the shadow's owner
	// when it differs.
	needed := []string{pubOwner}
	if !strings.EqualFold(shadowOwner, pubOwner) {
		needed = append(needed, shadowOwner)
	}
	var accounts []map[string]any
	var missing, sentences []string
	for _, a := range needed {
		if _, ok := installFor(lk.insts, a); ok {
			accounts = append(accounts, map[string]any{"account": a, "status": "installed", "install_url": ""})
			continue
		}
		accounts = append(accounts, map[string]any{"account": a, "status": "missing", "install_url": opt.InstallURL})
		missing = append(missing, a)
		var pick []string
		if strings.EqualFold(a, pubOwner) {
			pick = append(pick, p.Public)
		}
		if strings.EqualFold(a, shadowOwner) && lk.shadowExists {
			pick = append(pick, p.Shadow)
		}
		s := "Install the Ghostbranch app on " + a
		if len(pick) > 0 {
			s += " and pick " + strings.Join(pick, " and ")
		}
		if ownerType(a) == forge.OwnerOrganization {
			s += "; if you aren't an owner of " + a + ", GitHub asks an owner to approve the request, and setup continues once they do"
		}
		sentences = append(sentences, s+".")
	}
	details := map[string]any{"accounts": accounts}
	if len(missing) > 0 {
		set("app_installed", SetupNeedsHuman, opt.InstallURL, details, "%s", strings.Join(sentences, " "))
		set("app_access", SetupTodo, "", nil, "Ghostbranch checks the app can see %s once it is installed.", p.Public)
		return ordered(steps), nil
	}
	set("app_installed", SetupDone, "", details, "The Ghostbranch app is installed on %s.", strings.Join(needed, " and "))

	st, err := e.setupAccess(ctx, p.Public, p.Shadow, lk)
	if err != nil {
		return nil, err
	}
	steps["app_access"] = st
	return ordered(steps), nil
}

// setupAccess is the app_access step: whether the app can see the public
// repo and, once it exists, the shadow. A repo the installation's list
// doesn't name is still seen when GetRepo reaches it, except a personal
// account's public repo: the user sees all of that account's repos, so the
// list is the selection, and GetRepo reads any public repo anyway.
func (e *Engine) setupAccess(ctx context.Context, public, shadow string, lk setupLookup) (SetupStep, error) {
	pubOwner, _, _ := forge.SplitName(public)
	inst, _ := installFor(lk.insts, pubOwner)
	details := map[string]any{"selection": inst.Selection, "repos": []string{}}
	seen := func(full string, inst forge.Installation, reachable, personalPublic bool) (bool, error) {
		if inst.Selection == "all" {
			return true, nil
		}
		repos, err := e.installer().InstallationRepos(ctx, inst.ID)
		if err != nil {
			return false, forgeErr(err, "list the repos the Ghostbranch app can see on %s", inst.Account)
		}
		for _, r := range repos {
			if strings.EqualFold(r, full) {
				return true, nil
			}
		}
		return reachable && !personalPublic, nil
	}
	ok, err := seen(public, inst, lk.pubFound, inst.AccountType != forge.OwnerOrganization)
	if err != nil {
		return SetupStep{}, err
	}
	if !ok {
		details["repos"] = []string{public}
		details["configure_url"] = inst.ConfigureURL()
		msg := fmt.Sprintf("Add %s to the Ghostbranch app's repositories on %s.", public, inst.Account)
		if inst.AccountType == forge.OwnerOrganization {
			msg = fmt.Sprintf("The Ghostbranch app can't see %s: ask an owner of %s to add %s to the Ghostbranch app.", public, inst.Account, public)
		}
		return SetupStep{ID: "app_access", Status: SetupNeedsHuman, Message: msg, URL: inst.ConfigureURL(), Details: details}, nil
	}
	if !lk.shadowExists {
		return SetupStep{ID: "app_access", Status: SetupTodo, Details: details,
			Message: fmt.Sprintf("The Ghostbranch app can see %s; Ghostbranch checks %s once it creates it.", public, shadow)}, nil
	}
	// The shadow exists, so GetRepo reached it: the app keeps access to a
	// repo it created even when Configure doesn't list it.
	return SetupStep{ID: "app_access", Status: SetupDone, Details: details,
		Message: fmt.Sprintf("The Ghostbranch app can see %s and %s.", public, shadow)}, nil
}

// setupGuard is the guard step: skipped without a checkout.
func (e *Engine) setupGuard(ctx context.Context, checkout string, set func(id, status, url string, details map[string]any, format string, args ...any)) {
	if checkout == "" {
		set("guard", SetupSkipped, "", nil, "No checkout, so no push guard to install.")
		return
	}
	co, err := guard.Open(ctx, checkout)
	if err != nil {
		ce := contract.From(err)
		switch ce.Code {
		case contract.NotAPairCheckout:
			set("guard", SetupSkipped, "", nil, "%s isn't a git checkout, so no push guard to install.", checkout)
		case contract.GuardUnsupported:
			set("guard", SetupSkipped, "", nil, "The push guard can't run here: %s", ce.Message)
		default:
			set("guard", SetupTodo, "", nil, "Ghostbranch installs the push guard in %s.", checkout)
		}
		return
	}
	if _, installed, _, err := guard.ReadHook(co); err == nil && installed {
		set("guard", SetupDone, "", nil, "The push guard is installed in %s.", co.Top)
		return
	}
	set("guard", SetupTodo, "", nil, "Ghostbranch installs the push guard in %s.", co.Top)
}

// installer is e.Forge's Installer, or nil when it can't tell.
func (e *Engine) installer() forge.Installer {
	in, _ := e.Forge.(forge.Installer)
	return in
}

// installFor is the installation on account.
func installFor(insts []forge.Installation, account string) (forge.Installation, bool) {
	for _, in := range insts {
		if strings.EqualFold(in.Account, account) {
			return in, true
		}
	}
	return forge.Installation{}, false
}

// ordered is steps in SetupStepIDs order.
func ordered(steps map[string]SetupStep) *Setup {
	s := &Setup{Steps: make([]SetupStep, 0, len(SetupStepIDs))}
	for _, id := range SetupStepIDs {
		s.Steps = append(s.Steps, steps[id])
	}
	return s
}

// FirstStop is the first step that is needs_human or blocked, or nil.
func (s *Setup) FirstStop() *SetupStep {
	for i := range s.Steps {
		if st := s.Steps[i].Status; st == SetupNeedsHuman || st == SetupBlocked {
			return &s.Steps[i]
		}
	}
	return nil
}

// StopError turns a stop into its code: app_installed → APP_NOT_INSTALLED
// (details.accounts []{account, install_url}); app_access →
// APP_CANT_SEE_REPO (details.configure_url, details.repos); account →
// TOKEN_WRONG_ACCOUNT (details.token_account, details.owner); login (and
// an app_installed GitHub refused to list) → AUTH_MISSING; shadow →
// BAD_FLAG on --adopt. Every one carries details.steps = s.Steps.
func (s *Setup) StopError(step *SetupStep) *contract.Error {
	if step == nil {
		return nil
	}
	d := map[string]any{"steps": s.Steps}
	var ce *contract.Error
	switch {
	case step.ID == "app_installed" && step.Status == SetupNeedsHuman:
		ce = contract.New(contract.AppNotInstalled, "%s", step.Message)
		missing := []map[string]any{}
		accounts, _ := step.Details["accounts"].([]map[string]any)
		for _, a := range accounts {
			if a["status"] == "missing" {
				missing = append(missing, map[string]any{"account": a["account"], "install_url": a["install_url"]})
			}
		}
		d["accounts"] = missing
	case step.ID == "app_access":
		ce = contract.New(contract.AppCantSeeRepo, "%s", step.Message)
		d["configure_url"], d["repos"] = step.Details["configure_url"], step.Details["repos"]
	case step.ID == "account":
		ce = contract.New(contract.TokenWrongAccount, "%s", step.Message)
		d["token_account"], d["owner"] = step.Details["token_account"], step.Details["owner"]
	case step.ID == "login" || step.ID == "app_installed":
		ce = contract.New(contract.AuthMissing, "%s", step.Message)
	case step.ID == "shadow":
		ce = contract.New(contract.BadFlag, "%s", step.Message).WithField("adopt")
	default:
		ce = contract.New(contract.Internal, "setup step %s stopped: %s", step.ID, step.Message)
	}
	return ce.WithDetails(d)
}
