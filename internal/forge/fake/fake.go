// Package fake is an in-memory Forge whose repos are local bare git repos.
// Tests use it to run the whole loop offline.
package fake

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
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
	// next is each repo's last issue, PR or stack number: GitHub numbers
	// them from one sequence.
	next   map[string]int
	stacks map[string][]*stack

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
	// ForceCancelForbidden makes ForceCancelRun fail with ErrForbidden,
	// ForceCancelErr with that error. ForceCancelled records the run IDs
	// ForceCancelRun cancelled, per repo, and ForceCancelRepos the repo
	// each call named.
	ForceCancelForbidden map[string]bool
	ForceCancelErr       error
	ForceCancelled       map[string][]int64
	ForceCancelRepos     []string
	// GetPRErr, when set, is asked before each GetPR (under the fake's
	// lock, so it must not call the fake); a non-nil error fails that call.
	GetPRErr func(full string, number int) error
	// ClosePRErr makes ClosePullRequest fail with that error; ClosePRRepos
	// records the repo each ClosePullRequest call named.
	ClosePRErr   error
	ClosePRRepos []string
	// MissingPermission scripts a 403 that names its permission: the method
	// named (GetRepo, CreatePrivateRepo, SetDefaultBranch, FindOpenPR,
	// CreatePR, GetPR, ListChecks, SecretNames, PushRuns, CancelRun,
	// ForceCancelRun, ClosePullRequest, CreateStack, AddToStack, StackOf)
	// fails with a *forge.PermissionError naming that permission and the
	// repo, for any repo.
	MissingPermission map[string]string
	// Scopes is OAuthScopes' answer; nil means GitHub sent no X-OAuth-Scopes
	// header (a fine-grained or App token). PermsDenied makes
	// ProbePermissions answer "no" for these permissions, per repo; every
	// other read it probes is "yes", and Contents: write follows PushDenied.
	Scopes      []string
	PermsDenied map[string]map[string]bool
	// Orgs are the owners that are organizations; every other owner is a
	// user. GetRepo and CreatePrivateRepo report it in Repo.OwnerType.
	Orgs map[string]bool
	// CreateRefused makes CreatePrivateRepo under these owners fail with
	// ErrForbidden, carrying the message as GitHub's (a *forge.HostError).
	CreateRefused map[string]string
	// StacksUnavailable makes CreateStack and AddToStack fail with
	// forge.ErrStacksUnavailable and StackOf answer nil, as a host without
	// native stacks does. StackConflicts makes that many next CreateStack
	// or AddToStack calls fail with forge.ErrConflict (GitHub's 409 while
	// another change to the stack is in progress). StackCalls records each
	// Stacker call: "create 1,2", "add 4: 3", "of 2".
	StacksUnavailable bool
	StackConflicts    int
	StackCalls        []string
}

// view is r, the repo stored as full, as GitHub answers it. Callers hold f.mu.
func (f *Forge) view(full string, r *forge.Repo) forge.Repo {
	out := *r
	out.OwnerType = forge.OwnerUser
	if owner, _, err := forge.SplitName(full); err == nil && f.Orgs[owner] {
		out.OwnerType = forge.OwnerOrganization
	}
	return out
}

// missing is the error MissingPermission scripts for method on full, or
// nil. Callers hold f.mu.
func (f *Forge) missing(method, full string) error {
	if p := f.MissingPermission[method]; p != "" {
		return fmt.Errorf("%s %s: %w", method, full, &forge.PermissionError{Permission: p, Repo: full})
	}
	return nil
}

// Run is a seeded workflow run that a push of SHA started. The flags script
// GitHub's behaviours: IgnoreCancel accepts CancelRun but leaves the run
// going, IgnoreForceCancel does the same for ForceCancelRun, and Conflict
// makes CancelRun answer ErrConflict (a 409). A force-cancel of a
// completed run answers ErrConflict, as GitHub does. FinishAfterPolls, when
// greater than 0, has the run turn completed/success on its own once
// PushRuns has listed it that many times.
type Run struct {
	SHA string
	forge.Run
	IgnoreCancel      bool
	IgnoreForceCancel bool
	Conflict          bool
	// ForceCancelErr fails ForceCancelRun for this run only.
	ForceCancelErr   error
	FinishAfterPolls int

	polls int
}

