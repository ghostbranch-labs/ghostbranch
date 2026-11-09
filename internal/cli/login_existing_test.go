package cli_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// answeringConsole is a console device with a person at it: once it is
// asked "Log in again?" it types answer (an empty answer closes the
// console, as a person pressing Ctrl-D would). It records what is shown.
// Like typist, it types only after the question is shown, so the prompt's
// discard of stale input can't swallow the answer.
type answeringConsole struct {
	mu       sync.Mutex
	pr       *io.PipeReader
	pw       *io.PipeWriter
	answer   string
	answered bool
	buf      bytes.Buffer
}

func newAnsweringConsole(answer string) *answeringConsole {
	pr, pw := io.Pipe()
	return &answeringConsole{pr: pr, pw: pw, answer: answer}
}

func (c *answeringConsole) Read(p []byte) (int, error) { return c.pr.Read(p) }
func (c *answeringConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.buf.Write(p)
	if !c.answered && strings.Contains(c.buf.String(), "Log in again?") {
		c.answered = true
		if c.answer == "" {
			go func() { _ = c.pw.Close() }()
		} else {
			line := c.answer
			go func() { _, _ = c.pw.Write([]byte(line)) }()
		}
	}
	return len(p), nil
}
func (c *answeringConsole) Close() error { return c.pw.Close() }
func (c *answeringConsole) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// personLogin is deviceLogin for a person with no GHOSTBRANCH_TOKEN, whose
// console answers "Log in again?" with answer.
func personLogin(te *testEnv, answer string) (*authtest.Server, *answeringConsole, *[]string) {
	srv, _, opened := deviceLogin(te, authtest.Options{})
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	con := newAnsweringConsole(answer)
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { return con, nil }
	return srv, con, opened
}

func storedToken(t *testing.T, te *testEnv) string {
	t.Helper()
	l, err := auth.LoadLogin(te.env.Keychain)
	if err != nil || l == nil {
		t.Fatalf("stored login: %v %v", l, err)
	}
	return l.Token
}

