//go:build e2e

// Package e2e_test runs the whole loop against real GitHub, using the fixed
// sandbox repos ghostbranch-labs/e2e-public and e2e-shadow. It pushes to
// them, so run it only with the owner's go-ahead:
//
//	GHOSTBRANCH_E2E_TOKEN=... go test -tags e2e -count=1 -timeout 35m -v ./internal/e2e/
package e2e_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const (
	publicRepo = "ghostbranch-labs/e2e-public"
	shadowRepo = "ghostbranch-labs/e2e-shadow"
)

var baselineFiles = map[string]string{
	"README.md": "# e2e-public\n\nSandbox for Ghostbranch end-to-end tests. It is reset on every run.\n",
	".github/workflows/ci.yml": `name: ci
on:
  push:
    branches: [main]
  pull_request:
jobs:
  check:
    runs-on: ubuntu-latest
    steps:
      - run: echo "sandbox check passed"
`,
}

func token(t *testing.T) string {
	tok := os.Getenv("GHOSTBRANCH_E2E_TOKEN")
	if tok == "" {
		t.Skip("GHOSTBRANCH_E2E_TOKEN is not set")
	}
	return tok
}

// authed is a scratch bare repo with both sandbox remotes and token auth.
func authed(t *testing.T, tok string) *gitx.Git {
	t.Helper()
	g := testutil.BareRepo(t, t.TempDir())
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	g.Config = [][2]string{{"http.https://github.com/.extraheader", "AUTHORIZATION: basic " + cred}}
	ctx := context.Background()
	for name, full := range map[string]string{"public": publicRepo, "shadow": shadowRepo} {
		if err := g.AddRemote(ctx, name, "https://github.com/"+full+".git"); err != nil {
			t.Fatal(err)
		}
	}
	return g
}

