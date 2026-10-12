package cli_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
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
	want := []string{"✓ tools", "! token", "✓ pair", "– guard", "✓ state", "✓ ci", "update check:"}
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
	if checks, _ := data["checks"].([]any); len(checks) != 6 {
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
	}
}

func TestDoctorShowsAFailedLook(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.releaseBuild("v0.12.0", "http://127.0.0.1:1") // nothing listens on port 1
	_, out, _ := te.run("doctor")
	if !strings.Contains(out, "update check: on · last look failed: ") {
		t.Fatalf("%s", out)
	}
}
