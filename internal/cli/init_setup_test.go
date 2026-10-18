package cli_test

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Tier 1 setup guidance: guided init says who it acts as, asks before using
// gh's login, stops before a write when the token's account can't own the
// shadow repo, shows the app's install step after its own login, and turns
// "the app isn't installed" into install, then retry.

// userForge is the fake forge as seen with one token: CurrentUser answers
// login.
type userForge struct {
	forge.Forge
	login string
}

func (f userForge) CurrentUser(context.Context) (forge.User, error) {
	return forge.User{Login: f.login, ID: 7}, nil
}

// tokenUsers makes each token act as its own account (default for any
// other token), and records the tokens forges were made for.
func tokenUsers(te *testEnv, def string, users map[string]string) *[]string {
	var mu sync.Mutex
	var made []string
	te.env.NewForge = func(token string) (forge.Forge, error) {
		mu.Lock()
		made = append(made, token)
		mu.Unlock()
		login := def
		if u, ok := users[token]; ok {
			login = u
		}
		return userForge{te.forge, login}, nil
	}
	return &made
}

// initDeviceLogin points te's device flow at a test server; its first token is
// srv.AccessToken(1).
func initDeviceLogin(t *testing.T, te *testEnv) *authtest.Server {
	srv := authtest.New(t, authtest.Options{})
	te.env.ClientID, te.env.OAuthBaseURL, te.env.HTTPClient = srv.ClientID, srv.URL, srv.Client()
	return srv
}

// before reports whether line a (a prefix) comes before line b (a prefix)
// in p's log.
func before(p *person, a, b string) bool {
	ia := slices.IndexFunc(p.log, func(s string) bool { return strings.HasPrefix(s, a) })
	ib := slices.IndexFunc(p.log, func(s string) bool { return strings.HasPrefix(s, b) })
	return ia >= 0 && ib >= 0 && ia < ib
}

const (
	ghQ        = "Found a GitHub CLI (gh) login as acme. Use it, or log in with the Ghostbranch app instead? [app/gh]"
	installed  = "Press Enter once it's installed."
	loggedIn   = "Logged in to GitHub as acme with the Ghostbranch GitHub App."
	installNow = "Next, install the Ghostbranch app on acme and pick acme/widget (init creates the shadow repo next): " + installLink
)

func TestGuidedInitSaysWhichAccountItUses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, line string
		setup      func(te *testEnv)
	}{
		{"GHOSTBRANCH_TOKEN", "Using GitHub account acme (from GHOSTBRANCH_TOKEN)", func(*testEnv) {}},
		{"stored login", "Using GitHub account acme (from your Ghostbranch login)", func(te *testEnv) { appLogin(te) }},
		{"GH_TOKEN", "Using GitHub account acme (from GH_TOKEN)", func(te *testEnv) {
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			te.vars["GH_TOKEN"] = "gh-env-token"
		}},
		{"GITHUB_TOKEN", "Using GitHub account acme (from GITHUB_TOKEN)", func(te *testEnv) {
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			te.vars["GITHUB_TOKEN"] = "gh-env-token"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			tc.setup(te)
			p := atTerminal(te, "", "", "")
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			if !slices.Contains(p.said, tc.line) || !before(p, "! "+tc.line, "? Create the private repo") {
				t.Fatalf("no %q before the first write question: %q", tc.line, p.log)
			}
			if saidHas(p, "Next, install the Ghostbranch app") {
				t.Fatalf("an install step without a login in init: %q", p.said)
			}
		})
	}
}

