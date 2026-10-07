package gitx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// A remote that answers 403 (what GitHub does for a token without write
// access) gives a PermissionError, not a generic failure.
func TestPushPermissionDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "Write access to repository not granted.", http.StatusForbidden)
	}))
	defer srv.Close()
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	sha := testutil.Commit(t, g, "main", "c", map[string]string{"a.txt": "a\n"})
	if err := g.AddRemote(ctx, "public", srv.URL+"/acme/widget.git"); err != nil {
		t.Fatal(err)
	}
	err := g.Push(ctx, "public", gitx.PushOptions{}, sha+":refs/heads/x")
	var pe *gitx.PermissionError
	if !errors.As(err, &pe) {
		t.Fatalf("want PermissionError, got %T %v", err, err)
	}
}

// A server that refuses a ref for a reason of its own (here a pre-receive
// hook; on GitHub a missing token scope or a ruleset) gives a RefusedError
// carrying the reason, and a lost lease still gives a RejectedError.
func TestPushRemoteRefusedIsNotRejected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	remote := testutil.BareRepo(t, filepath.Join(dir, "remote.git"))
	hook := "#!/bin/sh\necho 'refusing to allow a change to .github/workflows/x.yml without workflow scope' >&2\nexit 1\n"
	if err := os.MkdirAll(filepath.Join(remote.Dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remote.Dir, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	local := testutil.BareRepo(t, filepath.Join(dir, "local.git"))
	sha := testutil.Commit(t, local, "main", "c", map[string]string{"a.txt": "a\n"})
	if err := local.AddRemote(ctx, "origin", remote.Dir); err != nil {
		t.Fatal(err)
	}
	err := local.Push(ctx, "origin", gitx.PushOptions{}, sha+":refs/heads/x")
	var ref *gitx.RefusedError
	if !errors.As(err, &ref) {
		t.Fatalf("want RefusedError, got %T %v", err, err)
	}
	var rej *gitx.RejectedError
	if errors.As(err, &rej) {
		t.Fatalf("a hook refusal must not also be a RejectedError: %v", err)
	}
	if len(ref.Refs) != 1 || ref.Refs[0] != "refs/heads/x" || !strings.Contains(ref.Reason, "pre-receive hook declined") {
		t.Fatalf("refused = %+v", ref)
	}

	// A lease that no longer matches is still a RejectedError.
	lease := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/y", Expect: sha}}}
	err = local.Push(ctx, "origin", lease, sha+":refs/heads/y")
	if !errors.As(err, &rej) || errors.As(err, &ref) {
		t.Fatalf("a lost lease must be RejectedError only, got %T %v", err, err)
	}
}
