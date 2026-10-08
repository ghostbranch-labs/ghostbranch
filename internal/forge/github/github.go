// Package github is the GitHub adapter: the only code in Ghostbranch that
// imports a GitHub API client.
package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

type Client struct {
	c    *gh.Client
	mu   sync.Mutex
	user *forge.User

	token   string
	gitBase string       // https://github.com, or a test server
	http    *http.Client // for git smart-HTTP probes

	limMu   sync.Mutex
	limited map[string]bool // repos whose checks came from the workflow-runs fallback
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
	return &Client{c: c, token: token, gitBase: "https://github.com", http: &http.Client{Timeout: 15 * time.Second}}
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
	return &Client{c: c, token: token, gitBase: "https://github.com", http: &http.Client{Timeout: 15 * time.Second}}, nil
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
		State: p.GetState(), Draft: p.GetDraft(), URL: p.GetHTMLURL(), Merged: p.GetMerged()}
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
	// The public repo was already read, so a 404 on an org owner means "not allowed".
	if isForbidden(resp, err) || (err != nil && org != "" && resp != nil && resp.StatusCode == 404) {
		return forge.Repo{}, fmt.Errorf("%w: create %s: %v", forge.ErrForbidden, full, err)
	}
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
	out, err := c.checkRuns(ctx, owner, name, full, sha)
	if err != nil {
		return nil, err
	}
	sopts := &gh.ListOptions{PerPage: 100}
	for {
		st, resp, err := c.c.Repositories.GetCombinedStatus(ctx, owner, name, sha, sopts)
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
		if resp.NextPage == 0 {
			break
		}
		sopts.Page = resp.NextPage
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

// ChecksLimited reports whether ListChecks had to fall back to workflow runs
// for full because the token cannot read check runs. Third-party check apps
// are invisible in that mode.
func (c *Client) ChecksLimited(full string) bool {
	c.limMu.Lock()
	defer c.limMu.Unlock()
	return c.limited[full]
}

func (c *Client) markLimited(full string) {
	c.limMu.Lock()
	defer c.limMu.Unlock()
	if c.limited == nil {
		c.limited = map[string]bool{}
	}
	c.limited[full] = true
}

// isForbidden reports a plain 403, as a fine-grained token gets for check
// runs. Rate limits also arrive as 403 but are not permission problems.
func isForbidden(resp *gh.Response, err error) bool {
	if err == nil || resp == nil || resp.StatusCode != 403 {
		return false
	}
	var rl *gh.RateLimitError
	var arl *gh.AbuseRateLimitError
	return !errors.As(err, &rl) && !errors.As(err, &arl)
}

// checkRuns lists check runs for sha. Fine-grained personal access tokens
// cannot read them (403), so it then falls back to GitHub Actions workflow
// runs for the commit.
func (c *Client) checkRuns(ctx context.Context, owner, name, full, sha string) ([]forge.Check, error) {
	var out []forge.Check
	opts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.c.Checks.ListCheckRunsForRef(ctx, owner, name, sha, opts)
		if isForbidden(resp, err) {
			return c.workflowRuns(ctx, owner, name, full, sha)
		}
		if err := wrap(resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.CheckRuns {
			out = append(out, forge.Check{Name: r.GetName(), Status: r.GetStatus(), Conclusion: r.GetConclusion()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) workflowRuns(ctx context.Context, owner, name, full, sha string) ([]forge.Check, error) {
	var out []forge.Check
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.c.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, opts)
		if isForbidden(resp, err) {
			return nil, fmt.Errorf("token cannot read checks: grant the fine-grained token \"Actions: read\" and "+
				"\"Commit statuses: read\" on %s, or use a token with repo scope: %w", full, err)
		}
		if err := wrap(resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.WorkflowRuns {
			ch := forge.Check{Name: r.GetName(), Status: "in_progress", Conclusion: r.GetConclusion()}
			if ch.Name == "" {
				ch.Name = fmt.Sprintf("workflow %d", r.GetID())
			}
			if r.GetStatus() == "completed" {
				ch.Status = "completed"
			}
			out = append(out, ch)
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	c.markLimited(full)
	return out, nil
}

// WithGitBase points git smart-HTTP probes at another host (tests).
func (c *Client) WithGitBase(base string) *Client {
	c.gitBase = strings.TrimRight(base, "/")
	return c
}

// PushAccess asks GitHub's git endpoint for push discovery. GitHub answers
// 200 only when the token may write, and 403 when it may not. Nothing is
// pushed. The REST permissions field is not used: it reflects the user's
// role, not what a fine-grained token may do.
func (c *Client) PushAccess(ctx context.Context, full string) forge.PushAccess {
	if _, _, err := forge.SplitName(full); err != nil {
		return forge.PushAccess{Can: "unknown"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.gitBase+"/"+full+".git/info/refs?service=git-receive-pack", nil)
	if err != nil {
		return forge.PushAccess{Can: "unknown"}
	}
	if c.token != "" {
		req.SetBasicAuth("x-access-token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return forge.PushAccess{Can: "unknown"}
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
		return forge.PushAccess{Can: "true", HTTPStatus: resp.StatusCode}
	case http.StatusForbidden:
		return forge.PushAccess{Can: "false", HTTPStatus: resp.StatusCode}
	}
	return forge.PushAccess{Can: "unknown", HTTPStatus: resp.StatusCode}
}

// SecretNames lists the Actions secret names of full and the org secrets
// shared with it, sorted and de-duplicated. Values are never read. A user
// repo has no org secrets, so a 404 on that listing is not an error.
func (c *Client) SecretNames(ctx context.Context, full string) ([]string, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	list := func(f func(context.Context, string, string, *gh.ListOptions) (*gh.Secrets, *gh.Response, error), notFoundOK bool) error {
		opts := &gh.ListOptions{PerPage: 100}
		for {
			s, resp, err := f(ctx, owner, name, opts)
			if isForbidden(resp, err) {
				return fmt.Errorf("%w: list secrets of %s: %v", forge.ErrForbidden, full, err)
			}
			if notFoundOK && err != nil && resp != nil && resp.StatusCode == 404 {
				return nil
			}
			if err := wrap(resp, err); err != nil {
				return err
			}
			for _, sec := range s.Secrets {
				seen[sec.Name] = true
			}
			if resp.NextPage == 0 {
				return nil
			}
			opts.Page = resp.NextPage
		}
	}
	if err := list(c.c.Actions.ListRepoSecrets, false); err != nil {
		return nil, err
	}
	if err := list(c.c.Actions.ListRepoOrgSecrets, true); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out, nil
}

// PushRuns lists the Actions runs for sha that a push started, any status.
func (c *Client) PushRuns(ctx context.Context, full, sha string) ([]forge.Run, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	out := []forge.Run{}
	opts := &gh.ListWorkflowRunsOptions{HeadSHA: sha, Event: "push", ListOptions: gh.ListOptions{PerPage: 100}}
	for {
		runs, resp, err := c.c.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, opts)
		if isForbidden(resp, err) {
			return nil, fmt.Errorf("%w: list workflow runs of %s: %v", forge.ErrForbidden, full, err)
		}
		if err := wrap(resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.WorkflowRuns {
			out = append(out, forge.Run{ID: r.GetID(), Workflow: r.GetName(), Status: r.GetStatus(), URL: r.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// CancelRun cancels workflow run id. GitHub answers 202; a 409 means the
// run has already finished, which is what cancelling wanted.
func (c *Client) CancelRun(ctx context.Context, full string, id int64) error {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return err
	}
	resp, err := c.c.Actions.CancelWorkflowRunByID(ctx, owner, name, id)
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) || (err != nil && resp != nil && resp.StatusCode == http.StatusConflict) {
		return nil
	}
	if isForbidden(resp, err) {
		return fmt.Errorf("%w: cancel workflow run %d of %s: %v", forge.ErrForbidden, id, full, err)
	}
	return wrap(resp, err)
}

// ClosePullRequest closes PR number. Closing a closed PR is a no-op on
// GitHub; a plain 403 is forge.ErrForbidden.
func (c *Client) ClosePullRequest(ctx context.Context, full string, number int) error {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return err
	}
	_, resp, err := c.c.PullRequests.Edit(ctx, owner, name, number, &gh.PullRequest{State: new("closed")})
	if isForbidden(resp, err) {
		return fmt.Errorf("%w: close PR #%d of %s: %v", forge.ErrForbidden, number, full, err)
	}
	return wrap(resp, err)
}
