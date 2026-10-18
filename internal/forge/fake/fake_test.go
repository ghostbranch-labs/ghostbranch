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

func TestPushAccess(t *testing.T) {
	f := fake.New(t.TempDir())
	f.PushDenied = map[string]bool{"acme/widget": true}
	ctx := context.Background()
	if a := f.PushAccess(ctx, "acme/widget"); a.Can != "false" || a.HTTPStatus != 403 {
		t.Fatalf("%+v", a)
	}
	if a := f.PushAccess(ctx, "acme/widget-shadow"); a.Can != "true" || a.HTTPStatus != 200 {
		t.Fatalf("%+v", a)
	}
}

func TestSecretsAndCreateForbidden(t *testing.T) {
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	f.Secrets = map[string][]string{"acme/widget-shadow": {"B", "A"}}
	f.SecretsForbidden = map[string]bool{"acme/locked": true}
	got, err := f.SecretNames(ctx, "acme/widget-shadow")
	if err != nil || len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("SecretNames = %v %v", got, err)
	}
	if got, err := f.SecretNames(ctx, "acme/none"); err != nil || len(got) != 0 {
		t.Fatalf("unlisted repo = %v %v", got, err)
	}
	if _, err := f.SecretNames(ctx, "acme/locked"); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("SecretsForbidden: %v", err)
	}
	f.CreateForbidden = true
	if _, err := f.CreatePrivateRepo(ctx, "acme/new-shadow"); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("CreateForbidden: %v", err)
	}
	if _, err := f.GetRepo(ctx, "acme/new-shadow"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("a refused create must not add the repo: %v", err)
	}
}

func TestFakePRMergedAndClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	if _, err := f.CreatePrivateRepo(ctx, "acme/s"); err != nil {
		t.Fatal(err)
	}
	g := gitx.New(f.RepoDir("acme/s"))
	testutil.Commit(t, g, "feat", "c", map[string]string{"a": "1"})
	for i := 0; i < 2; i++ {
		if _, err := f.CreatePR(ctx, "acme/s", forge.NewPR{Head: "feat", Base: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	f.SetPRMerged("acme/s", 1)
	if pr, err := f.GetPR(ctx, "acme/s", 1); err != nil || !pr.Merged || pr.State != "closed" {
		t.Fatalf("merged PR = %+v", pr)
	}
	if err := f.ClosePullRequest(ctx, "acme/s", 2); err != nil {
		t.Fatal(err)
	}
	if pr, err := f.GetPR(ctx, "acme/s", 2); err != nil || pr.State != "closed" || pr.Merged {
		t.Fatalf("closed PR = %+v", pr)
	}
	if err := f.ClosePullRequest(ctx, "acme/s", 2); err != nil {
		t.Fatalf("closing a closed PR: %v", err)
	}
	if err := f.ClosePullRequest(ctx, "acme/s", 99); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("unknown PR: %v", err)
	}
	boom := errors.New("down")
	f.GetPRErr = func(_ string, n int) error {
		if n == 1 {
			return boom
		}
		return nil
	}
	if _, err := f.GetPR(ctx, "acme/s", 1); !errors.Is(err, boom) {
		t.Fatalf("GetPRErr for PR 1: %v", err)
	}
	if pr, err := f.GetPR(ctx, "acme/s", 2); err != nil || pr.Number != 2 {
		t.Fatalf("PR 2 must still read: %+v %v", pr, err)
	}
}

// The run flags script GitHub's cancel behaviours.
func TestFakeCancelAndForceCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const repo = "acme/s"
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	f.Runs = map[string][]fake.Run{repo: {
		{SHA: "a", Run: forge.Run{ID: 1, Status: "queued"}},
		{SHA: "a", Run: forge.Run{ID: 2, Status: "in_progress"}, IgnoreCancel: true},
		{SHA: "a", Run: forge.Run{ID: 3, Status: "in_progress"}, IgnoreCancel: true, IgnoreForceCancel: true},
		{SHA: "a", Run: forge.Run{ID: 4, Status: "in_progress"}, Conflict: true},
		{SHA: "b", Run: forge.Run{ID: 5, Status: "in_progress"}, FinishAfterPolls: 1},
	}}
	status := func(sha string) map[int64]string {
		runs, err := f.PushRuns(ctx, repo, sha)
		if err != nil {
			t.Fatal(err)
		}
		out := map[int64]string{}
		for _, r := range runs {
			out[r.ID] = r.Status + "/" + r.Conclusion
		}
		return out
	}
	for id := int64(1); id <= 3; id++ {
		if err := f.CancelRun(ctx, repo, id); err != nil {
			t.Fatalf("cancel %d: %v", id, err)
		}
	}
	if err := f.CancelRun(ctx, repo, 4); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("cancel 4: %v", err)
	}
	if got := status("a"); got[1] != "completed/cancelled" || got[2] != "in_progress/" || got[3] != "in_progress/" || got[4] != "in_progress/" {
		t.Fatalf("after cancel: %v", got)
	}
	for _, id := range []int64{2, 3, 4} {
		if err := f.ForceCancelRun(ctx, repo, id); err != nil {
			t.Fatalf("force-cancel %d: %v", id, err)
		}
	}
	if got := status("a"); got[2] != "completed/cancelled" || got[3] != "in_progress/" || got[4] != "completed/cancelled" {
		t.Fatalf("after force-cancel: %v", got)
	}
	if got := f.ForceCancelled[repo]; len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("ForceCancelled = %v", f.ForceCancelled)
	}
	if got := status("b"); got[5] != "in_progress/" {
		t.Fatalf("first poll: %v", got)
	}
	if got := status("b"); got[5] != "completed/success" {
		t.Fatalf("second poll: %v", got)
	}
	f.ForceCancelForbidden = map[string]bool{repo: true}
	if err := f.ForceCancelRun(ctx, repo, 3); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("forbidden: %v", err)
	}
}

