package managed_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// findBash prefers Git for Windows' bash over WSL's System32 bash.exe. Elsewhere
// it is whatever bash is on PATH (3.2 on macOS, which the script supports).
func findBash(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("git", "--exec-path").Output()
		if err == nil {
			dir := filepath.Clean(strings.TrimSpace(string(out)))
			for i := 0; i < 5; i++ {
				for _, c := range []string{filepath.Join(dir, "bin", "bash.exe"), filepath.Join(dir, "usr", "bin", "bash.exe")} {
					if _, err := os.Stat(c); err == nil {
						return c
					}
				}
				dir = filepath.Dir(dir)
			}
		}
		t.Skip("Git for Windows bash not found")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	return p
}

// shells are the two ways the script runs: as written, and the way Actions
// runs a run: step (bash -eo pipefail). The outcomes must not differ.
var shells = []struct {
	name string
	args []string
}{
	{"plain", []string{"--noprofile", "--norc"}},
	{"errexit", []string{"--noprofile", "--norc", "-eo", "pipefail"}},
}

type syncEnv struct {
	t             *testing.T
	pub, sh       *gitx.Git
	pubDir, shDir string
	summary       string
	shell         []string
}

func newSyncEnv(t *testing.T, shell []string) *syncEnv {
	t.Helper()
	root := t.TempDir()
	pubDir, shDir := filepath.Join(root, "public.git"), filepath.Join(root, "shadow.git")
	return &syncEnv{t: t, pub: testutil.BareRepo(t, pubDir), sh: testutil.BareRepo(t, shDir),
		pubDir: pubDir, shDir: shDir, summary: filepath.Join(root, "summary.md"), shell: shell}
}

// eachShell runs body once per shell, on a fresh environment each time.
func eachShell(t *testing.T, body func(t *testing.T, s *syncEnv)) {
	t.Helper()
	for _, sh := range shells {
		t.Run(sh.name, func(t *testing.T) { body(t, newSyncEnv(t, sh.args)) })
	}
}

// run executes sync.sh for branches and returns its exit code and output.
func (s *syncEnv) run(branches string, publicURL string) (int, string) {
	s.t.Helper()
	script := filepath.Join(s.t.TempDir(), "sync.sh")
	if err := os.WriteFile(script, []byte(managed.Script()), 0o755); err != nil {
		s.t.Fatal(err)
	}
	cmd := exec.Command(findBash(s.t), append(append([]string{}, s.shell...), script)...)
	cmd.Env = append(os.Environ(),
		"PUBLIC_REPO=acme/widget", "BRANCHES="+branches,
		"PUBLIC_URL="+filepath.ToSlash(publicURL), "SHADOW_URL="+filepath.ToSlash(s.shDir),
		"GITHUB_STEP_SUMMARY="+filepath.ToSlash(s.summary), "SYNC_TOKEN=", "DEFAULT_TOKEN=")
	out, err := cmd.CombinedOutput()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		s.t.Fatal(err)
	}
	return code, string(out)
}

// mirror makes shadow's branch b point at public's b, as init's mirror does.
func (s *syncEnv) mirror(b string) {
	s.t.Helper()
	if _, err := s.pub.Run(context.Background(), "push", filepath.ToSlash(s.shDir), "refs/heads/"+b+":refs/heads/"+b); err != nil {
		s.t.Fatal(err)
	}
}

func ref(t *testing.T, g *gitx.Git, b string) string {
	t.Helper()
	sha, _, err := g.Resolve(context.Background(), "refs/heads/"+b)
	if err != nil {
		t.Fatal(err)
	}
	return sha
}

// hasIndented reports whether some line of out starts with four spaces and
// contains want: git's own output, shown under the script's hint.
func hasIndented(out, want string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "    ") && strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func TestSyncScriptOutcomes(t *testing.T) {
	eachShell(t, func(t *testing.T, s *syncEnv) {
		testutil.Commit(t, s.pub, "main", "base", map[string]string{"a": "1\n"})
		s.mirror("main")
		testutil.Commit(t, s.pub, "release", "rel", map[string]string{"r": "1\n"})

		code, out := s.run("main", s.pubDir)
		if code != 0 || !strings.Contains(out, "main: up to date") {
			t.Fatalf("up to date: %d %s", code, out)
		}
		next := testutil.Commit(t, s.pub, "main", "next", map[string]string{"b": "2\n"})
		code, out = s.run("main release", s.pubDir)
		if code != 0 || !strings.Contains(out, "main: fast-forwarded") || !strings.Contains(out, "release: created") {
			t.Fatalf("ff + create: %d %s", code, out)
		}
		if ref(t, s.sh, "main") != next || ref(t, s.sh, "release") != ref(t, s.pub, "release") {
			t.Fatal("shadow refs not moved")
		}
		sum, _ := os.ReadFile(s.summary)
		if !strings.Contains(string(sum), "- main: fast-forwarded") {
			t.Fatalf("summary = %s", sum)
		}
	})
}

