package cli_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func TestDoctorHumanOutput(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	code, out, errOut := te.run("doctor", "--repo", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	var marks []string
	for _, l := range lines {
		if !strings.HasPrefix(l, "    ") {
			marks = append(marks, strings.Join(strings.Fields(l)[:2], " "))
		}
	}
	want := []string{"✓ tools", "! token", "– app", "✓ pair", "– guard", "✓ state", "✓ ci", "update check:"}
	if !reflect.DeepEqual(marks, want) {
		t.Fatalf("lines %q, want %q\n%s", marks, want, out)
	}
}

func TestDoctorFailsWithTheFixesInNext(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	code, env := te.runJSON("doctor", "--repo", "acme/widget")
	e, _ := env["error"].(map[string]any)
	if code != 1 || e["code"] != string(contract.DoctorFailed) {
		t.Fatalf("exit %d: %v", code, env)
	}
	if failed := e["details"].(map[string]any)["failed"]; !reflect.DeepEqual(failed, []any{"token"}) {
		t.Fatalf("failed %v", failed)
	}
	n := nextOf(t, env)
	if len(n) != 1 || !reflect.DeepEqual(argv(n[0]), []string{"ghostbranch", "login"}) || n[0]["needs_human"] != true {
		t.Fatalf("next %v", n)
	}
	data, _ := env["data"].(map[string]any)
	if checks, _ := data["checks"].([]any); len(checks) != 7 {
		t.Fatalf("data %v", data)
	}
}

// Review Focus 4: with GHOSTBRANCH_TOKEN set, doctor never touches the
// keychain. An unavailable one would fail the token check if it did.
func TestDoctorWithAnEnvTokenNeverTouchesTheKeychain(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	kc := keychain.NewFake()
	kc.Unavailable = true
	te.env.Keychain = kc
	if code, env := te.runJSON("doctor", "--repo", "acme/widget"); code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
}

func TestTokenPermissionMissingNextIsDoctor(t *testing.T) {
	t.Parallel()
	err := contract.New(contract.TokenPermissionMissing, "the token lacks Workflows: write on acme/widget-shadow").
		WithDetails(map[string]any{"permission": "Workflows: write", "repo": "acme/widget-shadow", "command": "gate"})
	n := cli.NextFor("gate", err)
	if len(n) != 1 || !reflect.DeepEqual(n[0].Command, []string{"ghostbranch", "doctor"}) || n[0].NeedsHuman {
		t.Fatalf("next %+v", n)
	}
}

// An agent on the person's stored login: the token check warns, and the
// result carries AGENT_ON_PERSON_LOGIN.
func TestDoctorWarnsOfAnAgentOnThePersonsLogin(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.vars["GHOSTBRANCH_ACTOR"] = "agent:claude-code"
	storeLogin(te, "ghu_test_x", "ghr_test_x", 8*time.Hour)
	te.forge.Installations = []forge.Installation{{ID: 7, Account: "acme", AccountType: forge.OwnerOrganization, Selection: "all"}}
	code, env := te.runJSON("doctor", "--repo", "acme/widget")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	ws, _ := env["warnings"].([]any)
	if len(ws) != 1 || ws[0].(map[string]any)["code"] != string(contract.AgentOnPersonLogin) {
		t.Fatalf("warnings %v", ws)
	}
	if strings.Contains(fmt.Sprint(env), "ghu_test_x") || strings.Contains(fmt.Sprint(env), "ghr_test_x") {
		t.Fatalf("doctor shows the token: %v", env)
	}
}

// Doctor looks afresh, saves the look, and reports it as a line and an
// object; the reminder times are left alone.
func TestDoctorReportsTheUpdateCheck(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	f, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	te.env.StderrTerminal = false
	_, out, _ := te.run("doctor", "--repo", "acme/widget")
	// Two requests per look: the latest release, then (it is newer) the advisories.
	if f.count() != 2 || !strings.Contains(out, "update check: on · last looked just now · 0.13.0 available (you have 0.12.0) · GHOSTBRANCH_UPDATE_URL="+url+" (test-only)\n") {
		t.Fatalf("%d hits\n%s", f.count(), out)
	}
	_, env := te.runJSON("doctor", "--repo", "acme/widget")
	uc, _ := env["data"].(map[string]any)["update_check"].(map[string]any)
	if uc["enabled"] != true || uc["latest"] != "0.13.0" || uc["current"] != "0.12.0" || uc["override"] != url || uc["error"] != "" {
		t.Fatalf("update_check = %v", uc)
	}
	if f.count() != 4 {
		t.Fatalf("doctor must look afresh each time: %d hits", f.count())
	}
	raw, err := os.ReadFile(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "update-check.json"))
	if err != nil || !strings.Contains(string(raw), `"reminded_person_at": "0001-01-01T00:00:00Z"`) {
		t.Fatalf("cache %s %v", raw, err)
	}
}

