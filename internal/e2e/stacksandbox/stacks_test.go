//go:build stacksandbox

// Package stacksandbox_test checks GitHub's native stacked PRs on
// ghostbranch-labs/test for the cases spec § 9 left untested. It pushes
// branches and opens, merges and closes PRs there, so the owner runs it by
// hand and records the answers in the findings file:
//
//	GHOSTBRANCH_STACKS_TOKEN=... go test -tags stacksandbox -count=1 -v ./internal/e2e/stacksandbox/
package stacksandbox_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

const (
	repo       = "ghostbranch-labs/test"
	apiVersion = "2026-03-10"
)

func setup(t *testing.T) (*gh.Client, *gitx.Git, string, string) {
	t.Helper()
	tok := os.Getenv("GHOSTBRANCH_STACKS_TOKEN")
	if tok == "" {
		t.Skip("GHOSTBRANCH_STACKS_TOKEN is not set")
	}
	ghc, err := gh.NewClient(gh.WithAuthToken(tok))
	if err != nil {
		t.Fatal(err)
	}
	g := testutil.BareRepo(t, t.TempDir())
	cred := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + tok))
	g.Config = [][2]string{{"http.https://github.com/.extraheader", "AUTHORIZATION: basic " + cred}}
	if err := g.AddRemote(context.Background(), "origin", "https://github.com/"+repo+".git"); err != nil {
		t.Fatal(err)
	}
	if err := g.Fetch(context.Background(), "origin", "+refs/heads/main:refs/heads/main"); err != nil {
		t.Fatal(err)
	}
	owner, name, _ := strings.Cut(repo, "/")
	return ghc, g, owner, name
}

// call makes a REST call go-github has no wrapper for. It returns the HTTP
// status; a 202 is an answer, not an error.
func call(ctx context.Context, ghc *gh.Client, method, path string, body, out any) (int, error) {
	req, err := ghc.NewRequest(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	resp, err := ghc.Do(req, out)
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) {
		if out != nil && len(accepted.Raw) > 0 {
			err = json.Unmarshal(accepted.Raw, out)
		} else {
			err = nil
		}
		return http.StatusAccepted, err
	}
	if resp != nil {
		return resp.StatusCode, err
	}
	return 0, err
}

// branches pushes a chain of branches, each one commit on the one before
// (the first on main), and opens a PR for each on the branch below. It
// cleans up after the test: open PRs closed, branches deleted.
func branches(t *testing.T, ghc *gh.Client, g *gitx.Git, owner, name string, names ...string) []int {
	t.Helper()
	ctx := context.Background()
	var prs []int
	base := "main"
	for _, b := range names {
		from, err := g.MustResolve(ctx, "refs/heads/"+base)
		if err != nil {
			t.Fatal(err)
		}
		if err := g.UpdateRef(ctx, "refs/heads/"+b, from); err != nil {
			t.Fatal(err)
		}
		testutil.Commit(t, g, b, "sandbox "+b, map[string]string{b + ".txt": b + "\n"})
		if err := g.Push(ctx, "origin", gitx.PushOptions{}, "refs/heads/"+b+":refs/heads/"+b); err != nil {
			t.Fatal(err)
		}
		pr, _, err := ghc.PullRequests.Create(ctx, owner, name, gh.CreatePullRequest{Title: gh.Ptr("sandbox " + b), Head: b, Base: base})
		if err != nil {
			t.Fatal(err)
		}
		prs = append(prs, pr.GetNumber())
		base = b
	}
	t.Cleanup(func() {
		for _, n := range prs {
			_, _, _ = ghc.PullRequests.Edit(ctx, owner, name, n, &gh.PullRequest{State: gh.Ptr("closed")})
		}
		specs := make([]string, len(names))
		for i, b := range names {
			specs[i] = ":refs/heads/" + b
		}
		_ = g.Push(ctx, "origin", gitx.PushOptions{}, specs...)
	})
	return prs
}

func createStack(t *testing.T, ghc *gh.Client, owner, name string, prs []int) (int, int) {
	t.Helper()
	var st struct {
		Number int `json:"number"`
	}
	code, err := call(context.Background(), ghc, "POST", fmt.Sprintf("repos/%s/%s/stacks", owner, name), map[string]any{"pull_requests": prs}, &st)
	if err != nil && code != http.StatusUnprocessableEntity {
		t.Fatalf("POST /stacks %v: %d %v", prs, code, err)
	}
	return code, st.Number
}