// New stores bare repos under root.
func New(root string) *Forge {
	return &Forge{root: root, repos: map[string]*forge.Repo{}, prs: map[string][]forge.PR{},
		next: map[string]int{}, stacks: map[string][]*stack{},
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

// SetPRMerged marks PR n in full as merged (and so closed).
func (f *Forge) SetPRMerged(full string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs[full] {
		if f.prs[full][i].Number == n {
			f.prs[full][i].State = "closed"
			f.prs[full][i].Merged = true
		}
	}
}

// SetPRBase retargets PR n in full to base, as a base change on GitHub does.
func (f *Forge) SetPRBase(full string, n int, base string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.prs[full] {
		if f.prs[full][i].Number == n {
			f.prs[full][i].Base = base
		}
	}
}

// ClosePullRequest closes PR number without merging. A closed PR is not an
// error; an unknown one is ErrNotFound.
func (f *Forge) ClosePullRequest(_ context.Context, full string, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("ClosePullRequest", full); err != nil {
		return err
	}
	f.ClosePRRepos = append(f.ClosePRRepos, full)
	if f.ClosePRErr != nil {
		return f.ClosePRErr
	}
	for i := range f.prs[full] {
		if f.prs[full][i].Number == number {
			f.prs[full][i].State = "closed"
			return nil
		}
	}
	return fmt.Errorf("PR %s#%d: %w", full, number, forge.ErrNotFound)
}

func (f *Forge) GetRepo(_ context.Context, full string) (forge.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("GetRepo", full); err != nil {
		return forge.Repo{}, err
	}
	r, ok := f.repos[full]
	if !ok {
		return forge.Repo{}, fmt.Errorf("repo %s: %w", full, forge.ErrNotFound)
	}
	return f.view(full, r), nil
}

func (f *Forge) CreatePrivateRepo(_ context.Context, full string) (forge.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("CreatePrivateRepo", full); err != nil {
		return forge.Repo{}, err
	}
	if f.CreateForbidden {
		return forge.Repo{}, fmt.Errorf("create %s: %w", full, forge.ErrForbidden)
	}
	if owner, _, err := forge.SplitName(full); err == nil {
		if msg, ok := f.CreateRefused[owner]; ok {
			return forge.Repo{}, fmt.Errorf("create %s: %w", full,
				&forge.HostError{Message: msg, Err: fmt.Errorf("%w: 403 %s", forge.ErrForbidden, msg)})
		}
	}
	r, err := f.add(full, true)
	if err != nil {
		return forge.Repo{}, err
	}
	return f.view(full, r), nil
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
	if err := f.missing("SetDefaultBranch", full); err != nil {
		return err
	}
	if !f.branchExists(full, branch) {
		return fmt.Errorf("set default branch of %s: branch %s does not exist", full, branch)
	}
	f.repos[full].DefaultBranch = branch
	return nil
}

func (f *Forge) FindOpenPR(_ context.Context, full, head, base string) (*forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("FindOpenPR", full); err != nil {
		return nil, err
	}
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
	if err := f.missing("CreatePR", full); err != nil {
		return forge.PR{}, err
	}
	if !f.branchExists(full, in.Head) {
		return forge.PR{}, fmt.Errorf("create PR in %s: head branch %s does not exist", full, in.Head)
	}
	f.next[full]++
	n := f.next[full]
	pr := forge.PR{Number: n, Head: in.Head, Base: in.Base, State: "open", Draft: in.Draft,
		URL: fmt.Sprintf("https://github.example/%s/pull/%d", full, n)}
	f.prs[full] = append(f.prs[full], pr)
	return pr, nil
}

func (f *Forge) GetPR(_ context.Context, full string, number int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("GetPR", full); err != nil {
		return forge.PR{}, err
	}
	if f.GetPRErr != nil {
		if err := f.GetPRErr(full, number); err != nil {
			return forge.PR{}, err
		}
	}
	for _, pr := range f.prs[full] {
		if pr.Number == number {
			return pr, nil
		}
	}
	return forge.PR{}, fmt.Errorf("PR %s#%d: %w", full, number, forge.ErrNotFound)
}

func (f *Forge) ListChecks(_ context.Context, full, sha string) ([]forge.Check, error) {
	f.mu.Lock()
	script, miss := f.Checks, f.missing("ListChecks", full)
	f.mu.Unlock()
	if miss != nil {
		return nil, miss
	}
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
	if err := f.missing("SecretNames", full); err != nil {
		return nil, err
	}
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
	if err := f.missing("PushRuns", full); err != nil {
		return nil, err
	}
	if f.RunsForbidden[full] {
		return nil, fmt.Errorf("list workflow runs of %s: %w", full, forge.ErrForbidden)
	}
	if f.RunsErr != nil {
		return nil, f.RunsErr
	}
	out := []forge.Run{}
	for i := range f.Runs[full] {
		r := &f.Runs[full][i]
		if r.SHA != sha {
			continue
		}
		out = append(out, r.Run)
		r.polls++
		if r.FinishAfterPolls > 0 && r.polls >= r.FinishAfterPolls && r.Status != "completed" {
			r.Status, r.Conclusion = "completed", "success"
		}
	}
	return out, nil
}

func (f *Forge) CancelRun(_ context.Context, full string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("CancelRun", full); err != nil {
		return err
	}
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
		switch {
		case r.Conflict:
			return fmt.Errorf("cancel workflow run %d of %s: %w", id, full, forge.ErrConflict)
		case r.IgnoreCancel:
			// Accepted, as GitHub's 202 is, but the run keeps going.
		case r.Status != "completed":
			r.Status, r.Conclusion = "completed", "cancelled"
			if f.Cancelled == nil {
				f.Cancelled = map[string][]int64{}
			}
			f.Cancelled[full] = append(f.Cancelled[full], id)
		}
		return nil
	}
	return fmt.Errorf("workflow run %d of %s: %w", id, full, forge.ErrNotFound)
}

func (f *Forge) ForceCancelRun(_ context.Context, full string, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.missing("ForceCancelRun", full); err != nil {
		return err
	}
	f.ForceCancelRepos = append(f.ForceCancelRepos, full)
	if f.ForceCancelForbidden[full] {
		return fmt.Errorf("force-cancel workflow run %d of %s: %w", id, full, forge.ErrForbidden)
	}
	if f.ForceCancelErr != nil {
		return f.ForceCancelErr
	}
	for i := range f.Runs[full] {
		r := &f.Runs[full][i]
		if r.ID != id {
			continue
		}
		if r.ForceCancelErr != nil {
			return r.ForceCancelErr
		}
		if r.Status == "completed" {
			// GitHub refuses to force-cancel a finished run with a 409.
			return fmt.Errorf("force-cancel workflow run %d of %s: %w", id, full, forge.ErrConflict)
		}
		if !r.IgnoreForceCancel {
			r.Status, r.Conclusion = "completed", "cancelled"
			if f.ForceCancelled == nil {
				f.ForceCancelled = map[string][]int64{}
			}
			f.ForceCancelled[full] = append(f.ForceCancelled[full], id)
		}
		return nil
	}
	return fmt.Errorf("workflow run %d of %s: %w", id, full, forge.ErrNotFound)
}

func (f *Forge) ProbePermissions(_ context.Context, full string) (forge.RepoPermissions, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := forge.RepoPermissions{Have: map[string]string{}, Accepted: map[string]string{}}
	if _, ok := f.repos[full]; !ok {
		out.Have[forge.PermMetadataRead] = forge.AccessNo
		return out, nil
	}
	for _, p := range []string{forge.PermMetadataRead, forge.PermContentsRead, forge.PermPullsRead,
		forge.PermActionsRead, forge.PermStatusesRead, forge.PermSecretsRead} {
		out.Have[p] = forge.AccessYes
		if f.PermsDenied[full][p] {
			out.Have[p] = forge.AccessNo
		}
	}
	out.Have[forge.PermContentsWrite] = forge.AccessYes
	if f.PushDenied[full] {
		out.Have[forge.PermContentsWrite] = forge.AccessNo
	}
	return out, nil
}

func (f *Forge) OAuthScopes(context.Context) ([]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UserErr != nil {
		return nil, false, f.UserErr
	}
	return slices.Clone(f.Scopes), f.Scopes != nil, nil
}

var _ forge.PermissionProber = (*Forge)(nil)
