package cli_test

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitOut runs plain git in dir and returns its trimmed stdout, failing the
// test on error.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		msg := ""
		if ee, ok := err.(*exec.ExitError); ok {
			msg = string(ee.Stderr)
		}
		t.Fatalf("git %v: %v\n%s", args, err, msg)
	}
	return strings.TrimSpace(string(out))
}

// publicCheckout clones acme/widget to a checkout whose origin is its GitHub
// URL, and runs feature start from there. git reaches github.com/acme/... in
// the fake forge, or at reach when given, through insteadOf rules in the
// checkout's own config (longer than any a global config would have, so
// they win).
func publicCheckout(te *testEnv, reach ...string) string {
	te.t.Helper()
	t := te.t
	dir := filepath.Join(te.root, "work")
	gitOut(t, te.root, "clone", "-q", te.forge.RepoDir("acme/widget"), dir)
	gitOut(t, dir, "remote", "set-url", "origin", "https://github.com/acme/widget.git")
	base := filepath.ToSlash(filepath.Dir(te.forge.RepoDir("acme/widget"))) + "/"
	if len(reach) > 0 {
		base = reach[0]
	}
	for _, from := range []string{"https://github.com/acme/", "git@github.com:acme/", "ssh://git@github.com/acme/"} {
		gitOut(t, dir, "config", "--add", "url."+base+".insteadOf", from)
	}
	te.env.Getwd = func() (string, error) { return dir, nil }
	return dir
}

// startIn runs feature start feat-x --json and returns the exit code, the
// envelope and data.local.
func startIn(te *testEnv) (int, map[string]any, map[string]any) {
	te.t.Helper()
	return startInNamed(te, "feat-x")
}

func wantLocal(t *testing.T, local map[string]any, status, reason, remote string) {
	t.Helper()
	if local["status"] != status || local["reason"] != reason || local["remote"] != remote {
		t.Fatalf("local = %v, want %s/%q/%q", local, status, reason, remote)
	}
}

func head(t *testing.T, dir string) string {
	t.Helper()
	return gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func TestFeatureStartSwitchesInACheckout(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if got := head(t, dir); got != "feat-x" {
		t.Fatalf("HEAD is %s", got)
	}
	if up := gitOut(t, dir, "rev-parse", "--abbrev-ref", "feat-x@{upstream}"); up != "shadow/feat-x" {
		t.Fatalf("upstream is %s", up)
	}
	if u := gitOut(t, dir, "config", "remote.shadow.url"); u != "https://github.com/acme/widget-shadow.git" {
		t.Fatalf("shadow remote is %s", u)
	}
	if !strings.Contains(out, "Added remote shadow (https://github.com/acme/widget-shadow.git).") {
		t.Fatalf("stdout does not say the remote was added:\n%s", out)
	}
	sha := gitOut(t, dir, "rev-parse", "HEAD")
	want := "Switched to feat-x (from shadow main " + sha[:12] + "). Commit here, then: git push shadow feat-x"
	if !strings.Contains(out, want) {
		t.Fatalf("stdout lacks %q:\n%s", want, out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatalf("colour in piped output:\n%q", out)
	}
	// At a terminal, what the person does next stands out; NO_COLOR and
	// --json turn that off.
	gitOut(t, dir, "switch", "-q", "main")
	te.env.StdoutColor = true
	_, out, _ = te.run("feature", "start", "feat-y")
	if !strings.Contains(out, "\x1b[") || !strings.Contains(out, "git push shadow feat-y") {
		t.Fatalf("no highlight at a terminal:\n%q", out)
	}
	gitOut(t, dir, "switch", "-q", "main")
	te.vars["NO_COLOR"] = "1"
	if _, out, _ = te.run("feature", "start", "feat-z"); strings.Contains(out, "\x1b[") {
		t.Fatalf("colour with NO_COLOR:\n%q", out)
	}
	delete(te.vars, "NO_COLOR")
	gitOut(t, dir, "switch", "-q", "main")
	if _, out, _ = te.run("feature", "start", "feat-w", "--json"); strings.Contains(out, "\x1b[") {
		t.Fatalf("colour in --json:\n%q", out)
	}
}

func TestFeatureStartReusesAnExistingShadowRemote(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "remote", "add", "gb", "https://github.com/acme/widget-shadow.git")
	code, env, local := startIn(te)
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "switched", "", "gb")
	if r := gitOut(t, dir, "remote"); r != "gb\norigin" {
		t.Fatalf("remotes are %q; no shadow remote should be added", r)
	}
	if up := gitOut(t, dir, "rev-parse", "--abbrev-ref", "feat-x@{upstream}"); up != "gb/feat-x" {
		t.Fatalf("upstream is %s", up)
	}
	next, _ := env["next"].([]any)
	if len(next) == 0 || !strings.Contains(next[0].(map[string]any)["why"].(string), "git push gb feat-x") {
		t.Fatalf("next = %v", next)
	}
}