// A token from gh is used only when the person says so; the default is
// logging in with the app, after which every request uses the new token.
func TestGuidedInitAsksBeforeUsingGhsLogin(t *testing.T) {
	t.Parallel()
	ghToken := func(te *testEnv) {
		delete(te.vars, "GHOSTBRANCH_TOKEN")
		te.env.GHToken = func() (string, error) { return "gh-cli-token", nil }
	}
	t.Run("gh", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		ghToken(te)
		p := atTerminal(te, "gh", "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, ghQ, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?")
		if !slices.Contains(p.said, "Using GitHub account acme (from the GitHub CLI (gh))") {
			t.Fatalf("said %q", p.said)
		}
		if l, _ := auth.LoadLogin(te.env.Keychain); l != nil {
			t.Fatalf("logged in: %v", l)
		}
		wantPair(t, te, "acme/widget-shadow")
	})
	t.Run("app, the default", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		ghToken(te)
		srv := initDeviceLogin(t, te)
		made := tokenUsers(te, "acme", nil)
		p := atTerminal(te, "", "", "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, ghQ, installed, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?")
		for _, l := range []string{loggedIn, "Using GitHub account acme (from your Ghostbranch login)", installNow} {
			if !slices.Contains(p.said, l) {
				t.Errorf("never said %q: %q", l, p.said)
			}
		}
		if l, _ := auth.LoadLogin(te.env.Keychain); l == nil || l.Token != srv.AccessToken(1) {
			t.Fatalf("stored %v", l)
		}
		if last := (*made)[len(*made)-1]; last != srv.AccessToken(1) {
			t.Fatalf("the last forge used %q, not the new login", last)
		}
		wantPair(t, te, "acme/widget-shadow")
	})
	t.Run("an answer that is neither asks again", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		ghToken(te)
		p := atTerminal(te, "maybe", "GH", "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, ghQ, ghQ, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?")
		if !saidHas(p, "Answer app or gh.") {
			t.Fatalf("said %q", p.said)
		}
	})
}

const (
	mismatch = "acme/widget belongs to acme, but this token is octo's, so it can't create acme's shadow repo."
	asAcmeQ  = "Log in as acme with the Ghostbranch app now?"
)

// A personal account's public repo and a token of another account: init
// says so before any write, and offers to log in as the owner.
func TestGuidedInitStopsWhenTheTokenIsAnotherAccounts(t *testing.T) {
	t.Parallel()
	envToken := func(te *testEnv) {
		delete(te.vars, "GHOSTBRANCH_TOKEN")
		te.vars["GH_TOKEN"] = "gh-env-token"
	}
	t.Run("declined", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		envToken(te)
		p := atTerminal(te, "n")
		te.forge.User.Login = "octo"
		code, _, errOut := te.run("init", "acme/widget")
		if code != 1 || !strings.Contains(errOut, "[AUTH_MISSING]") || !strings.Contains(errOut, "ghostbranch login") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		for _, want := range []string{"error: The GitHub token is octo's, not acme's",
			"or keep the shadow repo on your own account: ghostbranch init acme/widget --shadow octo/widget-shadow"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q:\n%s", want, errOut)
			}
		}
		hasPrefixes(t, p.asked, asAcmeQ)
		if !slices.Contains(p.said, mismatch) || shadowExists(te) || len(registered(te)) != 0 {
			t.Fatalf("said %q; shadow made %v", p.said, shadowExists(te))
		}
	})
	t.Run("logged in as the owner", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		envToken(te)
		srv := initDeviceLogin(t, te)
		made := tokenUsers(te, "octo", map[string]string{srv.AccessToken(1): "acme"})
		p := atTerminal(te, "", "", "", "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, asAcmeQ, installed, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?")
		for _, l := range []string{mismatch, loggedIn, "Using GitHub account acme (from your Ghostbranch login)", installNow} {
			if !slices.Contains(p.said, l) {
				t.Errorf("never said %q: %q", l, p.said)
			}
		}
		if last := (*made)[len(*made)-1]; last != srv.AccessToken(1) {
			t.Fatalf("the last forge used %q", last)
		}
		wantPair(t, te, "acme/widget-shadow")
	})
	t.Run("logged in as the same other account", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		envToken(te)
		initDeviceLogin(t, te)
		tokenUsers(te, "octo", nil)
		p := atTerminal(te, "y")
		code, _, errOut := te.run("init", "acme/widget")
		if code != 1 || !strings.Contains(errOut, "[AUTH_MISSING]") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		for _, want := range []string{"error: The GitHub token is octo's, not acme's",
			"or keep the shadow repo on your own account: ghostbranch init acme/widget --shadow octo/widget-shadow"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("stderr lacks %q:\n%s", want, errOut)
			}
		}
		if !saidHas(p, "still") || shadowExists(te) {
			t.Fatalf("said %q", p.said)
		}
	})
	// GHOSTBRANCH_TOKEN wins over any login, so a login wouldn't help: init
	// says so and stops.
	t.Run("GHOSTBRANCH_TOKEN", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		p := atTerminal(te)
		te.forge.User.Login = "octo"
		code, _, errOut := te.run("init", "acme/widget")
		if code != 1 || !strings.Contains(errOut, "[AUTH_MISSING]") || !strings.Contains(errOut, "GHOSTBRANCH_TOKEN") ||
			!strings.Contains(errOut, "error: The GitHub token is octo's, not acme's") {
			t.Fatalf("exit %d: %s", code, errOut)
		}
		if len(p.asked) != 0 || !slices.Contains(p.said, mismatch) || shadowExists(te) {
			t.Fatalf("asked %q, said %q", p.asked, p.said)
		}
	})
	t.Run("an org's repo keeps the org flow", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		p := atOrgTerminal(te, "", "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		if slices.Contains(p.asked, asAcmeQ) || saidHas(p, "belongs to") {
			t.Fatalf("asked %q", p.asked)
		}
	})
	t.Run("a --shadow on the token's account needs no login", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		p := atTerminal(te, "", "")
		te.forge.User.Login = "octo"
		if code, out, errOut := te.run("init", "acme/widget", "--shadow", "octo/widget-shadow"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		if saidHas(p, "belongs to") {
			t.Fatalf("said %q", p.said)
		}
	})
}

