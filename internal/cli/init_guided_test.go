package cli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// person is a scripted person at the console. It records each question
// and each line said, interrupts (Ctrl-C) at question number interruptAt
// (1-based; 0 never), and runs onWait when asked to press Enter.
type person struct {
	inner       prompt.Asker
	asked, said []string
	log         []string // every question ("? q") and line said ("! m"), in order
	interruptAt int
	onWait      func()
}

func (p *person) ask(q string) error {
	p.asked = append(p.asked, q)
	p.log = append(p.log, "? "+q)
	if len(p.asked) == p.interruptAt {
		return contract.New(contract.Interrupted, "interrupted (Ctrl-C)")
	}
	return nil
}

func (p *person) YesNo(ctx context.Context, q string, def bool) (bool, error) {
	if err := p.ask(q); err != nil {
		return false, err
	}
	return p.inner.YesNo(ctx, q, def)
}

func (p *person) Text(ctx context.Context, q, def string) (string, error) {
	if err := p.ask(q); err != nil {
		return "", err
	}
	return p.inner.Text(ctx, q, def)
}

func (p *person) Wait(ctx context.Context, m string) error {
	if err := p.ask(m); err != nil {
		return err
	}
	if p.onWait != nil {
		p.onWait()
	}
	return p.inner.Wait(ctx, m)
}

func (p *person) Say(m string) {
	p.said = append(p.said, m)
	p.log = append(p.log, "! "+m)
	p.inner.Say(m)
}

// atTerminal makes te a person's terminal in a checkout whose origin is
// acme/widget (not a clone on disk: GuardState says so), answering with
// answers. The token is acme's own, as a person setting up their own
// public repo has; atOrgTerminal makes it octo's.
func atTerminal(te *testEnv, answers ...string) *person {
	te.forge.User.Login, te.forge.Orgs = "acme", nil // acme's own repo
	p := &person{inner: prompt.NewScript(answers...)}
	te.env.ConsoleUsable = true
	te.env.Asker = func() (prompt.Asker, error) { return p, nil }
	te.env.CwdRemotes = func() []workspace.Remote {
		return []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget.git"}}
	}
	return p
}

// hasPrefixes checks asked, in order, against the questions' openings.
func hasPrefixes(t *testing.T, asked []string, want ...string) {
	t.Helper()
	if len(asked) != len(want) {
		t.Fatalf("asked %q, want %d questions starting %q", asked, len(want), want)
	}
	for i := range want {
		if !strings.HasPrefix(asked[i], want[i]) {
			t.Fatalf("question %d is %q, want it to start %q (all: %q)", i+1, asked[i], want[i], asked)
		}
	}
}

func shadowExists(te *testEnv) bool {
	_, err := te.forge.GetRepo(context.Background(), "acme/widget-shadow")
	return !errors.Is(err, forge.ErrNotFound)
}

func registered(te *testEnv) []workspace.Pair {
	ws, err := workspace.Open(te.vars["GHOSTBRANCH_HOME"], "default")
	if err != nil {
		te.t.Fatal(err)
	}
	return ws.Pairs
}

// The whole flow in a real clone, as a person sees it: every default taken.
// hermeticGit uses t.Setenv, so this test is serial.
func TestGuidedInitWalksAPersonThrough(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	dir := pairCheckout(te)
	hook := filepath.Join(dir, ".git", "hooks", "pre-push")
	te.env.GuardState = func(string) (bool, bool) { _, err := os.Stat(hook); return true, err == nil }
	if err := os.MkdirAll(filepath.Join(te.root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(te.root, "bin", "ghostbranch"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := atTerminal(te, "", "", "", "", "")
	code, out, errOut := te.run("init")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Which public repo should get private branches? (owner/repo)", "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?",
		"Install the push guard in this checkout?", "Turn on scheduled sync?", "Make these changes now?")
	for _, want := range []string{"Shadow repo acme/widget-shadow is ready for acme/widget.", "the push guard is installed in this checkout",
		"Doctor: ", ", 0 failed,", "ghostbranch feature start <name>", "ghostbranch gate <name>", "ghostbranch promote <name>",
		"GHOSTBRANCH_TOKEN"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(hook); err != nil {
		t.Fatalf("no guard: %v", err)
	}
	if !slices.ContainsFunc(p.said, func(s string) bool { return strings.HasPrefix(s, "This will:") }) {
		t.Fatalf("no summary: %q", p.said)
	}
}

func TestGuidedInitAdoptsAShadowRepoGitHubWouldNotCreate(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.CreateForbidden = true
	p := atTerminal(te, "", "", "", "")
	p.onWait = func() {
		if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
			t.Error(err)
		}
	}
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
		"Press Enter once acme/widget-shadow exists.")
	if !slices.ContainsFunc(p.said, func(s string) bool {
		return strings.Contains(s, "https://github.com/new?owner=acme&name=widget-shadow&visibility=private")
	}) {
		t.Fatalf("no link: %q", p.said)
	}
	if pairs := registered(te); len(pairs) != 1 || pairs[0].Shadow != "acme/widget-shadow" {
		t.Fatalf("pairs %+v", pairs)
	}
}

func TestGuidedInitStopsWhenTheLoginIsDeclined(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	p := atTerminal(te, "n")
	code, _, errOut := te.run("init", "acme/widget")
	if code != 1 || !strings.Contains(errOut, "[AUTH_MISSING]") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	hasPrefixes(t, p.asked, "No GitHub login found. Log in with GitHub now?")
	if !slices.ContainsFunc(p.said, func(s string) bool { return strings.Contains(s, "ghostbranch login --with-token") }) || shadowExists(te) {
		t.Fatalf("said %q; shadow made %v", p.said, shadowExists(te))
	}
}

func TestGuidedInitOutsideACloneHasNoGuardQuestion(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t) // GuardState: not a clone
	p := atTerminal(te, "", "", "", "")
	if code, out, errOut := te.run("init"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Which public repo should get private branches? (owner/repo)", "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?")
}

func TestGuidedInitFlagsPreAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		args  []string
		asked []string
	}{
		{"every answer as a flag asks nothing", []string{"init", "acme/widget", "--shadow", "acme/widget-shadow", "--no-guard", "--no-sync"}, nil},
		{"--no-guard and --sync", []string{"init", "acme/widget", "--no-guard", "--sync"}, []string{"Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Make these changes now?"}},
		{"--adopt answers the shadow question", []string{"init", "acme/widget", "--adopt", "--no-sync"}, []string{"Make these changes now?"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if slices.Contains(tc.args, "--adopt") {
				if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
					t.Fatal(err)
				}
			}
			p := atTerminal(te, "", "", "")
			if code, out, errOut := te.run(tc.args...); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			hasPrefixes(t, p.asked, tc.asked...)
			if slices.Contains(tc.args, "--sync") {
				raw, _, err := gitx.New(te.forge.RepoDir("acme/widget-shadow")).ShowFile(context.Background(), "refs/heads/ghostbranch", ".ghostbranch.yml")
				if err != nil || !strings.Contains(string(raw), `schedule: "23 */6 * * *"`) {
					t.Fatalf("config:\n%s %v", raw, err)
				}
			}
		})
	}
}

