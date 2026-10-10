package github

import (
	"context"
	"fmt"
	"net/http"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// CreateStack makes a native stack of prs, bottom first (forge.Stacker):
// POST /repos/{o}/{r}/stacks.
func (c *Client) CreateStack(ctx context.Context, full string, prs []int) (forge.Stack, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.Stack{}, err
	}
	s, resp, err := c.c.PullRequests.CreateStack(ctx, owner, name, gh.PullRequestCreateStackRequest{PullRequests: prs})
	if err := stackErr(ctx, full, fmt.Sprintf("create a stack of PRs %v", prs), resp, err); err != nil {
		return forge.Stack{}, err
	}
	return toStack(s), nil
}

// AddToStack appends prs, bottom first, to stack number stack
// (forge.Stacker): POST /repos/{o}/{r}/stacks/{n}/add.
func (c *Client) AddToStack(ctx context.Context, full string, stack int, prs []int) (forge.Stack, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return forge.Stack{}, err
	}
	s, resp, err := c.c.PullRequests.AddToStack(ctx, owner, name, stack, gh.PullRequestAddToStackRequest{PullRequests: prs})
	if err := stackErr(ctx, full, fmt.Sprintf("add PRs %v to stack %d", prs, stack), resp, err); err != nil {
		return forge.Stack{}, err
	}
	return toStack(s), nil
}

// StackOf is the stack PR pr is in (forge.Stacker). The PR carries its
// stack's number even once merged; a PR without one, as on a host without
// native stacks, is nil.
func (c *Client) StackOf(ctx context.Context, full string, pr int) (*forge.Stack, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	p, resp, err := c.c.PullRequests.Get(ctx, owner, name, pr)
	if err := wrap(ctx, full, resp, err); err != nil {
		return nil, fmt.Errorf("read PR #%d of %s: %w", pr, full, err)
	}
	if p.GetStack() == nil || p.GetStack().GetNumber() == 0 {
		return nil, nil
	}
	n := p.GetStack().GetNumber()
	s, resp, err := c.c.PullRequests.GetStack(ctx, owner, name, n)
	if err := stackErr(ctx, full, fmt.Sprintf("read stack %d", n), resp, err); err != nil {
		return nil, err
	}
	out := toStack(s)
	return &out, nil
}

func toStack(s *gh.PullRequestStackDetails) forge.Stack {
	out := forge.Stack{Number: s.GetNumber()}
	for _, p := range s.GetPullRequests() {
		out.PRs = append(out.PRs, p.GetNumber())
	}
	return out
}

// stackErr classifies a failed stacks call. A 404 (a host without native
// stacks, or a stack that is gone) and a 422 (GitHub won't stack these PRs:
// they don't chain, one is from a fork, or one is already stacked) are
// forge.ErrStacksUnavailable, with GitHub's message kept: the caller falls
// back to plain chained PRs. A 409 is forge.ErrConflict: another change to
// the stack is in progress, so retry. Anything else is classified as any
// other call is.
func stackErr(ctx context.Context, full, what string, resp *gh.Response, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() == nil {
		switch statusOf(resp) {
		case http.StatusNotFound, http.StatusUnprocessableEntity:
			return fmt.Errorf("%s in %s: %w: %w", what, full, forge.ErrStacksUnavailable, hostErr(err))
		case http.StatusConflict:
			return fmt.Errorf("%s in %s: %w: %w", what, full, forge.ErrConflict, hostErr(err))
		}
	}
	return fmt.Errorf("%s in %s: %w", what, full, wrap(ctx, full, resp, err))
}
