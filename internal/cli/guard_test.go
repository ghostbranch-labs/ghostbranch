package cli_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func hermeticGit(t *testing.T) {
	t.Helper()
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// pairCheckout makes a clone-like checkout whose origin is acme/widget.
func pairCheckout(te *testEnv) string {
	te.t.Helper()
	dir := filepath.Join(te.root, "checkout")
	for _, args := range [][]string{{"init", "-q", "-b", "main", dir}, {"-C", dir, "remote", "add", "origin", "https://github.com/acme/widget.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			te.t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	te.env.Getwd = func() (string, error) { return dir, nil }
	return dir
}

// validData checks a successful envelope's data against guard.schema.json.
func validData(t *testing.T, env map[string]any) {
	t.Helper()
	raw, err := json.Marshal(env["data"])
	if err != nil {
		t.Fatal(err)
	}
	validate(t, "guard.schema.json", raw)
}

func TestGuardCommands(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN") // the guard needs no token
	dir := pairCheckout(te)
	code, env := te.runJSON("guard", "install")
	data, _ := env["data"].(map[string]any)
	if code != 0 || data["action"] != "installed" {
		t.Fatalf("install: %d %v", code, env)
	}
	validData(t, env)
	hook, err := os.ReadFile(filepath.Join(dir, ".git", "hooks", "pre-push"))
	if err != nil || !strings.Contains(string(hook), "GB_REMOTES='origin'") || !strings.Contains(string(hook), "GB_PUBLIC='acme/widget'") {
		t.Fatalf("hook:\n%s %v", hook, err)
	}
	if code, env := te.runJSON("guard", "status"); code != 0 || env["data"].(map[string]any)["installed"] != true {
		t.Fatalf("status: %d %v", code, env)
	} else {
		validData(t, env)
	}
	// What the hook runs: a push of public main passes, a feature is refused.
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	if code, env := te.runJSON("guard", "check", "origin", "https://github.com/acme/widget.git"); code != 0 {
		t.Fatalf("check main: %d %v", code, env)
	} else {
		validData(t, env)
	}
	te.env.Stdin = strings.NewReader("refs/heads/x 1111111111111111111111111111111111111111 refs/heads/ghostbranch 0000000000000000000000000000000000000000\n")
	code, env = te.runJSON("guard", "check", "origin", "/some/rewritten/path")
	if e, _ := env["error"].(map[string]any); code != 1 || e["code"] != "PUSH_BLOCKED" {
		t.Fatalf("check overlay: %d %v", code, env)
	}
	if code, env := te.runJSON("guard", "uninstall"); code != 0 || env["data"].(map[string]any)["action"] != "removed" {
		t.Fatalf("uninstall: %d %v", code, env)
	} else {
		validData(t, env)
	}
}

func TestGuardOutsideAPairCheckout(t *testing.T) {
	hermeticGit(t)
	te := newTestEnv(t)
	initOnly(te)
	dir := pairCheckout(te)
	if out, err := exec.Command("git", "-C", dir, "remote", "set-url", "origin", "https://github.com/someone/else.git").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	code, env := te.runJSON("guard", "install")
	if e, _ := env["error"].(map[string]any); code != 2 || e["code"] != "NOT_A_PAIR_CHECKOUT" {
		t.Fatalf("%d %v", code, env)
	}
}
