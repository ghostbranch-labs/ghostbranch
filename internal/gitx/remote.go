package gitx

import (
	"context"
	"fmt"
	"strings"
)

// AddRemote adds a remote, or updates its URL if it already exists.
func (g *Git) AddRemote(ctx context.Context, name, url string) error {
	if _, err := g.Run(ctx, "remote", "add", name, url); err != nil {
		if _, err2 := g.Run(ctx, "remote", "set-url", name, url); err2 != nil {
			return err
		}
	}
	return nil
}

// Fetch fetches refspecs from remote, pruning refs that disappeared.
// Tags are fetched only when a refspec names them.
func (g *Git) Fetch(ctx context.Context, remote string, refspecs ...string) error {
	_, err := g.Run(ctx, append([]string{"fetch", "--quiet", "--prune", "--no-tags", remote}, refspecs...)...)
	return err
}

// LsRemote maps every ref the remote advertises to its SHA.
func (g *Git) LsRemote(ctx context.Context, remote string) (map[string]string, error) {
	return g.lsRemote(ctx, remote)
}

func (g *Git) lsRemote(ctx context.Context, remote string, patterns ...string) (map[string]string, error) {
	out, err := g.Run(ctx, append([]string{"ls-remote", remote}, patterns...)...)
	if err != nil {
		return nil, err
	}
	refs := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if sha, ref, ok := strings.Cut(strings.TrimSpace(line), "\t"); ok {
			refs[ref] = sha
		}
	}
	return refs, nil
}

// Lease guards a remote ref during push. Expect "" means the ref must not
// exist yet.
type Lease struct {
	Ref    string
	Expect string
}

// PushOptions controls a push.
type PushOptions struct {
	Atomic bool
	Leases []Lease
}

// RejectedError means the remote refused at least one ref, for example
// because a lease did not match.
type RejectedError struct {
	Refs   []string
	Detail string
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("push rejected for %s: %s", strings.Join(e.Refs, ", "), e.Detail)
}

// Push pushes refspecs to remote. Refspec sources may be SHAs; destinations
// must be full ref names.
func (g *Git) Push(ctx context.Context, remote string, opt PushOptions, refspecs ...string) error {
	// git skips lease checks for refs that are already up to date, so verify
	// the leases ourselves first; --force-with-lease still guards the race
	// between this check and the push.
	if len(opt.Leases) > 0 {
		pats := make([]string, len(opt.Leases))
		for i, l := range opt.Leases {
			pats[i] = l.Ref
		}
		have, err := g.lsRemote(ctx, remote, pats...)
		if err != nil {
			return err
		}
		var bad []string
		for _, l := range opt.Leases {
			if have[l.Ref] != l.Expect {
				bad = append(bad, l.Ref)
			}
		}
		if len(bad) > 0 {
			return &RejectedError{Refs: bad, Detail: "lease check failed: remote ref does not match the expected value"}
		}
	}
	args := []string{"push", "--porcelain"}
	if opt.Atomic {
		args = append(args, "--atomic")
	}
	for _, l := range opt.Leases {
		args = append(args, "--force-with-lease="+l.Ref+":"+l.Expect)
	}
	args = append(append(args, remote), refspecs...)
	_, err := g.Run(ctx, args...)
	if err == nil {
		return nil
	}
	if ge, ok := err.(*Error); ok {
		var refs []string
		for _, line := range strings.Split(ge.Stdout, "\n") {
			if strings.HasPrefix(line, "!") {
				if parts := strings.Split(line, "\t"); len(parts) >= 2 {
					refs = append(refs, parts[1])
				}
			}
		}
		if len(refs) > 0 {
			return &RejectedError{Refs: refs, Detail: strings.TrimSpace(ge.Stdout + "\n" + ge.Stderr)}
		}
	}
	return err
}
