package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// consoleLog is a console device that records what is shown on it; login
// asks nothing, so a read is the end of input.
type consoleLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *consoleLog) Read([]byte) (int, error) { return 0, io.EOF }
func (c *consoleLog) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}
func (c *consoleLog) Close() error { return nil }
func (c *consoleLog) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// deviceLogin points te at a fake GitHub OAuth server, at a console.
func deviceLogin(te *testEnv, o authtest.Options) (*authtest.Server, *consoleLog, *[]string) {
	srv := authtest.New(te.t, o)
	con := &consoleLog{}
	var opened []string
	te.env.ClientID, te.env.AppSlug = srv.ClientID, "ghostbranch-test"
	te.env.OAuthBaseURL, te.env.HTTPClient = srv.URL, srv.Client()
	te.env.OpenBrowser = func(u string) error { opened = append(opened, u); return nil }
	te.env.ConsoleUsable = true
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { return con, nil }
	te.forge.User = forge.User{Login: "octo", ID: 1}
	return srv, con, &opened
}

func errCode(env map[string]any) (string, string) {
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	c, _ := e["code"].(string)
	r, _ := d["reason"].(string)
	return c, r
}

func TestLoginWithTheDeviceFlow(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, con, opened := deviceLogin(te, authtest.Options{Pending: 1})
	code, out, errOut := te.run("login", "--json")
	if code != 0 {
		t.Fatalf("%d\n%s\n%s", code, out, errOut)
	}
	shown := con.String()
	if !strings.Contains(shown, authtest.UserCode) || !strings.Contains(shown, srv.URL+"/login/device") {
		t.Fatalf("the console must show the code and URL:\n%s", shown)
	}
	if strings.Contains(out+errOut, authtest.UserCode) {
		t.Fatalf("the code left the console:\n%s\n%s", out, errOut)
	}
	if len(*opened) != 1 || (*opened)[0] != srv.URL+"/login/device" {
		t.Fatalf("browser: %v", *opened)
	}
	l, _ := auth.LoadLogin(te.env.Keychain)
	if l == nil || l.Token != srv.AccessToken(1) || l.Account != "octo" || l.Kind != "app" {
		t.Fatalf("stored: %v", l)
	}
	if !strings.Contains(out, `"install_url": "https://github.com/apps/ghostbranch-test/installations/new"`) ||
		!strings.Contains(out, `"replaced": false`) {
		t.Fatalf("first login: %s", out)
	}
	// A person reads where to install the app and what to pick: the public
	// repo only, as init creates the shadow repo.
	te2 := newTestEnv(t)
	deviceLogin(te2, authtest.Options{})
	code, out, errOut = te2.run("login")
	const install = "Next, install the Ghostbranch app on the account that owns your public repo and pick that repo; ghostbranch init creates the shadow repo: https://github.com/apps/ghostbranch-test/installations/new\n"
	if code != 0 || !strings.Contains(out, install) {
		t.Fatalf("human first login: %d\n%s\n%s", code, out, errOut)
	}
	// A second login replaces the first and doesn't repeat the install link.
	srv2, _, _ := deviceLogin(te, authtest.Options{})
	te.forge.User = forge.User{Login: "octo", ID: 1}
	code, env := te.runJSON("login")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["replaced"] != true || data["install_url"] != "" {
		t.Fatalf("second login: %d %v", code, env)
	}
	if l, _ := auth.LoadLogin(te.env.Keychain); l.Token != srv2.AccessToken(1) {
		t.Fatalf("not replaced: %v", l)
	}
}

func TestLoginNeedsAConsole(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, _, _ := deviceLogin(te, authtest.Options{})
	opened := false
	te.env.ConsoleUsable = false
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { opened = true; return nil, errors.New("no console") }
	code, _, errOut := te.run("login")
	if code != 1 || !strings.Contains(errOut, "LOGIN_UNAVAILABLE") || opened || len(srv.Forms()) != 0 {
		t.Fatalf("%d %s (console opened: %v, requests: %d)", code, errOut, opened, len(srv.Forms()))
	}
	te.env.ConsoleUsable = true
	if code, _, _ := te.run("login", "--non-interactive"); code != 1 {
		t.Fatalf("--non-interactive: %d", code)
	}
}

