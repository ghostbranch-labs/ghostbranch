// Package forge is the boundary between the engine and a code host. Only
// adapters under forge/ may talk to a host's API.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ErrNotFound wraps any "does not exist" answer from the host.
var ErrNotFound = errors.New("not found")

// ErrTransient wraps failures worth retrying: 5xx, rate limits, network.
var ErrTransient = errors.New("transient forge error")

// ErrForbidden wraps a plain permission refusal (a 403 that is not a rate
// limit): the token may not do this.
var ErrForbidden = errors.New("forbidden")

type Repo struct {
	FullName      string
	Private       bool
	Fork          bool
	DefaultBranch string
	CloneURL      string
}

type PR struct {
	Number int
	Head   string
	Base   string
	State  string
	Draft  bool
	URL    string
	// Merged is true when the PR was merged; State is then closed.
	Merged bool
}

type NewPR struct {
	Title string
	Body  string
	Head  string
	Base  string
	Draft bool
}

// Check is one check run or commit status on a commit.
type Check struct {
	Name       string
	Status     string // queued | in_progress | completed
	Conclusion string // success, failure, skipped, neutral, cancelled, timed_out, action_required, …
}

// Run is a GitHub Actions workflow run.
type Run struct {
	ID       int64
	Workflow string
	Status   string // queued | in_progress | waiting | requested | pending | completed
	URL      string
}

type User struct {
	Login string
	ID    int64
}

// Forge is everything the engine needs from a code host. Repos are named
// owner/name; PR heads are branch names in the same repo.
type Forge interface {
	GetRepo(ctx context.Context, full string) (Repo, error)
	CreatePrivateRepo(ctx context.Context, full string) (Repo, error)
	SetDefaultBranch(ctx context.Context, full, branch string) error
	FindOpenPR(ctx context.Context, full, head, base string) (*PR, error)
	CreatePR(ctx context.Context, full string, pr NewPR) (PR, error)
	GetPR(ctx context.Context, full string, number int) (PR, error)
	ListChecks(ctx context.Context, full, sha string) ([]Check, error)
	CurrentUser(ctx context.Context) (User, error)
	// SecretNames lists the Actions secret names visible to full: its own
	// and the org secrets shared with it. Values are never read.
	SecretNames(ctx context.Context, full string) ([]string, error)
	// PushRuns lists Actions runs for sha triggered by event "push" (any status).
	PushRuns(ctx context.Context, full, sha string) ([]Run, error)
	// CancelRun cancels a queued or in-progress run. A run that has already
	// finished is not an error.
	CancelRun(ctx context.Context, full string, id int64) error
	// ClosePullRequest closes PR number in full. A PR that is already
	// closed is not an error; a missing one is ErrNotFound, and a plain
	// permission refusal ErrForbidden.
	ClosePullRequest(ctx context.Context, full string, number int) error
}

// NoreplyEmail is the user's GitHub noreply address. Public commits use it
// so real email addresses never go public.
func NoreplyEmail(u User) string {
	return fmt.Sprintf("%d+%s@users.noreply.github.com", u.ID, u.Login)
}

// SplitName splits owner/name.
func SplitName(full string) (string, string, error) {
	owner, name, ok := strings.Cut(full, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("%q is not owner/repo", full)
	}
	return owner, name, nil
}

// CheckVisibility is an optional interface for forges that can see only part
// of a repo's CI, for example a token that cannot read check runs.
type CheckVisibility interface {
	// ChecksLimited reports whether ListChecks for repo had reduced visibility.
	ChecksLimited(repo string) bool
}

// PushAccess is whether the current token may push to a repo, from a probe
// that pushes nothing. Can is "true", "false" or "unknown".
type PushAccess struct {
	Can        string `json:"can"`
	HTTPStatus int    `json:"http_status"`
}

// PushProber is an optional interface for forges that can tell whether the
// token may push to a repo without pushing.
type PushProber interface {
	PushAccess(ctx context.Context, full string) PushAccess
}
