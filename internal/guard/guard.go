// Package guard installs and runs a git pre-push hook that refuses to push
// Ghostbranch feature work to the public repo (spec §11). Installing must see
// the user's own git config (core.hooksPath, remote URLs), so it runs plain
// git, not gitx: gitx deliberately hides user config from git.
package guard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

// Marker is the line that identifies a hook Ghostbranch wrote.
const Marker = "# ghostbranch pre-push guard"

// ChainedName is where an existing pre-push hook is kept, and run after the
// guard, while the guard is installed.
const ChainedName = "pre-push.ghostbranch-chained"

// Hook is what the installed script embeds.
type Hook struct {
	Remotes   []string // remote names whose configured URL is a public repo
	Publics   []string // public repos, lowercase owner/repo
	Workspace string
	Binary    string // absolute path, forward slashes
}

// The script never needs ghostbranch for pushes to other remotes, and it
// refuses a public push when it cannot find the binary: a guard that
// silently stops working is worse than none.
const scriptTemplate = `#!/bin/sh
# ghostbranch pre-push guard
# Installed by "ghostbranch guard install"; remove it with "ghostbranch guard uninstall".
# Pushes to the public repo below are checked by "ghostbranch guard check".
# Pushes to any other remote go straight to the chained hook, if there is one.
GB_BIN=@BIN@
GB_WORKSPACE=@WORKSPACE@
GB_REMOTES=@REMOTES@
GB_PUBLIC=@PUBLIC@
chained="$(dirname "$0")/pre-push.ghostbranch-chained"

norm() {
	printf '%s' "$1" | tr 'A-Z' 'a-z' | sed -e 's#/*$##' -e 's#\.git$##'
}

public=no
for r in $GB_REMOTES; do
	if [ "$1" = "$r" ]; then public=yes; fi
done
u1=$(norm "$1")
u2=$(norm "$2")
for p in $GB_PUBLIC; do
	case "$u1" in *github.com/"$p"|*github.com:"$p") public=yes ;; esac
	case "$u2" in *github.com/"$p"|*github.com:"$p") public=yes ;; esac
done

if [ "$public" = no ]; then
	if [ -x "$chained" ]; then exec "$chained" "$@"; fi
	exit 0
fi

bin=$GB_BIN
if [ ! -f "$bin" ]; then bin=$(command -v ghostbranch 2>/dev/null || true); fi
if [ -z "$bin" ]; then
	echo "ghostbranch guard: cannot find the ghostbranch binary, so this push to the public repo is refused." >&2
	echo "Reinstall the guard with the current binary: ghostbranch guard install" >&2
	exit 1
fi

input=$(mktemp "${TMPDIR:-/tmp}/ghostbranch-pre-push.XXXXXX") || exit 1
trap 'rm -f "$input"' EXIT
cat > "$input"
"$bin" guard check --workspace "$GB_WORKSPACE" "$1" "$2" < "$input" || exit 1
if [ -x "$chained" ]; then
	"$chained" "$@" < "$input"
	exit $?
fi
exit 0
`

// sq quotes s for POSIX sh.
func sq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func unsq(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		s = s[1 : len(s)-1]
	}
	return strings.ReplaceAll(s, `'\''`, "'")
}

// Script is the pre-push hook for h.
func Script(h Hook) string {
	return strings.NewReplacer(
		"@BIN@", sq(h.Binary),
		"@WORKSPACE@", sq(h.Workspace),
		"@REMOTES@", sq(strings.Join(h.Remotes, " ")),
		"@PUBLIC@", sq(strings.Join(h.Publics, " ")),
	).Replace(scriptTemplate)
}

var slugPattern = regexp.MustCompile(`github\.com[:/]([a-z0-9_.-]+/[a-z0-9_.-]+)$`)

