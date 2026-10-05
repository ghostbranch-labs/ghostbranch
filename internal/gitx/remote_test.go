package gitx_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func setupRemote(t *testing.T) (remote, local *gitx.Git, c1 string) {
	t.Helper()
	dir := t.TempDir()
	remote = testutil.BareRepo(t, filepath.Join(dir, "remote.git"))
	c1 = testutil.Commit(t, remote, "main", "first", map[string]string{"f.txt": "1\n"})
	local = testutil.BareRepo(t, filepath.Join(dir, "local.git"))
	if err := local.AddRemote(context.Background(), "origin", remote.Dir); err != nil {
		t.Fatal(err)
	}
	if err := local.AddRemote(context.Background(), "origin", remote.Dir); err != nil {
		t.Fatalf("AddRemote must be idempotent: %v", err)
	}
	return remote, local, c1
}

func TestFetchAndLsRemote(t *testing.T) {
	ctx := context.Background()
	_, local, c1 := setupRemote(t)
	if err := local.Fetch(ctx, "origin", "+refs/heads/*:refs/remotes/origin/*"); err != nil {
		t.Fatal(err)
	}
	if sha, _, _ := local.Resolve(ctx, "refs/remotes/origin/main"); sha != c1 {
		t.Fatalf("fetched main = %s, want %s", sha, c1)
	}
	refs, err := local.LsRemote(ctx, "origin")
	if err != nil || refs["refs/heads/main"] != c1 {
		t.Fatalf("LsRemote = %v %v", refs, err)
	}
}

func TestPushLeases(t *testing.T) {
	ctx := context.Background()
	remote, local, c1 := setupRemote(t)
	_ = local.Fetch(ctx, "origin", "+refs/heads/*:refs/remotes/origin/*")
	c2 := testutil.Commit(t, local, "work", "w", map[string]string{"g.txt": "2\n"})

	mustNotExist := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/new", Expect: ""}}}
	if err := local.Push(ctx, "origin", mustNotExist, c2+":refs/heads/new"); err != nil {
		t.Fatalf("create with must-not-exist lease: %v", err)
	}
	var rej *gitx.RejectedError
	if err := local.Push(ctx, "origin", mustNotExist, c1+":refs/heads/new"); !errors.As(err, &rej) {
		t.Fatalf("second create must be rejected, got %v", err)
	}

	wrong := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/main", Expect: c2}}}
	if err := local.Push(ctx, "origin", wrong, c2+":refs/heads/main"); !errors.As(err, &rej) {
		t.Fatalf("stale lease must be rejected, got %v", err)
	}
	right := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/main", Expect: c1}}}
	if err := local.Push(ctx, "origin", right, c2+":refs/heads/main"); err != nil {
		t.Fatalf("correct lease: %v", err)
	}
	if sha, _, _ := remote.Resolve(ctx, "refs/heads/main"); sha != c2 {
		t.Fatalf("remote main = %s, want %s", sha, c2)
	}
}

func TestAtomicPushAppliesAllOrNothing(t *testing.T) {
	ctx := context.Background()
	remote, local, c1 := setupRemote(t)
	_ = local.Fetch(ctx, "origin", "+refs/heads/*:refs/remotes/origin/*")
	opt := gitx.PushOptions{Atomic: true, Leases: []gitx.Lease{
		{Ref: "refs/heads/a", Expect: ""},
		{Ref: "refs/heads/main", Expect: ""}, // main exists: this lease fails
	}}
	err := local.Push(ctx, "origin", opt, c1+":refs/heads/a", c1+":refs/heads/main")
	if err == nil {
		t.Fatal("expected rejection")
	}
	if _, ok, _ := remote.Resolve(ctx, "refs/heads/a"); ok {
		t.Fatal("atomic push must not create refs/heads/a when another ref fails")
	}
}

func TestPushCustomNamespace(t *testing.T) {
	ctx := context.Background()
	remote, local, c1 := setupRemote(t)
	_ = local.Fetch(ctx, "origin", "+refs/heads/*:refs/remotes/origin/*")
	if err := local.Push(ctx, "origin", gitx.PushOptions{}, c1+":refs/ghostbranch/features/x"); err != nil {
		t.Fatal(err)
	}
	if sha, _, _ := remote.Resolve(ctx, "refs/ghostbranch/features/x"); sha != c1 {
		t.Fatal("custom ref not pushed")
	}
}
