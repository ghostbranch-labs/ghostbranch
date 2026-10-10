// Package github is the GitHub adapter: the only code in Ghostbranch that
// imports a GitHub API client.
package github

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
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

// ErrNoToken means New was given an empty token. Ghostbranch never builds
// an unauthenticated client: it would read public repos and then fail on
// private ones with confusing 404s.
var ErrNoToken = errors.New("github: no token")

// New returns a client for api.github.com. The token is trimmed.
func New(token string) (*Client, error) { return newClient(token, nil) }

// NewWithBaseURL points the client at another API root (tests).
func NewWithBaseURL(token, baseURL string) (*Client, error) { return newClient(token, &baseURL) }

func newClient(token string, baseURL *string) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrNoToken
	}
	opts := []gh.ClientOptionsFunc{gh.WithAuthToken(token)}
	if baseURL != nil {
		opts = append(opts, gh.WithURLs(baseURL, nil))
	}
	c, err := gh.NewClient(opts...)
	if err != nil {
		return nil, err
	}
	return &Client{c: c, token: token, gitBase: "https://github.com", http: probeClient()}, nil
}

// probeClient is PushAccess's HTTP client. It never follows a redirect: a
// redirect to a page that answers 200 would read as "can push".
func probeClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// wrap classifies a go-github error and keeps it in the chain (%w), so
// callers can errors.As to *gh.ErrorResponse. full names the repo the call
// was about, "" for none.
func wrap(ctx context.Context, full string, resp *gh.Response, err error) error {
	if err == nil {
		return nil
	}
	// A cancelled or timed-out command isn't a GitHub failure: retrying
	// wouldn't help, and the CLI reports INTERRUPTED.
	if cerr := ctx.Err(); cerr != nil {
		if errors.Is(err, cerr) {
			return err
		}
		return fmt.Errorf("%w: %w", cerr, err)
	}
	status := statusOf(resp)
	switch {
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: %w", forge.ErrNotFound, err)
	case isForbidden(resp, err):
		return forbidden(full, resp, err)
	case status == http.StatusUnauthorized:
		// A bad, expired or revoked token: retrying sends the same token.
		return err
	}
	var rl *gh.RateLimitError
	var arl *gh.AbuseRateLimitError
	if errors.As(err, &rl) || errors.As(err, &arl) || waitAsked(resp) || status == http.StatusTooManyRequests || status == 0 || status >= 500 {
		return fmt.Errorf("%w: %w", forge.ErrTransient, err)
	}
	return err
}

func statusOf(resp *gh.Response) int {
	if resp == nil || resp.Response == nil {
		return 0
	}
	return resp.StatusCode
}

// forbidden wraps a plain 403: a *forge.PermissionError when GitHub says
// the resource is not accessible and its headers name what the token
// lacks, otherwise forge.ErrForbidden. X-Accepted-GitHub-Permissions comes
// on every 403 to a fine-grained or app token, SAML SSO, archived repos and
// org policies included, so the header alone names nothing.
func forbidden(full string, resp *gh.Response, err error) error {
	if p := missingPermission(resp.Header); p != "" && inaccessible(err) {
		return fmt.Errorf("%w: %w", &forge.PermissionError{Permission: p, Repo: full}, err)
	}
	return fmt.Errorf("%w: %w", forge.ErrForbidden, err)
}

// inaccessible reports GitHub's "Resource not accessible by …" 403, the one
// that means the token lacks a permission.
func inaccessible(err error) bool {
	var ge *gh.ErrorResponse
	return errors.As(err, &ge) && strings.HasPrefix(ge.Message, "Resource not accessible by")
}

// waitAsked reports a 403 that tells the caller to wait (Retry-After or no
// rate limit left) but that go-github didn't classify as a rate limit, such
// as an unrecognised secondary limit. It may pass on its own.
func waitAsked(resp *gh.Response) bool {
	if statusOf(resp) != http.StatusForbidden {
		return false
	}
	return resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0"
}

// permissionNames are GitHub's settings-page names for the permission keys
// X-Accepted-GitHub-Permissions uses.
var permissionNames = map[string]string{
	"actions": "Actions", "administration": "Administration", "checks": "Checks", "contents": "Contents",
	"deployments": "Deployments", "environments": "Environments", "issues": "Issues", "metadata": "Metadata",
	"pull_requests": "Pull requests", "secrets": "Secrets", "statuses": "Commit statuses", "workflows": "Workflows",
}

