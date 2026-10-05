package gitx

import (
	"context"
	"fmt"
	"strings"
)

// Resolve returns the object rev names, or ok=false if it does not exist.
func (g *Git) Resolve(ctx context.Context, rev string) (string, bool, error) {
	out, err := g.Run(ctx, "rev-parse", "--verify", "--quiet", rev+"^{object}")
	if err == nil {
		return out, true, nil
	}
	if exitCode(err) == 1 {
		return "", false, nil
	}
	return "", false, err
}

// MustResolve resolves rev and fails if it does not exist.
func (g *Git) MustResolve(ctx context.Context, rev string) (string, error) {
	sha, ok, err := g.Resolve(ctx, rev)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("git: %s does not exist", rev)
	}
	return sha, nil
}

// IsAncestor reports whether a is an ancestor of (or equal to) b.
func (g *Git) IsAncestor(ctx context.Context, a, b string) (bool, error) {
	_, err := g.Run(ctx, "merge-base", "--is-ancestor", a, b)
	if err == nil {
		return true, nil
	}
	if exitCode(err) == 1 {
		return false, nil
	}
	return false, err
}

// Refs maps every ref under prefix (e.g. "refs/heads") to its SHA.
func (g *Git) Refs(ctx context.Context, prefix string) (map[string]string, error) {
	out, err := g.Run(ctx, "for-each-ref", "--format=%(objectname) %(refname)", prefix)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), " "); ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// UpdateRef points ref at sha in this repository.
func (g *Git) UpdateRef(ctx context.Context, ref, sha string) error {
	_, err := g.Run(ctx, "update-ref", ref, sha)
	return err
}

// DeleteRef deletes ref in this repository.
func (g *Git) DeleteRef(ctx context.Context, ref string) error {
	_, err := g.Run(ctx, "update-ref", "-d", ref)
	return err
}
