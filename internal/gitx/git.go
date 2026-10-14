// Package gitx runs the git CLI. It knows nothing about features, forges,
// or Ghostbranch's rules; it only makes git calls predictable.
package gitx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// isolation is a private directory, made once per process, that holds the
// paths git is pointed at instead of the user's global config, hooks and
// attributes: an empty hooks directory and two files that never exist.
// These paths must never be under g.Dir: in a worktree, g.Dir is the root of
// a checkout whose content is attacker-controlled (feature content, or
// public main), so a tracked file there would become config (core.fsmonitor
// runs commands), hooks or attributes. /dev/null is not used for the hooks
// directory because Git for Windows resolves "/dev/null/<hook>" against the
// current drive (<drive>:/dev/null/<hook>) rather than as the null device.
// Cleanup removes it.
type isolation struct {
	hooks, globalConfig, attributes string
}

// iso is the process's isolation directory, made on the first git call and
// removed by Cleanup.
var iso struct {
	mu  sync.Mutex
	dir string
	val isolation
}

func isolated() (isolation, error) {
	iso.mu.Lock()
	defer iso.mu.Unlock()
	if iso.dir != "" {
		return iso.val, nil
	}
	dir, err := os.MkdirTemp("", "ghostbranch-git-") // created 0700
	if err != nil {
		return isolation{}, fmt.Errorf("make git isolation directory: %w", err)
	}
	v := isolation{
		hooks:        filepath.Join(dir, "hooks"),
		globalConfig: filepath.Join(dir, "no-global-config"),
		attributes:   filepath.Join(dir, "no-attributes"),
	}
	if err := os.Mkdir(v.hooks, 0o700); err != nil {
		_ = os.RemoveAll(dir)
		return isolation{}, fmt.Errorf("make git isolation directory: %w", err)
	}
	iso.dir, iso.val = dir, v
	return v, nil
}

// Cleanup removes the isolation directory, if this process made one. Call it
// once no git command is running: main calls it after the command returns,
// and test binaries call it from TestMain (testutil.Main). A git call after
// Cleanup makes a new directory.
func Cleanup() error {
	iso.mu.Lock()
	defer iso.mu.Unlock()
	if iso.dir == "" {
		return nil
	}
	err := os.RemoveAll(iso.dir)
	iso.dir, iso.val = "", isolation{}
	return err
}

// overrides neutralize user and system config that would change content
// (autocrlf, attributes), block non-interactive runs (signing, credential
// prompts), run user code (hooks, fsmonitor), or resolve conflicts from
// recorded resolutions (rerere).
func overrides(iso isolation) [][2]string {
	return [][2]string{
		{"core.autocrlf", "false"},
		{"core.safecrlf", "false"},
		{"core.hooksPath", iso.hooks},
		{"core.attributesFile", iso.attributes},
		{"commit.gpgSign", "false"},
		{"tag.gpgSign", "false"},
		{"credential.helper", ""},
		{"advice.detachedHead", "false"},
		{"init.defaultBranch", "main"},
		{"rebase.autoSquash", "false"},
		{"rebase.autoStash", "false"},
		{"rebase.updateRefs", "false"},
		{"gc.auto", "0"},
		{"core.fsmonitor", "false"},
		{"rerere.enabled", "false"},
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

func (g *Git) environ() ([]string, error) {
	iso, err := isolated()
	if err != nil {
		return nil, err
	}
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
	cfg := append(overrides(iso), g.Config...)
	// The user's global config (~/.gitconfig and the XDG file) is ignored
	// wholesale by pointing GIT_CONFIG_GLOBAL at a file that never exists:
	// url.*.insteadOf, url.*.pushInsteadOf, core.sshCommand, http.proxy and
	// the like there could redirect a push, bypass the token header, or hang
	// on a prompt. System config stays: Git for Windows keeps its TLS CA
	// settings there, and the overrides above neutralize the rest.
	env = append(env, "GIT_CONFIG_GLOBAL="+iso.globalConfig,
		"GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_CONFIG_COUNT="+strconv.Itoa(len(cfg)))
	for i, kv := range cfg {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, kv[0]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, kv[1]))
	}
	return append(env, g.Env...), nil
}

func (g *Git) runRaw(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	env, err := g.environ()
	if err != nil {
		return nil, err
	}
	cmd.Env = env
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

// Init creates a repository at dir, which may not exist yet. A mirror holds
// unreleased shadow commits (F-28), so the repo is private whatever the
// umask: its directory and every parent Init creates are 0700, the files
// git init wrote lose their group and other bits, and
// core.sharedRepository=0600 makes git write every later file 0600 and
// directory 0700. The key is set after init rather than with --shared,
// which would also set receive.denyNonFastForwards and so change how a
// bare repo made here takes pushes.
func Init(ctx context.Context, dir string, bare bool) (*Git, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	if _, err := g.Run(ctx, "config", "core.sharedRepository", "0600"); err != nil {
		return nil, err
	}
	gitDir := dir
	if !bare {
		gitDir = filepath.Join(dir, ".git")
	}
	if err := privateTree(gitDir); err != nil {
		return nil, err
	}
	return g, nil
}

// privateTree takes group and other bits off everything under root and
// makes its directories 0700. Windows has no mode bits to change.
func privateTree(root string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(p, 0o700)
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if perm := fi.Mode().Perm(); perm&0o077 != 0 {
			return os.Chmod(p, perm&^0o077)
		}
		return nil
	})
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