func TestFeatureStartKeepsAnotherShadowRemote(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "remote", "add", "shadow", "https://github.com/acme/other.git")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if u := gitOut(t, dir, "config", "remote.shadow.url"); u != "https://github.com/acme/other.git" {
		t.Fatalf("the shadow remote was rewritten to %s", u)
	}
	if head(t, dir) != "main" {
		t.Fatal("switched with the wrong shadow remote")
	}
	for _, want := range []string{"https://github.com/acme/other.git", "https://github.com/acme/widget-shadow.git", "left it alone"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	code, env, local := startInNamed(te, "feat-y")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "skipped", "remote_differs", "shadow")
}

func TestFeatureStartAcceptsAnSSHShadowRemote(t *testing.T) {
	t.Parallel()
	for _, u := range []string{"git@github.com:acme/widget-shadow.git", "ssh://git@github.com/acme/widget-shadow", "https://github.com/acme/widget-shadow"} {
		t.Run(u, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			initOnly(te)
			dir := publicCheckout(te)
			gitOut(t, dir, "remote", "add", "shadow", u)
			code, env, local := startIn(te)
			if code != 0 {
				t.Fatalf("exit %d: %v", code, env)
			}
			wantLocal(t, local, "switched", "", "shadow")
			if got := gitOut(t, dir, "config", "remote.shadow.url"); got != u {
				t.Fatalf("the remote was rewritten to %s", got)
			}
			if head(t, dir) != "feat-x" {
				t.Fatal("not switched")
			}
		})
	}
}

// snapshot is every file under dir outside .git, by path.
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	m := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			m[p] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFeatureStartDirtyTreeFetchesOnly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, file, says string }{
		{"modified", "README.md", "commit or stash first, then `git switch feat-x`"},
		{"untracked", "notes.txt", "stash them with `git stash --include-untracked`, then `git switch feat-x`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			te := newTestEnv(t)
			initOnly(te)
			dir := publicCheckout(te)
			if err := os.WriteFile(filepath.Join(dir, tc.file), []byte("work in progress\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			before, status := snapshot(t, dir), gitOut(t, dir, "status", "--porcelain")
			code, out, errOut := te.run("feature", "start", "feat-x")
			if code != 0 {
				t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
			}
			after := snapshot(t, dir)
			if len(after) != len(before) {
				t.Fatalf("files changed: %v -> %v", before, after)
			}
			for p, b := range before {
				if after[p] != b {
					t.Fatalf("%s changed", p)
				}
			}
			if head(t, dir) != "main" || gitOut(t, dir, "status", "--porcelain") != status {
				t.Fatal("the checkout moved")
			}
			gitOut(t, dir, "rev-parse", "--verify", "shadow/feat-x") // fetched
			if !strings.Contains(out, tc.says) {
				t.Fatalf("stdout:\n%s", out)
			}
			_, _, local := startInNamed(te, "feat-y")
			wantLocal(t, local, "fetched_only", "dirty_tree", "shadow")
		})
	}
}