func TestGuidedInitResumes(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te) // registered, overlay made: shadow and sync are answered
	p := atTerminal(te, "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Make these changes now?")
	if !slices.ContainsFunc(p.said, func(s string) bool { return strings.HasPrefix(s, "Resuming the setup of acme/widget") }) {
		t.Fatalf("said %q", p.said)
	}
}

// Ctrl-C at any question stops init before anything changes.
func TestGuidedInitCtrlCAtEachQuestion(t *testing.T) {
	t.Parallel()
	for n := 1; n <= 4; n++ { // public repo, create, sync, proceed
		t.Run(fmt.Sprintf("question %d", n), func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			p := atTerminal(te, "", "", "", "")
			p.interruptAt = n
			code, _, errOut := te.run("init")
			if code != 1 || !strings.Contains(errOut, "[INTERRUPTED]") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if len(p.asked) != n || shadowExists(te) || len(registered(te)) != 0 {
				t.Fatalf("asked %q; shadow made %v; pairs %v", p.asked, shadowExists(te), registered(te))
			}
		})
	}
	t.Run("the guard question", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.env.GuardState = func(string) (bool, bool) { return true, false }
		p := atTerminal(te, "", "", "")
		p.interruptAt = 3 // public repo, create, guard
		if code, _, errOut := te.run("init"); code != 1 || !strings.Contains(errOut, "[INTERRUPTED]") || shadowExists(te) {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	})
	t.Run("waiting for a repo made by hand", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		te.forge.CreateForbidden = true
		p := atTerminal(te, "", "", "", "")
		p.interruptAt = 4 // create, sync, proceed, press Enter
		if code, _, errOut := te.run("init", "acme/widget"); code != 1 || !strings.Contains(errOut, "[INTERRUPTED]") || len(registered(te)) != 0 {
			t.Fatalf("exit %d: %s", code, errOut)
		}
	})
}

// Review Focus 2: no console, --json, --non-interactive or --yes: init asks
// nothing, opens no console, and never reads stdin, so piped "y"s answer
// nothing.
func TestGuidedInitNeedsAConsole(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		console bool
		args    []string
	}{
		{"no console", false, []string{"init", "acme/widget"}},
		{"--json", true, []string{"init", "acme/widget", "--json"}},
		{"--non-interactive", true, []string{"init", "acme/widget", "--non-interactive"}},
		{"--yes", true, []string{"init", "acme/widget", "--yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			const piped = "y\ny\ny\ny\ny\n"
			in := strings.NewReader(piped)
			te.env.Stdin = in
			te.env.ConsoleUsable = tc.console
			te.env.Asker = func() (prompt.Asker, error) {
				t.Error("init opened the console")
				return nil, errors.New("no console")
			}
			if code, out, errOut := te.run(tc.args...); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			if in.Len() != len(piped) {
				t.Fatal("init read stdin")
			}
		})
	}
}

func TestInitGuardFlagsAreChecked(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"init", "acme/widget", "--guard"}, // not a clone of acme/widget
		{"init", "acme/widget", "--sync", "--no-sync"},
		{"init", "acme/widget", "--guard", "--no-guard"},
	} {
		te := newTestEnv(t)
		code, env := te.runJSON(args...)
		e, _ := env["error"].(map[string]any)
		if code != 2 || e["code"] != "BAD_FLAG" || shadowExists(te) {
			t.Fatalf("%v: exit %d %v", args, code, env)
		}
	}
}

// Answering yes to the login question runs the device flow on the asker,
// stores the login, and init goes on with it.
func TestGuidedInitLogsInFirst(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	srv := authtest.New(t, authtest.Options{Pending: 1})
	te.env.ClientID, te.env.OAuthBaseURL, te.env.HTTPClient = srv.ClientID, srv.URL, srv.Client()
	p := atTerminal(te, "y", "", "", "", "")
	p.onWait = func() { installApp(te) }
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "No GitHub login found. Log in with GitHub now?", installed, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?",
		"Turn on scheduled sync?", "Make these changes now?")
	if !slices.ContainsFunc(p.said, func(s string) bool { return strings.Contains(s, authtest.UserCode) }) {
		t.Fatalf("no device code said: %q", p.said)
	}
	if strings.Contains(out+errOut, authtest.UserCode) {
		t.Fatalf("the code left the console:\n%s\n%s", out, errOut)
	}
	if l, err := auth.LoadLogin(te.env.Keychain); err != nil || l == nil || l.Token != srv.AccessToken(1) {
		t.Fatalf("stored login %v %v", l, err)
	}
	if pairs := registered(te); len(pairs) != 1 || !shadowExists(te) {
		t.Fatalf("pairs %+v", pairs)
	}
}

// saidHas reports whether p said a line containing want.
func saidHas(p *person, want string) bool {
	return slices.ContainsFunc(p.said, func(s string) bool { return strings.Contains(s, want) })
}