func TestLoginNoJSONNoConsoleStillRefuses(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv, _, _ := deviceLogin(te, authtest.Options{})
	te.env.ConsoleUsable = false
	code, out, errOut := te.run("login")
	if code != 1 || len(srv.Forms()) != 0 || !strings.Contains(errOut+out, "LOGIN_UNAVAILABLE") {
		t.Fatalf("%d (requests: %d)\n%s\n%s", code, len(srv.Forms()), out, errOut)
	}
	te.env.ConsoleUsable = true
	if code, _, _ := te.run("login", "--non-interactive"); code != 1 || len(srv.Forms()) != 0 {
		t.Fatalf("--non-interactive: %d (requests: %d)", code, len(srv.Forms()))
	}
}

func TestLoginJSONNoConsoleWritesDeviceCodeFirst(t *testing.T) {
	t.Parallel()
	for _, nonInteractive := range []bool{false, true} {
		te := newTestEnv(t)
		srv, con, opened := deviceLogin(te, authtest.Options{Pending: 1})
		args := []string{"login", "--json"}
		if nonInteractive {
			args = append(args, "--non-interactive")
		} else {
			te.env.ConsoleUsable = false
		}
		code, out, errOut := te.run(args...)
		if code != 0 {
			t.Fatalf("%d\n%s\n%s", code, out, errOut)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 2 {
			t.Fatalf("want 2 lines, got %d:\n%s", len(lines), out)
		}
		testutil.ValidateSchema(t, "login-event.schema.json", []byte(lines[0]))
		var ev map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
			t.Fatal(err)
		}
		if ev["type"] != "device_code" || ev["user_code"] != authtest.UserCode || ev["verification_uri"] != srv.URL+"/login/device" {
			t.Fatalf("event: %v", ev)
		}
		if strings.Contains(out+errOut, authtest.DeviceCode) {
			t.Fatalf("the device code left the process:\n%s\n%s", out, errOut)
		}
		var env map[string]any
		if err := json.Unmarshal([]byte(lines[1]), &env); err != nil || env["ok"] != true {
			t.Fatalf("last line: %v %s", err, lines[1])
		}
		if con.String() != "" || len(*opened) != 0 {
			t.Fatalf("no console or browser is used: %q %v", con.String(), *opened)
		}
		if l, _ := auth.LoadLogin(te.env.Keychain); l == nil || l.Token != srv.AccessToken(1) {
			t.Fatalf("stored: %v", l)
		}
	}
}

func TestLoginRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name, reason string
		prep         func(*testEnv)
	}{
		{"no keychain", "no_keychain", func(te *testEnv) { te.env.Keychain = nil }},
		{"no client id", "no_client_id", func(te *testEnv) { te.env.ClientID = "" }},
		{"declined", "denied", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			deviceLogin(te, authtest.Options{Deny: c.reason == "denied"})
			if c.prep != nil {
				c.prep(te)
			}
			code, env := te.runJSON("login")
			if cd, r := errCode(env); code != 1 || cd != "LOGIN_UNAVAILABLE" || r != c.reason {
				t.Fatalf("%d %v", code, env)
			}
			if te.env.Keychain != nil {
				if l, _ := auth.LoadLogin(te.env.Keychain); l != nil {
					t.Fatalf("stored after a refusal: %v", l)
				}
			}
		})
	}
}

func TestABrowserThatWontOpenDoesntFailLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	deviceLogin(te, authtest.Options{})
	te.env.OpenBrowser = func(string) error { return errors.New("no browser") }
	if code, env := te.runJSON("login"); code != 0 {
		t.Fatalf("%d %v", code, env)
	}
}

func TestLoginWithToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.User = forge.User{Login: "octo", ID: 1}
	te.env.Stdin = strings.NewReader("  ghp_test_pasted_token \n")
	code, env := te.runJSON("login", "--with-token")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["account"] != "octo" || data["kind"] != "pat" || data["token_kind"] != "classic" ||
		data["expires"] != nil || data["env_token"] != "GHOSTBRANCH_TOKEN" {
		t.Fatalf("%d %v", code, env)
	}
	if l, _ := auth.LoadLogin(te.env.Keychain); l == nil || l.Token != "ghp_test_pasted_token" || l.Kind != "pat" {
		t.Fatalf("stored: %v", l)
	}
	te.env.Stdin = strings.NewReader("\n")
	if code, env := te.runJSON("login", "--with-token"); code != 2 {
		t.Fatalf("empty stdin: %d %v", code, env)
	}
	te.env.Stdin = strings.NewReader("two tokens\n")
	if code, env := te.runJSON("login", "--with-token"); code != 2 {
		t.Fatalf("two words: %d %v", code, env)
	}
}

func TestLoginWithARejectedToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.UserErr = errors.New("401 Bad credentials")
	te.env.Stdin = strings.NewReader("ghp_test_rejected\n")
	code, env := te.runJSON("login", "--with-token")
	if c, _ := errCode(env); code != 1 || c != "AUTH_MISSING" {
		t.Fatalf("%d %v", code, env)
	}
	if l, _ := auth.LoadLogin(te.env.Keychain); l != nil {
		t.Fatalf("a rejected token was stored: %v", l)
	}
	te.forge.UserErr = fmt.Errorf("%w: offline", forge.ErrTransient)
	te.env.Stdin = strings.NewReader("ghp_test_rejected\n")
	if code, env := te.runJSON("login", "--with-token"); code != 1 || func() string { c, _ := errCode(env); return c }() != "FORGE_ERROR" {
		t.Fatalf("offline: %d %v", code, env)
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	storeLogin(te, "ghu_test_x", "ghr_test_x", 8*time.Hour)
	code, env := te.runJSON("logout")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["removed"] != true || data["account"] != "octo" || data["kind"] != "app" ||
		data["revoke_url"] != "https://github.com/settings/apps/authorizations" || data["env_token"] != "GHOSTBRANCH_TOKEN" {
		t.Fatalf("%d %v", code, env)
	}
	if l, _ := auth.LoadLogin(te.env.Keychain); l != nil {
		t.Fatalf("still stored: %v", l)
	}
	code, env = te.runJSON("logout")
	if data, _ := env["data"].(map[string]any); code != 0 || data["removed"] != false {
		t.Fatalf("nothing stored: %d %v", code, env)
	}
	_ = te.env.Keychain.Set(auth.Service, auth.Account, "{not json")
	code, env = te.runJSON("logout")
	if data, _ := env["data"].(map[string]any); code != 0 || data["removed"] != true {
		t.Fatalf("corrupt: %d %v", code, env)
	}
}

func TestAuthStatus(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.User = forge.User{Login: "octo", ID: 1}
	initOnly(te)
	// The env token wins, and the keychain is never touched.
	te.env.Keychain = keychainTripwire{t}
	code, env := te.runJSON("auth", "status")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["source"] != "GHOSTBRANCH_TOKEN" || data["stored_login_checked"] != false ||
		data["can_publish"] != "true" || data["pair"] != "acme/widget" || data["account"] != "octo" ||
		func() bool { w, _ := env["warnings"].([]any); return len(w) != 0 }() {
		t.Fatalf("env token: %d %v", code, env)
	}
	// The stored login, from an agent's shell.
	te2 := newTestEnv(t)
	te2.forge.User = forge.User{Login: "octo", ID: 1}
	delete(te2.vars, "GHOSTBRANCH_TOKEN")
	te2.vars["GHOSTBRANCH_ACTOR"] = "agent:claude-code"
	storeLogin(te2, "ghu_test_x", "ghr_test_x", 8*time.Hour)
	code, env = te2.runJSON("auth", "status")
	data, _ = env["data"].(map[string]any)
	stored, _ := data["stored_login"].(map[string]any)
	warns, _ := env["warnings"].([]any)
	var warnCode any
	if len(warns) == 1 {
		warnCode = warns[0].(map[string]any)["code"]
	}
	if code != 0 || data["source"] != "login" || data["token_kind"] != "app" || data["valid"] != "true" ||
		warnCode != "AGENT_ON_PERSON_LOGIN" || stored["account"] != "octo" || stored["expires"] == nil {
		t.Fatalf("stored login: %d %v", code, env)
	}
	// Offline: the stored fields are still there.
	te2.forge.UserErr = fmt.Errorf("%w: offline", forge.ErrTransient)
	code, env = te2.runJSON("auth", "status")
	data, _ = env["data"].(map[string]any)
	if code != 0 || data["valid"] != "unknown" || data["account"] != "octo" || data["stored_login"] == nil {
		t.Fatalf("offline: %d %v", code, env)
	}
	// No token at all is a status, not an error.
	te3 := newTestEnv(t)
	delete(te3.vars, "GHOSTBRANCH_TOKEN")
	code, env = te3.runJSON("auth", "status")
	data, _ = env["data"].(map[string]any)
	if code != 0 || data["source"] != "" || data["problem"] != "" || data["stored_login"] != nil {
		t.Fatalf("no token: %d %v", code, env)
	}
}