// missingPermission names what a 403 says the token needs, or "".
//
// Fine-grained PATs and GitHub App tokens get X-Accepted-GitHub-Permissions:
// commas join the permissions one set needs, semicolons separate
// alternative sets; the first set is named. Classic and OAuth tokens get
// X-Accepted-OAuth-Scopes, with the token's own scopes in X-OAuth-Scopes;
// a scope is named only when the token holds none of the accepted ones.
func missingPermission(h http.Header) string {
	if v := strings.TrimSpace(h.Get("X-Accepted-GitHub-Permissions")); v != "" {
		set, _, _ := strings.Cut(v, ";")
		var names []string
		for _, p := range splitList(set) {
			key, level, ok := strings.Cut(p, "=")
			if !ok || key == "" || level == "" {
				continue
			}
			names = append(names, permissionName(key)+": "+level)
		}
		return strings.Join(names, " and ")
	}
	accepted := splitList(h.Get("X-Accepted-OAuth-Scopes"))
	if _, told := h[http.CanonicalHeaderKey("X-OAuth-Scopes")]; len(accepted) == 0 || !told {
		return ""
	}
	held := splitList(h.Get("X-OAuth-Scopes"))
	for _, s := range accepted {
		if slices.Contains(held, s) {
			return ""
		}
	}
	return "OAuth scope: " + strings.Join(accepted, " or ")
}

