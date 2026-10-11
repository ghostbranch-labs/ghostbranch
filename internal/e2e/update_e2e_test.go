//go:build e2e

package e2e_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v92/github"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// TestSandboxUpdateAppends: promote a feature, add a commit, promote again.
// The open PR gains exactly one commit, and its first commit keeps its SHA
// (no force-push). Like the other sandbox tests it runs alone, never in
// parallel: they share the sandbox repos.
func TestSandboxUpdateAppends(t *testing.T) {
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
		testutil.Commit(t, g, branch, "e2e update work on "+branch, files)
		if err := g.Push(ctx, "shadow", gitx.PushOptions{}, "refs/heads/"+branch+":refs/heads/"+branch); err != nil {
			t.Fatal(err)
		}
	}
	runID := time.Now().UTC().Format("20060102t150405")
	run(0, "init", publicRepo, "--shadow", shadowRepo, "--adopt")
	run(0, "sync")
	now := time.Now().UTC()
	exp := now.Add(time.Hour)
	ws := filepath.Join(home, "workspaces", "default")
	if _, err := grant.NewStore(filepath.Join(ws, "grants.json"), filepath.Join(ws, "grants.lock")).Put(grant.Grant{
		ID: "g-e2e002", Public: publicRepo, Shadow: shadowRepo, CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:e2e"}, now); err != nil {
		t.Fatal(err)
	}

	f := "upd-" + runID
	run(0, "feature", "start", f)
	commit(f, map[string]string{"update-1.txt": "one for " + runID + "\n"})
	run(0, "gate", f)
	pl := run(0, "promote", f, "--mode", "preserve", "--plan")["data"].(map[string]any)["plan"].(map[string]any)
	first := run(0, "apply", pl["id"].(string), "--yes")["data"].(map[string]any)
	pr := int(first["public_pr"].(float64))

	ghc, err := gh.NewClient(gh.WithAuthToken(tok))
	if err != nil {
		t.Fatal(err)
	}
	owner, name, _ := strings.Cut(publicRepo, "/")
	before, _, err := ghc.PullRequests.ListCommits(ctx, owner, name, pr, nil)
	if err != nil || len(before) != 1 {
		t.Fatalf("PR #%d's commits after the first publish: %d (%v)", pr, len(before), err)
	}

	commit(f, map[string]string{"update-2.txt": "two for " + runID + "\n"})
	run(0, "gate", f)
	pl = run(0, "promote", f, "--mode", "preserve", "--title", "E2E update "+runID, "--plan")["data"].(map[string]any)["plan"].(map[string]any)
	pub := pl["publish"].(map[string]any)
	if pub["update"] != true || pub["style"] != "append" || pub["expected_head"] != before[0].GetSHA() {
		t.Fatalf("the update plan's publish = %v", pub)
	}
	second := run(0, "apply", pl["id"].(string), "--yes")["data"].(map[string]any)
	if int(second["public_pr"].(float64)) != pr {
		t.Fatalf("the update opened PR #%v, want #%d", second["public_pr"], pr)
	}
	after, _, err := ghc.PullRequests.ListCommits(ctx, owner, name, pr, nil)
	if err != nil || len(after) != 2 {
		t.Fatalf("PR #%d's commits after the update: %d, want 2 (%v)", pr, len(after), err)
	}
	if after[0].GetSHA() != before[0].GetSHA() {
		t.Fatalf("PR #%d's first commit is %s after the update, want %s unchanged (no force-push)", pr, after[0].GetSHA(), before[0].GetSHA())
	}
}
