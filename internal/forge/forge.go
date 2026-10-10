// Package forge is the boundary between the engine and a code host. Only
// adapters under forge/ may talk to a host's API.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// ErrNotFound wraps any "does not exist" answer from the host.
var ErrNotFound = errors.New("not found")

// ErrTransient wraps failures worth retrying: 5xx, rate limits, network.
var ErrTransient = errors.New("transient forge error")

// ErrForbidden wraps a plain permission refusal (a 403 that is not a rate
// limit): the token may not do this. Every Forge method wraps a plain 403
// with it, ListChecks included once its workflow-runs fallback is refused
// too. A *PermissionError is one whose missing permission the host named.
var ErrForbidden = errors.New("forbidden")

// ErrConflict wraps a 409: the host refused the change in the resource's
// current state.
var ErrConflict = errors.New("conflict")

// PermissionError is a permission refusal whose missing permission the
// host named. It unwraps to ErrForbidden, so every errors.Is(err,
// ErrForbidden) check still holds. Its text never includes a token.
type PermissionError struct {
	// Permission is what the token lacks, as GitHub's settings page names
	// it: "Workflows: write", or a set joined with " and ". For a classic
	// or OAuth token, "OAuth scope: repo".
	Permission string
	// Repo is the owner/name the refused call was about; "" for none.
	Repo string
}

func (e *PermissionError) Error() string {
	if e.Repo == "" {
		return "the token lacks " + e.Permission
	}
	return "the token lacks " + e.Permission + " on " + e.Repo
}

func (e *PermissionError) Unwrap() error { return ErrForbidden }

// HostError carries the host's own message for a failed call (GitHub's
// "message" field) and nothing else from the response. Its text is Err's.
type HostError struct {
	Message string
	Err     error
}

func (e *HostError) Error() string { return e.Err.Error() }

func (e *HostError) Unwrap() error { return e.Err }

// HostMessage is the host's message carried in err's chain, on one line
// of printable characters (no terminal escapes), or "".
func HostMessage(err error) string {
	var h *HostError
	if !errors.As(err, &h) {
		return ""
	}
	m := strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			return r
		}
		return -1
	}, h.Message)
	m = strings.Join(strings.Fields(m), " ")
	if r := []rune(m); len(r) > 200 {
		m = string(r[:200]) + "…"
	}
	return m
}

type Repo struct {
	FullName      string
	Private       bool
	Fork          bool
	DefaultBranch string
	CloneURL      string
	// OwnerType is GitHub's type of the repo's owner: OwnerOrganization or
	// OwnerUser ("" when unknown).
	OwnerType string
}

// The owner types GitHub reports.
const (
	OwnerOrganization = "Organization"
	OwnerUser         = "User"
)

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
	ID         int64
	Workflow   string
	Status     string // queued | in_progress | waiting | requested | pending | completed
	Conclusion string // set once Status is completed: success | failure | cancelled | …
	URL        string
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
	// CancelRun asks to cancel a queued or in-progress run. The host only
	// accepts the request: the run may keep going. A run the host won't
	// cancel in its current state, finished or not, is ErrConflict.
	CancelRun(ctx context.Context, full string, id int64) error
	// ForceCancelRun force-cancels a run that ignored CancelRun. Like
	// CancelRun, the host only accepts the request (a 202): the run may
	// still keep going. A 409 is ErrConflict, a plain permission refusal
	// ErrForbidden.
	ForceCancelRun(ctx context.Context, full string, id int64) error
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