// Creating the shadow repo by hand: init waits until it exists and is
// private, saying what is wrong at each Enter, and adopts it only then.
func TestGuidedInitWaitsForAPrivateHandMadeShadowRepo(t *testing.T) {
	t.Parallel()
	const shadow = "acme/widget-shadow"
	for _, tc := range []struct {
		name, line string
		first      func(te *testEnv) // at the first Enter
		second     func(te *testEnv) // at the second
	}{
		{"created public, then made private", shadow + " is public; make it private in its settings.",
			func(te *testEnv) { _, _ = te.forge.AddRepo(shadow, false) },
			func(te *testEnv) { te.forge.SetPrivate(shadow, true) }},
		{"not created yet, then created", shadow + " isn't there yet, or your login can't see it.",
			func(*testEnv) {},
			func(te *testEnv) { _, _ = te.forge.AddRepo(shadow, true) }},
		{"a fork, then replaced", shadow + " is a fork; a shadow repo can't be one.",
			func(te *testEnv) { _, _ = te.forge.AddRepo(shadow, true); te.forge.SetFork(shadow, true) },
			func(te *testEnv) { te.forge.SetFork(shadow, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			te.forge.CreateForbidden = true
			p := atTerminal(te, "", "", "", "", "")
			waits := 0
			p.onWait = func() {
				waits++
				switch waits {
				case 1:
					tc.first(te)
				case 2:
					if len(registered(te)) != 0 {
						t.Error("adopted before the second Enter")
					}
					tc.second(te)
				}
			}
			code, out, errOut := te.run("init", "acme/widget")
			if code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
				"Press Enter once acme/widget-shadow exists.", "Press Enter once acme/widget-shadow exists.")
			if !saidHas(p, tc.line) {
				t.Fatalf("never said %q: %q", tc.line, p.said)
			}
			if pairs := registered(te); len(pairs) != 1 || pairs[0].Shadow != shadow {
				t.Fatalf("pairs %+v", pairs)
			}
		})
	}
}

// I8 (anti-goal 2): a hand-made shadow repo still not right at the second
// check stops init with exactly what is still wrong, and the adopt that
// finishes it; there is no third Enter.
func TestCreateByHandStopsAfterSecondFailedCheck(t *testing.T) {
	t.Parallel()
	const shadow = "acme/widget-shadow"
	for _, tc := range []struct {
		name, wrong string
		first       func(te *testEnv)
	}{
		{"never created", shadow + " isn't there yet, or your login can't see it.", func(*testEnv) {}},
		{"left public", shadow + " is public; make it private in its settings.", func(te *testEnv) { _, _ = te.forge.AddRepo(shadow, false) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			te.forge.CreateForbidden = true
			p := atTerminal(te, "", "", "", "", "", "")
			waits := 0
			p.onWait = func() {
				if waits++; waits == 1 {
					tc.first(te)
				}
			}
			code, _, errOut := te.run("init", "acme/widget")
			if code != 1 || !strings.Contains(errOut, "[SHADOW_CREATE_DENIED]") {
				t.Fatalf("exit %d: %s", code, errOut)
			}
			if waits != 2 {
				t.Fatalf("%d Enters, want 2", waits)
			}
			for _, want := range []string{shadow + " was checked twice and still isn't ready: " + tc.wrong,
				"run ghostbranch init acme/widget --adopt --shadow " + shadow} {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr lacks %q:\n%s", want, errOut)
				}
			}
			if len(registered(te)) != 0 {
				t.Fatalf("pairs %+v", registered(te))
			}
		})
	}
}

// flakyForge fails the next GetRepo of one repo as GitHub being unreachable.
type flakyForge struct {
	forge.Forge
	repo string
	fail *atomic.Bool
}

func (f flakyForge) GetRepo(ctx context.Context, full string) (forge.Repo, error) {
	if full == f.repo && f.fail.CompareAndSwap(true, false) {
		return forge.Repo{}, fmt.Errorf("get %s: %w", full, forge.ErrTransient)
	}
	return f.Forge.GetRepo(ctx, full)
}

// A network blip while waiting for a hand-made shadow repo asks for Enter
// again instead of ending init.
func TestGuidedInitRetriesAHandMadeShadowRepoLookup(t *testing.T) {
	t.Parallel()
	const shadow = "acme/widget-shadow"
	te := newTestEnv(t)
	te.forge.CreateForbidden = true
	var fail atomic.Bool
	te.env.NewForge = func(string) (forge.Forge, error) { return flakyForge{te.forge, shadow, &fail}, nil }
	p := atTerminal(te, "", "", "", "", "")
	waits := 0
	p.onWait = func() {
		waits++
		if waits == 1 {
			_, _ = te.forge.AddRepo(shadow, true)
			fail.Store(true)
		}
	}
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if waits != 2 || !saidHas(p, "couldn't reach GitHub; press Enter to try again") {
		t.Fatalf("%d waits, said %q", waits, p.said)
	}
	if pairs := registered(te); len(pairs) != 1 || pairs[0].Shadow != shadow {
		t.Fatalf("pairs %+v", pairs)
	}
}

// A fork can't be a shadow repo: askShadow says so and asks for another.
func TestGuidedInitRefusesAForkAsTheShadowRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
		t.Fatal(err)
	}
	te.forge.SetFork("acme/widget-shadow", true)
	p := atTerminal(te, "acme/other-shadow", "", "", "")
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Which repo should be the private shadow repo instead?", "Create the private repo acme/other-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?")
	if !saidHas(p, "acme/widget-shadow is a fork; a shadow repo can't be one.") {
		t.Fatalf("said %q", p.said)
	}
	if pairs := registered(te); len(pairs) != 1 || pairs[0].Shadow != "acme/other-shadow" {
		t.Fatalf("pairs %+v", pairs)
	}
}

// Task 10: where the shadow repo of an organization's public repo lives.

const installLink = "https://github.com/apps/ghostbranch-cli/installations/new"

// atOrgTerminal is atTerminal with acme an organization; the login is octo.
func atOrgTerminal(te *testEnv, answers ...string) *person {
	p := atTerminal(te, answers...)
	te.forge.Orgs = map[string]bool{"acme": true}
	te.forge.User.Login = "octo"
	return p
}

// appLogin makes the run's token a stored app login (ghu_) instead of
// GHOSTBRANCH_TOKEN.
func appLogin(te *testEnv) {
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	storeLogin(te, "ghu_test", "ghr_test", time.Hour)
}

// GitHub's message when an app login creates a repo on an account the app
// isn't installed on.
const notInstalledMsg = "Resource not accessible by integration"

// refuseIn has GitHub refuse to create repos under these owners, saying msg.
func refuseIn(te *testEnv, msg string, owners ...string) {
	te.forge.CreateRefused = map[string]string{}
	for _, o := range owners {
		te.forge.CreateRefused[o] = msg
	}
}