// reset puts both sandbox repos back to the baseline: public main is the
// baseline commit and nothing else (public tags are kept); the shadow repo
// holds only main at the baseline (GitHub never lets the last default branch
// be deleted). Deleting a branch closes its PRs.
func reset(t *testing.T, tok string) {
	t.Helper()
	ctx := context.Background()
	g := authed(t, tok)
	baseline := testutil.Commit(t, g, "main", "e2e baseline", baselineFiles)

	deletes := func(remote string, keep func(ref string) bool) []string {
		refs, err := g.LsRemote(ctx, remote)
		if err != nil {
			t.Fatal(err)
		}
		var specs []string
		for ref := range refs {
			if ref == "HEAD" || strings.HasPrefix(ref, "refs/pull/") || strings.HasSuffix(ref, "^{}") || keep(ref) {
				continue
			}
			specs = append(specs, ":"+ref)
		}
		return specs
	}
	pub := append([]string{"+" + baseline + ":refs/heads/main"},
		deletes("public", func(ref string) bool { return ref == "refs/heads/main" || strings.HasPrefix(ref, "refs/tags/") })...)
	if err := g.Push(ctx, "public", gitx.PushOptions{}, pub...); err != nil {
		t.Fatalf("reset public: %v", err)
	}
	if err := g.Push(ctx, "shadow", gitx.PushOptions{}, "+"+baseline+":refs/heads/main"); err != nil {
		t.Fatalf("reset shadow main: %v", err)
	}
	if err := ghforge.New(tok).SetDefaultBranch(ctx, shadowRepo, "main"); err != nil {
		t.Fatalf("reset shadow default branch: %v", err)
	}
	if specs := deletes("shadow", func(ref string) bool { return ref == "refs/heads/main" }); len(specs) > 0 {
		if err := g.Push(ctx, "shadow", gitx.PushOptions{}, specs...); err != nil {
			t.Fatalf("reset shadow refs: %v", err)
		}
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func TestSandboxLoop(t *testing.T) {
	tok := token(t)
	ctx := context.Background()
	reset(t, tok)

	home := t.TempDir()
	vars := map[string]string{"GHOSTBRANCH_HOME": home, "GHOSTBRANCH_TOKEN": tok, "GHOSTBRANCH_ACTOR": "human:e2e"}
	env := cli.DefaultEnv()
	env.Getenv = func(k string) string {
		if v, ok := vars[k]; ok {
			return v
		}
		return os.Getenv(k)
	}
	env.ConsoleUsable = false
	run := func(want int, args ...string) map[string]any {
		t.Helper()
		var out bytes.Buffer
		e := env
		e.Stdout, e.Stderr = &out, testWriter{t}
		code := cli.Run(append(args, "--json"), e)
		var m map[string]any
		if err := json.Unmarshal(out.Bytes(), &m); err != nil {
			t.Fatalf("%v: not JSON: %v\n%s", args, err, out.String())
		}
		if code != want {
			t.Fatalf("%v: exit %d, want %d\n%s", args, code, want, out.String())
		}
		return m
	}
	commit := func(branch string, files map[string]string) {
		t.Helper()
		g := authed(t, tok)
		if err := g.Fetch(ctx, "shadow", "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
		testutil.Commit(t, g, branch, "e2e work on "+branch, files)
		if err := g.Push(ctx, "shadow", gitx.PushOptions{}, "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
	runID := time.Now().UTC().Format("20060102t150405")

	run(0, "init", publicRepo, "--shadow", shadowRepo, "--adopt")
	run(0, "sync")

	feat := "e2e-" + runID
	run(0, "feature", "start", feat)
	commit(feat, map[string]string{"hello.txt": "hello from " + runID + "\n", ".ghostbranch/notes.md": "private notes\n"})
	run(0, "gate", feat)
	res := run(0, "promote", feat, "--mode", "squash", "-m", "Add hello for "+runID, "--plan")
	planID := res["data"].(map[string]any)["plan"].(map[string]any)["id"].(string)
	m := run(5, "apply", planID, "--yes")
	if d := m["error"].(map[string]any)["details"].(map[string]any); d["reason"] != "no_console" {
		t.Fatalf("without a grant or a console, apply must stop: %v", m)
	}
	// A person's grant, written as `agents allow` writes it after the console
	// code; the binary itself has no way to skip that step.
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	ws := filepath.Join(home, "workspaces", "default")
	if _, err := grant.NewStore(filepath.Join(ws, "grants.json"), filepath.Join(ws, "grants.lock")).Put(grant.Grant{
		ID: "g-e2e000", Public: publicRepo, Shadow: shadowRepo, CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:e2e"}, now); err != nil {
		t.Fatal(err)
	}
	res = run(0, "apply", planID, "--yes")
	applied := res["data"].(map[string]any)
	if applied["approval"].(map[string]any)["kind"] != "grant" {
		t.Fatalf("approval = %v", applied["approval"])
	}
	if applied["public_pr"].(float64) < 1 {
		t.Fatalf("no public PR: %v", applied)
	}

	g := authed(t, tok)
	if err := g.Fetch(ctx, "public", "+refs/heads/"+feat+":refs/heads/published"); err != nil {
		t.Fatalf("published branch missing: %v", err)
	}
	es, err := g.ListTree(ctx, "refs/heads/published")
	if err != nil {
		t.Fatal(err)
	}
	set := overlay.NewSet()
	var sawHello bool
	for _, e := range es {
		if set.Match(e.Path) {
			t.Fatalf("overlay path %s reached the public repo", e.Path)
		}
		sawHello = sawHello || e.Path == "hello.txt"
	}
	if !sawHello {
		t.Fatal("hello.txt was not published")
	}

	leaky := "leak-" + runID
	run(0, "feature", "start", leaky)
	commit(leaky, map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
	m = run(3, "gate", leaky)
	if m["error"].(map[string]any)["code"] != "LEAK_FOUND" {
		t.Fatalf("want LEAK_FOUND: %v", m)
	}
	m = run(3, "promote", leaky, "--mode", "squash", "-m", "leak attempt", "--yes")
	if m["error"].(map[string]any)["code"] != "LEAK_FOUND" {
		t.Fatalf("promote of a leaking feature: want LEAK_FOUND: %v", m)
	}
	refs, err := g.LsRemote(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs["refs/heads/"+leaky]; ok {
		t.Fatal("a leaking feature reached the public repo")
	}

	entries, err := ledger.Read(filepath.Join(home, "workspaces", "default", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	ops := map[string]bool{}
	for _, e := range entries {
		ops[e.Op] = true
	}
	for _, op := range []string{"init", "sync", "feature start", "gate", "promote"} {
		if !ops[op] {
			t.Errorf("ledger has no %q entry", op)
		}
	}
}