// GitHub answers 409 to a force-cancel of a finished run; so does the fake.
// A per-run ForceCancelErr fails only that run's force-cancel.
func TestFakeForceCancelOfAFinishedRunAndPerRunErrors(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const repo = "acme/s"
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	boom := errors.New("boom")
	f.Runs = map[string][]fake.Run{repo: {
		{SHA: "a", Run: forge.Run{ID: 1, Status: "completed", Conclusion: "success"}},
		{SHA: "a", Run: forge.Run{ID: 2, Status: "in_progress"}, ForceCancelErr: boom},
		{SHA: "a", Run: forge.Run{ID: 3, Status: "in_progress"}},
	}}
	if err := f.ForceCancelRun(ctx, repo, 1); !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("finished run: %v, want ErrConflict", err)
	}
	if err := f.ForceCancelRun(ctx, repo, 2); !errors.Is(err, boom) {
		t.Fatalf("run 2: %v", err)
	}
	if err := f.ForceCancelRun(ctx, repo, 3); err != nil {
		t.Fatalf("run 3: %v", err)
	}
	if got := f.ForceCancelled[repo]; len(got) != 1 || got[0] != 3 {
		t.Fatalf("ForceCancelled = %v", f.ForceCancelled)
	}
}

func TestMissingPermissionFailsTheScriptedCall(t *testing.T) {
	t.Parallel()
	f := fake.New(t.TempDir())
	if _, err := f.AddRepo("acme/widget", false); err != nil {
		t.Fatal(err)
	}
	f.MissingPermission = map[string]string{"GetRepo": "Metadata: read"}
	_, err := f.GetRepo(context.Background(), "acme/widget")
	var pe *forge.PermissionError
	if !errors.As(err, &pe) || pe.Permission != "Metadata: read" || pe.Repo != "acme/widget" || !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("GetRepo = %v", err)
	}
	if _, err := f.SecretNames(context.Background(), "acme/widget"); err != nil {
		t.Fatalf("an unscripted call failed: %v", err)
	}
}

// Repos say whether their owner is an organization or a user, as GitHub's do.
func TestFakeOwnerType(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	f.Orgs = map[string]bool{"acme": true}
	if r, err := f.CreatePrivateRepo(ctx, "acme/widget-shadow"); err != nil || r.OwnerType != forge.OwnerOrganization {
		t.Fatalf("org repo = %+v %v", r, err)
	}
	if _, err := f.AddRepo("octo/widget-shadow", true); err != nil {
		t.Fatal(err)
	}
	if r, err := f.GetRepo(ctx, "octo/widget-shadow"); err != nil || r.OwnerType != forge.OwnerUser {
		t.Fatalf("user repo = %+v %v", r, err)
	}
}

// CreateRefused refuses as GitHub does, carrying its message.
func TestFakeCreateRefused(t *testing.T) {
	t.Parallel()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	f.CreateRefused = map[string]string{"acme": "Resource not accessible by integration"}
	_, err := f.CreatePrivateRepo(context.Background(), "acme/widget-shadow")
	if !errors.Is(err, forge.ErrForbidden) || forge.HostMessage(err) != "Resource not accessible by integration" {
		t.Fatalf("%v (message %q)", err, forge.HostMessage(err))
	}
	if _, err := f.CreatePrivateRepo(context.Background(), "octo/widget-shadow"); err != nil {
		t.Fatal(err)
	}
}

func TestFakeInstaller(t *testing.T) {
	ctx := context.Background()
	f := fake.New(filepath.Join(t.TempDir(), "forge"))
	var _ forge.Installer = f
	f.Installations = []forge.Installation{{ID: 7, Account: "me", AccountType: "User", Selection: "all"}}
	f.InstallRepos = map[int64][]string{7: {"me/a"}}
	ins, err := f.UserInstallations(ctx)
	if err != nil || len(ins) != 1 || ins[0].ID != 7 {
		t.Fatalf("UserInstallations = %+v %v", ins, err)
	}
	repos, err := f.InstallationRepos(ctx, 7)
	if err != nil || len(repos) != 1 || repos[0] != "me/a" {
		t.Fatalf("InstallationRepos = %v %v", repos, err)
	}
	f.InstallErr = forge.ErrForbidden
	if _, err := f.UserInstallations(ctx); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("UserInstallations err = %v", err)
	}
	if _, err := f.InstallationRepos(ctx, 7); !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("InstallationRepos err = %v", err)
	}
}