func wantPair(t *testing.T, te *testEnv, shadow string) {
	t.Helper()
	if pairs := registered(te); len(pairs) != 1 || pairs[0].Shadow != shadow {
		t.Fatalf("pairs %+v, want shadow %s", pairs, shadow)
	}
	if _, err := te.forge.GetRepo(context.Background(), shadow); err != nil {
		t.Fatalf("shadow %s: %v", shadow, err)
	}
}

const whereQ = "Which one? (1, 2 or 3)"

func TestGuidedInitAsksWhereAnOrgsShadowLives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	p := atOrgTerminal(te, "", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?")
	for _, want := range []string{"Where should the private shadow repo live?",
		"  1. acme/widget-shadow   (the org; teammates can use it)   [default]",
		"  2. octo/widget-shadow   (your account; only you)",
		"  3. another repo name, or an existing repo to adopt"} {
		if !slices.Contains(p.said, want) {
			t.Errorf("never said %q: %q", want, p.said)
		}
	}
	if saidHas(p, installLink) {
		t.Errorf("a token login was shown the app's install link: %q", p.said)
	}
	wantPair(t, te, "acme/widget-shadow")
}

func TestGuidedInitPutsTheShadowOnThePersonsAccount(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	p := atOrgTerminal(te, "4", "2", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, whereQ, "Create the private repo octo/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?")
	if !saidHas(p, "Answer 1, 2 or 3.") {
		t.Fatalf("said %q", p.said)
	}
	wantPair(t, te, "octo/widget-shadow")
	if shadowExists(te) {
		t.Fatal("the org's shadow was made too")
	}
}

func TestGuidedInitAdoptsANamedShadowRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("octo/private-mirror", true); err != nil {
		t.Fatal(err)
	}
	p := atOrgTerminal(te, "3", "octo/private-mirror", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Which repo should be the private shadow repo? (owner/repo", "octo/private-mirror already exists. Adopt it", "Turn on scheduled sync?", "Make these changes now?")
	wantPair(t, te, "octo/private-mirror")
}

func TestGuidedInitRefusesAPublicNamedShadowRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("octo/open", false); err != nil {
		t.Fatal(err)
	}
	p := atOrgTerminal(te, "3", "octo/open", "octo/closed", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Which repo should be the private shadow repo? (owner/repo", "Which repo should be the private shadow repo instead?", "Create the private repo octo/closed to hold your private branches (the shadow repo)?",
		"Turn on scheduled sync?", "Make these changes now?")
	if !saidHas(p, "octo/open is public; a shadow repo must be private.") {
		t.Fatalf("said %q", p.said)
	}
	wantPair(t, te, "octo/closed")
}

// No question for a public repo on a personal account, nor with --shadow.
func TestGuidedInitAsksWhereOnlyForAnOrgWithoutShadow(t *testing.T) {
	t.Parallel()
	t.Run("personal public repo", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t) // acme is a user here
		p := atTerminal(te, "", "", "")
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?")
		if saidHas(p, "Where should the private shadow repo live?") {
			t.Fatalf("said %q", p.said)
		}
	})
	t.Run("--shadow", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		p := atOrgTerminal(te, "", "")
		if code, out, errOut := te.run("init", "acme/widget", "--shadow", "octo/widget-shadow"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		hasPrefixes(t, p.asked, "Turn on scheduled sync?", "Make these changes now?")
		if !saidHas(p, "octo/widget-shadow") {
			t.Fatalf("the summary doesn't name the shadow: %q", p.said)
		}
		wantPair(t, te, "octo/widget-shadow")
	})
}

func TestGuidedInitOffersThePersonalAccountWhenTheOrgRefuses(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	refuseIn(te, "Repository creation failed.", "acme")
	p := atOrgTerminal(te, "", "", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
		"Use octo/widget-shadow instead?")
	wantPair(t, te, "octo/widget-shadow")
	if shadowExists(te) {
		t.Fatal("the org's shadow exists")
	}
}

func TestGuidedInitDeclinedPersonalAccountMakesTheOrgsByHand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	refuseIn(te, "Repository creation failed.", "acme")
	p := atOrgTerminal(te, "", "", "", "", "n", "")
	p.onWait = func() { _, _ = te.forge.AddRepo("acme/widget-shadow", true) }
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
		"Use octo/widget-shadow instead?", "Press Enter once acme/widget-shadow exists.")
	wantPair(t, te, "acme/widget-shadow")
}

// The org and the personal account both refuse: with an app login, init
// says the app isn't installed on the account and gives its install link;
// either way it waits for the personal repo made by hand.
func TestGuidedInitPersonalAccountRefusedToo(t *testing.T) {
	t.Parallel()
	for _, app := range []bool{true, false} {
		t.Run(map[bool]string{true: "app login", false: "token"}[app], func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if app {
				appLogin(te)
			}
			refuseIn(te, notInstalledMsg, "acme", "octo")
			te.forge.Orgs = map[string]bool{"acme": true}
			installOn(te, "acme") // the check passes; GitHub's refusals it didn't predict
			// An app login waits once for the app's install on each account
			// before the next way on.
			p := atOrgTerminal(te, "", "", "", "", "", "", "", "", "")
			p.onWait = func() { _, _ = te.forge.AddRepo("octo/widget-shadow", true) }
			if app {
				p.onWait = func() {
					if slices.Contains(p.asked, "Press Enter once octo/widget-shadow exists.") {
						_, _ = te.forge.AddRepo("octo/widget-shadow", true)
					}
				}
			}
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			asked := []string{whereQ, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
				"Use octo/widget-shadow instead?", "Press Enter once octo/widget-shadow exists."}
			if app {
				w := "Press Enter once it's installed."
				asked = slices.Concat(asked[:4], []string{w}, asked[4:5], []string{w}, asked[5:])
			}
			hasPrefixes(t, p.asked, asked...)
			if !slices.Contains(p.said, `GitHub refused to create octo/widget-shadow on octo. GitHub said: "`+notInstalledMsg+`"`) {
				t.Fatalf("never said the personal account refused: %q", p.said)
			}
			const notInstalled = "  - the ghostbranch-cli GitHub App isn't installed on octo: install it at " + installLink
			if got := slices.Contains(p.said, notInstalled); got != app {
				t.Fatalf("app %v, said the install link %v: %q", app, got, p.said)
			}
			if !saidHas(p, "GitHub didn't let your login create octo/widget-shadow. Create it yourself:") ||
				!saidHas(p, "https://github.com/new?owner=octo&name=widget-shadow&visibility=private") {
				t.Fatalf("no create-by-hand steps for the personal repo: %q", p.said)
			}
			wantPair(t, te, "octo/widget-shadow")
		})
	}
}

