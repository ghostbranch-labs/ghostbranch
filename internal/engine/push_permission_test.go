package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func TestPublicPushWithoutWriteAccessIsTokenCannotPublish(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "denied", http.StatusForbidden)
	}))
	defer srv.Close()
	ctx := context.Background()
	g := testutil.BareRepo(t, t.TempDir())
	sha := testutil.Commit(t, g, "main", "c", map[string]string{"a.txt": "a\n"})
	for _, remote := range []string{remotePublic, remoteShadow} {
		if err := g.AddRemote(ctx, remote, srv.URL+"/acme/x.git"); err != nil {
			t.Fatal(err)
		}
	}
	e := &Engine{TokenSource: "GHOSTBRANCH_TOKEN"}
	err := e.push(ctx, &pairCtx{op: "apply", git: g}, remotePublic, gitx.PushOptions{}, sha+":refs/heads/x")
	ce := contract.From(err)
	if ce == nil || ce.Code != contract.TokenCannotPublish || ce.Field != "token" || ce.Details["token_source"] != "GHOSTBRANCH_TOKEN" {
		t.Fatalf("public: %v", err)
	}
	err = e.push(ctx, &pairCtx{op: "gate", git: g}, remoteShadow, gitx.PushOptions{}, sha+":refs/heads/x")
	if ce := contract.From(err); ce == nil || ce.Code != contract.ForgeError {
		t.Fatalf("shadow: want FORGE_ERROR, got %v", err)
	}
}
