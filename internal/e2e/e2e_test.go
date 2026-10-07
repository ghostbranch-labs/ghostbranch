//go:build e2e

// Package e2e_test runs the whole loop against real GitHub, using the fixed
// sandbox repos ghostbranch-labs/e2e-public and e2e-shadow. It pushes to
// them, so run it only with the owner's go-ahead.
//
// The token needs Actions: read and write on e2e-shadow. TestSandboxSyncWorkflow
// dispatches the opt-in sync workflow and disables it again, which read access
// alone cannot do.
//
//	GHOSTBRANCH_E2E_TOKEN=... go test -tags e2e -count=1 -timeout 45m -v ./internal/e2e/
package e2e_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
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
	// A job guarded to the public repo: the gate rewrites the guard, so it runs
	// on the shadow repo too instead of being skipped.
	".github/workflows/guarded.yml": `name: guarded
on:
  pull_request:
jobs:
  upstream-only:
    if: github.repository == 'ghostbranch-labs/e2e-public'
    runs-on: ubuntu-latest
    steps:
      - run: echo "ran in $GITHUB_REPOSITORY"
`,
}

const syncWorkflowFile = "ghostbranch-sync.yml"

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

// newRunner returns a function that drives the CLI in-process against a fresh
// GHOSTBRANCH_HOME and returns the parsed JSON envelope, failing the test when
// the exit code is not want. It also returns the home directory.
func newRunner(t *testing.T, tok string) (func(want int, args ...string) map[string]any, string) {
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
	return func(want int, args ...string) map[string]any {
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
	}, home
}