// Slug returns lowercase owner/repo for a GitHub URL in any common form. It
// normalizes the way the hook script's norm does.
func Slug(url string) (string, bool) {
	s := strings.TrimSuffix(strings.TrimRight(strings.ToLower(strings.TrimSpace(url)), "/"), ".git")
	m := slugPattern.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// run runs plain git in dir with the user's own config, and returns stdout
// and the exit code.
func run(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return strings.TrimSpace(out.String()), ee.ExitCode(), fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	if err != nil {
		return "", -1, err
	}
	return strings.TrimSpace(out.String()), 0, nil
}

// Checkout is a user's clone, as the guard sees it.
type Checkout struct {
	Dir      string // where commands run
	Top      string // the working tree root
	GitDir   string // the common git directory
	HooksDir string // where git looks for hooks (core.hooksPath respected)
}

var gitVersion = regexp.MustCompile(`git version (\d+)\.(\d+)`)

func clean(p string) string { return filepath.Clean(filepath.FromSlash(p)) }

// Open inspects the checkout that contains dir.
func Open(ctx context.Context, dir string) (*Checkout, error) {
	top, _, err := run(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil || top == "" {
		return nil, contract.New(contract.NotAPairCheckout, "%s is not inside a git checkout", dir)
	}
	v, _, err := run(ctx, dir, "version")
	m := gitVersion.FindStringSubmatch(v)
	if err != nil || m == nil {
		return nil, contract.New(contract.GuardUnsupported, "cannot read the git version")
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	if major < 2 || (major == 2 && minor < 31) {
		return nil, contract.New(contract.GuardUnsupported, "the guard needs git 2.31 or later; this is %s.%s", m[1], m[2])
	}
	gitDir, _, err := run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "find the git directory")
	}
	hooks, _, err := run(ctx, dir, "rev-parse", "--path-format=absolute", "--git-path", "hooks")
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "find the hooks directory")
	}
	return &Checkout{Dir: dir, Top: clean(top), GitDir: clean(gitDir), HooksDir: clean(hooks)}, nil
}

// Remotes maps each remote to its URL as configured: before insteadOf
// rewriting, which git applies to the URL it hands the hook.
func (c *Checkout) Remotes(ctx context.Context) (map[string]string, error) {
	out, code, err := run(ctx, c.Dir, "config", "--get-regexp", `^remote\..*\.url$`)
	if code == 1 {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "list the remotes")
	}
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		key, url, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			m[strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url")] = url
		}
	}
	return m, nil
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// committable reports whether a hook file could end up in a commit: the
// hooks directory is in the working tree, outside .git, and not ignored.
func (c *Checkout) committable(ctx context.Context) (bool, error) {
	if within(c.GitDir, c.HooksDir) || !within(c.Top, c.HooksDir) {
		return false, nil
	}
	_, code, err := run(ctx, c.Dir, "check-ignore", "-q", filepath.Join(c.HooksDir, "pre-push"))
	switch code {
	case 0:
		return false, nil
	case 1:
		return true, nil
	}
	return false, err
}

func (c *Checkout) hookPath() string    { return filepath.Join(c.HooksDir, "pre-push") }
func (c *Checkout) chainedPath() string { return filepath.Join(c.HooksDir, ChainedName) }

func readFile(p string) (string, bool, error) {
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	return string(b), err == nil, err
}

// Status is what is installed in a checkout.
type Status struct {
	HooksDir    string   `json:"hooks_dir"`
	Installed   bool     `json:"installed"`
	Chained     bool     `json:"chained"`
	Remotes     []string `json:"remotes"`
	Publics     []string `json:"publics"`
	Binary      string   `json:"binary"`
	BinaryFound bool     `json:"binary_found"`
}

// Result is the outcome of install or uninstall.
type Result struct {
	Action  string           `json:"action"`
	Status  Status           `json:"status"`
	Preview *preview.Preview `json:"preview"`
}

// Inspect reads the installed hook, if any.
func Inspect(c *Checkout) (Status, error) {
	st := Status{HooksDir: c.HooksDir, Remotes: []string{}, Publics: []string{}}
	s, ok, err := readFile(c.hookPath())
	if err != nil {
		return st, contract.Wrap(contract.Internal, err, "read %s", c.hookPath())
	}
	_, st.Chained, _ = readFile(c.chainedPath())
	if !ok || !strings.Contains(s, Marker) {
		return st, nil
	}
	st.Installed = true
	vars := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "GB_") {
			vars[k] = unsq(v)
		}
	}
	st.Binary = vars["GB_BIN"]
	st.Remotes, st.Publics = strings.Fields(vars["GB_REMOTES"]), strings.Fields(vars["GB_PUBLIC"])
	if fi, err := os.Stat(filepath.FromSlash(st.Binary)); err == nil && !fi.IsDir() {
		st.BinaryFound = true
	}
	return st, nil
}

