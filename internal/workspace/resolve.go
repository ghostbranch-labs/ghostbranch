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

// Resolve picks the pair a command targets: --repo first, then the current
// directory's git remotes, then the workspace's only pair.
func (w *Workspace) Resolve(repoFlag string, remoteURLs []string) (Pair, error) {
	if repoFlag != "" {
		if p, ok := w.Find(repoFlag); ok {
			return p, nil
		}
		return Pair{}, contract.New(contract.PairAmbiguous, "no repo %q in workspace %q; run ghostbranch init first", repoFlag, w.Name).WithField("repo")
	}
	for _, u := range remoteURLs {
		if name, ok := ParseRemoteURL(u); ok {
			if p, ok := w.Find(name); ok {
				return p, nil
			}
		}
	}
	switch len(w.Pairs) {
	case 1:
		return w.Pairs[0], nil
	case 0:
		return Pair{}, contract.New(contract.PairAmbiguous, "workspace %q has no repos; run ghostbranch init <owner/repo>", w.Name)
	}
	var names []string
	for _, p := range w.Pairs {
		names = append(names, p.Public)
	}
	sort.Strings(names)
	return Pair{}, contract.New(contract.PairAmbiguous, "several repos in workspace %q; pass --repo", w.Name).
		WithField("repo").WithDetails(map[string]any{"candidates": names})
}
