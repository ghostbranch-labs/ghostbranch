// Package fake is an in-memory Forge whose repos are local bare git repos.
// Tests use it to run the whole loop offline.
package fake

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

type Forge struct {
	mu    sync.Mutex
	root  string
	repos map[string]*forge.Repo
	prs   map[string][]forge.PR

	// User is returned by CurrentUser.
	User    forge.User
	UserErr error // CurrentUser fails with it when set
	// Checks scripts check results. Nil means one passing check, "ci".
	Checks func(repo, sha string) []forge.Check
	// LimitedChecks makes ChecksLimited report true for every repo.
	LimitedChecks bool
	// PushDenied makes PushAccess report "false" (403) for these repos.
	PushDenied map[string]bool
	// Secrets are SecretNames' answer per repo; SecretsForbidden makes it
	// fail with ErrForbidden. CreateForbidden makes CreatePrivateRepo fail
	// with ErrForbidden.
	Secrets          map[string][]string
	SecretsForbidden map[string]bool
	CreateForbidden  bool
	// Runs are the push-triggered workflow runs per repo, for PushRuns.
	// RunsForbidden makes PushRuns fail with ErrForbidden, RunsErr with
	// that error; CancelForbidden makes CancelRun fail with ErrForbidden,
	// CancelErr with that error. Cancelled records the run IDs CancelRun
	// cancelled, per repo; PushRunsCalls counts PushRuns calls, and
	// PushRunsRepos and CancelRunRepos record the repo each call named.
	Runs            map[string][]Run
	RunsForbidden   map[string]bool
	RunsErr         error
	CancelForbidden map[string]bool
	CancelErr       error
	Cancelled       map[string][]int64
	PushRunsCalls   int
	PushRunsRepos   []string
	CancelRunRepos  []string
}

// Run is a seeded workflow run that a push of SHA started.
type Run struct {
	SHA string
	forge.Run
}

// New stores bare repos under root.
func New(root string) *Forge {
	return &Forge{root: root, repos: map[string]*forge.Repo{}, prs: map[string][]forge.PR{},
		User: forge.User{Login: "octo", ID: 42}}
}

// AddRepo creates a repo and returns its bare repo directory.
func (f *Forge) AddRepo(full string, private bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.add(full, private)
	if err != nil {
		return "", err
	}
	return r.CloneURL, nil
}

func (f *Forge) add(full string, private bool) (*forge.Repo, error) {
	owner, name, err := forge.SplitName(full)
	if err != nil {
		return nil, err
	}
	if _, ok := f.repos[full]; ok {
		return nil, fmt.Errorf("repo %s already exists", full)
	}
	dir := filepath.Join(f.root, owner, name+".git")
	if _, err := gitx.Init(context.Background(), dir, true); err != nil {
		return nil, err
	}
	r := &forge.Repo{FullName: full, Private: private, DefaultBranch: "main", CloneURL: dir}
	f.repos[full] = r
	return r, nil
}

// RepoDir is the bare repo directory of full.
func (f *Forge) RepoDir(full string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.repos[full]; ok {
		return r.CloneURL
	}
	return ""
}

func (f *Forge) SetPrivate(full string, private bool) {
	f.mu.Lock()
	f.repos[full].Private = private
	f.mu.Unlock()
}

// SetFullName makes GetRepo of full answer with canonical as the repo's
// name, as GitHub does for a name typed in another case.
func (f *Forge) SetFullName(full, canonical string) {
	f.mu.Lock()
	f.repos[full].FullName = canonical
	f.mu.Unlock()
}

func (f *Forge) SetFork(full string, fork bool) {
	f.mu.Lock()
	f.repos[full].Fork = fork
	f.mu.Unlock()
}

// PRs returns a copy of every PR in full.
func (f *Forge) PRs(full string) []forge.PR {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]forge.PR(nil), f.prs[full]...)
}

// ClosePR closes PR n in full.
func (f *Forge) ClosePR(full string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs[full] {
		if f.prs[full][i].Number == n {
			f.prs[full][i].State = "closed"
		}
	}
}

func (f *Forge) GetRepo(_ context.Context, full string) (forge.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[full]
	if !ok {
		return forge.Repo{}, fmt.Errorf("repo %s: %w", full, forge.ErrNotFound)
	}
	return *r, nil
}

