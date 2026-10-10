package cli_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// A 403 that names its permission reaches the caller as
// TOKEN_PERMISSION_MISSING: the permission, the repo, the command, and
// doctor as the next step. The token never shows.
func TestAMissingPermissionNamesItAndTheCommand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	withFeature(te)
	te.forge.MissingPermission = map[string]string{"CreatePR": "Pull requests: write"}
	code, out, errOut := te.run("gate", "feat-x", "--json")
	env := map[string]any{}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, out)
	}
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 1 || e["code"] != "TOKEN_PERMISSION_MISSING" || e["retryable"] != false ||
		d["permission"] != "Pull requests: write" || d["repo"] != "acme/widget-shadow" || d["command"] != "gate" {
		t.Fatalf("exit %d, error %v", code, e)
	}
	next, _ := env["next"].([]any)
	if len(next) == 0 {
		t.Fatal("no next step")
	}
	first, _ := next[0].(map[string]any)
	argv, _ := first["command"].([]any)
	if len(argv) < 2 || argv[0] != "ghostbranch" || argv[1] != "doctor" {
		t.Fatalf("next = %v, want doctor first", next)
	}
	if strings.Contains(out+errOut, "test-token") {
		t.Fatal("the token is in the output")
	}
	// Human output says the same.
	te.forge.MissingPermission = map[string]string{"FindOpenPR": "Pull requests: read"}
	_, _, errOut = te.run("gate", "feat-x")
	if !strings.Contains(errOut, "[TOKEN_PERMISSION_MISSING]") || !strings.Contains(errOut, "Pull requests: read") ||
		!strings.Contains(errOut, "ghostbranch doctor") {
		t.Fatalf("stderr = %s", errOut)
	}
}

// A code the engine chose on purpose from a refusal stays: init's
// SHADOW_CREATE_DENIED hands the person the adopt steps.
func TestAChosenCodeOutranksTheMissingPermission(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.forge.MissingPermission = map[string]string{"CreatePrivateRepo": "Administration: write"}
	code, env := te.runJSON("init", "acme/widget")
	e, _ := env["error"].(map[string]any)
	if code != 1 || e["code"] != "SHADOW_CREATE_DENIED" {
		t.Fatalf("exit %d, error %v", code, e)
	}
	if next, _ := env["next"].([]any); slices.ContainsFunc(next, func(n any) bool {
		m, _ := n.(map[string]any)
		argv, _ := m["command"].([]any)
		return len(argv) > 1 && argv[1] == "doctor"
	}) {
		t.Fatalf("next offers doctor for a chosen code: %v", next)
	}
}
