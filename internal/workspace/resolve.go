package workspace

import (
	"regexp"
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

// firstRemoteRepo is the owner/name of the first remote URL that parses.
func firstRemoteRepo(remoteURLs []string) string {
	for _, u := range remoteURLs {
		if name, ok := ParseRemoteURL(u); ok {
			return name
		}
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
func (w *Workspace) Resolve(repoFlag string, remoteURLs []string) (Pair, error) {
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
		if ownerName.MatchString(repoFlag) {
			public = repoFlag
		}
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "no repo %q in workspace %q; run ghostbranch init first", repoFlag, w.Name).WithField("repo"), public)
	}
	for _, u := range remoteURLs {
		if name, ok := ParseRemoteURL(u); ok {
			if p, ok := w.Find(name); ok {
				return p, nil
			}
		}
	}
	if here {
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "the remotes of this checkout name no repo in workspace %q", w.Name).WithField("repo"), firstRemoteRepo(remoteURLs))
	}
	switch len(w.Pairs) {
	case 1:
		return w.Pairs[0], nil
	case 0:
		return Pair{}, notRegistered(contract.New(contract.PairAmbiguous, "workspace %q has no repos; run ghostbranch init <owner/repo>", w.Name), firstRemoteRepo(remoteURLs))
	}
	var names []string
	for _, p := range w.Pairs {
		names = append(names, p.Public)
	}
	sort.Strings(names)
	return Pair{}, contract.New(contract.PairAmbiguous, "several repos in workspace %q; pass --repo", w.Name).
		WithField("repo").WithDetails(map[string]any{"candidates": names})
}
