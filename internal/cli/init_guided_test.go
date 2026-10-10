package cli_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
