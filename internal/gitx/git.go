// Package gitx runs the git CLI. It knows nothing about features, forges,
// or Ghostbranch's rules; it only makes git calls predictable.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Git runs git against one repository directory (bare repo or worktree).
// Config entries are passed through GIT_CONFIG_* environment variables, so
// values such as auth headers never appear in a process listing.
type Git struct {
	Dir    string
	Config [][2]string
	Env    []string
}

// New returns a Git for dir.
func New(dir string) *Git { return &Git{Dir: dir} }

// With returns a copy of g that runs in dir, keeping config and env.
func (g *Git) With(dir string) *Git {
	c := *g
	c.Dir = dir
	c.Config = append([][2]string(nil), g.Config...)
	c.Env = append([]string(nil), g.Env...)
	return &c
}

// Error is a failed git invocation. Args never contain secrets: credentials
// travel in the environment.
type Error struct {
	Args     []string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (e *Error) Error() string {
	return fmt.Sprintf("git %s: exit %d: %s", strings.Join(e.Args, " "), e.ExitCode, strings.TrimSpace(e.Stderr))
}

// overrides neutralize user and system config that would change content
// (autocrlf, attributes), block non-interactive runs (signing, credential
// prompts), or run user code (hooks).
func (g *Git) overrides() [][2]string {
	return [][2]string{
		{"core.autocrlf", "false"},
		{"core.safecrlf", "false"},
		{"core.hooksPath", filepath.Join(g.Dir, "ghostbranch-no-hooks")},
		{"core.attributesFile", filepath.Join(g.Dir, "ghostbranch-no-attributes")},
		{"commit.gpgSign", "false"},
		{"tag.gpgSign", "false"},
		{"credential.helper", ""},
		{"advice.detachedHead", "false"},
		{"init.defaultBranch", "main"},
		{"rebase.autoSquash", "false"},
		{"rebase.autoStash", "false"},
		{"rebase.updateRefs", "false"},
		{"gc.auto", "0"},
	}
}

// droppedEnv are inherited variables that would redirect git to another
// repository, index, or identity.
var droppedEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_NAMESPACE",
	"GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_AUTHOR_DATE",
	"GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL", "GIT_COMMITTER_DATE",
	"GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
}

func (g *Git) environ() []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		drop := strings.HasPrefix(key, "GIT_CONFIG_KEY_") || strings.HasPrefix(key, "GIT_CONFIG_VALUE_")
		for _, d := range droppedEnv {
			if strings.EqualFold(key, d) {
				drop = true
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	cfg := append(g.overrides(), g.Config...)
	// The user's global config (~/.gitconfig and the XDG file) is ignored
	// wholesale by pointing GIT_CONFIG_GLOBAL at a file that does not exist:
	// url.*.insteadOf, url.*.pushInsteadOf, core.sshCommand, http.proxy and
	// the like there could redirect a push, bypass the token header, or hang
	// on a prompt. System config stays: Git for Windows keeps its TLS CA
	// settings there, and the overrides above neutralize the rest.
	env = append(env, "GIT_CONFIG_GLOBAL="+filepath.Join(g.Dir, "ghostbranch-no-global-config"),
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return append(env, g.Env...)
}

func (g *Git) runRaw(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = g.environ()
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			return nil, fmt.Errorf("run git %s: %w", args[0], err)
		}
		return stdout.Bytes(), &Error{Args: args, ExitCode: ee.ExitCode(), Stdout: stdout.String(), Stderr: stderr.String()}
	}
	return stdout.Bytes(), nil
}

// Run runs git and returns stdout without trailing newlines.
func (g *Git) Run(ctx context.Context, args ...string) (string, error) {
	return g.RunInput(ctx, nil, args...)
}

// RunInput runs git with stdin and returns stdout without trailing newlines.
func (g *Git) RunInput(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	out, err := g.runRaw(ctx, stdin, args...)
	return strings.TrimRight(string(out), "\r\n"), err
}

// Init creates a repository at dir, which may not exist yet.
func Init(ctx context.Context, dir string, bare bool) (*Git, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g := New(dir)
	args := []string{"init", "--quiet"}
	if bare {
		args = append(args, "--bare")
	}
	if _, err := g.Run(ctx, append(args, ".")...); err != nil {
		return nil, err
	}
	return g, nil
}

func exitCode(err error) int {
	var ge *Error
	if errors.As(err, &ge) {
		return ge.ExitCode
	}
	return -1
}

func splitNUL(b []byte) []string {
	var out []string
	for _, p := range bytes.Split(b, []byte{0}) {
		if len(p) > 0 {
			out = append(out, string(p))
		}
	}
	return out
}
