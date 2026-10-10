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
	"sort"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
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
	printf '%s' "$1" | tr 'A-Z' 'a-z' | sed -E -e 's#/*$##' -e 's#\.git$##' \
		-e 's#^((git\+ssh|ssh\+git|ssh|https?|git)://[^/]*):[0-9]+/#\1/#' \
		-e 's#^((git\+ssh|ssh\+git|ssh|https?)://([^/@]*@)?)ssh\.github\.com/#\1github.com/#'
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
cat > "$input" || exit 1
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

var (
	slugPattern = regexp.MustCompile(`github\.com[:/]([a-z0-9_.-]+/[a-z0-9_.-]+)$`)
	// For scheme URLs only: a port after the host, and GitHub's SSH-over-443
	// host. The scp form (git@github.com:owner/repo) is left alone, since a
	// numeric owner there is not a port. The hook script's norm runs the
	// same two expressions.
	portPattern    = regexp.MustCompile(`^((git\+ssh|ssh\+git|ssh|https?|git)://[^/]*):[0-9]+/`)
	sshHostPattern = regexp.MustCompile(`^((git\+ssh|ssh\+git|ssh|https?)://([^/@]*@)?)ssh\.github\.com/`)
)

// Slug returns lowercase owner/repo for a GitHub URL in any common form. It
// normalizes the way the hook script's norm does.
func Slug(url string) (string, bool) {
	s := strings.TrimSuffix(strings.TrimRight(strings.ToLower(strings.TrimSpace(url)), "/"), ".git")
	s = portPattern.ReplaceAllString(s, "${1}/")
	s = sshHostPattern.ReplaceAllString(s, "${1}github.com/")
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

// remoteConfig reads every remote.<name>.url and remote.<name>.pushurl, in
// config order, as configured: before insteadOf rewriting, which git
// applies to the URL it hands the hook.
func (c *Checkout) remoteConfig(ctx context.Context) (urls, pushURLs map[string][]string, err error) {
	urls, pushURLs = map[string][]string{}, map[string][]string{}
	out, code, err := run(ctx, c.Dir, "config", "--get-regexp", `^remote\..+\.(push)?url$`)
	if code == 1 {
		return urls, pushURLs, nil
	}
	if err != nil {
		return nil, nil, contract.Wrap(contract.Internal, err, "list the remotes")
	}
	for _, line := range strings.Split(out, "\n") {
		key, url, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		name := strings.TrimPrefix(key, "remote.")
		if n, ok := strings.CutSuffix(name, ".pushurl"); ok {
			pushURLs[n] = append(pushURLs[n], url)
		} else if n, ok := strings.CutSuffix(name, ".url"); ok {
			urls[n] = append(urls[n], url)
		}
	}
	return urls, pushURLs, nil
}

// RemoteURLs maps each remote to every URL configured for it: its url
// values, then its pushurl values. git pushes to the pushurls when there
// are any, else to every url, so a push through the remote is public when
// any of them is a public repo.
func (c *Checkout) RemoteURLs(ctx context.Context) (map[string][]string, error) {
	urls, pushURLs, err := c.remoteConfig(ctx)
	if err != nil {
		return nil, err
	}
	for n, us := range pushURLs {
		urls[n] = append(urls[n], us...)
	}
	return urls, nil
}

// Remotes maps each remote to its first url, the one git fetches from.
func (c *Checkout) Remotes(ctx context.Context) (map[string]string, error) {
	urls, _, err := c.remoteConfig(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(urls))
	for n, us := range urls {
		m[n] = us[0]
	}
	return m, nil
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// committable reports whether a file the guard writes could end up in a
// commit: the hooks directory is in the working tree, outside .git, and
// pre-push, its chained copy or install's temporary file is not ignored.
func (c *Checkout) committable(ctx context.Context) (bool, error) {
	if within(c.GitDir, c.HooksDir) || !within(c.Top, c.HooksDir) {
		return false, nil
	}
	for _, p := range []string{c.hookPath(), c.chainedPath(), c.tmpPath()} {
		_, code, err := run(ctx, c.Dir, "check-ignore", "-q", p)
		switch code {
		case 0:
			continue
		case 1:
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (c *Checkout) hookPath() string    { return filepath.Join(c.HooksDir, "pre-push") }
func (c *Checkout) chainedPath() string { return filepath.Join(c.HooksDir, ChainedName) }
func (c *Checkout) tmpPath() string     { return filepath.Join(c.HooksDir, "pre-push.ghostbranch-tmp") }

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
	vars := hookVars(s)
	st.Binary = vars["GB_BIN"]
	st.Remotes, st.Publics = strings.Fields(vars["GB_REMOTES"]), strings.Fields(vars["GB_PUBLIC"])
	if fi, err := os.Stat(filepath.FromSlash(st.Binary)); err == nil && !fi.IsDir() {
		st.BinaryFound = true
	}
	return st, nil
}

// hookVars reads the GB_* assignments of an installed hook script.
func hookVars(s string) map[string]string {
	vars := map[string]string{}
	for _, line := range strings.Split(s, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok && strings.HasPrefix(k, "GB_") {
			vars[k] = unsq(v)
		}
	}
	return vars
}

// ReadHook returns the installed guard's settings. installed is false when
// the pre-push hook isn't the guard. current reports whether the script is
// exactly what this version writes for those settings: an older guard (one
// that misses pushurl, say) is not current.
func ReadHook(c *Checkout) (h Hook, installed, current bool, err error) {
	s, ok, err := readFile(c.hookPath())
	if err != nil {
		return Hook{}, false, false, contract.Wrap(contract.Internal, err, "read %s", c.hookPath())
	}
	if !ok || !strings.Contains(s, Marker) {
		return Hook{}, false, false, nil
	}
	vars := hookVars(s)
	h = Hook{Remotes: strings.Fields(vars["GB_REMOTES"]), Publics: strings.Fields(vars["GB_PUBLIC"]),
		Workspace: vars["GB_WORKSPACE"], Binary: vars["GB_BIN"]}
	return h, true, s == Script(h), nil
}

// PublicRemotes finds the remotes with any url or pushurl at a registered
// public repo (never a shadow repo).
func PublicRemotes(ctx context.Context, co *Checkout, ws *workspace.Workspace) (Hook, error) {
	h := Hook{Workspace: ws.Name}
	remotes, err := co.RemoteURLs(ctx)
	if err != nil {
		return h, err
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, n := range names {
		public := false
		for _, u := range remotes[n] {
			slug, ok := Slug(u)
			if !ok {
				continue
			}
			if p, ok := ws.Find(slug); ok && strings.EqualFold(p.Public, slug) {
				public = true
				if !seen[slug] {
					seen[slug] = true
					h.Publics = append(h.Publics, slug)
				}
			}
		}
		if public {
			h.Remotes = append(h.Remotes, n)
		}
	}
	if len(h.Remotes) == 0 {
		return h, contract.New(contract.NotAPairCheckout,
			"no remote of %s points at a public repo registered in workspace %q; run ghostbranch init <owner/repo> first", co.Top, ws.Name)
	}
	return h, nil
}

// huskyDir reports whether any component of the hooks path is .husky. Husky
// dispatches on the hook's own file name and regenerates its hooks on npm
// install, so a chained Husky hook would silently stop running.
func huskyDir(p string) bool {
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(p)), "/") {
		if strings.EqualFold(part, ".husky") {
			return true
		}
	}
	return false
}

// Install puts the guard in front of the checkout's pre-push hook. An
// existing hook that is not ours is kept and chained; ours is rewritten in
// place, so the guard never chains to itself.
func Install(ctx context.Context, c *Checkout, h Hook, dryRun bool) (Result, error) {
	old, exists, err := readFile(c.hookPath())
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "read %s", c.hookPath())
	}
	ours := strings.Contains(old, Marker)
	if huskyDir(c.HooksDir) || (exists && !ours && strings.Contains(strings.ToLower(old), "husky")) {
		return Result{}, contract.New(contract.GuardUnsupported,
			"this checkout's hooks are managed by Husky, which runs hooks by their own file name and rewrites them on install, so the guard cannot run in front of them")
	}
	_, chainedExists, err := readFile(c.chainedPath())
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "read %s", c.chainedPath())
	}
	if !exists && chainedExists {
		return Result{}, contract.New(contract.GuardUnsupported,
			"%s exists but %s does not, so installing would run an unknown file as your pre-push hook; move it away first", c.chainedPath(), c.hookPath())
	}
	bad, err := c.committable(ctx)
	if err != nil {
		return Result{}, contract.Wrap(contract.Internal, err, "check whether the hooks directory is ignored")
	}
	if bad {
		return Result{}, contract.New(contract.GuardUnsupported,
			"the hooks directory %s is in the working tree and the guard's files there (pre-push and pre-push.ghostbranch-*) are not all git-ignored, so the hook could be committed; ignore them or point core.hooksPath elsewhere", c.HooksDir)
	}
	script := Script(h)
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
		if chainedExists {
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
		tmp := c.tmpPath()
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
