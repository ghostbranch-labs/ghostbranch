package fake_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

var _ forge.Forge = (*fake.Forge)(nil)

func TestFakeForgeBehavesLikeGitHub(t *testing.T) {
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	if _, err := f.GetRepo(ctx, "acme/none"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("missing repo: %v", err)
	}
	sh, err := f.CreatePrivateRepo(ctx, "acme/widget-shadow")
	if err != nil || !sh.Private || sh.Fork || sh.CloneURL == "" {
		t.Fatalf("CreatePrivateRepo = %+v %v", sh, err)
	}
	if _, err := f.CreatePrivateRepo(ctx, "acme/widget-shadow"); err == nil {
		t.Fatal("creating an existing repo must fail")
	}
	g := gitx.New(f.RepoDir("acme/widget-shadow"))
	if err := f.SetDefaultBranch(ctx, "acme/widget-shadow", "ghostbranch"); err == nil {
		t.Fatal("default branch must exist first")
	}
	c := testutil.Commit(t, g, "ghostbranch", "overlay", map[string]string{".ghostbranch.yml": "x"})
	if err := f.SetDefaultBranch(ctx, "acme/widget-shadow", "ghostbranch"); err != nil {
		t.Fatal(err)
	}
	if r, _ := f.GetRepo(ctx, "acme/widget-shadow"); r.DefaultBranch != "ghostbranch" {
		t.Fatalf("default branch = %s", r.DefaultBranch)
	}

	if pr, _ := f.FindOpenPR(ctx, "acme/widget-shadow", "ghostbranch", "main"); pr != nil {
		t.Fatal("no PR yet")
	}
	if _, err := f.CreatePR(ctx, "acme/widget-shadow", forge.NewPR{Head: "missing", Base: "main"}); err == nil {
		t.Fatal("PR from a missing branch must fail")
	}
	pr, err := f.CreatePR(ctx, "acme/widget-shadow", forge.NewPR{Title: "t", Head: "ghostbranch", Base: "main", Draft: true})
	if err != nil || pr.Number != 1 || !pr.Draft || pr.State != "open" {
		t.Fatalf("CreatePR = %+v %v", pr, err)
	}
	if got, _ := f.FindOpenPR(ctx, "acme/widget-shadow", "ghostbranch", "main"); got == nil || got.Number != 1 {
		t.Fatalf("FindOpenPR = %+v", got)
	}
	f.ClosePR("acme/widget-shadow", 1)
	if got, _ := f.FindOpenPR(ctx, "acme/widget-shadow", "ghostbranch", "main"); got != nil {
		t.Fatal("closed PR must not be found")
	}

	checks, _ := f.ListChecks(ctx, "acme/widget-shadow", c)
	if len(checks) != 1 || checks[0].Conclusion != "success" {
		t.Fatalf("default checks = %+v", checks)
	}
	u, _ := f.CurrentUser(ctx)
	if forge.NoreplyEmail(u) != "42+octo@users.noreply.github.com" {
		t.Fatalf("noreply = %s", forge.NoreplyEmail(u))
	}
}
