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