func TestGuidedInitCtrlCAtWhereTheShadowLives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	p := atOrgTerminal(te, "", "", "", "")
	p.interruptAt = 1
	code, _, errOut := te.run("init", "acme/widget")
	if code != 1 || !strings.Contains(errOut, "[INTERRUPTED]") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(p.asked) != 1 || !strings.HasPrefix(p.asked[0], whereQ) || shadowExists(te) || len(registered(te)) != 0 {
		t.Fatalf("asked %q; shadow made %v; pairs %v", p.asked, shadowExists(te), registered(te))
	}
	if _, err := te.forge.GetRepo(context.Background(), "octo/widget-shadow"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("personal shadow: %v", err)
	}
}

// An app login only reaches repos on accounts where the app is installed:
// the texts say so and give the install link. A token login keeps the
// plain texts.
func TestGuidedInitAppLoginTexts(t *testing.T) {
	t.Parallel()
	const (
		option2   = "     For 2, the ghostbranch-cli GitHub App must be installed on your account (octo): " + installLink
		offer     = "For that, the ghostbranch-cli GitHub App must be installed on your account (octo): " + installLink
		step2     = "  2. Give Ghostbranch access to it: install the ghostbranch-cli GitHub App on acme and pick both acme/widget and acme/widget-shadow (publishing needs acme/widget), or add them to its repositories, at " + installLink
		notMade   = "- If you haven't created acme/widget-shadow yet: create it at https://github.com/new?owner=acme&name=widget-shadow&visibility=private."
		cantSee   = "- If you have: the Ghostbranch app can't see it yet. Add acme/widget-shadow (and acme/widget) to the app's repositories at " + installLink + "."
		notYetPAT = "acme/widget-shadow isn't there yet, or your login can't see it."
	)
	step2PAT := "  2. Give your token access to it: add acme/widget-shadow (and acme/widget) to its repositories, or make a fine-grained token at " +
		forge.TokenLink("ghostbranch-widget", "Ghostbranch: acme/widget and acme/widget-shadow", "acme", 90, forge.PersonTokenLinkPermissions) +
		` (choose "Only select repositories", then pick acme/widget and acme/widget-shadow), then set the new token as GHOSTBRANCH_TOKEN and run ghostbranch init again.`
	for _, app := range []bool{true, false} {
		t.Run(map[bool]string{true: "app login", false: "token"}[app], func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if app {
				appLogin(te)
			}
			refuseIn(te, notInstalledMsg, "acme")
			te.forge.Orgs = map[string]bool{"acme": true}
			installOn(te, "acme") // the check passes; GitHub's refusals it didn't predict
			// Org shadow refused (an app login waits once for the install),
			// personal declined, by hand: not there at the first Enter, there
			// at the second.
			answers := []string{"", "", "", "", "n", "", ""}
			byHand := 2
			if app {
				answers = []string{"", "", "", "", "", "n", "", ""}
				byHand = 3
			}
			p := atOrgTerminal(te, answers...)
			waits := 0
			p.onWait = func() {
				if waits++; waits == byHand {
					_, _ = te.forge.AddRepo("acme/widget-shadow", true)
				}
			}
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			for _, tc := range []struct {
				line string
				want bool
			}{{option2, app}, {offer, app}, {step2, app}, {notMade, app}, {cantSee, app}, {step2PAT, !app}, {notYetPAT, !app}} {
				if slices.Contains(p.said, tc.line) != tc.want {
					t.Errorf("said %q: %v, want %v (all: %q)", tc.line, !tc.want, tc.want, p.said)
				}
			}
			wantPair(t, te, "acme/widget-shadow")
		})
	}
}

// A refused creation says which account refused and why: GitHub's message,
// then the likely causes for the case, each with its fix.
func TestGuidedInitSaysWhyCreationWasRefused(t *testing.T) {
	t.Parallel()
	const (
		appMissing = "  - the ghostbranch-cli GitHub App isn't installed on %s: install it at " + installLink
		orgPolicy  = "  - acme's settings may not let members create private repos: ask an org owner to create acme/widget-shadow (%s), or choose your personal account"
		tokenLacks = "  - your token may lack Administration: write (fine-grained) or the repo scope (classic) on acme: ghostbranch doctor shows what's missing"
	)
	for _, tc := range []struct {
		name, msg, shadow string
		app, org          bool
		args, answers     []string
		want, never       []string
	}{
		{name: "org, app not installed", msg: notInstalledMsg, shadow: "acme/widget-shadow", app: true, org: true,
			answers: []string{"", "", "", "", "", "n", ""},
			want: []string{`GitHub refused to create acme/widget-shadow on acme. GitHub said: "Resource not accessible by integration"`,
				"Likely cause:", fmt.Sprintf(appMissing, "acme")},
			never: []string{fmt.Sprintf(orgPolicy, "then add it to the app"), tokenLacks}},
		{name: "org policy", msg: "Repository creation failed.", shadow: "acme/widget-shadow", app: true, org: true,
			answers: []string{"", "", "", "", "n", ""},
			want: []string{`GitHub refused to create acme/widget-shadow on acme. GitHub said: "Repository creation failed."`,
				"Likely cause:", fmt.Sprintf(orgPolicy, "then add it to the app")},
			never: []string{fmt.Sprintf(appMissing, "acme"), tokenLacks}},
		{name: "personal account, app not installed", msg: notInstalledMsg, shadow: "octo/widget-shadow", app: true,
			args: []string{"--shadow", "octo/widget-shadow"}, answers: []string{"", "", "", ""},
			want: []string{`GitHub refused to create octo/widget-shadow on octo. GitHub said: "Resource not accessible by integration"`,
				"Likely cause:", fmt.Sprintf(appMissing, "octo")}},
		{name: "token", msg: "Resource not accessible by personal access token", shadow: "acme/widget-shadow", org: true,
			answers: []string{"", "", "", "", "n", ""},
			want: []string{`GitHub refused to create acme/widget-shadow on acme. GitHub said: "Resource not accessible by personal access token"`,
				"Likely causes:", tokenLacks, fmt.Sprintf(orgPolicy, "then give your token access to it")},
			never: []string{fmt.Sprintf(appMissing, "acme")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.app {
				appLogin(te)
			}
			owner, _, _ := forge.SplitName(tc.shadow)
			refuseIn(te, tc.msg, owner)
			p := atTerminal(te, tc.answers...)
			if tc.org {
				te.forge.Orgs = map[string]bool{"acme": true}
				te.forge.User.Login = "octo"
			}
			installOn(te, "acme", "octo") // the check passes; GitHub's refusals it didn't predict
			// The repo appears at the create-by-hand Enter, after any waits
			// for the app's install.
			p.onWait = func() {
				if slices.Contains(p.asked, "Press Enter once "+tc.shadow+" exists.") {
					_, _ = te.forge.AddRepo(tc.shadow, true)
				}
			}
			if code, out, errOut := te.run(append([]string{"init", "acme/widget"}, tc.args...)...); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			for _, l := range tc.want {
				if !slices.Contains(p.said, l) {
					t.Errorf("never said %q (all: %q)", l, p.said)
				}
			}
			for _, l := range tc.never {
				if slices.Contains(p.said, l) {
					t.Errorf("said %q", l)
				}
			}
			if strings.Contains(strings.Join(p.said, "\n"), "ghu_test") {
				t.Fatal("the token was said")
			}
			wantPair(t, te, tc.shadow)
		})
	}
}