// Install puts the guard in front of the checkout's pre-push hook. An
// existing hook that is not ours is kept and chained; ours is rewritten in
// place, so the guard never chains to itself.
func Install(ctx context.Context, c *Checkout, h Hook, dryRun bool) (Result, error) {
	bad, err := c.committable(ctx)
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "check whether the hooks directory is ignored")
	}
	if bad {
		return Result{}, contract.New(contract.GuardUnsupported,
			"the hooks directory %s is in the working tree and not git-ignored, so the hook could be committed; ignore it or point core.hooksPath elsewhere", c.HooksDir)
	}
	script := Script(h)
	old, exists, err := readFile(c.hookPath())
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "read %s", c.hookPath())
	}
	ours := strings.Contains(old, Marker)
	pv := preview.New(nil)
	res := Result{Action: "installed"}
	switch {
	case !exists:
		pv.Add(preview.WriteFile(c.hookPath()))
	case ours && old == script:
		res.Action = "unchanged"
	case ours:
		res.Action = "updated"
		pv.Add(preview.WriteFile(c.hookPath()))
	default:
		if _, taken, _ := readFile(c.chainedPath()); taken {
			return Result{}, contract.New(contract.GuardUnsupported, "both %s and %s exist; move one of them away first", c.hookPath(), c.chainedPath())
		}
		pv.Add(preview.MoveFile(c.hookPath(), c.chainedPath()), preview.WriteFile(c.hookPath()))
	}
	if dryRun {
		res.Preview = pv
		res.Status, err = Inspect(c)
		return res, err
	}
	if res.Action != "unchanged" {
		if err := os.MkdirAll(c.HooksDir, 0o755); err != nil {
			return Result{}, contract.Wrap(contract.Internal, err, "create %s", c.HooksDir)
		}
		if exists && !ours {
			if err := os.Rename(c.hookPath(), c.chainedPath()); err != nil {
				return Result{}, contract.Wrap(contract.Internal, err, "keep the existing pre-push hook")
			}
		}
		tmp := c.hookPath() + ".ghostbranch-tmp"
		if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
			return Result{}, contract.Wrap(contract.Internal, err, "write the hook")
		}
		if err := os.Rename(tmp, c.hookPath()); err != nil {
			return Result{}, contract.Wrap(contract.Internal, err, "install the hook")
		}
	}
	res.Status, err = Inspect(c)
	return res, err
}

// Uninstall removes the guard and restores a chained hook.
func Uninstall(c *Checkout, dryRun bool) (Result, error) {
	old, exists, err := readFile(c.hookPath())
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "read %s", c.hookPath())
	}
	pv := preview.New(nil)
	res := Result{Action: "absent"}
	if exists && strings.Contains(old, Marker) {
		res.Action = "removed"
		pv.Add(preview.RemoveFile(c.hookPath()))
		if _, chained, _ := readFile(c.chainedPath()); chained {
			pv.Add(preview.MoveFile(c.chainedPath(), c.hookPath()))
		}
	}
	if dryRun {
		res.Preview = pv
		res.Status, err = Inspect(c)
		return res, err
	}
	if res.Action == "removed" {
		if err := os.Remove(c.hookPath()); err != nil {
			return Result{}, contract.Wrap(contract.Internal, err, "remove the hook")
		}
		if _, chained, _ := readFile(c.chainedPath()); chained {
			if err := os.Rename(c.chainedPath(), c.hookPath()); err != nil {
				return Result{}, contract.Wrap(contract.Internal, err, "restore the chained hook")
			}
		}
	}
	res.Status, err = Inspect(c)
	return res, err
}