func TestDoctorSaysWhyTheCheckIsOff(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		version string
		vars    map[string]string
		want    string
	}{
		{"v0.12.0", map[string]string{"GHOSTBRANCH_NO_UPDATE_CHECK": "1"}, "update check: off (GHOSTBRANCH_NO_UPDATE_CHECK)"},
		{"v0.12.0", map[string]string{"CI": "1"}, "update check: off (CI)"},
		{"(devel)", nil, "update check: off (development build)"},
	} {
		te := newTestEnv(t)
		f, url := newFakeReleases(t, "v0.13.0", "[]")
		te.releaseBuild(c.version, url)
		for k, v := range c.vars {
			te.vars[k] = v
		}
		_, out, _ := te.run("doctor")
		if !strings.Contains(out, c.want+"\n") || f.count() != 0 {
			t.Errorf("%v: %d hits\n%s", c.vars, f.count(), out)
		}
		// The override is reported in the object even when the check is off.
		_, env := te.runJSON("doctor")
		uc, _ := env["data"].(map[string]any)["update_check"].(map[string]any)
		if uc["enabled"] != false || uc["override"] != url || f.count() != 0 {
			t.Errorf("%v: update_check = %v, %d hits", c.vars, uc, f.count())
		}
	}
}

func TestDoctorShowsAFailedLook(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close() // a port that was just in use and now refuses
	te.releaseBuild("v0.12.0", closed.URL)
	_, out, _ := te.run("doctor")
	if !strings.Contains(out, "update check: on · last look failed: ") {
		t.Fatalf("%s", out)
	}
}

// No state directory: the check is off, and doctor says so.
func TestDoctorSaysTheCheckIsOffWithoutAStateDirectory(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.releaseBuild("v0.12.0", "http://127.0.0.1:1")
	delete(te.vars, "GHOSTBRANCH_HOME")
	// The state directory comes from Env.UserHome (darwin, linux) or
	// LOCALAPPDATA (windows, unset in te.vars): it fails on every OS, for the
	// update check and the workspace alike, and the real home is never read.
	te.env.UserHome = func() (string, error) { return "", errors.New("no home") }
	_, out, _ := te.run("doctor")
	if !strings.Contains(out, "update check: off (no state directory)") {
		t.Fatalf("%s", out)
	}
	_, env := te.runJSON("doctor")
	checks, _ := env["data"].(map[string]any)["checks"].([]any)
	if len(checks) != 7 {
		t.Fatalf("checks %v", checks)
	}
	for _, c := range checks {
		if c := c.(map[string]any); c["id"] == "state" && (c["status"] != "fail" || !strings.Contains(c["message"].(string), "find the state directory")) {
			t.Fatalf("state check %v", c)
		}
	}
}

// A look still running when doctor's checks end shows a step while doctor
// waits for it, so doctor never looks hung.
func TestDoctorShowsItsWaitForTheLook(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.12.0", "[]") // not newer: one request
	gate := make(chan struct{})
	f.mu.Lock()
	f.gate = gate
	f.mu.Unlock()
	te.releaseBuild("v0.12.0", url)
	te.env.StderrTerminal = false
	// The fake holds its answer until doctor has shown its wait, so the look
	// is always still running when the checks end.
	var out bytes.Buffer
	errb := &lockedBuffer{}
	env := te.env
	env.Stdout, env.Stderr = &out, errb
	go func() {
		defer close(gate)
		deadline := time.Now().Add(30 * time.Second)
		for !strings.Contains(errb.String(), "Looking for a newer Ghostbranch\n") && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
	}()
	cli.Run([]string{"doctor"}, env)
	errOut := errb.String()
	if !strings.Contains(errOut, "Looking for a newer Ghostbranch\n") || !strings.Contains(out.String(), "update check: on · last looked just now · up to date") {
		t.Fatalf("stdout %s\nstderr %s", out.String(), errOut)
	}
}

