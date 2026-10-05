// Package github is the GitHub adapter: the only code in Ghostbranch that
// imports a GitHub API client.
package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

type Client struct {
	c    *gh.Client
	mu   sync.Mutex
	user *forge.User
}

// New returns a client for api.github.com.
func New(token string) *Client {
	// go-github v92 rejects an empty token; fall back to unauthenticated.
	var opts []gh.ClientOptionsFunc
	if token != "" {
		opts = append(opts, gh.WithAuthToken(token))
	}
	// These options cannot fail, so the error is always nil.
	c, _ := gh.NewClient(opts...)
	return &Client{c: c}
}

// NewWithBaseURL points the client at another API root (tests).
func NewWithBaseURL(token, baseURL string) (*Client, error) {
	opts := []gh.ClientOptionsFunc{gh.WithURLs(&baseURL, nil)}
	if token != "" {
		opts = append(opts, gh.WithAuthToken(token))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &Client{c: c}, nil
}

func wrap(resp *gh.Response, err error) error {
	if err == nil {
		return nil
	}
	if resp != nil && resp.StatusCode == 404 {
		return fmt.Errorf("%w: %v", forge.ErrNotFound, err)
	}
	var rl *gh.RateLimitError
	var arl *gh.AbuseRateLimitError
	if errors.As(err, &rl) || errors.As(err, &arl) || resp == nil || resp.StatusCode >= 500 {
		return fmt.Errorf("%w: %v", forge.ErrTransient, err)
	}
	return err
}

func toRepo(r *gh.Repository) forge.Repo {
	return forge.Repo{FullName: r.GetFullName(), Private: r.GetPrivate(), Fork: r.GetFork(),
		DefaultBranch: r.GetDefaultBranch(), CloneURL: r.GetCloneURL()}
}

func toPR(p *gh.PullRequest) forge.PR {
	return forge.PR{Number: p.GetNumber(), Head: p.GetHead().GetRef(), Base: p.GetBase().GetRef(),
		State: p.GetState(), Draft: p.GetDraft(), URL: p.GetHTMLURL()}
}

func (c *Client) GetRepo(ctx context.Context, full string) (forge.Repo, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.Repo{}, err
	}
	r, resp, err := c.c.Repositories.Get(ctx, owner, name)
	if err := wrap(resp, err); err != nil {
		return forge.Repo{}, err
	}
	return toRepo(r), nil
}

func (c *Client) CreatePrivateRepo(ctx context.Context, full string) (forge.Repo, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.Repo{}, err
	}
	u, err := c.CurrentUser(ctx)
	if err != nil {
		return forge.Repo{}, err
	}
	org := owner
	if strings.EqualFold(owner, u.Login) {
		org = "" // personal account
	}
	r, resp, err := c.c.Repositories.Create(ctx, org, &gh.Repository{
		Name:        new(name),
		Private:     new(true),
		AutoInit:    new(false),
		Description: new("Ghostbranch shadow repo (private). Do not make public."),
	})
	if err := wrap(resp, err); err != nil {
		return forge.Repo{}, err
	}
	return toRepo(r), nil
}

func (c *Client) SetDefaultBranch(ctx context.Context, full, branch string) error {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return err
	}
	_, resp, err := c.c.Repositories.Edit(ctx, owner, name, &gh.Repository{DefaultBranch: new(branch)})
	return wrap(resp, err)
}

func (c *Client) FindOpenPR(ctx context.Context, full, head, base string) (*forge.PR, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	prs, resp, err := c.c.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + head, Base: base,
	})
	if err := wrap(resp, err); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	p := toPR(prs[0])
	return &p, nil
}

func (c *Client) CreatePR(ctx context.Context, full string, in forge.NewPR) (forge.PR, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.PR{}, err
	}
	p, resp, err := c.c.PullRequests.Create(ctx, owner, name, gh.CreatePullRequest{
		Title: new(in.Title), Body: new(in.Body), Head: in.Head, Base: in.Base, Draft: new(in.Draft),
	})
	if err := wrap(resp, err); err != nil {
		return forge.PR{}, err
	}
	return toPR(p), nil
}

func (c *Client) GetPR(ctx context.Context, full string, number int) (forge.PR, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.PR{}, err
	}
	p, resp, err := c.c.PullRequests.Get(ctx, owner, name, number)
	if err := wrap(resp, err); err != nil {
		return forge.PR{}, err
	}
	return toPR(p), nil
}

// ListChecks merges check runs and legacy commit statuses for sha.
func (c *Client) ListChecks(ctx context.Context, full, sha string) ([]forge.Check, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	var out []forge.Check
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.c.Checks.ListCheckRunsForRef(ctx, owner, name, sha, opts)
		if err := wrap(resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.CheckRuns {
			out = append(out, forge.Check{Name: r.GetName(), Status: r.GetStatus(), Conclusion: r.GetConclusion()})
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	st, resp, err := c.c.Repositories.GetCombinedStatus(ctx, owner, name, sha, &gh.ListOptions{PerPage: 100})
	if err := wrap(resp, err); err != nil {
		return nil, err
	}
	for _, s := range st.Statuses {
		ch := forge.Check{Name: s.GetContext()}
		switch s.GetState() {
		case "pending":
			ch.Status = "in_progress"
		case "success":
			ch.Status, ch.Conclusion = "completed", "success"
		default: // failure, error
			ch.Status, ch.Conclusion = "completed", "failure"
		}
		out = append(out, ch)
	}
	return out, nil
}

func (c *Client) CurrentUser(ctx context.Context) (forge.User, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.user != nil {
		return *c.user, nil
	}
	u, resp, err := c.c.Users.Get(ctx, "")
	if err := wrap(resp, err); err != nil {
		return forge.User{}, err
	}
	c.user = &forge.User{Login: u.GetLogin(), ID: u.GetID()}
	return *c.user, nil
}