// After init's own login (no token at all), it confirms the account and
// shows the install step, then waits for it.
func TestGuidedInitShowsTheInstallStepAfterItsLogin(t *testing.T) {
	t.Parallel()
	for _, org := range []bool{false, true} {
		t.Run(map[bool]string{false: "personal", true: "org"}[org], func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			initDeviceLogin(t, te)
			answers := []string{"y", "", "", "", ""}
			if org {
				answers = append(answers, "")
			}
			p := atTerminal(te, answers...)
			if org {
				te.forge.Orgs = map[string]bool{"acme": true}
				te.forge.User.Login = "octo"
			}
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			who := "acme"
			if org {
				who = "octo"
			}
			if !slices.Contains(p.said, "Logged in to GitHub as "+who+" with the Ghostbranch GitHub App.") || !slices.Contains(p.said, installNow) {
				t.Fatalf("said %q", p.said)
			}
			if !before(p, "! "+installNow, "? "+installed) || !before(p, "? "+installed, "? Make these changes now?") {
				t.Fatalf("order: %q", p.log)
			}
		})
	}
}

const notInstalledLine = "The Ghostbranch app isn't installed on acme. Install it at " + installLink +
	" and pick acme/widget, then press Enter: init tries creating acme/widget-shadow again."

// An app login refused because the app isn't installed: install, then init
// creates the shadow itself.
func TestGuidedInitRetriesTheCreationOnceTheAppIsInstalled(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	refuseIn(te, notInstalledMsg, "acme")
	p := atTerminal(te, "", "", "", "")
	p.onWait = func() { te.forge.CreateRefused = nil }
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?", installed)
	if !slices.Contains(p.said, notInstalledLine) || saidHas(p, "Create it yourself") {
		t.Fatalf("said %q", p.said)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// Refused again for another reason: the create-by-hand steps, whose step 2
// picks both repos in the app's install.
func TestGuidedInitMakesTheShadowByHandWhenTheRetryIsRefused(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	refuseIn(te, notInstalledMsg, "acme")
	p := atTerminal(te, "", "", "", "", "")
	waits := 0
	p.onWait = func() {
		switch waits++; waits {
		case 1:
			refuseIn(te, "Repository creation failed.", "acme")
		case 2:
			_, _ = te.forge.AddRepo("acme/widget-shadow", true)
		}
	}
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?", installed,
		"Press Enter once acme/widget-shadow exists.")
	step2 := "  2. Give Ghostbranch access to it: install the ghostbranch-cli GitHub App on acme and pick both acme/widget and acme/widget-shadow " +
		"(publishing needs acme/widget), or add them to its repositories, at " + installLink
	if !slices.Contains(p.said, step2) {
		t.Fatalf("no step 2 %q: %q", step2, p.said)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// The install step is offered once: refused again, even for the same
// reason, init goes straight to the create-by-hand steps.
func TestGuidedInitInstallLoopIsBounded(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	refuseIn(te, notInstalledMsg, "acme")
	p := atTerminal(te, "", "", "", "", "")
	waits := 0
	p.onWait = func() {
		if waits++; waits == 2 {
			_, _ = te.forge.AddRepo("acme/widget-shadow", true)
		}
	}
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?",
		installed, "Press Enter once acme/widget-shadow exists.")
	wantPair(t, te, "acme/widget-shadow")
}

// A token that isn't the app's never hears "add it to the app's
// repositories": it gets the token advice, with a pre-filled token link
// and the repository choice the link can't make. Where the new token goes follows
// the token's source: a stored login is never read while GHOSTBRANCH_TOKEN
// is set.
func TestGuidedInitGivesATokenTheTokenFix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, store, never string
		env                map[string]string
	}{
		{"GHOSTBRANCH_TOKEN", "set the new token as GHOSTBRANCH_TOKEN", "ghostbranch login --with-token", nil},
		{"GH_TOKEN", "store it with ghostbranch login --with-token", "set the new token as GHOSTBRANCH_TOKEN", map[string]string{"GH_TOKEN": "gh-env-token"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.env != nil {
				delete(te.vars, "GHOSTBRANCH_TOKEN")
				for k, v := range tc.env {
					te.vars[k] = v
				}
			}
			te.forge.CreateForbidden = true
			p := atTerminal(te, "", "", "", "")
			p.onWait = func() { _, _ = te.forge.AddRepo("acme/widget-shadow", true) }
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			all := strings.Join(p.said, "\n")
			if strings.Contains(all, "App's repositories") || strings.Contains(all, "app's repositories") {
				t.Fatalf("app advice for a token: %q", p.said)
			}
			link := forge.TokenLink("ghostbranch-widget", "Ghostbranch: acme/widget and acme/widget-shadow", "acme", 90, forge.PersonTokenPermissions)
			for _, want := range []string{link, `choose "Only select repositories", then pick acme/widget and acme/widget-shadow`, tc.store} {
				if !strings.Contains(all, want) {
					t.Errorf("never said %q: %q", want, p.said)
				}
			}
			if strings.Contains(all, tc.never) {
				t.Errorf("said %q: %q", tc.never, p.said)
			}
		})
	}
}