// lockedBuffer is a stderr that a test can read while doctor still writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *lockedBuffer) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// F-27: doctor's next for a repo that isn't set up here is exactly
// PAIR_NOT_FOUND's: adopt first, then create, both dry runs.
func TestDoctorNextForAnUnregisteredRepoIsInit(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	code, env := te.runJSON("doctor", "--repo", "acme/other")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	_, pairEnv := te.runJSON("agents", "status", "--repo", "acme/other")
	got, want := nextOf(t, env), nextOf(t, pairEnv)
	if len(got) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("doctor next %v, PAIR_NOT_FOUND next %v", got, want)
	}
	data, _ := env["data"].(map[string]any)
	for _, c := range data["checks"].([]any) {
		if m := c.(map[string]any); m["id"] == "pair" && m["fix"] != "ghostbranch init acme/other --adopt --plan" {
			t.Fatalf("pair fix %q", m["fix"])
		}
	}
}

// F-26: doctor's help says what it does write: its fetch into the local
// copy of the pair, and the update check's cache.
func TestDoctorHelpSaysWhatItWrites(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, out, _ := te.run("doctor", "--help")
	flat := strings.Join(strings.Fields(out), " ")
	if code != 0 || strings.Contains(flat, "never writes") ||
		!strings.Contains(flat, "It changes nothing in your repos, records or keychain and never prompts; it fetches into its local copy of the pair (making it if needed) and refreshes the update check's cache. It isn't ledgered.") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

// A person reads the plain init command for a repo that isn't set up here,
// not the --plan forms agents get; --json keeps them.
func TestDoctorShowsAPersonThePlainInit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		shadow bool
		args   []string
		fix    string
	}{
		{"--repo, no shadow yet", false, []string{"--repo", "acme/other"}, "ghostbranch init acme/other"},
		{"--repo, the shadow exists", true, []string{"--repo", "acme/other"}, "ghostbranch init acme/other --adopt"},
		{"a checkout", false, nil, "ghostbranch init acme/other"},
		{"a checkout, the shadow exists", true, nil, "ghostbranch init acme/other --adopt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			if tc.shadow {
				if _, err := te.forge.AddRepo("acme/other-shadow", true); err != nil {
					t.Fatal(err)
				}
			}
			if tc.args == nil {
				te.env.CwdRemotes = func() []workspace.Remote {
					return []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/other.git"}}
				}
			}
			_, out, errOut := te.run(append([]string{"doctor"}, tc.args...)...)
			if strings.Contains(out+errOut, "--plan") || !strings.Contains(out, "run "+tc.fix+"\n") && !strings.Contains(out, "run "+tc.fix+";") {
				t.Fatalf("want %q, no --plan:\n%s\n%s", tc.fix, out, errOut)
			}
			if !strings.Contains(out, "    fix: "+tc.fix+"\n") {
				t.Fatalf("no fix line %q:\n%s", tc.fix, out)
			}
			if tc.args != nil && !strings.Contains(errOut, "  "+tc.fix+" (a person must run this)") {
				t.Fatalf("next isn't %q:\n%s", tc.fix, errOut)
			}
		})
	}
}

// A person whose token lacks permissions gets a pre-filled link for one
// that has them all, with the repository choice the link can't make. An
// agent's run never gets it: that token could
// publish, and an agent must not relay it.
func TestDoctorLinksAPrefilledToken(t *testing.T) {
	t.Parallel()
	link, _ := forge.PairTokenLink("acme/widget", "acme/widget-shadow")
	for _, tc := range []struct {
		name   string
		person bool
		prep   func(te *testEnv)
	}{
		{"a person at a console", true, func(te *testEnv) { te.env.ConsoleUsable = true }},
		{"no console", false, func(*testEnv) {}},
		{"an agent at a console", false, func(te *testEnv) {
			te.env.ConsoleUsable = true
			te.vars["GHOSTBRANCH_ACTOR"] = "agent:claude-code"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			initOnly(te)
			tc.prep(te)
			te.forge.PermsDenied = map[string]map[string]bool{"acme/widget-shadow": {"Actions: read": true}}
			_, env := te.runJSON("doctor", "--repo", "acme/widget")
			data, _ := env["data"].(map[string]any)
			for _, c := range data["checks"].([]any) {
				m := c.(map[string]any)
				if m["id"] != "token" {
					continue
				}
				msg, _ := m["message"].(string)
				if !strings.Contains(msg, "it lacks permissions these commands need") || m["fix"] != "ghostbranch guide" {
					t.Fatalf("token check %v", m)
				}
				for _, want := range []string{link, `choose "Only select repositories", then pick acme/widget and acme/widget-shadow`} {
					if strings.Contains(msg, want) != tc.person {
						t.Errorf("person %v, message has %q: %v\n%s", tc.person, want, !tc.person, msg)
					}
				}
				return
			}
			t.Fatal("no token check")
		})
	}
}

