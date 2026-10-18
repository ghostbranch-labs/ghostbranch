package github

import (
	"context"
	"fmt"
	"net/http"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// installErr is wrap, except that a 401 is forge.ErrForbidden too: a token
// that is not an app login is refused by these two endpoints, and a caller
// treats both the same way.
func installErr(ctx context.Context, resp *gh.Response, err error) error {
	if err != nil && ctx.Err() == nil && statusOf(resp) == http.StatusUnauthorized {
		return fmt.Errorf("%w: %w", forge.ErrForbidden, err)
	}
	return wrap(ctx, "", resp, err)
}

// UserInstallations lists the app's installs the user can see
// (forge.Installer): GET /user/installations, every page.
func (c *Client) UserInstallations(ctx context.Context) ([]forge.Installation, error) {
	var out []forge.Installation
	opt := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.c.Apps.ListUserInstallations(ctx, opt)
		if err := installErr(ctx, resp, err); err != nil {
			return nil, fmt.Errorf("list the app's installations: %w", err)
		}
		for _, in := range page {
			out = append(out, forge.Installation{
				ID:          in.GetID(),
				Account:     in.GetAccount().GetLogin(),
				AccountType: in.GetAccount().GetType(),
				Selection:   in.GetRepositorySelection(),
			})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}

// InstallationRepos lists the repos installation id covers that the user
// can also access (forge.Installer):
// GET /user/installations/{id}/repositories, every page.
func (c *Client) InstallationRepos(ctx context.Context, id int64) ([]string, error) {
	var out []string
	opt := &gh.ListOptions{PerPage: 100}
	for {
		page, resp, err := c.c.Apps.ListUserRepos(ctx, id, opt)
		if err := installErr(ctx, resp, err); err != nil {
			return nil, fmt.Errorf("list the repos of installation %d: %w", id, err)
		}
		for _, r := range page.Repositories {
			out = append(out, r.GetFullName())
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opt.Page = resp.NextPage
	}
}
