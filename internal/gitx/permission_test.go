package gitx_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
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