func TestFeatureStartDetachedHeadSwitches(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "switch", "-q", "--detach", "HEAD")
	code, env, local := startIn(te)
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "switched", "", "shadow")
	if head(t, dir) != "feat-x" {
		t.Fatal("not switched")
	}
}

func TestFeatureStartBranchExistsLocally(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "branch", "feat-x")
	was := gitOut(t, dir, "rev-parse", "feat-x")
	code, env, local := startIn(te)
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "fetched_only", "branch_exists_locally", "shadow")
	if head(t, dir) != "main" || gitOut(t, dir, "rev-parse", "feat-x") != was {
		t.Fatal("the local branch or HEAD moved")
	}
	gitOut(t, dir, "rev-parse", "--verify", "shadow/feat-x")
}

func TestFeatureStartNoCheckoutFlag(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	code, out, errOut := te.run("feature", "start", "feat-x", "--no-checkout")
	if code != 0 || !strings.Contains(out, "Check it out with: git fetch shadow && git switch feat-x") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if r := gitOut(t, dir, "remote"); r != "origin" || head(t, dir) != "main" {
		t.Fatalf("the checkout was touched: remotes %q", r)
	}
	code, env := te.runJSON("feature", "start", "feat-y", "--no-checkout")
	data, _ := env["data"].(map[string]any)
	local, _ := data["local"].(map[string]any)
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "skipped", "no_checkout_flag", "")
}

func TestFeatureStartOutsideACheckout(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 || !strings.Contains(out, "Check it out with: git fetch shadow && git switch feat-x") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	_, _, local := startInNamed(te, "feat-y")
	wantLocal(t, local, "skipped", "not_a_checkout", "")
	// A checkout of another repo is not a checkout of the pair's public repo.
	other := filepath.Join(te.root, "other")
	gitOut(t, te.root, "init", "-q", other)
	gitOut(t, other, "remote", "add", "origin", "https://github.com/acme/gadget.git")
	te.env.Getwd = func() (string, error) { return other, nil }
	_, _, local = startInNamed(te, "feat-z")
	wantLocal(t, local, "skipped", "not_a_checkout", "")
	if r := gitOut(t, other, "remote"); r != "origin" {
		t.Fatalf("remotes %q", r)
	}
}

func TestFeatureStartFetchNotFoundExplainsTheAccount(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Repository not found.", http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te, srv.URL+"/acme/")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("a failed fetch is a warning, not an error: exit %d\n%s\n%s", code, out, errOut)
	}
	// acme is an organization: the URL names the login Ghostbranch signed
	// in as (octo), never the org nobody can sign in as.
	for _, want := range []string{"a different GitHub account than the one that can see acme/widget-shadow", "git remote set-url shadow https://octo@github.com/acme/widget-shadow.git", "The branch exists in the shadow repo"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if head(t, dir) != "main" {
		t.Fatal("switched")
	}
	_, env, local := startInNamed(te, "feat-y")
	wantLocal(t, local, "skipped", "fetch_failed", "shadow")
	next, _ := env["next"].([]any)
	if len(next) == 0 || !strings.Contains(next[0].(map[string]any)["why"].(string), "git remote set-url shadow https://octo@github.com/acme/widget-shadow.git") {
		t.Fatalf("next = %v", next)
	}
	if env["ok"] != true {
		t.Fatalf("envelope: %v", env)
	}
	if strings.Contains(out, "https://acme@") {
		t.Fatalf("the org is named as the login:\n%s", out)
	}
	// With GHOSTBRANCH_ACTOR set, Ghostbranch never asked GitHub who it is:
	// the person fills in their own login.
	te.vars["GHOSTBRANCH_ACTOR"] = "agent:test"
	_, out, _ = te.run("feature", "start", "feat-z")
	if !strings.Contains(out, "git remote set-url shadow https://<your-github-login>@github.com/acme/widget-shadow.git") {
		t.Fatalf("no placeholder:\n%s", out)
	}
}

func TestFeatureStartOtherFetchFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te, srv.URL+"/acme/")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{"git fetch shadow failed: ", "Once git can reach the shadow repo: git fetch shadow && git switch feat-x"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "set-url") || head(t, dir) != "main" {
		t.Fatalf("not a not-found, or switched:\n%s", out)
	}
	_, _, local := startInNamed(te, "feat-y")
	wantLocal(t, local, "skipped", "fetch_failed", "shadow")
}

// A detached HEAD whose commits nothing else reaches stays where it is: a
// switch would strand them.
func TestFeatureStartKeepsDetachedCommits(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "switch", "-q", "--detach", "HEAD")
	gitOut(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "fix on a detached HEAD")
	sha := gitOut(t, dir, "rev-parse", "HEAD")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if gitOut(t, dir, "rev-parse", "HEAD") != sha || head(t, dir) != "HEAD" {
		t.Fatal("HEAD moved")
	}
	for _, want := range []string{"detached at " + sha[:12] + " with 1 commit(s)", "git switch -c <branch-name>"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	_, _, local := startInNamed(te, "feat-y")
	wantLocal(t, local, "fetched_only", "detached_commits", "shadow")
}

func TestFeatureStartSwitchFailed(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	// A merge in progress on a clean tree: git switch refuses.
	gitOut(t, dir, "branch", "side")
	gitOut(t, dir, "switch", "-q", "side")
	gitOut(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "commit", "-q", "--allow-empty", "-m", "side")
	gitOut(t, dir, "switch", "-q", "main")
	gitOut(t, dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "merge", "-q", "--no-ff", "--no-commit", "side")
	code, env, local := startIn(te)
	if code != 0 {
		t.Fatalf("exit %d: %v", code, env)
	}
	wantLocal(t, local, "fetched_only", "switch_failed", "shadow")
	if head(t, dir) != "main" {
		t.Fatal("switched")
	}
}

func TestFeatureStartRemoteAddFailed(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	// A shadow remote with no URL: not a remote Ghostbranch can read, and
	// git remote add says it exists.
	gitOut(t, dir, "config", "remote.shadow.fetch", "+refs/heads/*:refs/remotes/shadow/*")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	if !strings.Contains(out, "git remote add shadow failed: ") || strings.Contains(out, "git fetch shadow failed") {
		t.Fatalf("stdout:\n%s", out)
	}
	_, _, local := startInNamed(te, "feat-y")
	wantLocal(t, local, "skipped", "fetch_failed", "shadow")
}

// A token in a remote's URL never reaches the output.
func TestFeatureStartRedactsRemoteURLs(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	dir := publicCheckout(te)
	gitOut(t, dir, "remote", "add", "shadow", "https://user:ghp_secret@github.com/acme/other.git")
	code, out, errOut := te.run("feature", "start", "feat-x")
	if code != 0 || !strings.Contains(out, "points at https://github.com/acme/other.git,") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	code, raw, _ := te.run("feature", "start", "feat-y", "--json")
	if code != 0 || !strings.Contains(raw, `"remote_differs"`) {
		t.Fatalf("exit %d\n%s", code, raw)
	}
	for _, o := range []string{out, errOut, raw} {
		if strings.Contains(o, "ghp_secret") || strings.Contains(o, "user:") {
			t.Fatalf("the token leaked:\n%s", o)
		}
	}
}

// startInNamed is startIn for feature name.
func startInNamed(te *testEnv, name string) (int, map[string]any, map[string]any) {
	te.t.Helper()
	code, env := te.runJSON("feature", "start", name)
	data, _ := env["data"].(map[string]any)
	local, _ := data["local"].(map[string]any)
	return code, env, local
}