func TestSyncScriptRefusesDivergedAndMissing(t *testing.T) {
	eachShell(t, func(t *testing.T, s *syncEnv) {
		testutil.Commit(t, s.pub, "main", "base", map[string]string{"a": "1\n"})
		s.mirror("main")
		extra := testutil.Commit(t, s.sh, "main", "shadow-only", map[string]string{"x": "1\n"})
		testutil.Commit(t, s.pub, "main", "next", map[string]string{"b": "2\n"})
		code, out := s.run("main nope", s.pubDir)
		if code != 1 || !strings.Contains(out, "main: diverged") || !strings.Contains(out, "public repo has no branch nope") {
			t.Fatalf("%d %s", code, out)
		}
		if ref(t, s.sh, "main") != extra {
			t.Fatal("a diverged shadow branch was pushed")
		}
	})
}

// F-19: a private public repo is not something a token can fix here (a
// token that could read it could also write it): the hint says to turn
// scheduled sync off and sync locally, never to add a read token.
func TestSyncScriptPublicUnreadable(t *testing.T) {
	eachShell(t, func(t *testing.T, s *syncEnv) {
		code, out := s.run("main", filepath.Join(t.TempDir(), "missing.git"))
		if code != 1 || !strings.Contains(out, "cannot read the public repo acme/widget: ") ||
			!strings.Contains(out, "scheduled sync isn't supported for a private public repo") ||
			!strings.Contains(out, "run ghostbranch sync --repo acme/widget locally. Nothing was pushed.") ||
			strings.Contains(out, "GHOSTBRANCH_SYNC_TOKEN") {
			t.Fatalf("%d %s", code, out)
		}
	})
}

// Review Focus 5: the echoed git line never carries credentials from a URL.
func TestSyncScriptPublicUnreadableHidesURLCredentials(t *testing.T) {
	eachShell(t, func(t *testing.T, s *syncEnv) {
		code, out := s.run("main", "https://x-access-token:s3cr3t-value@127.0.0.1:9/acme/widget.git")
		if code != 1 || strings.Contains(out, "s3cr3t-value") || !strings.Contains(out, "cannot read the public repo acme/widget: ") {
			t.Fatalf("%d %s", code, out)
		}
	})
}

// A shadow branch that differs from public but cannot be fetched (its commit's
// tree is gone from the shadow, while ls-remote still lists the ref) is
// reported and not pushed over.
func TestSyncScriptShadowFetchFails(t *testing.T) {
	eachShell(t, func(t *testing.T, s *syncEnv) {
		ctx := context.Background()
		testutil.Commit(t, s.pub, "main", "base", map[string]string{"a": "1\n"})
		extra := testutil.Commit(t, s.sh, "main", "shadow-only", map[string]string{"x": "1\n"})
		testutil.Commit(t, s.pub, "main", "next", map[string]string{"b": "2\n"})
		tree, err := s.sh.Run(ctx, "rev-parse", extra+"^{tree}")
		if err != nil {
			t.Fatal(err)
		}
		obj := filepath.Join(s.shDir, "objects", tree[:2], tree[2:])
		if err := os.Chmod(obj, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(obj); err != nil {
			t.Fatalf("the shadow's tree is not a loose object: %v", err)
		}
		code, out := s.run("main", s.pubDir)
		if code != 1 || !strings.Contains(out, "main: could not fetch it from this shadow repo") {
			t.Fatalf("%d %s", code, out)
		}
		if got, _, _ := s.sh.Resolve(ctx, "refs/heads/main"); got != extra {
			t.Fatal("the shadow branch moved")
		}
	})
}

func hook(t *testing.T, repoDir, body string) {
	t.Helper()
	p := filepath.Join(repoDir, "hooks", "pre-receive")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestSyncScriptExplainsRefusedPushes(t *testing.T) {
	for _, tc := range []struct{ name, hook, want, gitSaid string }{
		{"rule", `echo "error: GH013: Repository rule violations found for refs/heads/main." >&2; exit 1`,
			"GitHub refused the push under a repository rule (GH013)", "Repository rule violations found"},
		{"workflow", `echo "refusing to allow a GitHub App to create or update workflow .github/workflows/ci.yml without workflows permission" >&2; exit 1`,
			"change workflow files", "without workflows permission"},
		{"generic", `echo "pre-receive hook declined for reasons of its own" >&2; exit 1`,
			"the push was refused; run `ghostbranch sync` locally", "declined for reasons of its own"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			eachShell(t, func(t *testing.T, s *syncEnv) {
				testutil.Commit(t, s.pub, "main", "base", map[string]string{"a": "1\n"})
				s.mirror("main")
				before := ref(t, s.sh, "main")
				testutil.Commit(t, s.pub, "main", "next", map[string]string{"b": "2\n"})
				hook(t, s.shDir, tc.hook)
				code, out := s.run("main", s.pubDir)
				if code != 1 || !strings.Contains(out, tc.want) {
					t.Fatalf("hook %q: %d %s", tc.hook, code, out)
				}
				if !hasIndented(out, tc.gitSaid) {
					t.Fatalf("git's own text %q is missing or not indented: %s", tc.gitSaid, out)
				}
				if ref(t, s.sh, "main") != before {
					t.Fatal("the refused push moved the ref")
				}
			})
		})
	}
}
