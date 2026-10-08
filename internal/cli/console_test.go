package cli_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// An agent's shell: stdin is a pipe with `yes` already in it, so no console
// is usable, the console device is never opened, and --yes alone does not
// publish.
func TestPipedStdinNeverApproves(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, _ = w.WriteString("y\nyes\nABCDEF\n")
	_ = w.Close()
	opened := false
	te.env.Stdin, te.env.ConsoleUsable = r, console.Usable(r, r)
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { opened = true; return nil, errors.New("no console") }
	code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 5 || d["reason"] != "no_console" || opened {
		t.Fatalf("%d %v (console opened: %v)", code, env, opened)
	}
	if _, ok, _ := te.pubGit.Resolve(context.Background(), "refs/heads/feat-x"); ok {
		t.Fatal("published without a person")
	}
}

func TestConsoleApprovalThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	ty := atConsole(te, typeTheCode)
	code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature")
	data, _ := env["data"].(map[string]any)
	applied, _ := data["applied"].(map[string]any)
	appr, _ := applied["approval"].(map[string]any)
	if code != 0 || appr["kind"] != "console" {
		t.Fatalf("%d %v", code, env)
	}
	if !strings.Contains(ty.seen.String(), "About to publish to acme/widget") {
		t.Fatalf("the console must show the summary:\n%s", ty.seen.String())
	}
}

func TestAgentsAllowThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	code, env := te.runJSON("agents", "allow")
	if e, _ := env["error"].(map[string]any); code != 5 || e["code"] != "APPROVAL_REQUIRED" {
		t.Fatalf("without a console: %d %v", code, env)
	}
	atConsole(te, typeTheCode)
	if code, env := te.runJSON("agents", "allow", "--for", "24h"); code != 0 {
		t.Fatalf("at a console: %d %v", code, env)
	}
	te.env.ConsoleUsable, te.env.OpenConsole = false, nil
	if code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--yes"); code != 0 {
		t.Fatalf("under the grant: %d %v", code, env)
	}
	if code, env := te.runJSON("agents", "revoke"); code != 0 {
		t.Fatalf("revoke: %d %v", code, env)
	}
}

// --non-interactive means never ask: even with a usable console, nothing is
// shown there and the publish needs approval.
func TestNonInteractiveNeverAsksAtTheConsole(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	gatedFeature(te)
	ty := atConsole(te, typeTheCode)
	code, env := te.runJSON("promote", "feat-x", "--mode", "squash", "-m", "Add feature", "--non-interactive")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 5 || d["reason"] != "no_console" {
		t.Fatalf("%d %v", code, env)
	}
	if ty.seen.Len() != 0 {
		t.Fatalf("the console was used:\n%s", ty.seen.String())
	}
	if _, ok, _ := te.pubGit.Resolve(context.Background(), "refs/heads/feat-x"); ok {
		t.Fatal("published without a person")
	}
}

func TestWaiversAddThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	leakyWithID(te)
	ty := atConsole(te, typeTheCode)
	if code, out, errOut := te.run("waivers", "add", "feat-x", te.finding, "--reason", "test fixture"); code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(ty.seen.String(), te.finding) || strings.Contains(ty.seen.String(), testutil.FakeGitHubToken()) {
		t.Fatalf("the console must show the finding but never the secret:\n%s", ty.seen.String())
	}
	te.env.ConsoleUsable, te.env.OpenConsole = false, nil
	te.mustRun("gate", "feat-x")
}

func TestPipedStdinNeverWaives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	leakyWithID(te)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, _ = w.WriteString("y\nyes\nABCDEF\n")
	_ = w.Close()
	opened := false
	te.env.Stdin, te.env.ConsoleUsable = r, console.Usable(r, r)
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { opened = true; return nil, errors.New("no console") }
	code, env := te.runJSON("waivers", "add", "feat-x", te.finding, "--reason", "fixture")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 5 || d["reason"] != "no_console" || opened {
		t.Fatalf("%d %v (console opened: %v)", code, env, opened)
	}
	if _, err := os.Stat(te.waiversFile()); !os.IsNotExist(err) {
		t.Fatal("a waiver was written without a person")
	}
}

func TestNonInteractiveNeverWaives(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	leakyWithID(te)
	ty := atConsole(te, typeTheCode)
	code, env := te.runJSON("waivers", "add", "feat-x", te.finding, "--reason", "fixture", "--non-interactive")
	e, _ := env["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	if code != 5 || d["reason"] != "no_console" || ty.seen.Len() != 0 {
		t.Fatalf("%d %v; console used: %q", code, env, ty.seen.String())
	}
	if _, err := os.Stat(te.waiversFile()); !os.IsNotExist(err) {
		t.Fatal("a waiver was written without a person")
	}
}