func permissionName(key string) string {
	if n, ok := permissionNames[key]; ok {
		return n
	}
	s := strings.ReplaceAll(key, "_", " ")
	return strings.ToUpper(s[:1]) + s[1:]
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func toRepo(r *gh.Repository) forge.Repo {
	return forge.Repo{FullName: r.GetFullName(), Private: r.GetPrivate(), Fork: r.GetFork(),
		DefaultBranch: r.GetDefaultBranch(), CloneURL: r.GetCloneURL(), OwnerType: r.GetOwner().GetType()}
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
	if err := wrap(ctx, full, resp, err); err != nil {
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
	// A refusal keeps GitHub's message, for the person to read.
	if isForbidden(resp, err) {
		return forge.Repo{}, fmt.Errorf("create %s: %w", full, forbidden(full, resp, hostErr(err)))
	}
	if err != nil && org != "" && statusOf(resp) == http.StatusNotFound {
		return forge.Repo{}, fmt.Errorf("%w: create %s: %w", forge.ErrForbidden, full, hostErr(err))
	}
	if err := wrap(ctx, full, resp, err); err != nil {
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
	return wrap(ctx, full, resp, err)
}

func (c *Client) FindOpenPR(ctx context.Context, full, head, base string) (*forge.PR, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	prs, resp, err := c.c.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{
		State: "open", Head: owner + ":" + head, Base: base,
	})
	if err := wrap(ctx, full, resp, err); err != nil {
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
	if err := wrap(ctx, full, resp, err); err != nil {
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
	if err := wrap(ctx, full, resp, err); err != nil {
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
		if err := wrap(ctx, full, resp, err); err != nil {
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
	if err := wrap(ctx, "", resp, err); err != nil {
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
// runs. Rate limits, and a 403 that says to wait, also arrive as 403 but
// are not permission problems.
func isForbidden(resp *gh.Response, err error) bool {
	if err == nil || statusOf(resp) != http.StatusForbidden || waitAsked(resp) {
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
		if err := wrap(ctx, full, resp, err); err != nil {
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
			return nil, fmt.Errorf("token cannot read checks: grant the fine-grained token \"Actions: read\" and \"Commit statuses: read\" on %s, or use a token with repo scope: %w", full, forbidden(full, resp, err))
		}
		if err := wrap(ctx, full, resp, err); err != nil {
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
	// No token, no probe: an anonymous request would never be told it may push.
	if c.token == "" || c.http == nil {
		return forge.PushAccess{Can: "unknown"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.gitBase+"/"+full+".git/info/refs?service=git-receive-pack", nil)
	if err != nil {
		return forge.PushAccess{Can: "unknown"}
	}
	req.SetBasicAuth("x-access-token", c.token)
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
				return fmt.Errorf("list secrets of %s: %w", full, forbidden(full, resp, err))
			}
			if notFoundOK && err != nil && resp != nil && resp.StatusCode == 404 {
				return nil
			}
			if err := wrap(ctx, full, resp, err); err != nil {
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
			return nil, fmt.Errorf("list workflow runs of %s: %w", full, forbidden(full, resp, err))
		}
		if err := wrap(ctx, full, resp, err); err != nil {
			return nil, err
		}
		for _, r := range runs.WorkflowRuns {
			out = append(out, forge.Run{ID: r.GetID(), Workflow: r.GetName(), Status: r.GetStatus(), Conclusion: r.GetConclusion(), URL: r.GetHTMLURL()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// CancelRun asks GitHub to cancel workflow run id. GitHub answers 202, which
// only accepts the request; a 409 (forge.ErrConflict) means GitHub won't
// cancel the run in its current state, often because it has finished.
func (c *Client) CancelRun(ctx context.Context, full string, id int64) error {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return err
	}
	resp, err := c.c.Actions.CancelWorkflowRunByID(ctx, owner, name, id)
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) {
		return nil
	}
	if err != nil && resp != nil && resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: cancel workflow run %d of %s", forge.ErrConflict, id, full)
	}
	if isForbidden(resp, err) {
		return fmt.Errorf("cancel workflow run %d of %s: %w", id, full, forbidden(full, resp, err))
	}
	return wrap(ctx, full, resp, err)
}

// ForceCancelRun force-cancels workflow run id, for a run that ignored
// CancelRun. go-github has no wrapper, so this is a raw request; GitHub
// answers 202.
func (c *Client) ForceCancelRun(ctx context.Context, full string, id int64) error {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return err
	}
	req, err := c.c.NewRequest(ctx, "POST", fmt.Sprintf("repos/%s/%s/actions/runs/%d/force-cancel", owner, name, id), nil)
	if err != nil {
		return err
	}
	resp, err := c.c.Do(req, nil)
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) {
		return nil
	}
	if err != nil && resp != nil && resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: force-cancel workflow run %d of %s", forge.ErrConflict, id, full)
	}
	if isForbidden(resp, err) {
		return fmt.Errorf("force-cancel workflow run %d of %s: %w", id, full, forbidden(full, resp, err))
	}
	return wrap(ctx, full, resp, err)
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
		return fmt.Errorf("close PR #%d of %s: %w", number, full, forbidden(full, resp, err))
	}
	return wrap(ctx, full, resp, err)
}

// ProbePermissions learns what the token may do on full with read calls
// only (forge.PermissionProber). A fine-grained token can't be asked what
// it may write, so only Contents: write is probed, through PushAccess.
func (c *Client) ProbePermissions(ctx context.Context, full string) (forge.RepoPermissions, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.RepoPermissions{}, err
	}
	out := forge.RepoPermissions{Have: map[string]string{}, Accepted: map[string]string{}}
	r, resp, err := c.c.Repositories.Get(ctx, owner, name)
	switch {
	case err == nil:
		out.Have[forge.PermMetadataRead] = forge.AccessYes
	case ctx.Err() == nil && (statusOf(resp) == http.StatusNotFound || isForbidden(resp, err)):
		out.Have[forge.PermMetadataRead] = forge.AccessNo
		return out, nil
	default:
		return out, wrap(ctx, full, resp, err)
	}
	note := func(perm string, resp *gh.Response, err error) {
		switch {
		case err == nil:
			out.Have[perm] = forge.AccessYes
		case isForbidden(resp, err):
			out.Have[perm] = forge.AccessNo
			if h := resp.Header.Get("X-Accepted-GitHub-Permissions"); h != "" {
				out.Accepted[perm] = h
			}
		case statusOf(resp) == http.StatusConflict && perm == forge.PermContentsRead:
			out.Have[perm] = forge.AccessYes // an empty repo has no commits to list
		}
	}
	one := gh.ListOptions{PerPage: 1}
	_, resp, err = c.c.Repositories.ListCommits(ctx, owner, name, &gh.CommitsListOptions{ListOptions: one})
	note(forge.PermContentsRead, resp, err)
	_, resp, err = c.c.PullRequests.List(ctx, owner, name, &gh.PullRequestListOptions{State: "all", ListOptions: one})
	note(forge.PermPullsRead, resp, err)
	_, resp, err = c.c.Actions.ListRepositoryWorkflowRuns(ctx, owner, name, &gh.ListWorkflowRunsOptions{ListOptions: one})
	note(forge.PermActionsRead, resp, err)
	_, resp, err = c.c.Repositories.GetCombinedStatus(ctx, owner, name, r.GetDefaultBranch(), &one)
	note(forge.PermStatusesRead, resp, err)
	_, resp, err = c.c.Actions.ListRepoSecrets(ctx, owner, name, &one)
	note(forge.PermSecretsRead, resp, err)
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	switch c.PushAccess(ctx, full).Can {
	case "true":
		out.Have[forge.PermContentsWrite] = forge.AccessYes
	case "false":
		out.Have[forge.PermContentsWrite] = forge.AccessNo
	}
	return out, nil
}

// OAuthScopes reads a classic token's scopes from GET /user's
// X-OAuth-Scopes header (forge.PermissionProber).
func (c *Client) OAuthScopes(ctx context.Context) ([]string, bool, error) {
	_, resp, err := c.c.Users.Get(ctx, "")
	if err := wrap(ctx, "", resp, err); err != nil {
		return nil, false, err
	}
	if resp == nil || resp.Response == nil {
		return nil, false, nil
	}
	vals, ok := resp.Header[http.CanonicalHeaderKey("X-OAuth-Scopes")]
	if !ok {
		return nil, false, nil
	}
	scopes := []string{}
	for _, v := range vals {
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				scopes = append(scopes, s)
			}
		}
	}
	return scopes, true, nil
}

// hostErr keeps GitHub's message of err (its "message" field only) in the
// chain as a *forge.HostError; err without one is returned as is.
func hostErr(err error) error {
	var ge *gh.ErrorResponse
	if !errors.As(err, &ge) || ge.Message == "" {
		return err
	}
	return &forge.HostError{Message: ge.Message, Err: err}
}
