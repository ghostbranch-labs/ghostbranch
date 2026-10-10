package cli_test

import (
	"fmt"
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
	want := []string{"✓ tools", "! token", "✓ pair", "– guard", "✓ state", "✓ ci"}
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