// A refusal of a token whose account isn't the shadow owner's (here a
// registered pair, which skips the check before writes) says how to get the
// owner's token, not that a permission is missing. A login can't take
// effect while GHOSTBRANCH_TOKEN is set, so then it says to set that.
func TestGuidedInitRefusalNamesTheAccountMismatch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, want string
		ghToken    bool
	}{
		{"GH_TOKEN", "  - your token is octo's, so it can't create repos on acme: run ghostbranch login and approve it as acme (the Ghostbranch app), then ghostbranch init again", true},
		{"GHOSTBRANCH_TOKEN", "  - your token is octo's, so it can't create repos on acme: set GHOSTBRANCH_TOKEN to a token of acme's (a login can't take effect while it is set), then ghostbranch init again", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.ghToken {
				delete(te.vars, "GHOSTBRANCH_TOKEN")
				te.vars["GH_TOKEN"] = "gh-env-token"
			}
			ws, err := workspace.Open(te.vars["GHOSTBRANCH_HOME"], "default")
			if err != nil {
				t.Fatal(err)
			}
			ws.AddPair(workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"})
			if err := ws.Save(); err != nil {
				t.Fatal(err)
			}
			refuseIn(te, "Not Found", "acme")
			p := atTerminal(te, "", "")
			te.forge.User.Login = "octo"
			p.onWait = func() { _, _ = te.forge.AddRepo("acme/widget-shadow", true) }
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			if !slices.Contains(p.said, tc.want) || saidHas(p, "Administration: write") {
				t.Fatalf("said %q", p.said)
			}
		})
	}
}