// Choice 3 can't name the public repo itself: init says so and asks again.
func TestGuidedInitRefusesThePublicRepoAsTheShadow(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	p := atOrgTerminal(te, "3", "ACME/widget", "octo/other", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Which repo should be the private shadow repo? (owner/repo", "Which repo should be the private shadow repo? (owner/repo", "Create the private repo octo/other to hold your private branches (the shadow repo)?",
		"Turn on scheduled sync?", "Make these changes now?")
	if !slices.Contains(p.said, "ACME/widget is the public repo; the shadow repo must be a different repo from the public one.") {
		t.Fatalf("said %q", p.said)
	}
	wantPair(t, te, "octo/other")
}

// A shadow repo named by --shadow, or by a registered pair, is the one the
// person wants: a refusal goes straight to making that repo by hand.
func TestGuidedInitOffersNoOtherAccountForANamedShadow(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		args  []string
		asked []string
	}{
		{"--shadow", []string{"--shadow", "acme/widget-shadow"}, []string{"Turn on scheduled sync?", "Make these changes now?", "Press Enter once acme/widget-shadow exists."}},
		{"registered pair", nil, []string{"Make these changes now?", "Press Enter once acme/widget-shadow exists."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.args == nil {
				ws, err := workspace.Open(te.vars["GHOSTBRANCH_HOME"], "default")
				if err != nil {
					t.Fatal(err)
				}
				ws.AddPair(workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"})
				if err := ws.Save(); err != nil {
					t.Fatal(err)
				}
			}
			refuseIn(te, "Repository creation failed.", "acme")
			p := atOrgTerminal(te, "", "", "")
			p.onWait = func() { _, _ = te.forge.AddRepo("acme/widget-shadow", true) }
			if code, out, errOut := te.run(append([]string{"init", "acme/widget"}, tc.args...)...); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			hasPrefixes(t, p.asked, tc.asked...)
			wantPair(t, te, "acme/widget-shadow")
		})
	}
}

// The org refuses and <login>/<repo>-shadow already exists, private and
// empty: taking the offer adopts it.
func TestGuidedInitAdoptsAnExistingPersonalShadowAfterARefusal(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("octo/widget-shadow", true); err != nil {
		t.Fatal(err)
	}
	refuseIn(te, "Repository creation failed.", "acme")
	p := atOrgTerminal(te, "", "", "", "", "y", "")
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create the private repo acme/widget-shadow to hold your private branches (the shadow repo)?", "Turn on scheduled sync?", "Make these changes now?",
		"Use octo/widget-shadow instead?", "octo/widget-shadow already exists. Adopt it")
	wantPair(t, te, "octo/widget-shadow")
	if shadowExists(te) {
		t.Fatal("the org's shadow exists")
	}
}

// Guided init opens with what it does and how defaults work, once, before
// its first question, whether or not the public repo is asked for.
func TestGuidedInitIntroducesItselfOnce(t *testing.T) {
	t.Parallel()
	intro := []string{
		"! Ghostbranch will set up private branches for a public GitHub repo.",
		"! It asks a few questions. Press Enter to accept the suggestion in [brackets].",
	}
	for _, args := range [][]string{{"init"}, {"init", "acme/widget"}, {"init", "acme/widget", "--shadow", "acme/widget-shadow", "--no-sync"}} {
		te := newTestEnv(t)
		p := atTerminal(te, "", "", "", "")
		if code, out, errOut := te.run(args...); code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", args, code, out, errOut)
		}
		if len(p.log) < 3 || p.log[0] != intro[0] || p.log[1] != intro[1] {
			t.Fatalf("%v: the run doesn't open with the intro: %q", args, p.log)
		}
		for _, l := range intro {
			if n := slices.Index(p.log[2:], l); n >= 0 {
				t.Fatalf("%v: %q said again: %q", args, l, p.log)
			}
		}
		if len(args) == 1 && p.asked[0] != "Which public repo should get private branches? (owner/repo)" {
			t.Fatalf("first question %q", p.asked[0])
		}
	}
}

// An empty public repo is refused by the summary's preview, before the
// confirm question: nothing is created, and next suggests nothing.
func TestGuidedInitRefusesAnEmptyPublicRepoBeforeTheConfirm(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("acme/empty", false); err != nil {
		t.Fatal(err)
	}
	p := atTerminal(te, "", "", "")
	code, _, errOut := te.run("init", "acme/empty")
	if code != 2 || !strings.Contains(errOut, "acme/empty has no commits on main yet: push a first commit (a README is enough), then run ghostbranch init again") ||
		!strings.Contains(errOut, "[BAD_FLAG]") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/empty-shadow", "Turn on scheduled sync?")
	if _, err := te.forge.GetRepo(context.Background(), "acme/empty-shadow"); !errors.Is(err, forge.ErrNotFound) || len(registered(te)) != 0 {
		t.Fatalf("shadow %v, pairs %v", err, registered(te))
	}
	te2 := newTestEnv(t)
	if _, err := te2.forge.AddRepo("acme/empty", false); err != nil {
		t.Fatal(err)
	}
	code, env := te2.runJSON("init", "acme/empty")
	e, _ := env["error"].(map[string]any)
	if next, _ := env["next"].([]any); code != 2 || e["code"] != "BAD_FLAG" || e["field"] != "public" || e["retryable"] == true || len(next) != 0 {
		t.Fatalf("exit %d %v", code, env)
	}
}

