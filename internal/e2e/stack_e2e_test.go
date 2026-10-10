//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// stacksAPIVersion is the REST version the stacks endpoints were checked
// against (spec § 9).
const stacksAPIVersion = "2026-03-10"

// rawJSON makes a REST call go-github has no wrapper for and decodes the
// answer into out (nil to ignore it). A 202 is an answer, not an error.
func rawJSON(ctx context.Context, ghc *gh.Client, method, path string, body, out any) error {
	req, err := ghc.NewRequest(ctx, method, path, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-GitHub-Api-Version", stacksAPIVersion)
	_, err = ghc.Do(req, out)
	var accepted *gh.AcceptedError
	if errors.As(err, &accepted) {
		if out != nil && len(accepted.Raw) > 0 {
			return json.Unmarshal(accepted.Raw, out)
		}
		return nil
	}
	return err
}

// mergeAsync merges a stacked PR the only way GitHub allows: PUT
// …/merge-async, then poll …/merge-async/{uuid} until it has merged.
// Merging the top PR merges the whole stack at once.
func mergeAsync(t *testing.T, ghc *gh.Client, owner, repo string, pr int, method string) {
	t.Helper()
	ctx := context.Background()
	var started struct {
		Status  string `json:"status"`
		Details struct {
			UUID string `json:"uuid"`
		} `json:"details"`
	}
	if err := rawJSON(ctx, ghc, "PUT", fmt.Sprintf("repos/%s/%s/pulls/%d/merge-async", owner, repo, pr),
		map[string]any{"merge_method": method}, &started); err != nil {
		t.Fatalf("merge-async PR #%d: %v", pr, err)
	}
	if started.Details.UUID == "" {
		t.Fatalf("merge-async PR #%d gave no uuid: %+v", pr, started)
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var st struct {
			Status string `json:"status"`
		}
		if err := rawJSON(ctx, ghc, "GET", fmt.Sprintf("repos/%s/%s/pulls/%d/merge-async/%s", owner, repo, pr, started.Details.UUID), nil, &st); err != nil {
			t.Fatalf("poll merge-async PR #%d: %v", pr, err)
		}
		switch st.Status {
		case "merged":
			return
		case "failed":
			t.Fatalf("merge-async PR #%d failed", pr)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("merge-async PR #%d did not merge within 5 minutes", pr)
}

func strs(v any) []string {
	l, _ := v.([]any)
	out := make([]string, len(l))
	for i, x := range l {
		out[i], _ = x.(string)
	}
	return out
}

func nexts(env map[string]any) []string {
	var out []string
	ns, _ := env["next"].([]any)
	for _, n := range ns {
		m, _ := n.(map[string]any)
		out = append(out, strings.Join(strs(m["command"]), " "))
	}
	return out
}

func hasNextStep(env map[string]any, want string) bool {
	for _, c := range nexts(env) {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// TestSandboxStack: a two-branch stack on the sandbox pair. It goes from
// start --from, through gating bottom up, to one approval publishing both
// PRs as a native stack, then an atomic merge of the stack, then sync
// closing both.
func TestSandboxStack(t *testing.T) {
	tok := token(t)
	ctx := context.Background()
	reset(t, tok)
	run, home := newRunner(t, tok)
	commit := func(branch string, files map[string]string) {
		t.Helper()
		g := authed(t, tok)
		if err := g.Fetch(ctx, "shadow", "+refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
		testutil.Commit(t, g, branch, "e2e stack work on "+branch, files)
		if err := g.Push(ctx, "shadow", gitx.PushOptions{}, "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
	runID := time.Now().UTC().Format("20060102t150405")
	run(0, "init", publicRepo, "--shadow", shadowRepo, "--adopt")
	run(0, "sync")

	a, b := "stk-a-"+runID, "stk-b-"+runID
	run(0, "feature", "start", a)
	commit(a, map[string]string{"stack-a.txt": "a for " + runID + "\n"})
	run(0, "feature", "start", b, "--from", a)
	commit(b, map[string]string{"stack-b.txt": "b for " + runID + "\n"})

	if env := run(0, "gate", a); !hasNextStep(env, "gate "+b) {
		t.Fatalf("after gating %s, next must gate %s: %v", a, b, nexts(env))
	}
	if env := run(0, "gate", b); !hasNextStep(env, "promote "+a+" "+b) {
		t.Fatalf("after gating %s, next must promote the stack: %v", b, nexts(env))
	}
	planned := run(0, "promote", a, b, "--mode", "squash", "--plan")["data"].(map[string]any)
	pl := planned["plan"].(map[string]any)
	prs, _ := pl["prs"].([]any)
	if len(prs) != 2 || prs[0].(map[string]any)["base"] != "main" || prs[1].(map[string]any)["base"] != a {
		t.Fatalf("plan prs = %v", pl["prs"])
	}
	planID := pl["id"].(string)

	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	ws := filepath.Join(home, "workspaces", "default")
	if _, err := grant.NewStore(filepath.Join(ws, "grants.json"), filepath.Join(ws, "grants.lock")).Put(grant.Grant{
		ID: "g-e2e001", Public: publicRepo, Shadow: shadowRepo, CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:e2e"}, now); err != nil {
		t.Fatal(err)
	}
	applied := run(0, "apply", planID, "--yes")["data"].(map[string]any)
	aprs, _ := applied["prs"].([]any)
	if len(aprs) != 2 || applied["stack_number"] == nil {
		t.Fatalf("apply must open two PRs as a native stack: %v", applied)
	}
	prA := int(aprs[0].(map[string]any)["number"].(float64))
	prB := int(aprs[1].(map[string]any)["number"].(float64))
	stack := int(applied["stack_number"].(float64))

	ghc, err := gh.NewClient(gh.WithAuthToken(tok))
	if err != nil {
		t.Fatal(err)
	}
	owner, name, _ := strings.Cut(publicRepo, "/")
	pb, _, err := ghc.PullRequests.Get(ctx, owner, name, prB)
	if err != nil || pb.GetBase().GetRef() != a {
		t.Fatalf("PR #%d's base is %q, want %s (%v)", prB, pb.GetBase().GetRef(), a, err)
	}
	var st struct {
		Number       int `json:"number"`
		PullRequests []struct {
			Number int `json:"number"`
		} `json:"pull_requests"`
	}
	if err := rawJSON(ctx, ghc, "GET", fmt.Sprintf("repos/%s/%s/stacks/%d", owner, name, stack), nil, &st); err != nil {
		t.Fatalf("read stack #%d: %v", stack, err)
	}
	if len(st.PullRequests) != 2 || st.PullRequests[0].Number != prA || st.PullRequests[1].Number != prB {
		t.Fatalf("stack #%d holds %+v, want #%d then #%d", stack, st.PullRequests, prA, prB)
	}
	// Only b's change is in b's PR.
	files, _, err := ghc.PullRequests.ListFiles(ctx, owner, name, prB, nil)
	if err != nil || len(files) != 1 || files[0].GetFilename() != "stack-b.txt" {
		t.Fatalf("PR #%d changes %v (%v), want only stack-b.txt", prB, files, err)
	}

	// A maintainer merges the stack atomically (the top PR); sync closes both.
	mergeAsync(t, ghc, owner, name, prB, "squash")
	syncData := run(0, "sync")["data"].(map[string]any)
	closed := map[string]bool{}
	for _, c := range syncData["closed"].([]any) {
		closed[c.(map[string]any)["name"].(string)] = true
	}
	if !closed[a] || !closed[b] {
		t.Fatalf("sync must close %s and %s after the stack merged: %v", a, b, syncData["closed"])
	}
	shadowRefs, err := authed(t, tok).LsRemote(ctx, "shadow")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{a, b} {
		for _, ref := range []string{"refs/heads/" + f, "refs/ghostbranch/features/" + f} {
			if _, ok := shadowRefs[ref]; ok {
				t.Errorf("%s is still in the shadow repo after sync", ref)
			}
		}
	}
}