// Review Focus 1: no output of login, logout or auth status shows a token,
// whether GitHub issued it, a person pasted it, GitHub rejected it, or it
// sits in a corrupt keychain entry. Nor does any file under GHOSTBRANCH_HOME
// (the ledger included).
func TestLoginOutputsNeverShowTheToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	_, con, _ := deviceLogin(te, authtest.Options{Pending: 1})
	const pasted = "ghp_test_pasted_secret"
	var seen strings.Builder
	run := func(stdin string, args ...string) {
		te.env.Stdin = strings.NewReader(stdin)
		for _, extra := range [][]string{nil, {"--json"}} {
			_, out, errOut := te.run(append(args, extra...)...)
			seen.WriteString(out + errOut)
		}
	}
	run("", "login")
	run("", "auth", "status")
	run("", "sync") // a command using the stored login
	run(pasted+"\n", "login", "--with-token")
	run("", "auth", "status")
	te.forge.UserErr = errors.New("401 Bad credentials " + pasted) // even an error that repeats it
	run(pasted+"\n", "login", "--with-token")
	te.forge.UserErr = nil
	run("", "logout")
	_ = te.env.Keychain.Set(auth.Service, auth.Account, `{"token":"`+pasted+`","kind":`)
	run("", "auth", "status")
	run("", "sync")
	run("", "logout")
	all := seen.String() + con.String()
	for _, s := range []string{"ghu_test", "ghr_test", "pasted_secret"} {
		if strings.Contains(all, s) {
			t.Fatalf("output shows %q:\n%s", s, all)
		}
	}
	_ = filepath.WalkDir(te.vars["GHOSTBRANCH_HOME"], func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, s := range []string{"ghu_test", "ghr_test", "pasted_secret"} {
			if bytes.Contains(b, []byte(s)) {
				t.Errorf("%s holds %q", p, s)
			}
		}
		return nil
	})
}

// auth status shows the stored login as it is after Resolve, so a refresh's
// new expiry shows; a keychain that can't be read is a problem, never
// "Stored login: none".
func TestAuthStatusShowsTheStoredLoginAfterResolve(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.User = forge.User{Login: "octo", ID: 1}
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	te.env.ClientID, te.env.OAuthBaseURL, te.env.HTTPClient = srv.ClientID, srv.URL, srv.Client()
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	storeLogin(te, "ghu_test_old", "ghr_test_seed", 2*time.Minute)
	code, env := te.runJSON("auth", "status")
	data, _ := env["data"].(map[string]any)
	stored, _ := data["stored_login"].(map[string]any)
	if want := cliNow.Add(8 * time.Hour).Format(time.RFC3339); code != 0 || srv.Refreshes() != 1 || stored["expires"] != want {
		t.Fatalf("refreshed: %d %v (want expires %s)", code, env, want)
	}

	locked := newTestEnv(t)
	delete(locked.vars, "GHOSTBRANCH_TOKEN")
	kc := keychain.NewFake()
	kc.GetErr = errors.New("keychain: locked")
	locked.env.Keychain = kc
	code, env = locked.runJSON("auth", "status")
	data, _ = env["data"].(map[string]any)
	if code != 0 || data["problem"] != "LOGIN_UNAVAILABLE" || data["stored_login_checked"] != true || data["stored_login"] != nil {
		t.Fatalf("locked: %d %v", code, env)
	}
	_, out, _ := locked.run("auth", "status")
	if !strings.Contains(out, "Stored login: can't be read (see the problem above)") || strings.Contains(out, "Stored login: none") {
		t.Fatalf("locked, human:\n%s", out)
	}
}