// With --adopt or a registered pair, init creates no shadow repo, so the
// install step doesn't say it will.
func TestGuidedInitInstallStepForAnExistingShadow(t *testing.T) {
	t.Parallel()
	const line = "Next, install the Ghostbranch app on acme and pick acme/widget: " + installLink
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"--adopt", []string{"--adopt", "--no-sync"}},
		{"registered", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.args == nil {
				initOnly(te)
			} else if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
				t.Fatal(err)
			}
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			initDeviceLogin(t, te)
			p := atTerminal(te, "y", "", "", "")
			if code, out, errOut := te.run(append([]string{"init", "acme/widget"}, tc.args...)...); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			if !slices.Contains(p.said, line) || saidHas(p, "init creates the shadow repo next") {
				t.Fatalf("said %q", p.said)
			}
		})
	}
}

// Plain init (agents' JSON) works out the setup checklist first: --plan
// returns it, and a stop is its code before any write.

// personal makes acme/widget acme's own repo, with acme's token.
func personal(te *testEnv) { te.forge.User.Login, te.forge.Orgs = "acme", nil }

// installApp installs the Ghostbranch app on acme (a personal account), for
// every repo.
func installApp(te *testEnv) {
	te.forge.Installations = []forge.Installation{{ID: 7, Account: "acme", AccountType: forge.OwnerUser, Selection: "all"}}
}

// setupSteps is data.setup.steps as id → status, and the ids in order.
func setupSteps(t *testing.T, env map[string]any) (map[string]string, []string) {
	t.Helper()
	d, _ := env["data"].(map[string]any)
	s, _ := d["setup"].(map[string]any)
	steps, _ := s["steps"].([]any)
	if len(steps) == 0 {
		t.Fatalf("no data.setup.steps: %v", env["data"])
	}
	status := map[string]string{}
	var ids []string
	for _, st := range steps {
		m, _ := st.(map[string]any)
		id, _ := m["id"].(string)
		status[id], _ = m["status"].(string)
		ids = append(ids, id)
	}
	return status, ids
}

func errDetails(env map[string]any) map[string]any {
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	return d
}

func nextNeedsHuman(env map[string]any, i int) bool {
	ns, _ := env["next"].([]any)
	if i >= len(ns) {
		return false
	}
	m, _ := ns[i].(map[string]any)
	h, _ := m["needs_human"].(bool)
	return h
}

func noWrites(t *testing.T, te *testEnv) {
	t.Helper()
	if len(te.forge.Created) != 0 || shadowExists(te) || len(registered(te)) != 0 {
		t.Fatalf("wrote: created %v, pairs %v", te.forge.Created, registered(te))
	}
}