// A run that left an empty private <public>-shadow behind, unregistered, is
// picked up again: guided init offers to adopt it.
func TestGuidedInitAdoptsALeftoverEmptyShadow(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
		t.Fatal(err)
	}
	p := atTerminal(te, "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "acme/widget-shadow already exists. Adopt it", "Turn on scheduled sync?", "Make these changes now?")
	wantPair(t, te, "acme/widget-shadow")
}

// 8.7: guided init works out the setup checklist after the repo question,
// shows it, and waits for each step a person does, checking it again.

const (
	doneQ       = "Press Enter once it's done."
	installStep = "Install the Ghostbranch app on acme and pick acme/widget" // the checklist's step
	noGHHint    = "To stop Ghostbranch using gh's login, add GHOSTBRANCH_NO_GH=1 to your shell profile."
)

// installOn installs the Ghostbranch app on these accounts, for every repo.
func installOn(te *testEnv, accounts ...string) {
	te.forge.Installations = nil
	for i, a := range accounts {
		typ := forge.OwnerUser
		if te.forge.Orgs[a] {
			typ = forge.OwnerOrganization
		}
		te.forge.Installations = append(te.forge.Installations,
			forge.Installation{ID: int64(7 + i), Account: a, AccountType: typ, Selection: "all"})
	}
}

