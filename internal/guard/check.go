package guard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// Line is one line of git's pre-push input.
type Line struct{ LocalRef, LocalSHA, RemoteRef, RemoteSHA string }

// ParseInput reads git's pre-push input: one "<local ref> <local sha>
// <remote ref> <remote sha>" per line.
func ParseInput(r io.Reader) ([]Line, error) {
	var out []Line
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 0 {
			continue
		}
		if len(f) != 4 {
			return nil, fmt.Errorf("unexpected pre-push input line %q", sc.Text())
		}
		out = append(out, Line{f[0], f[1], f[2], f[3]})
	}
	return out, sc.Err()
}

// Facts are what the pair's mirror knows about features.
type Facts struct {
	Names      map[string]bool   // every feature
	Heads      map[string]string // feature -> shadow head, for features with commits not in public main
	Commits    map[string]string // unpublished feature commit -> its feature
	PublicMain string            // the tip of the pair's base branch in the mirror, if known
	// Archived maps the public branch of each feature handed off to the
	// public repo (6.5) to that feature: a push to that branch isn't
	// refused for the feature's name or head. The archive's private
	// commits stay in Commits (R7).
	Archived map[string]string
}

// overlayRef is the shadow's overlay branch in the engine's mirror (the
// engine's OverlayBranch); its .ghostbranch.yml names the pair's base branch.
const overlayRef = "refs/remotes/shadow/ghostbranch"

// LoadFacts reads features from the engine's mirror, offline, checked
// against the pair's base branch. Any git error, or an overlay config it
// can't read, is returned, so the check fails closed.
func LoadFacts(ctx context.Context, mirror *gitx.Git) (Facts, error) {
	f := Facts{Names: map[string]bool{}, Heads: map[string]string{}, Commits: map[string]string{}, Archived: map[string]string{}}
	metas, err := mirror.Refs(ctx, "refs/shadow-meta/features")
	if err != nil {
		return f, err
	}
	base, err := baseBranch(ctx, mirror)
	if err != nil {
		return f, err
	}
	pm, hasPM, err := mirror.Resolve(ctx, "refs/remotes/public/"+base)
	if err != nil {
		return f, err
	}
	if hasPM {
		f.PublicMain = pm
	}
	// Sorted, and the first feature to claim a commit keeps it, so a commit
	// shared by two features always names the same one.
	all := make([]string, 0, len(metas))
	for ref := range metas {
		all = append(all, strings.TrimPrefix(ref, "refs/shadow-meta/features/"))
	}
	sort.Strings(all)
	for _, name := range all {
		f.Names[name] = true
		if err := loadArchive(ctx, mirror, &f, name, pm, hasPM); err != nil {
			return f, err
		}
		head, ok, err := mirror.Resolve(ctx, "refs/remotes/shadow/"+name)
		if err != nil {
			return f, err
		}
		if !ok {
			continue
		}
		if hasPM {
			in, err := mirror.IsAncestor(ctx, head, pm)
			if err != nil {
				return f, err
			}
			if in {
				continue
			}
		}
		f.Heads[name] = head
		args := []string{"rev-list", head}
		if hasPM {
			args = append(args, "^"+pm)
		}
		out, err := mirror.Run(ctx, args...)
		if err != nil {
			return f, err
		}
		for _, c := range strings.Fields(out) {
			if _, ok := f.Commits[c]; !ok {
				f.Commits[c] = name
			}
		}
	}
	return f, nil
}

// baseBranch reads the pair's base branch from the overlay's .ghostbranch.yml
// in the mirror: public_branch, or main when the key is missing. A mirror
// with no overlay branch is main, as before the key existed. An overlay with
// no config, or one that doesn't parse (a key from a newer Ghostbranch
// included), is an error, never a guess at main: main may not be the pair's
// branch at all, and the guard never checks a push against the wrong one.
func baseBranch(ctx context.Context, mirror *gitx.Git) (string, error) {
	O, ok, err := mirror.Resolve(ctx, overlayRef)
	if err != nil {
		return "", err
	}
	if !ok {
		return config.Config{}.Base(), nil
	}
	raw, ok, err := mirror.ShowFile(ctx, O, ".ghostbranch.yml")
	if err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("the overlay branch has no .ghostbranch.yml, so the guard can't tell the pair's base branch")
	}
	cfg, _, err := config.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("read the pair's base branch from .ghostbranch.yml: %w", err)
	}
	return cfg.Base(), nil
}