// shadowLookups counts GetRepo calls for one repo, and fails them when
// fail is set.
type shadowLookups struct {
	forge.Forge
	repo  string
	fail  bool
	calls *int
}

func (f shadowLookups) GetRepo(ctx context.Context, full string) (forge.Repo, error) {
	if full == f.repo {
		*f.calls++
		if f.fail {
			return forge.Repo{}, fmt.Errorf("get %s: %w", full, forge.ErrTransient)
		}
	}
	return f.Forge.GetRepo(ctx, full)
}

// Doctor looks up <repo>-shadow only for a person's plain init line, never
// for --json; a lookup that fails gives the plain line with an --adopt note.
func TestDoctorLooksUpTheShadowOnlyForAPerson(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	if _, err := te.forge.AddRepo("acme/other-shadow", true); err != nil {
		t.Fatal(err)
	}
	calls := 0
	look := shadowLookups{Forge: te.forge, repo: "acme/other-shadow", calls: &calls}
	te.env.NewForge = func(string) (forge.Forge, error) { return look, nil }
	te.runJSON("doctor", "--repo", "acme/other")
	if calls != 0 {
		t.Fatalf("--json looked up the shadow %d times", calls)
	}
	look.fail = true
	te.env.NewForge = func(string) (forge.Forge, error) { return look, nil }
	_, out, _ := te.run("doctor", "--repo", "acme/other")
	if want := "run ghostbranch init acme/other (add --adopt if its shadow repo already exists)\n"; calls != 1 || !strings.Contains(out, want) {
		t.Fatalf("%d lookups; want %q in:\n%s", calls, want, out)
	}
	if !strings.Contains(out, "    fix: ghostbranch init acme/other\n") {
		t.Fatalf("fix line:\n%s", out)
	}
}

// Outside a checkout, with one registered pair and no --repo, doctor checks
// that pair.
func TestDoctorFindsTheOnlyPairWithoutRepo(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, env := te.runJSON("doctor")
	data, _ := env["data"].(map[string]any)
	checks, _ := data["checks"].([]any)
	for _, c := range checks {
		if m, _ := c.(map[string]any); m["id"] == "pair" && m["status"] != "ok" {
			t.Fatalf("pair check %v", m)
		}
	}
	if len(checks) != 7 {
		t.Fatalf("checks %v", checks)
	}
}

// An app login without the app installed fails the app check; the fix is the
// install link, which is not a command, so next carries nothing for it.
func TestDoctorAppNotInstalled(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	storeLogin(te, "ghu_test_x", "ghr_test_x", 8*time.Hour)
	code, env := te.runJSON("doctor", "--repo", "acme/widget")
	e, _ := env["error"].(map[string]any)
	if code != 1 || e["code"] != string(contract.DoctorFailed) {
		t.Fatalf("exit %d: %v", code, env)
	}
	if failed := e["details"].(map[string]any)["failed"]; !reflect.DeepEqual(failed, []any{"app"}) {
		t.Fatalf("failed %v", failed)
	}
	for _, c := range env["data"].(map[string]any)["checks"].([]any) {
		if m := c.(map[string]any); m["id"] == "app" && !strings.HasPrefix(m["fix"].(string), "https://github.com/apps/") {
			t.Fatalf("app check %v", m)
		}
	}
	if n := nextOf(t, env); len(n) != 0 {
		t.Fatalf("next %v", n)
	}
}