func TestSandboxLoop(t *testing.T) {
	tok := token(t)
	ctx := context.Background()
	reset(t, tok)

	run, home := newRunner(t, tok)
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
	commit(feat, map[string]string{"hello.txt": "hello from " + runID + "\n", ".ghostbranch/notes.md": "private notes\n",
		"internal.txt": "public line\n# BEGIN-INTERNAL\ninternal line\n# END-INTERNAL\n"})
	gateRes := run(0, "gate", feat)["data"].(map[string]any)
	// The public workflow's guarded job must have run on the candidate: the
	// gate rewrote its repo guard, so it is a success, not a skip.
	guards, _ := gateRes["repo_guards"].(map[string]any)
	if rw, _ := guards["rewritten"].([]any); len(rw) != 1 {
		t.Fatalf("want one rewritten repo guard: %v", gateRes["repo_guards"])
	}
	candidate, _ := gateRes["candidate"].(string)
	checks, err := ghforge.New(tok).ListChecks(ctx, shadowRepo, candidate)
	if err != nil {
		t.Fatal(err)
	}
	var sawGuarded bool
	for _, c := range checks {
		if !strings.Contains(c.Name, "upstream-only") && !strings.Contains(c.Name, "guarded") {
			continue
		}
		sawGuarded = true
		if c.Conclusion != "success" {
			t.Errorf("guarded check %q: status %q conclusion %q, want success (the guard was not rewritten, or the job was skipped)", c.Name, c.Status, c.Conclusion)
		}
	}
	if !sawGuarded {
		t.Fatalf("no check named upstream-only or guarded on candidate %s: %+v", candidate, checks)
	}
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
	if blob, ok, err := g.ShowFile(ctx, "refs/heads/published", "internal.txt"); err != nil || !ok || string(blob) != "public line\n" {
		t.Fatalf("internal.txt was published as %q (%v)", blob, err)
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

	// A codename the overlay config blocks is refused, then published once a
	// person has waived it. The waiver is written the way waivers add writes
	// it after the console code; the binary has no other way.
	cfgText := strings.Replace(string(config.Render(publicRepo, shadowRepo)), "block_patterns: []", `block_patterns: ["project-falcon"]`, 1)
	if !strings.Contains(cfgText, "project-falcon") {
		t.Fatal("config.Render no longer has an empty block_patterns line to replace")
	}
	commit(engine.OverlayBranch, map[string]string{".ghostbranch.yml": cfgText})
	named := "named-" + runID
	run(0, "feature", "start", named)
	commit(named, map[string]string{"docs/codename.txt": "shipping project-falcon\n"})
	m = run(3, "gate", named)
	ids, _ := m["error"].(map[string]any)["details"].(map[string]any)["waivable"].([]any)
	if len(ids) != 1 {
		t.Fatalf("want one waivable finding: %v", m)
	}
	sum := sha256.Sum256([]byte("project-falcon"))
	if err := waiver.NewStore(filepath.Join(ws, "waivers.json"), filepath.Join(ws, "waivers.lock")).Add([]waiver.Waiver{{
		ID: ids[0].(string), Public: publicRepo, Rule: leak.BlockRule, Where: "tree", Path: "docs/codename.txt",
		MatchSHA256: hex.EncodeToString(sum[:]), Reason: "e2e: the codename is public", CreatedAt: now, CreatedBy: "human:e2e"}}, now); err != nil {
		t.Fatal(err)
	}
	run(0, "gate", named)
	res = run(0, "promote", named, "--mode", "squash", "-m", "Add the codename for "+runID, "--plan")
	planID = res["data"].(map[string]any)["plan"].(map[string]any)["id"].(string)
	res = run(0, "apply", planID, "--yes")
	if w, _ := res["data"].(map[string]any)["waived"].([]any); len(w) != 1 {
		t.Fatalf("apply must report the waived finding: %v", res["data"])
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

// fetchOverlay fetches the shadow overlay branch into g.
func fetchOverlay(t *testing.T, g *gitx.Git) {
	t.Helper()
	if err := g.Fetch(context.Background(), "shadow", "+refs/heads/"+engine.OverlayBranch+":refs/heads/"+engine.OverlayBranch); err != nil {
		t.Fatal(err)
	}
}

// overlayHasSyncWorkflow fetches the shadow overlay branch into g and reports
// whether it holds the managed sync workflow file.
func overlayHasSyncWorkflow(t *testing.T, g *gitx.Git) bool {
	t.Helper()
	fetchOverlay(t, g)
	_, ok, err := g.ShowFile(context.Background(), "refs/heads/"+engine.OverlayBranch, managed.SyncPath)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// editOverlayConfig rewrites .ghostbranch.yml on the shadow overlay branch with
// edit and pushes it with a lease on the branch's old tip, so a concurrent
// change is never overwritten. edit must change the text.
func editOverlayConfig(t *testing.T, tok string, edit func(cfg string) string) {
	t.Helper()
	ctx := context.Background()
	g := authed(t, tok)
	fetchOverlay(t, g)
	ref := "refs/heads/" + engine.OverlayBranch
	old, ok, err := g.Resolve(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("overlay branch missing: %v", err)
	}
	cfg, ok, err := g.ShowFile(ctx, ref, ".ghostbranch.yml")
	if err != nil || !ok {
		t.Fatalf("overlay has no .ghostbranch.yml: %v", err)
	}
	edited := edit(string(cfg))
	if edited == string(cfg) {
		t.Fatal("the config edit changed nothing")
	}
	testutil.Commit(t, g, engine.OverlayBranch, "e2e: edit the overlay config", map[string]string{".ghostbranch.yml": edited})
	if err := g.Push(ctx, "shadow", gitx.PushOptions{Leases: []gitx.Lease{{Ref: ref, Expect: old}}}, ref+":"+ref); err != nil {
		t.Fatal(err)
	}
}

// replaceOnce replaces the single occurrence of from in text with to.
func replaceOnce(t *testing.T, text, from, to string) string {
	t.Helper()
	if n := strings.Count(text, from); n != 1 {
		t.Fatalf("want exactly one %q in the config, found %d", from, n)
	}
	return strings.Replace(text, from, to, 1)
}

// pushPublicReadme adds one commit that touches only README.md on public main
// and returns its SHA.
func pushPublicReadme(t *testing.T, tok, text string) string {
	t.Helper()
	ctx := context.Background()
	g := authed(t, tok)
	if err := g.Fetch(ctx, "public", "+refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	old, _, err := g.Resolve(ctx, "refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	sha := testutil.Commit(t, g, "main", "e2e: touch the readme", map[string]string{"README.md": text})
	if err := g.Push(ctx, "public", gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/main", Expect: old}}}, "refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	return sha
}

// httpStatus is the HTTP status of a go-github error, or 0 for a network or
// other error that carries none.
func httpStatus(err error) int {
	var er *gh.ErrorResponse
	if errors.As(err, &er) && er.Response != nil {
		return er.Response.StatusCode
	}
	return 0
}

// dispatchSyncWorkflow enables (a workflow disabled by an earlier failed run's
// cleanup stays disabled when its file is re-added) and dispatches the sync
// workflow on the overlay branch, and returns the ID of the run it started.
// GitHub can take a few seconds to register a just-pushed workflow, so 404 and
// 422 are retried for a minute; a 401 or 403 means the token cannot write
// Actions and fails at once.
func dispatchSyncWorkflow(t *testing.T, ghc *gh.Client, owner, repo string) int64 {
	t.Helper()
	ctx := context.Background()
	wantDetails := true
	const hint = "the token needs Actions: read and write on " + shadowRepo
	var last error
	for i := 0; i < 6; i++ {
		if i > 0 {
			time.Sleep(10 * time.Second)
		}
		if _, err := ghc.Actions.EnableWorkflowByFileName(ctx, owner, repo, syncWorkflowFile); err != nil {
			switch httpStatus(err) {
			case http.StatusUnauthorized, http.StatusForbidden:
				t.Fatalf("enabling %s: %v (%s)", syncWorkflowFile, err, hint)
			}
			// 404: not registered yet. Anything else: let the dispatch report it.
		}
		details, _, err := ghc.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, syncWorkflowFile,
			gh.CreateWorkflowDispatchEventRequest{Ref: engine.OverlayBranch, ReturnRunDetails: &wantDetails})
		if err == nil {
			if details == nil || details.WorkflowRunID == nil {
				t.Fatalf("GitHub accepted the dispatch but returned no run ID (return_run_details): %+v", details)
			}
			return *details.WorkflowRunID
		}
		last = err
		switch httpStatus(err) {
		case http.StatusNotFound, http.StatusUnprocessableEntity:
		case http.StatusUnauthorized, http.StatusForbidden:
			t.Fatalf("dispatching %s: %v (%s)", syncWorkflowFile, err, hint)
		default:
			t.Fatalf("dispatching %s: %v", syncWorkflowFile, err)
		}
	}
	t.Fatalf("dispatching %s kept failing; the workflow may be disabled or not registered yet: %v (%s)", syncWorkflowFile, last, hint)
	return 0
}

// waitForRun polls a workflow run every 15 s for up to 5 minutes until it is
// completed. Up to 3 consecutive transient errors (network, 5xx) are
// tolerated.
func waitForRun(t *testing.T, ghc *gh.Client, owner, repo string, id int64) *gh.WorkflowRun {
	t.Helper()
	ctx := context.Background()
	var last *gh.WorkflowRun
	failures := 0
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); {
		time.Sleep(15 * time.Second)
		r, _, err := ghc.Actions.GetWorkflowRunByID(ctx, owner, repo, id)
		if err != nil {
			if st := httpStatus(err); st != 0 && st < 500 {
				t.Fatalf("reading sync run %d: %v", id, err)
			}
			if failures++; failures > 3 {
				t.Fatalf("reading sync run %d failed %d times in a row: %v", id, failures, err)
			}
			continue
		}
		failures = 0
		last = r
		if r.GetStatus() == "completed" {
			return r
		}
	}
	if last == nil {
		t.Fatalf("sync run %d could not be read within 5 minutes", id)
	}
	t.Fatalf("sync run %s did not complete within 5 minutes (last status %q)", last.GetHTMLURL(), last.GetStatus())
	return nil
}

// upgradeHas reports whether an upgrade result lists the sync workflow with
// the given action.
func upgradeHas(up map[string]any, action string) bool {
	files, _ := up["files"].([]any)
	for _, f := range files {
		m, _ := f.(map[string]any)
		if m["path"] == managed.SyncPath && m["action"] == action {
			return true
		}
	}
	return false
}

// TestSandboxSyncWorkflow covers the opt-in sync workflow: init does not
// install it, a configured schedule makes upgrade add it, a dispatched run
// fast-forwards shadow main to public main, and turning the schedule off makes
// upgrade remove it. It runs on its own reset sandbox, after TestSandboxLoop.
func TestSandboxSyncWorkflow(t *testing.T) {
	tok := token(t)
	ctx := context.Background()
	reset(t, tok)
	run, _ := newRunner(t, tok)

	ghc, err := gh.NewClient(gh.WithAuthToken(tok))
	if err != nil {
		t.Fatal(err)
	}
	owner, repo, _ := strings.Cut(shadowRepo, "/")

	// A failed run must not leave a scheduled workflow burning Actions
	// minutes: if the file is still on the overlay at the end, disable it.
	cleanupRepo := authed(t, tok)
	t.Cleanup(func() {
		ctx := context.Background()
		refs, err := cleanupRepo.LsRemote(ctx, "shadow")
		if err != nil {
			t.Errorf("cleanup: cannot read %s, the sync workflow may still be scheduled: %v", shadowRepo, err)
			return
		}
		if _, ok := refs["refs/heads/"+engine.OverlayBranch]; !ok {
			return // init never got as far as creating the overlay
		}
		if err := cleanupRepo.Fetch(ctx, "shadow", "+refs/heads/"+engine.OverlayBranch+":refs/heads/"+engine.OverlayBranch); err != nil {
			t.Errorf("cleanup: cannot read the overlay, the sync workflow may still be scheduled: %v", err)
			return
		}
		_, present, err := cleanupRepo.ShowFile(ctx, "refs/heads/"+engine.OverlayBranch, managed.SyncPath)
		if err != nil {
			t.Errorf("cleanup: %v", err)
			return
		}
		if !present {
			return
		}
		if _, err := ghc.Actions.DisableWorkflowByFileName(ctx, owner, repo, syncWorkflowFile); err != nil {
			t.Errorf("cleanup: the sync workflow is still on %s and could not be disabled (the token needs Actions: read and write): %v", shadowRepo, err)
		}
	})

	run(0, "init", publicRepo, "--shadow", shadowRepo, "--adopt")

	// 1. init installs no sync workflow: the schedule defaults to off.
	if overlayHasSyncWorkflow(t, authed(t, tok)) {
		t.Fatalf("init installed %s with the schedule off", managed.SyncPath)
	}

	// 2. Opt in: set a schedule on the overlay, then upgrade adds the file.
	editOverlayConfig(t, tok, func(cfg string) string {
		return replaceOnce(t, cfg, `schedule: "off"`, `schedule: "23 */6 * * *"`)
	})
	up := run(0, "upgrade", "--yes")["data"].(map[string]any)
	if up["changed"] != true || !upgradeHas(up, "add") {
		t.Fatalf("upgrade with a schedule must add %s: %v", managed.SyncPath, up)
	}
	if !overlayHasSyncWorkflow(t, authed(t, tok)) {
		t.Fatalf("%s is not on the overlay after upgrade", managed.SyncPath)
	}

	// 3. Public moves ahead; the dispatched workflow brings shadow main along.
	publicHead := pushPublicReadme(t, tok, "# e2e-public\n\nTouched at "+time.Now().UTC().Format(time.RFC3339)+".\n")
	refs, err := authed(t, tok).LsRemote(ctx, "shadow")
	if err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/main"] == publicHead {
		t.Fatal("shadow main already equals public main before the workflow ran")
	}
	finished := waitForRun(t, ghc, owner, repo, dispatchSyncWorkflow(t, ghc, owner, repo))
	if finished.GetConclusion() != "success" {
		t.Fatalf("sync run %s: conclusion %q, want success", finished.GetHTMLURL(), finished.GetConclusion())
	}
	refs, err = authed(t, tok).LsRemote(ctx, "shadow")
	if err != nil {
		t.Fatal(err)
	}
	if refs["refs/heads/main"] != publicHead {
		t.Fatalf("shadow main = %s after the sync run, want public main %s", refs["refs/heads/main"], publicHead)
	}

	// 4. Opt out: schedule off, and upgrade removes the file.
	editOverlayConfig(t, tok, func(cfg string) string {
		return replaceOnce(t, cfg, `schedule: "23 */6 * * *"`, `schedule: "off"`)
	})
	up = run(0, "upgrade", "--yes")["data"].(map[string]any)
	if up["changed"] != true || !upgradeHas(up, "remove") {
		t.Fatalf("upgrade with the schedule off must remove %s: %v", managed.SyncPath, up)
	}
	if overlayHasSyncWorkflow(t, authed(t, tok)) {
		t.Fatalf("%s is still on the overlay after upgrade removed it", managed.SyncPath)
	}
}