// loadArchive reads name's record. A feature handed off to the public repo
// maps its public branch to name, and its archive's commits that aren't in
// public main join f.Commits under name (R7), never f.Heads: a head
// already on its PR's branch would refuse every fix. A record that can't
// be read, or an archive ref missing from the mirror, changes nothing, so
// the name stays refused (fail closed); a git failure is an error.
func loadArchive(ctx context.Context, mirror *gitx.Git, f *Facts, name, pm string, hasPM bool) error {
	raw, ok, err := mirror.ShowFile(ctx, "refs/shadow-meta/features/"+name, feature.MetaFile)
	if err != nil || !ok {
		return err
	}
	m, err := feature.Decode(raw)
	if err != nil || m.Archived == nil || m.Promoted == nil {
		return nil
	}
	branch := m.Promoted.Branch
	if branch == "" {
		branch = name
	}
	arch, ok, err := mirror.Resolve(ctx, "refs/shadow-meta/archive/"+name)
	if err != nil || !ok {
		return err
	}
	args := []string{"rev-list", arch}
	if hasPM {
		args = append(args, "^"+pm)
	}
	out, err := mirror.Run(ctx, args...)
	if err != nil {
		return err
	}
	for _, c := range strings.Fields(out) {
		if _, ok := f.Commits[c]; !ok {
			f.Commits[c] = name
		}
	}
	// Only now, with the archive's private commits known, is the branch
	// handed to name: without them the exception would let them through.
	f.Archived[branch] = name
	return nil
}

// Block is a refused ref and why.
type Block struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason"`
}

// CheckResult is `guard check` output when the push may go ahead.
type CheckResult struct {
	Checked int `json:"checked"`
}

// reserved says why ref may never be pushed; own is the archived feature
// whose public branch the line pushes to ("" when none): its own name is
// allowed there (6.5).
func reserved(ref string, f Facts, own string) string {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	switch {
	case !ok:
		return ""
	case b == "ghostbranch":
		return "it is the ghostbranch overlay branch, which never leaves the shadow repo"
	case strings.HasPrefix(b, "ghostbranch-gate/"):
		return "it is a ghostbranch gate branch, which never leaves the shadow repo"
	case f.Names[b] && b != own:
		return fmt.Sprintf("%q is a Ghostbranch feature", b)
	}
	return ""
}

// archivedOwner is the archived feature whose public branch ref is, or "".
func archivedOwner(ref string, f Facts) string {
	b, ok := strings.CutPrefix(ref, "refs/heads/")
	if !ok {
		return ""
	}
	return f.Archived[b]
}

// Objects reads commits from a repository.
type Objects interface {
	Resolve(ctx context.Context, rev string) (string, bool, error)
	IsAncestor(ctx context.Context, a, b string) (bool, error)
	RevList(ctx context.Context, args ...string) ([]string, error)
}

// Resolve returns the object rev names, with ok=false when there is none.
// It runs plain git, so the user's config (safe.directory and the like)
// applies.
func (c *Checkout) Resolve(ctx context.Context, rev string) (string, bool, error) {
	out, code, err := run(ctx, c.Dir, "rev-parse", "--verify", "--quiet", rev+"^{object}")
	switch {
	case code == 1:
		return "", false, nil
	case err != nil:
		return "", false, err
	}
	return out, true, nil
}

// IsAncestor reports whether a is an ancestor of b (or equal to it). Any
// git failure, a missing object included, is an error.
func (c *Checkout) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, code, err := run(ctx, c.Dir, "merge-base", "--is-ancestor", a, b)
	switch {
	case code == 0 && err == nil:
		return true, nil
	case code == 1:
		return false, nil
	}
	return false, err
}