func mergeAsync(t *testing.T, ghc *gh.Client, owner, name string, pr int, method string) {
	t.Helper()
	ctx := context.Background()
	var started struct {
		Details struct {
			UUID string `json:"uuid"`
		} `json:"details"`
	}
	if code, err := call(ctx, ghc, "PUT", fmt.Sprintf("repos/%s/%s/pulls/%d/merge-async", owner, name, pr), map[string]any{"merge_method": method}, &started); err != nil {
		t.Fatalf("merge-async #%d: %d %v", pr, code, err)
	}
	for deadline := time.Now().Add(5 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		var st struct {
			Status string `json:"status"`
		}
		if _, err := call(ctx, ghc, "GET", fmt.Sprintf("repos/%s/%s/pulls/%d/merge-async/%s", owner, name, pr, started.Details.UUID), nil, &st); err != nil {
			t.Fatal(err)
		}
		if st.Status == "merged" {
			return
		}
		if st.Status == "failed" {
			t.Fatalf("merge-async #%d failed", pr)
		}
	}
	t.Fatalf("merge-async #%d did not finish", pr)
}

// TestStackMergeCommitPartialMerge: merge only the bottom PR of a native
// stack of three with a merge commit. Sync accepts the upper branches only
// if their patch-ids are what was published, whether GitHub rewrote them or
// not. This checks exactly that, and reports whether GitHub rewrote them.
func TestStackMergeCommitPartialMerge(t *testing.T) {
	ghc, g, owner, name := setup(t)
	ctx := context.Background()
	id := time.Now().UTC().Format("150405")
	names := []string{"sbx-" + id + "-a", "sbx-" + id + "-b", "sbx-" + id + "-c"}
	prs := branches(t, ghc, g, owner, name, names...)
	if code, n := createStack(t, ghc, owner, name, prs); code != http.StatusCreated {
		t.Fatalf("POST /stacks: %d", code)
	} else {
		t.Logf("stack #%d holds PRs %v", n, prs)
	}
	before := map[string]string{}
	ids := map[string][]string{}
	for i, b := range names[1:] {
		head, _ := g.MustResolve(ctx, "refs/heads/"+b)
		commits, err := g.CommitsBetween(ctx, "refs/heads/"+names[i], head)
		if err != nil {
			t.Fatal(err)
		}
		before[b] = head
		if ids[b], err = g.PatchIDs(ctx, commits); err != nil {
			t.Fatal(err)
		}
	}
	mergeAsync(t, ghc, owner, name, prs[0], "merge")

	// GitHub retargets b's PR to main within seconds.
	var base string
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
		pr, _, err := ghc.PullRequests.Get(ctx, owner, name, prs[1])
		if err != nil {
			t.Fatal(err)
		}
		if base = pr.GetBase().GetRef(); base == "main" {
			break
		}
	}
	if base != "main" {
		t.Fatalf("PR #%d's base is %q a minute after the merge, want main", prs[1], base)
	}
	time.Sleep(10 * time.Second) // GitHub's restack of the upper branches took about 3 s in the squash test
	if err := g.Fetch(ctx, "origin", "+refs/heads/*:refs/heads/*"); err != nil {
		t.Fatal(err)
	}
	below := map[string]string{names[1]: "refs/heads/main", names[2]: "refs/heads/" + names[1]}
	for _, b := range names[1:] {
		head, _ := g.MustResolve(ctx, "refs/heads/"+b)
		commits, err := g.CommitsBetween(ctx, below[b], head)
		if err != nil {
			t.Fatal(err)
		}
		got, err := g.PatchIDs(ctx, commits)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("FINDING: after a merge-commit partial merge, %s was rewritten by GitHub: %v (%s -> %s)", b, head != before[b], before[b][:12], head[:12])
		if !slices.Equal(got, ids[b]) {
			t.Errorf("%s's patch-ids changed: %v, published %v; sync would flag GitHub's own rewrite", b, got, ids[b])
		}
	}
	if pc, _, err := ghc.PullRequests.Get(ctx, owner, name, prs[2]); err != nil || pc.GetBase().GetRef() != names[1] {
		t.Errorf("PR #%d's base is %q, want %s: only the PR directly above is retargeted (%v)", prs[2], pc.GetBase().GetRef(), names[1], err)
	}
}

// TestOnePRStack: can a stack of one PR be registered? Apply never makes
// one; the answer goes in the findings.
func TestOnePRStack(t *testing.T) {
	ghc, g, owner, name := setup(t)
	id := time.Now().UTC().Format("150405")
	prs := branches(t, ghc, g, owner, name, "sbx-"+id+"-one")
	code, n := createStack(t, ghc, owner, name, prs)
	switch code {
	case http.StatusCreated:
		t.Logf("FINDING: a one-PR stack is accepted (stack #%d); unstacking it", n)
		if c, err := call(context.Background(), ghc, "POST", fmt.Sprintf("repos/%s/%s/stacks/%d/unstack", owner, name, n), nil, nil); err != nil {
			t.Errorf("unstack #%d: %d %v", n, c, err)
		}
	case http.StatusUnprocessableEntity:
		t.Logf("FINDING: a one-PR stack is refused with 422")
	default:
		t.Fatalf("POST /stacks with one PR answered %d", code)
	}
}