func TestPlainInitPlanJSONHasSetup(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	personal(te)
	installApp(te)
	code, env := te.runJSON("init", "acme/widget", "--plan")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	status, ids := setupSteps(t, env)
	if !slices.Equal(ids, engine.SetupStepIDs) {
		t.Fatalf("ids %v", ids)
	}
	want := map[string]string{"login": "done", "account": "done", "app_installed": "done", "shadow": "todo",
		"app_access": "todo", "pair": "todo", "guard": "skipped", "doctor": "todo"}
	if !maps.Equal(status, want) {
		t.Fatalf("steps %v, want %v", status, want)
	}
	if d, _ := env["data"].(map[string]any); d["preview"] == nil {
		t.Fatalf("no preview: %v", d)
	}
	noWrites(t, te)
}

// A plan with a stop is still a plan: the checklist says what needs a
// person, exit 0, and nothing is written.
func TestPlainInitPlanWithAStopWritesNothing(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	personal(te)
	code, env := te.runJSON("init", "acme/widget", "--plan")
	if code != 0 || env["error"] != nil {
		t.Fatalf("exit %d: %v", code, env)
	}
	if status, _ := setupSteps(t, env); status["app_installed"] != "needs_human" {
		t.Fatalf("steps %v", status)
	}
	noWrites(t, te)
}

func TestPlainInitAppNotInstalledStopsBeforeWrite(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	personal(te)
	code, env := te.runJSON("init", "acme/widget")
	if c, _ := errCode(env); code != 2 || c != "APP_NOT_INSTALLED" {
		t.Fatalf("exit %d: %v", code, env)
	}
	if n := nextCommands(env); len(n) != 1 || n[0] != "ghostbranch init acme/widget --json" || !nextNeedsHuman(env, 0) {
		t.Fatalf("next %v", env["next"])
	}
	accts, _ := errDetails(env)["accounts"].([]any)
	if len(accts) != 1 || accts[0].(map[string]any)["account"] != "acme" || accts[0].(map[string]any)["install_url"] != installLink {
		t.Fatalf("details %v", errDetails(env))
	}
	if status, _ := setupSteps(t, env); status["app_installed"] != "needs_human" {
		t.Fatalf("steps %v", status)
	}
	noWrites(t, te)
}

func TestPlainInitWrongAccountCode(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, why string }{
		{"GH_TOKEN", "a person logs in as acme at a terminal"},
		{"GHOSTBRANCH_TOKEN", "GHOSTBRANCH_TOKEN wins over any login"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.name == "GH_TOKEN" {
				delete(te.vars, "GHOSTBRANCH_TOKEN")
				te.vars["GH_TOKEN"] = "gh-env-token"
			}
			te.forge.User.Login, te.forge.Orgs = "octo", nil // acme's own repo
			code, env := te.runJSON("init", "acme/widget")
			if c, _ := errCode(env); code != 2 || c != "TOKEN_WRONG_ACCOUNT" {
				t.Fatalf("exit %d: %v", code, env)
			}
			if d := errDetails(env); d["token_account"] != "octo" || d["owner"] != "acme" {
				t.Fatalf("details %v", d)
			}
			n := nextCommands(env)
			if len(n) != 2 || n[0] != "ghostbranch login" || !nextNeedsHuman(env, 0) ||
				n[1] != "ghostbranch init acme/widget --shadow octo/widget-shadow" || nextNeedsHuman(env, 1) {
				t.Fatalf("next %v", env["next"])
			}
			if ns, _ := env["next"].([]any); !strings.Contains(ns[0].(map[string]any)["why"].(string), tc.why) {
				t.Fatalf("why %v", ns[0])
			}
			noWrites(t, te)
		})
	}
}

// A token that isn't an app login keeps v0.1.1's refusal.
func TestPlainInitNonAppTokenUnchanged(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.CreateForbidden = true
	code, env := te.runJSON("init", "acme/widget")
	if c, _ := errCode(env); code != 1 || c != "SHADOW_CREATE_DENIED" {
		t.Fatalf("exit %d: %v", code, env)
	}
	if status, _ := setupSteps(t, env); status["app_installed"] != "skipped" || status["app_access"] != "skipped" {
		t.Fatalf("steps %v", status)
	}
}