// RevList runs git rev-list in the checkout and returns the commits.
func (c *Checkout) RevList(ctx context.Context, args ...string) ([]string, error) {
	out, _, err := run(ctx, c.Dir, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// Check returns the pushed refs that would put feature work in the public
// repo (spec §11.4): by name, or by carrying any unpublished feature commit.
// Any git error fails the check: a guard that waves pushes through when git
// breaks is worse than none. Commits force-pushed out of the mirror can't be
// recognized by content; the name check still applies. The content check
// lists the whole push's commits with one rev-list (carried). A push to an
// archived feature's own public branch passes the name and head checks for
// that feature, but still fails on its archive's private commits (6.5, R7).
func Check(ctx context.Context, checkout Objects, lines []Line, f Facts) ([]Block, error) {
	names := make([]string, 0, len(f.Heads))
	for n := range f.Heads {
		names = append(names, n)
	}
	sort.Strings(names)
	why := make([]string, len(lines))
	var unchecked []int // lines left for the content check
	for i, l := range lines {
		if strings.Trim(l.LocalSHA, "0") == "" {
			continue // a deletion publishes nothing
		}
		own := archivedOwner(l.RemoteRef, f)
		w := reserved(l.RemoteRef, f, own)
		if w == "" {
			w = reserved(l.LocalRef, f, own)
		}
		if w == "" && len(f.Commits) > 0 {
			// A tree or blob lists no commits, so it would pass the content
			// check whatever files it carries. With no unpublished feature
			// commits there is nothing it could carry that the guard knows.
			_, ok, err := checkout.Resolve(ctx, l.LocalSHA+"^{commit}")
			if err != nil {
				return nil, err
			}
			if !ok {
				w = "it does not point at a commit, so the guard can't check it"
			}
		}
		for _, n := range names {
			if w != "" {
				break
			}
			if n == own {
				continue // its commits on the PR are public; carried refuses its private ones (R7)
			}
			head := f.Heads[n]
			_, ok, err := checkout.Resolve(ctx, head)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue // this clone never had the feature's commits
			}
			in, err := checkout.IsAncestor(ctx, head, l.LocalSHA)
			if err != nil {
				return nil, err
			}
			if in {
				w = fmt.Sprintf("it contains the commits of feature %q", n)
			}
		}
		why[i] = w
		if w == "" && len(f.Commits) > 0 {
			unchecked = append(unchecked, i)
		}
	}
	if len(unchecked) > 0 {
		if err := carried(ctx, checkout, lines, unchecked, f, why); err != nil {
			return nil, err
		}
	}
	var blocks []Block
	for i, l := range lines {
		if why[i] != "" {
			blocks = append(blocks, Block{Ref: l.RemoteRef, Reason: why[i]})
		}
	}
	return blocks, nil
}

// carried sets why[i] for each line i in idx that carries an unpublished
// feature commit. One rev-list covers the whole push (git push --tags would
// otherwise walk the shared history once per tag): every pushed commit,
// minus the ones the public repo already has, which are public main and
// each line's remote SHA. The remote holds those, so they are published
// whichever ref this push puts them on. Tracking refs aren't excluded: a
// remote that fetches from the shadow, or any fetch into refs/remotes, can
// hold unpublished feature commits there. Only when the list has a feature
// commit is each line asked which one it carries.
func carried(ctx context.Context, checkout Objects, lines []Line, idx []int, f Facts, why []string) error {
	var args []string
	pushed := map[string]bool{}
	for _, i := range idx {
		if sha := lines[i].LocalSHA; !pushed[sha] {
			pushed[sha] = true
			args = append(args, sha)
		}
	}
	args = append(args, "--not")
	bases := []string{f.PublicMain}
	for _, l := range lines {
		bases = append(bases, l.RemoteSHA)
	}
	excluded := map[string]bool{}
	for _, base := range bases {
		if strings.Trim(base, "0") == "" || excluded[base] {
			continue
		}
		excluded[base] = true
		if _, ok, err := checkout.Resolve(ctx, base); err != nil {
			return err
		} else if ok {
			args = append(args, base)
		}
	}
	commits, err := checkout.RevList(ctx, args...)
	if err != nil {
		return err
	}
	var found []string
	for _, c := range commits {
		if _, ok := f.Commits[c]; ok {
			found = append(found, c)
		}
	}
	for _, i := range idx {
		for _, c := range found {
			in, err := checkout.IsAncestor(ctx, c, lines[i].LocalSHA)
			if err != nil {
				return err
			}
			if in {
				why[i] = fmt.Sprintf("it contains commit %s of feature %q", c[:12], f.Commits[c])
				break
			}
		}
	}
	return nil
}