// A stored login that works is kept unless the person says to log in
// again; Enter keeps it and opens nothing.
func TestLoginKeepsAWorkingLoginUnlessAskedTo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, con, opened := personLogin(te, "\n")
	storeLogin(te, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	code, out, errOut := te.run("login")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	shown := con.String()
	if !strings.Contains(shown, "Already logged in to GitHub as octo with the Ghostbranch GitHub App (refreshable until ") ||
		!strings.Contains(shown, "Log in again?") {
		t.Fatalf("console:\n%s", shown)
	}
	if len(*opened) != 0 || storedToken(t, te) != "ghu_test_old" {
		t.Fatalf("kept login was replaced: opened %v", *opened)
	}
	if !strings.Contains(out, "Kept the stored GitHub login for octo.") {
		t.Fatalf("out:\n%s", out)
	}

	te2 := newTestEnv(t)
	srv, _, opened := personLogin(te2, "y\n")
	storeLogin(te2, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	if code, out, errOut := te2.run("login"); code != 0 || len(*opened) != 1 || storedToken(t, te2) != srv.AccessToken(1) {
		t.Fatalf("yes: %d opened %v\n%s\n%s", code, *opened, out, errOut)
	}
}

// --yes never asks: it logs in again, as login always did.
func TestLoginYesReplacesWithoutAsking(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, con, _ := personLogin(te, "")
	storeLogin(te, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	if code, out, errOut := te.run("login", "--yes"); code != 0 || storedToken(t, te) != srv.AccessToken(1) {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	if strings.Contains(con.String(), "Log in again?") {
		t.Fatalf("--yes asked:\n%s", con.String())
	}
}

// A stored login that can't be used any more is replaced without a
// question, and the person reads why.
func TestLoginReplacesALoginThatNoLongerWorks(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, con, _ := personLogin(te, "")
	if err := auth.SaveLogin(te.env.Keychain, auth.Login{Token: "ghu_test_old", RefreshToken: "ghr_test_old",
		Expires: cliNow.Add(-time.Hour), RefreshExpires: cliNow.Add(-time.Minute), Account: "octo", Kind: "app"}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := te.run("login"); code != 0 || storedToken(t, te) != srv.AccessToken(1) {
		t.Fatalf("expired: %d\n%s\n%s", code, out, errOut)
	}
	if shown := con.String(); !strings.Contains(shown, "Your stored GitHub login has expired; logging in again.") ||
		strings.Contains(shown, "Log in again?") {
		t.Fatalf("expired console:\n%s", shown)
	}

	te2 := newTestEnv(t)
	srv2, con2, _ := personLogin(te2, "")
	storeLogin(te2, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	te2.forge.UserErr = errors.New("401 Bad credentials")
	if code, out, errOut := te2.run("login"); code != 0 || storedToken(t, te2) != srv2.AccessToken(1) {
		t.Fatalf("rejected: %d\n%s\n%s", code, out, errOut)
	}
	if shown := con2.String(); !strings.Contains(shown, "GitHub no longer accepts your stored login; logging in again.") {
		t.Fatalf("rejected console:\n%s", shown)
	}
}

// With nothing stored but a working gh login, login says commands already
// work and why the app login is the safer one, then logs in.
func TestLoginExplainsAWorkingGHLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, con, _ := personLogin(te, "")
	te.env.GHToken = func() (string, error) { return "gho_test_gh", nil }
	if code, out, errOut := te.run("login"); code != 0 || storedToken(t, te) != srv.AccessToken(1) {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	const note = "Commands already work with the GitHub CLI's login (gh), which reaches every repo you can. " +
		"The Ghostbranch app login reaches only the repos you pick, so it's the safer one to use."
	if shown := con.String(); !strings.Contains(shown, note) || strings.Contains(shown, "Log in again?") {
		t.Fatalf("console:\n%s", shown)
	}
}

// auth status says why it checked a pair when nothing picked it: outside a
// checkout, with no --repo, it is the workspace's only pair.
func TestAuthStatusSaysWhyItPickedThePair(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, out, _ := te.run("auth", "status")
	if !strings.Contains(out, "Can publish to acme/widget (the only repo set up on this machine): true\n") {
		t.Fatalf("only pair:\n%s", out)
	}
	_, out, _ = te.run("auth", "status", "--repo", "acme/widget")
	if !strings.Contains(out, "Can publish to acme/widget: true\n") {
		t.Fatalf("--repo:\n%s", out)
	}
	te.env.CwdRemotes = func() []workspace.Remote {
		return []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/widget.git"}}
	}
	_, out, _ = te.run("auth", "status")
	if !strings.Contains(out, "Can publish to acme/widget: true\n") {
		t.Fatalf("checkout:\n%s", out)
	}
}

// feature start's help says --from takes the base branch's name, or main,
// as well as a feature.
func TestFeatureStartHelpNamesTheBaseAlias(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, out, _ := te.run("feature", "start", "--help")
	if !strings.Contains(out, "--from main or --from <base branch> starts from the shadow's base branch") {
		t.Fatalf("help:\n%s", out)
	}
}

// GHOSTBRANCH_TOKEN never stands in for the stored login when login checks
// it: the stored token is the one GitHub is asked about, and an expired
// stored login is still named as expired.
func TestLoginChecksTheStoredLoginNotAnEnvToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, _, _ = personLogin(te, "\n")
	te.vars["GHOSTBRANCH_TOKEN"] = "ghp_test_env"
	storeLogin(te, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	var used []string
	te.env.NewForge = func(tok string) (forge.Forge, error) { used = append(used, tok); return te.forge, nil }
	if code, out, errOut := te.run("login"); code != 0 || len(used) == 0 || used[0] != "ghu_test_old" {
		t.Fatalf("%d asked GitHub with %v\n%s\n%s", code, used, out, errOut)
	}

	te2 := newTestEnv(t)
	_, con, _ := personLogin(te2, "")
	te2.vars["GHOSTBRANCH_TOKEN"] = "ghp_test_env"
	if err := auth.SaveLogin(te2.env.Keychain, auth.Login{Token: "ghu_test_old", RefreshToken: "ghr_test_old",
		Expires: cliNow.Add(-time.Hour), RefreshExpires: cliNow.Add(-time.Minute), Account: "octo", Kind: "app"}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := te2.run("login"); code != 0 || !strings.Contains(con.String(), "has expired; logging in again.") {
		t.Fatalf("expired with an env token: %d\n%s\n%s\n%s", code, con.String(), out, errOut)
	}
}

// Closing the console at the question stops login: INTERRUPTED, and the
// stored login is untouched.
func TestLoginClosedAtTheQuestionKeepsTheLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, _, opened := personLogin(te, "")
	storeLogin(te, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	code, env := te.runJSON("login")
	if c, _ := errCode(env); code == 0 || c != "INTERRUPTED" || len(*opened) != 0 || storedToken(t, te) != "ghu_test_old" {
		t.Fatalf("%d %v opened %v", code, env, *opened)
	}
}

// login --json at a console, kept: the envelope is the stored login, not
// replaced, and the contract is unchanged.
func TestLoginJSONKeptReadsAsTheStoredLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	personLogin(te, "\n")
	storeLogin(te, "ghu_test_old", "ghr_test_old", 8*time.Hour)
	code, env := te.runJSON("login")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["replaced"] != false || data["account"] != "octo" || data["kind"] != "app" ||
		data["install_url"] != "" || env["schema_version"] != "0.17" {
		t.Fatalf("%d %v", code, env)
	}
	if _, ok := data["kept"]; ok {
		t.Fatalf("kept leaked into the JSON: %v", data)
	}
}

// --yes doesn't look at the stored login at all: no question, no note.
func TestLoginYesDoesntCheckTheStoredLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, con, _ := personLogin(te, "")
	if err := auth.SaveLogin(te.env.Keychain, auth.Login{Token: "ghu_test_old", RefreshToken: "ghr_test_old",
		Expires: cliNow.Add(-time.Hour), RefreshExpires: cliNow.Add(-time.Minute), Account: "octo", Kind: "app"}); err != nil {
		t.Fatal(err)
	}
	if code, out, errOut := te.run("login", "--yes"); code != 0 || strings.Contains(con.String(), "expired") {
		t.Fatalf("%d\n%s\n%s\n%s", code, con.String(), out, errOut)
	}
}