// The shadow's name is taken on GitHub, but the app can't see that repo:
// with an app login it is APP_CANT_SEE_REPO, with the install's Configure
// page; another token keeps today's error.
func TestPlainInitShadowExistsButHiddenIsAppCantSeeRepo(t *testing.T) {
	t.Parallel()
	t.Run("app login", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		appLogin(te)
		personal(te)
		installApp(te)
		te.forge.CreateTaken = map[string]bool{"acme/widget-shadow": true}
		code, env := te.runJSON("init", "acme/widget")
		if c, _ := errCode(env); code != 2 || c != "APP_CANT_SEE_REPO" {
			t.Fatalf("exit %d: %v", code, env)
		}
		d := errDetails(env)
		repos, _ := d["repos"].([]any)
		if d["configure_url"] != "https://github.com/settings/installations/7" || len(repos) != 1 || repos[0] != "acme/widget-shadow" {
			t.Fatalf("details %v", d)
		}
		if steps, _ := d["steps"].([]any); len(steps) != len(engine.SetupStepIDs) {
			t.Fatalf("details.steps %v", d["steps"])
		}
		if e, _ := env["error"].(map[string]any); !strings.Contains(e["message"].(string), "tick acme/widget-shadow") {
			t.Fatalf("message %v", e["message"])
		}
		if n := nextCommands(env); len(n) != 1 || n[0] != "ghostbranch init acme/widget --json" || !nextNeedsHuman(env, 0) {
			t.Fatalf("next %v", env["next"])
		}
		if status, _ := setupSteps(t, env); status["app_access"] != "needs_human" || status["shadow"] != "done" {
			t.Fatalf("steps %v", status)
		}
		if len(registered(te)) != 0 {
			t.Fatalf("registered %v", registered(te))
		}
	})
	t.Run("another token", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.forge.CreateTaken = map[string]bool{"acme/widget-shadow": true}
		_, env := te.runJSON("init", "acme/widget")
		if c, _ := errCode(env); c == "" || c == "APP_CANT_SEE_REPO" {
			t.Fatalf("error %v", env["error"])
		}
	})
}

// On success data.setup is the checklist once init is done.
func TestPlainInitSuccessHasTheFinalSetup(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	personal(te)
	installApp(te)
	code, env := te.runJSON("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	want := map[string]string{"login": "done", "account": "done", "app_installed": "done", "shadow": "done",
		"app_access": "done", "pair": "done", "guard": "skipped", "doctor": "todo"}
	if status, _ := setupSteps(t, env); !maps.Equal(status, want) {
		t.Fatalf("steps %v, want %v", status, want)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// AppLogin is set only for a stored login whose Kind is app: not for a
// stored token of another kind (even one with the app's ghu_ prefix), and
// not for GHOSTBRANCH_TOKEN, which wins over a stored app login.
func appStepsRan(t *testing.T, te *testEnv) bool {
	t.Helper()
	personal(te)
	installApp(te)
	code, env := te.runJSON("init", "acme/widget", "--plan")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	status, _ := setupSteps(t, env)
	return status["app_installed"] != "skipped"
}

func TestAppLoginWiringAppLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	if !appStepsRan(t, te) {
		t.Fatal("an app login skipped the app steps")
	}
}

func TestAppLoginWiringPATLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	if err := auth.SaveLogin(te.env.Keychain, auth.Login{Token: "ghu_looks_like_the_app", Account: "acme", Kind: "pat"}); err != nil {
		t.Fatal(err)
	}
	if appStepsRan(t, te) {
		t.Fatal("a stored login of kind pat ran the app steps")
	}
}

func TestAppLoginWiringGhostbranchToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	storeLogin(te, "ghu_test", "ghr_test", time.Hour)
	te.vars["GHOSTBRANCH_TOKEN"] = "ghu_from_env"
	if appStepsRan(t, te) {
		t.Fatal("GHOSTBRANCH_TOKEN ran the app steps")
	}
}