func (f *Forge) CreatePrivateRepo(_ context.Context, full string) (forge.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateForbidden {
		return forge.Repo{}, fmt.Errorf("create %s: %w", full, forge.ErrForbidden)
	}
	r, err := f.add(full, true)
	if err != nil {
		return forge.Repo{}, err
	}
	return *r, nil
}

func (f *Forge) branchExists(full, branch string) bool {
	r, ok := f.repos[full]
	if !ok {
		return false
	}
	_, exists, err := gitx.New(r.CloneURL).Resolve(context.Background(), "refs/heads/"+branch)
	return err == nil && exists
}

func (f *Forge) SetDefaultBranch(_ context.Context, full, branch string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.branchExists(full, branch) {
		return fmt.Errorf("set default branch of %s: branch %s does not exist", full, branch)
	}
	f.repos[full].DefaultBranch = branch
	return nil
}

func (f *Forge) FindOpenPR(_ context.Context, full, head, base string) (*forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.prs[full] {
		if pr.Head == head && pr.Base == base && pr.State == "open" {
			p := pr
			return &p, nil
		}
	}
	return nil, nil
}

func (f *Forge) CreatePR(_ context.Context, full string, in forge.NewPR) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.branchExists(full, in.Head) {
		return forge.PR{}, fmt.Errorf("create PR in %s: head branch %s does not exist", full, in.Head)
	}
	n := len(f.prs[full]) + 1
	pr := forge.PR{Number: n, Head: in.Head, Base: in.Base, State: "open", Draft: in.Draft,
		URL: fmt.Sprintf("https://github.example/%s/pull/%d", full, n)}
	f.prs[full] = append(f.prs[full], pr)
	return pr, nil
}

func (f *Forge) GetPR(_ context.Context, full string, number int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, pr := range f.prs[full] {
		if pr.Number == number {
			return pr, nil
		}
	}
	return forge.PR{}, fmt.Errorf("PR %s#%d: %w", full, number, forge.ErrNotFound)
}

func (f *Forge) ListChecks(_ context.Context, full, sha string) ([]forge.Check, error) {
	f.mu.Lock()
	script := f.Checks
	f.mu.Unlock()
	if script != nil {
		return script(full, sha), nil
	}
	return []forge.Check{{Name: "ci", Status: "completed", Conclusion: "success"}}, nil
}

func (f *Forge) CurrentUser(context.Context) (forge.User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UserErr != nil {
		return forge.User{}, f.UserErr
	}
	return f.User, nil
}

// ChecksLimited implements forge.CheckVisibility.
func (f *Forge) ChecksLimited(string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.LimitedChecks
}

func (f *Forge) PushAccess(_ context.Context, full string) forge.PushAccess {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PushDenied[full] {
		return forge.PushAccess{Can: "false", HTTPStatus: 403}
	}
	return forge.PushAccess{Can: "true", HTTPStatus: 200}
}

func (f *Forge) SecretNames(_ context.Context, full string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SecretsForbidden[full] {
		return nil, fmt.Errorf("list secrets of %s: %w", full, forge.ErrForbidden)
	}
	out := append([]string{}, f.Secrets[full]...)
	sort.Strings(out)
	return out, nil
}

func (f *Forge) PushRuns(_ context.Context, full, sha string) ([]forge.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.PushRunsCalls++
	f.PushRunsRepos = append(f.PushRunsRepos, full)
	if f.RunsForbidden[full] {
		return nil, fmt.Errorf("list workflow runs of %s: %w", full, forge.ErrForbidden)
	}
	if f.RunsErr != nil {
		return nil, f.RunsErr
	}
	out := []forge.Run{}
	for _, r := range f.Runs[full] {
		if r.SHA == sha {
			out = append(out, r.Run)
		}
	}
	return out, nil
}

func (f *Forge) CancelRun(_ context.Context, full string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.CancelRunRepos = append(f.CancelRunRepos, full)
	if f.CancelForbidden[full] {
		return fmt.Errorf("cancel workflow run %d of %s: %w", id, full, forge.ErrForbidden)
	}
	if f.CancelErr != nil {
		return f.CancelErr
	}
	for i := range f.Runs[full] {
		r := &f.Runs[full][i]
		if r.ID != id {
			continue
		}
		if r.Status != "completed" {
			r.Status = "completed"
			if f.Cancelled == nil {
				f.Cancelled = map[string][]int64{}
			}
			f.Cancelled[full] = append(f.Cancelled[full], id)
		}
		return nil
	}
	return fmt.Errorf("workflow run %d of %s: %w", id, full, forge.ErrNotFound)
}
