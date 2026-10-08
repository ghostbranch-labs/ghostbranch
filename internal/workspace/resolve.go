package workspace

import (
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

var remoteURL = regexp.MustCompile(`^(?:https://github\.com/|git@github\.com:|ssh://git@github\.com/)([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git)?/?$`)

// ParseRemoteURL extracts owner/repo from a GitHub remote URL.
func ParseRemoteURL(u string) (string, bool) {
	m := remoteURL.FindStringSubmatch(strings.TrimSpace(u))
	if m == nil {
		return "", false
	}
	return m[1], true
}

var ownerName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Remote is a git remote of the current checkout.
type Remote struct{ Name, URL string }

func isShadowRemote(name string) bool {
	return name == "shadow" || strings.HasPrefix(name, "ghostbranch-shadow")
}

// publicFromRemotes is the public repo the checkout's remotes name, or "".
// Remotes Ghostbranch uses for the shadow repo are skipped. origin wins;
// otherwise the remotes must name exactly one distinct repo.
func publicFromRemotes(remotes []Remote) string {
	var found []string
	for _, r := range remotes {
		if isShadowRemote(r.Name) {
			continue
		}
		name, ok := ParseRemoteURL(r.URL)
		if !ok || strings.HasSuffix(strings.ToLower(name), "-shadow") {
			continue
		}
		if r.Name == "origin" {
			return name
		}
		if !slices.Contains(found, name) {
			found = append(found, name)
		}
	}
	if len(found) == 1 {
		return found[0]
	}
	return ""
}

// Resolve picks the pair a command targets: --repo first, then the current
// directory's git remotes, then the workspace's only pair. "--repo ." means
// the checkout the command runs in, so it skips the first and, when no remote
// names a registered pair, never falls back to the only pair.
//
// When nothing is registered for the repo asked about, the error carries
// details.public (the public repo it can name) so next can offer init.
func (w *Workspace) Resolve(repoFlag string, remotes []Remote) (Pair, error) {
	here := repoFlag == "."
	if here {
		repoFlag = ""
	}
	notRegistered := func(e *contract.Error, public string) error {
		if public != "" {
			e = e.WithDetails(map[string]any{"public": public})
		}
		return e
	}
	if repoFlag != "" {
		if p, ok := w.Find(repoFlag); ok {
			return p, nil
		}
		public := ""
		if ownerName.MatchString(repoFlag) && !strings.HasSuffix(strings.ToLower(repoFlag), "-shadow") {
			public = repoFlag
		}
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "no repo %q in workspace %q; run ghostbranch init first", repoFlag, w.Name).WithField("repo"), public)
	}
	for _, r := range remotes {
		if name, ok := ParseRemoteURL(r.URL); ok {
			if p, ok := w.Find(name); ok {
				return p, nil
			}
		}
	}
	if here {
		hasGitHub := false
		for _, r := range remotes {
			if _, ok := ParseRemoteURL(r.URL); ok {
				hasGitHub = true
			}
		}
		if !hasGitHub {
			return Pair{}, contract.New(contract.PairAmbiguous, "this directory is not a checkout with a GitHub remote").WithField("repo")
		}
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "the remotes of this checkout name no repo in workspace %q", w.Name).WithField("repo"), publicFromRemotes(remotes))
	}
	switch len(w.Pairs) {
	case 1:
		return w.Pairs[0], nil
	case 0:
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "workspace %q has no repos; run ghostbranch init <owner/repo>", w.Name), publicFromRemotes(remotes))
	}
	var names []string
	for _, p := range w.Pairs {
		names = append(names, p.Public)
	}
	sort.Strings(names)
	return Pair{}, contract.New(contract.PairAmbiguous, "several repos in workspace %q; pass --repo", w.Name).
		WithField("repo").WithDetails(map[string]any{"candidates": names})
}