// The app isn't installed: the install step comes before any create, and
// once it is done the shadow repo is created once, with no refusal.
func TestGuidedInitInstallBeforeCreate(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	p := atTerminal(te, "", "", "", "")
	var createdAtWait []string
	p.onWait = func() {
		createdAtWait = slices.Clone(te.forge.Created)
		installApp(te)
	}
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, installed, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?")
	if !before(p, "! ! Install the Ghostbranch app on acme", "? "+installed) || !slices.Contains(p.said, "    1. Open "+installLink) {
		t.Fatalf("no install step before the wait: %q", p.log)
	}
	if createdAtWait == nil {
		createdAtWait = []string{}
	}
	if len(createdAtWait) != 0 || !slices.Equal(te.forge.Created, []string{"acme/widget-shadow"}) {
		t.Fatalf("created %v at the wait, %v in all", createdAtWait, te.forge.Created)
	}
	if saidHas(p, "GitHub refused") || !strings.Contains(errOut, "Created the private repo acme/widget-shadow") {
		t.Fatalf("said %q\n%s", p.said, errOut)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// The install never appears: two waits, each checked again, then the stop's
// code with what is still wrong. It never asks a third time.
func TestGuidedInitStopsAfterSecondFailedRecheck(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	p := atTerminal(te, "", "", "")
	code, _, errOut := te.run("init", "acme/widget")
	if code != 2 || !strings.Contains(errOut, "[APP_NOT_INSTALLED]") ||
		!strings.Contains(errOut, "error: Install the Ghostbranch app on acme and pick acme/widget.") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	waits := 0
	for _, q := range p.asked {
		if strings.HasPrefix(q, "Press Enter") {
			waits++
		}
	}
	hasPrefixes(t, p.asked, installed, installed)
	if waits != 2 || len(te.forge.Created) != 0 || len(registered(te)) != 0 {
		t.Fatalf("%d waits; created %v", waits, te.forge.Created)
	}
}

// Everything already in place (the app installed, the pair on GitHub, init
// --adopt on a new machine): the questions are v0.1.1's for the same run,
// pinned here.
func TestGuidedInitAllDoneAsksNothingNew(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.vars["GHOSTBRANCH_HOME"] = filepath.Join(te.root, "home-2") // another machine
	appLogin(te)
	p := atTerminal(te, "")
	installApp(te)
	if code, out, errOut := te.run("init", "acme/widget", "--adopt"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	// v0.1.1 (ba53e6a) asks exactly this for the same inputs.
	if v011 := []string{"Make these changes now?"}; !slices.Equal(p.asked, v011) {
		t.Fatalf("asked %q, want v0.1.1's %q", p.asked, v011)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// Choosing the app over gh's login says, once, how to turn gh's login off.
func TestGuidedInitNoGHHintAfterApp(t *testing.T) {
	t.Parallel()
	for _, answer := range []string{"app", "gh"} {
		t.Run(answer, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			delete(te.vars, "GHOSTBRANCH_TOKEN")
			te.env.GHToken = func() (string, error) { return "gh-cli-token", nil }
			initDeviceLogin(t, te)
			tokenUsers(te, "acme", nil)
			p := atTerminal(te, answer, "", "", "")
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			n := 0
			for _, s := range p.said {
				if s == noGHHint {
					n++
				}
			}
			if want := map[string]int{"app": 1, "gh": 0}[answer]; n != want {
				t.Fatalf("said the hint %d times, want %d: %q", n, want, p.said)
			}
		})
	}
}

// GHOSTBRANCH_TOKEN of another account: no login can help, so init stops
// with TOKEN_WRONG_ACCOUNT, and the message gives the --shadow way on.
func TestGuidedInitGhostbranchTokenMismatchOffersShadow(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("acme/trial", false); err != nil {
		t.Fatal(err)
	}
	p := atTerminal(te)
	te.forge.User.Login = "ayushguptax"
	code, _, errOut := te.run("init", "acme/trial")
	if code != 2 || !strings.Contains(errOut, "[TOKEN_WRONG_ACCOUNT]") ||
		!strings.Contains(errOut, "--shadow ayushguptax/trial-shadow") || !strings.Contains(errOut, "set GHOSTBRANCH_TOKEN to a token of acme's") {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if len(p.asked) != 0 || len(te.forge.Created) != 0 {
		t.Fatalf("asked %q, created %v", p.asked, te.forge.Created)
	}
}

// The check passes, yet GitHub refuses the create because the app isn't
// installed: 8.6's install-then-retry still runs.
func TestGuidedInitUnpredictedRefusalStillRetries(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	installApp(te)
	refuseIn(te, notInstalledMsg, "acme")
	p := atTerminal(te, "", "", "", "")
	p.onWait = func() { te.forge.CreateRefused = nil }
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "Create the private repo acme/widget-shadow", "Turn on scheduled sync?", "Make these changes now?", installed)
	if !slices.Contains(p.said, notInstalledLine) {
		t.Fatalf("said %q", p.said)
	}
	wantPair(t, te, "acme/widget-shadow")
}

// An org's public repo with the shadow put on the person's account: that
// account needs the app too, so the checklist is checked again for the
// shadow chosen, before any create.
func TestGuidedInitChecksAgainForAPersonalShadow(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	appLogin(te)
	p := atOrgTerminal(te, "2", "", "", "", "")
	installOn(te, "acme")
	var createdAtWait []string
	p.onWait = func() {
		createdAtWait = slices.Clone(te.forge.Created)
		installOn(te, "acme", "octo")
	}
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create the private repo octo/widget-shadow", installed, "Turn on scheduled sync?", "Make these changes now?")
	if !slices.Contains(p.said, `    2. Choose octo, then "Only select repositories", then pick any one of your repos for now (GitHub needs one); init adds octo/widget-shadow when it creates it`) {
		t.Fatalf("no plain wording for an account with no repo to pick: %q", p.said)
	}
	if !slices.Contains(p.said, "! Install the Ghostbranch app on octo") || !slices.Contains(p.said, "    1. Open "+installLink) || len(createdAtWait) != 0 {
		t.Fatalf("said %q; created %v at the wait", p.said, createdAtWait)
	}
	wantPair(t, te, "octo/widget-shadow")
}

// The install step says what the app is for and how to do it, numbered, with
// the public repo and the shadow repo named; an org adds the request line.
func TestGuidedInitInstallStepExplains(t *testing.T) {
	t.Parallel()
	const (
		why   = "    Ghostbranch acts on GitHub through its app, and only on the repos you pick."
		open  = "    1. Open " + installLink
		click = "    3. Click Install, then come back here"
		note  = "       (init creates acme/widget-shadow next; the app gets it automatically)"
		ask   = "    If you aren't an owner of acme, GitHub sends an owner a request; setup continues once they approve."
	)
	t.Run("new shadow", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		appLogin(te)
		p := atTerminal(te, "", "", "", "")
		p.onWait = func() { installApp(te) }
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		want := []string{"! Install the Ghostbranch app on acme", why, open,
			`    2. Choose acme, then "Only select repositories", then pick acme/widget`, note, click}
		i := slices.Index(p.said, want[0])
		if i < 0 || !slices.Equal(p.said[i:i+len(want)], want) {
			t.Fatalf("install step: %q", p.said)
		}
		if saidHas(p, "Link for") || slices.Contains(p.said, ask) {
			t.Fatalf("old link line or org line for a personal account: %q", p.said)
		}
		if p.asked[0] != "Press Enter once it's installed." {
			t.Fatalf("asked %q", p.asked)
		}
	})
	t.Run("org", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		appLogin(te)
		p := atOrgTerminal(te, "1", "", "", "", "")
		p.onWait = func() { installOn(te, "acme") }
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		i := slices.Index(p.said, click)
		if i < 0 || p.said[i+1] != ask {
			t.Fatalf("no request line after step 3: %q", p.said)
		}
	})
	t.Run("shadow exists", func(t *testing.T) {
		t.Parallel()
		te := newTestEnv(t)
		appLogin(te)
		if _, err := te.forge.AddRepo("acme/widget-shadow", true); err != nil {
			t.Fatal(err)
		}
		p := atTerminal(te, "", "", "", "")
		p.onWait = func() { installApp(te) }
		if code, out, errOut := te.run("init", "acme/widget", "--adopt"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		if !slices.Contains(p.said, `    2. Choose acme, then "Only select repositories", then pick acme/widget and acme/widget-shadow`) || slices.Contains(p.said, note) {
			t.Fatalf("said %q", p.said)
		}
	})
}

// Colour marks what needs the person only when the stream is a terminal
// that takes it.
func TestChecklistColourOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, terminal bool, noColor string) []string {
		te := newTestEnv(t)
		appLogin(te)
		p := atTerminal(te, "", "", "", "")
		p.onWait = func() { installApp(te) }
		te.env.StderrColor = terminal
		if noColor != "" {
			te.vars["NO_COLOR"] = noColor
		}
		if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
			t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
		}
		return p.said
	}
	hasEsc := func(said []string, prefix string) bool {
		return slices.ContainsFunc(said, func(l string) bool { return strings.HasPrefix(l, prefix) && strings.Contains(l, "\x1b[") })
	}
	on := run(t, true, "")
	if !hasEsc(on, "\x1b[1m\x1b[93m! Install the Ghostbranch app on acme") || !hasEsc(on, "\x1b[1m\x1b[93m    1. Open ") ||
		!slices.ContainsFunc(on, func(l string) bool { return strings.HasPrefix(l, "\x1b[92m✓") }) ||
		!slices.ContainsFunc(on, func(l string) bool { return strings.HasPrefix(l, "\x1b[2m·") }) {
		t.Fatalf("no colour on a terminal: %q", on)
	}
	for name, said := range map[string][]string{"NO_COLOR": run(t, true, "1"), "pipe": run(t, false, "")} {
		for _, l := range said {
			if strings.Contains(l, "\x1b") {
				t.Errorf("%s: escape code in %q", name, l)
			}
		}
	}
}

// A login inside init is announced once: "Using GitHub account" only says
// something new when no login happened in this run.
func TestGuidedInitAnnouncesTheLoginOnce(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	initDeviceLogin(t, te)
	p := atTerminal(te, "y", "", "", "", "")
	p.onWait = func() { installApp(te) }
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !slices.Contains(p.said, loggedIn) {
		t.Fatalf("no login line: %q", p.said)
	}
	if saidHas(p, "Using GitHub account") || saidHas(p, "the token from") {
		t.Fatalf("the login was announced again: %q", p.said)
	}
	if !slices.Contains(p.said, "✓ Logged in as acme (from your Ghostbranch login).") {
		t.Fatalf("checklist login line: %q", p.said)
	}
}
