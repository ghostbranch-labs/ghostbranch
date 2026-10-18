package gitx

import (
	"context"
	"fmt"
	"regexp"
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

// RefusedError means the remote refused at least one ref for a reason of
// its own (permissions, a ruleset, a hook); retrying won't help.
type RefusedError struct {
	Refs   []string
	Reason string // the server's reason, e.g. text inside "[remote rejected] (...)"
	// RefReasons is each ref's own "[remote rejected]" reason, in Refs order.
	RefReasons []string
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf("push refused for %s: %s", strings.Join(e.Refs, ", "), e.Reason)
}

// PermissionError means the remote refused the push because the credentials
// may not write to it (GitHub answers 403).
type PermissionError struct{ Detail string }

func (e *PermissionError) Error() string { return "push not permitted: " + e.Detail }

var permissionDenied = regexp.MustCompile(`(?i)(returned error: 403|permission to \S+ denied|write access to repository not granted)`)

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
		if pe := pushFailure(ge.Stdout, ge.Stderr); pe != nil {
			return pe
		}
	}
	return err
}

// lostRace matches a [remote rejected] reason that means the ref moved on
// the server while the push ran, as a lost lease does; re-running helps.
var lostRace = regexp.MustCompile(`(?i)cannot lock ref|failed to update ref|incorrect old value|atomic`)

// pushFailure classifies a failed push from its porcelain output: a
// RefusedError when the server refused a ref for a reason of its own, a
// RejectedError when a ref was rejected or lost a race, a PermissionError
// when the credentials may not write; nil when it can't tell.
func pushFailure(stdout, stderr string) error {
	var refs, refused, reasons []string
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "!") {
			continue
		}
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		if len(parts) < 2 {
			continue
		}
		if len(parts) >= 3 && strings.HasPrefix(strings.TrimSpace(parts[2]), "[remote rejected]") {
			if reason := remoteRejectedReason(parts[2]); !lostRace.MatchString(reason) {
				_, dst, ok := strings.Cut(parts[1], ":")
				if !ok {
					dst = parts[1]
				}
				refused = append(refused, dst)
				reasons = append(reasons, reason)
				continue
			}
		}
		refs = append(refs, parts[1])
	}
	if len(refused) > 0 {
		reason := strings.Join(reasons, "; ")
		// A hook or ruleset usually says why in "remote:" lines on stderr.
		if said := remoteSaid(stderr); said != "" {
			reason += ": " + said
		}
		return &RefusedError{Refs: refused, Reason: reason, RefReasons: reasons}
	}
	if len(refs) > 0 {
		return &RejectedError{Refs: refs, Detail: strings.TrimSpace(stdout + "\n" + stderr)}
	}
	if permissionDenied.MatchString(stderr) {
		return &PermissionError{Detail: strings.TrimSpace(stderr)}
	}
	return nil
}

// remoteRejectedReason is the text inside the parentheses of a porcelain
// status such as "[remote rejected] (pre-receive hook declined)".
func remoteRejectedReason(status string) string {
	status = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(status), "[remote rejected]"))
	if strings.HasPrefix(status, "(") && strings.HasSuffix(status, ")") {
		return status[1 : len(status)-1]
	}
	return status
}

// remoteSaid joins the messages the server sent with "remote: " on stderr.
func remoteSaid(stderr string) string {
	var said []string
	for _, line := range strings.Split(stderr, "\n") {
		if msg, ok := strings.CutPrefix(strings.TrimSpace(line), "remote:"); ok {
			if msg = strings.TrimSpace(msg); msg != "" {
				said = append(said, msg)
			}
		}
	}
	return strings.Join(said, " ")
}
