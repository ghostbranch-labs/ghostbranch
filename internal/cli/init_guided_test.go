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
	interruptAt int
	onWait      func()
}

func (p *person) ask(q string) error {
	p.asked = append(p.asked, q)
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

func (p *person) Say(m string) { p.said = append(p.said, m); p.inner.Say(m) }

// atTerminal makes te a person's terminal in a checkout whose origin is
// acme/widget (not a clone on disk: GuardState says so), answering with
// answers.
func atTerminal(te *testEnv, answers ...string) *person {
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
	hasPrefixes(t, p.asked, "Public repo (owner/repo)", "Create private repo acme/widget-shadow?",
		"Install the push guard in this checkout?", "Turn on scheduled sync?", "Proceed?")
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
	hasPrefixes(t, p.asked, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?",
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
	hasPrefixes(t, p.asked, "Public repo (owner/repo)", "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?")
}

func TestGuidedInitFlagsPreAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		args  []string
		asked []string
	}{
		{"every answer as a flag asks nothing", []string{"init", "acme/widget", "--shadow", "acme/widget-shadow", "--no-guard", "--no-sync"}, nil},
		{"--no-guard and --sync", []string{"init", "acme/widget", "--no-guard", "--sync"}, []string{"Create private repo acme/widget-shadow?", "Proceed?"}},
		{"--adopt answers the shadow question", []string{"init", "acme/widget", "--adopt", "--no-sync"}, []string{"Proceed?"}},
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
	hasPrefixes(t, p.asked, "Proceed?")
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
	p := atTerminal(te, "y", "", "", "")
	code, out, errOut := te.run("init", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, "No GitHub login found. Log in with GitHub now?", "Create private repo acme/widget-shadow?",
		"Turn on scheduled sync?", "Proceed?")
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
			hasPrefixes(t, p.asked, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?",
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
	hasPrefixes(t, p.asked, "Shadow repo to use instead", "Create private repo acme/other-shadow?", "Turn on scheduled sync?", "Proceed?")
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
	te.forge.Orgs = map[string]bool{"acme": true}
	return atTerminal(te, answers...)
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

const whereQ = "Choose 1, 2 or 3"

func TestGuidedInitAsksWhereAnOrgsShadowLives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	p := atOrgTerminal(te, "", "", "", "")
	if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	hasPrefixes(t, p.asked, whereQ, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?")
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
	hasPrefixes(t, p.asked, whereQ, whereQ, "Create private repo octo/widget-shadow?", "Turn on scheduled sync?", "Proceed?")
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
	hasPrefixes(t, p.asked, whereQ, "Shadow repo (owner/repo", "octo/private-mirror already exists. Adopt it", "Turn on scheduled sync?", "Proceed?")
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
	hasPrefixes(t, p.asked, whereQ, "Shadow repo (owner/repo", "Shadow repo to use instead", "Create private repo octo/closed?",
		"Turn on scheduled sync?", "Proceed?")
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
		hasPrefixes(t, p.asked, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?")
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
		hasPrefixes(t, p.asked, "Turn on scheduled sync?", "Proceed?")
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
	hasPrefixes(t, p.asked, whereQ, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?",
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
	hasPrefixes(t, p.asked, whereQ, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?",
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
			p := atOrgTerminal(te, "", "", "", "", "", "")
			p.onWait = func() { _, _ = te.forge.AddRepo("octo/widget-shadow", true) }
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			hasPrefixes(t, p.asked, whereQ, "Create private repo acme/widget-shadow?", "Turn on scheduled sync?", "Proceed?",
				"Use octo/widget-shadow instead?", "Press Enter once octo/widget-shadow exists.")
			if !slices.Contains(p.said, `GitHub refused to create octo/widget-shadow on octo. GitHub said: "`+notInstalledMsg+`"`) {
				t.Fatalf("never said the personal account refused: %q", p.said)
			}
			const notInstalled = "  - the Ghostbranch app isn't installed on octo: install it at " + installLink
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
		step2     = "  2. Give Ghostbranch access to it: install the ghostbranch-cli GitHub App on acme, or add acme/widget-shadow to its repositories, at " + installLink
		notYet    = "acme/widget-shadow isn't there yet, or the ghostbranch-cli GitHub App can't see it: install the app on acme, or add acme/widget-shadow to its repositories, at " + installLink
		step2PAT  = "  2. Give Ghostbranch access to it: add it to the Ghostbranch GitHub App's repositories (or to your token's)."
		notYetPAT = "acme/widget-shadow isn't there yet, or your login can't see it."
	)
	for _, app := range []bool{true, false} {
		t.Run(map[bool]string{true: "app login", false: "token"}[app], func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if app {
				appLogin(te)
			}
			refuseIn(te, notInstalledMsg, "acme")
			// Org shadow refused, personal declined, by hand: not there at the
			// first Enter, there at the second.
			p := atOrgTerminal(te, "", "", "", "", "n", "", "")
			waits := 0
			p.onWait = func() {
				if waits++; waits == 2 {
					_, _ = te.forge.AddRepo("acme/widget-shadow", true)
				}
			}
			if code, out, errOut := te.run("init", "acme/widget"); code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			for _, tc := range []struct {
				line string
				want bool
			}{{option2, app}, {offer, app}, {step2, app}, {notYet, app}, {step2PAT, !app}, {notYetPAT, !app}} {
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
		appMissing = "  - the Ghostbranch app isn't installed on %s: install it at " + installLink
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
			answers: []string{"", "", "", "", "n", ""},
			want: []string{`GitHub refused to create acme/widget-shadow on acme. GitHub said: "Resource not accessible by integration"`,
				"Likely cause:", fmt.Sprintf(appMissing, "acme")},
			never: []string{fmt.Sprintf(orgPolicy, "then add it to the app"), tokenLacks}},
		{name: "org policy", msg: "Repository creation failed.", shadow: "acme/widget-shadow", app: true, org: true,
			answers: []string{"", "", "", "", "n", ""},
			want: []string{`GitHub refused to create acme/widget-shadow on acme. GitHub said: "Repository creation failed."`,
				"Likely cause:", fmt.Sprintf(orgPolicy, "then add it to the app")},
			never: []string{fmt.Sprintf(appMissing, "acme"), tokenLacks}},
		{name: "personal account, app not installed", msg: notInstalledMsg, shadow: "octo/widget-shadow", app: true,
			args: []string{"--shadow", "octo/widget-shadow"}, answers: []string{"", "", ""},
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
			if tc.org {
				te.forge.Orgs = map[string]bool{"acme": true}
			}
			owner, _, _ := forge.SplitName(tc.shadow)
			refuseIn(te, tc.msg, owner)
			p := atTerminal(te, tc.answers...)
			p.onWait = func() { _, _ = te.forge.AddRepo(tc.shadow, true) }
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
