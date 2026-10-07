package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type SyncResult struct {
	Public           string           `json:"public"`
	Shadow           string           `json:"shadow"`
	Action           string           `json:"action"`
	PublicMain       string           `json:"public_main"`
	ShadowMainBefore string           `json:"shadow_main_before"`
	ShadowMainAfter  string           `json:"shadow_main_after"`
	Branches         []SyncBranch     `json:"branches"`
	CancelledRuns    []CancelledRun   `json:"cancelled_runs"`
	Preview          *preview.Preview `json:"preview"`
}

// SyncOptions are sync's flags. NoCancel leaves the workflow runs the
// sync's pushes start in the shadow repo running.
type SyncOptions struct {
	NoCancel bool
}

// CancelledRun is a workflow run sync cancelled in the shadow repo.
type CancelledRun struct {
	ID       int64  `json:"id"`
	Workflow string `json:"workflow"`
	URL      string `json:"url"`
}

// SyncBranch is what sync did to one branch in sync.branches. From is ""
// when the shadow repo had no such branch.
type SyncBranch struct {
	Name   string `json:"name"`
	From   string `json:"from"`
	To     string `json:"to"`
	Status string `json:"status"` // up_to_date | created | fast_forwarded | diverged
}

// Sync fast-forwards each branch in sync.branches in the shadow repo to the
// public branch, creating it when missing. It never force-pushes: a diverged
// branch is reported and left alone while the others still fast-forward.
// The top-level main fields describe main. A push with the person's token
// starts the shadow repo's push-triggered workflows, re-testing what public
// CI already tested, so sync then cancels those runs unless o.NoCancel.
func (e *Engine) Sync(ctx context.Context, p workspace.Pair, o SyncOptions) (res *SyncResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		if res != nil {
			parts := make([]string, 0, len(res.Branches))
			for _, b := range res.Branches {
				parts = append(parts, b.Name+" "+b.Status)
			}
			summary = strings.Join(parts, ", ")
			if n := len(res.CancelledRuns); n > 0 {
				summary += fmt.Sprintf("; cancelled %d workflow run(s)", n)
			}
		}
		e.record("sync", &p, "", "", summary, err)
	}()
	pc, err := e.open(ctx, "sync", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	cfg, _, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	g := pc.git
	pubs := map[string]string{}
	for _, b := range cfg.Sync.Branches {
		pb, ok, err := g.Resolve(ctx, publicRef(b))
		if err != nil {
			return nil, warns, internalErr(err, "read public "+b)
		}
		if !ok {
			return nil, warns, contract.New(contract.ConfigInvalid,
				"sync.branches names %q, but the public repo has no such branch; nothing was pushed", b).WithField("sync.branches")
		}
		pubs[b] = pb
	}
	pm := pubs["main"]
	sm, _, err := g.Resolve(ctx, shadowRef("main"))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow main")
	}
	res = &SyncResult{Public: p.Public, Shadow: p.Shadow, PublicMain: pm, ShadowMainBefore: sm, ShadowMainAfter: sm, Branches: []SyncBranch{}, CancelledRuns: []CancelledRun{}}
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"public_main": pm, "shadow_main": sm})
	}
	var diverged []string
	var pushed []pushedCommit
	for _, b := range cfg.Sync.Branches {
		pb := pubs[b]
		sb, ok, err := g.Resolve(ctx, shadowRef(b))
		if err != nil {
			return res, warns, internalErr(err, "read shadow "+b)
		}
		br := SyncBranch{Name: b, From: sb, To: sb}
		switch {
		case ok && sb == pb:
			br.Status = "up_to_date"
		case ok:
			ff, err := g.IsAncestor(ctx, sb, pb)
			if err != nil {
				return res, warns, internalErr(err, "compare shadow and public "+b)
			}
			if !ff {
				br.Status = "diverged"
				diverged = append(diverged, b)
				break
			}
			br.Status = "fast_forwarded"
		default:
			br.Status = "created"
		}
		if br.Status == "fast_forwarded" || br.Status == "created" {
			if e.DryRun {
				res.Preview.Add(preview.PushRef(p.Shadow, "refs/heads/"+b, sb, pb))
			} else {
				e.progressf("fast-forwarding shadow %s %s -> %s", b, short(sb), short(pb))
				opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + b, Expect: sb}}}
				if err := e.push(ctx, pc, remoteShadow, opt, pb+":refs/heads/"+b); err != nil {
					return res, warns, err
				}
				_ = g.UpdateRef(ctx, shadowRef(b), pb)
				br.To = pb
				pushed = addPushed(pushed, pb, b)
			}
		}
		if b == "main" {
			res.Action = syncAction[br.Status]
			res.ShadowMainAfter = br.To
		}
		res.Branches = append(res.Branches, br)
	}
	if len(pushed) > 0 && !o.NoCancel {
		var cw []contract.Warning
		res.CancelledRuns, cw = e.cancelPushRuns(ctx, pc, pushed)
		warns = addWarnings(warns, cw...)
	}
	if len(diverged) > 0 {
		pronoun := "them"
		if len(diverged) == 1 {
			pronoun = "it"
		}
		return res, warns, contract.New(contract.SyncDiverged,
			"shadow %s has commits that are not in public; nothing was pushed for %s", strings.Join(diverged, ", "), pronoun).
			WithDetails(map[string]any{"branches": diverged, "shadow_main": sm, "public_main": pm})
	}
	return res, warns, nil
}

// syncAction is the top-level action for main's per-branch status.
var syncAction = map[string]string{"up_to_date": "noop", "fast_forwarded": "fast_forward", "created": "fast_forward", "diverged": "diverged"}

// pushedCommit is a commit sync pushed and the branches it pushed it to.
type pushedCommit struct {
	sha      string
	branches []string
}

func addPushed(list []pushedCommit, sha, branch string) []pushedCommit {
	for i := range list {
		if list[i].sha == sha {
			list[i].branches = append(list[i].branches, branch)
			return list
		}
	}
	return append(list, pushedCommit{sha: sha, branches: []string{branch}})
}

// pushStartsWorkflows reports whether pushing sha to any of branches starts
// a workflow: whether sha's tree has a workflow file push-triggered for one
// of them. Files that don't parse start nothing (GitHub rejects them), and
// neither do files whose every job carries the audit's push guard, which
// compares with the public repo and so skips pushes in the shadow repo. If
// the tree can't be read, it answers true, so sync still looks.
func (e *Engine) pushStartsWorkflows(ctx context.Context, pc *pairCtx, sha string, branches []string) bool {
	entries, err := pc.git.ListTree(ctx, sha)
	if err != nil {
		return true
	}
	var paths, shas []string
	for _, en := range entries {
		if en.Type == "blob" && workflow.IsWorkflowPath(en.Path) {
			paths, shas = append(paths, en.Path), append(shas, en.SHA)
		}
	}
	blobs, err := pc.git.ReadBlobs(ctx, shas)
	if err != nil {
		return true
	}
	names := guardNames(pc)
	for i, path := range paths {
		if f, err := workflow.Parse(path, blobs[shas[i]], names); err == nil && f.PushTriggered(branches) && !f.PushGuarded {
			return true
		}
	}
	return false
}

// activeRun is the run statuses sync cancels.
var activeRun = map[string]bool{"queued": true, "in_progress": true, "waiting": true, "requested": true, "pending": true}

// cancelPushRuns cancels the workflow runs that the pushes started in the
// shadow repo. A commit whose push starts no workflow is skipped without
// asking GitHub. For the others it looks for up to CIAppearWindow, every
// CIAppearPoll, and stops once runs have appeared and a further look finds
// no new one.
// Runs are cancelled as soon as they are seen, since GitHub bills each
// started job at least a minute. It never fails the sync, which has
// already pushed: trouble is a CI_NOT_CANCELLED warning. Once a cancel is
// forbidden, every other would be too, so it stops looking.
func (e *Engine) cancelPushRuns(ctx context.Context, pc *pairCtx, pushed []pushedCommit) ([]CancelledRun, []contract.Warning) {
	repo := pc.pair.Shadow
	interval := e.CIAppearPoll
	if interval <= 0 {
		interval = DefaultCIAppearPoll
	}
	cancelled := []CancelledRun{}
	denied := 0
	listDenied := false
	var failure error
	for _, pu := range pushed {
		sha := pu.sha
		if !e.pushStartsWorkflows(ctx, pc, sha, pu.branches) {
			continue
		}
		e.progressf("looking for workflow runs the push of %s started in %s", short(sha), repo)
		start := e.Now()
		seen := map[int64]bool{}
		for {
			runs, err := e.Forge.PushRuns(ctx, repo, sha)
			if err != nil {
				listDenied = errors.Is(err, forge.ErrForbidden)
				if !listDenied {
					failure = err
				}
				break
			}
			fresh := 0
			for _, r := range runs {
				if seen[r.ID] {
					continue
				}
				seen[r.ID] = true
				fresh++
				if !activeRun[r.Status] {
					continue
				}
				if denied > 0 {
					denied++ // the token can't cancel: count it, don't ask
					continue
				}
				switch err := e.Forge.CancelRun(ctx, repo, r.ID); {
				case err == nil:
					e.progressf("cancelled workflow run %s (%d)", r.Workflow, r.ID)
					cancelled = append(cancelled, CancelledRun{ID: r.ID, Workflow: r.Workflow, URL: r.URL})
				case errors.Is(err, forge.ErrForbidden):
					denied++
				case failure == nil:
					failure = err
				}
			}
			if denied > 0 || (len(seen) > 0 && fresh == 0) || e.Now().Sub(start) >= e.CIAppearWindow {
				break
			}
			e.Sleep(interval)
		}
		if listDenied || denied > 0 || failure != nil {
			break
		}
	}
	const fix = ", and the token can't cancel them; give it Actions write, or add the guard the audit suggests"
	var warns []contract.Warning
	switch {
	case denied > 0:
		warns = append(warns, contract.Warning{Code: contract.CINotCancelled,
			Message: fmt.Sprintf("the push started %d workflow run(s) in the shadow repo", denied) + fix})
	case listDenied:
		warns = append(warns, contract.Warning{Code: contract.CINotCancelled,
			Message: "the push may have started workflow runs in the shadow repo" + fix})
	}
	if failure != nil {
		warns = append(warns, contract.Warning{Code: contract.CINotCancelled,
			Message: fmt.Sprintf("the push may have started workflow runs in the shadow repo that sync could not cancel (%v); check its Actions tab", failure)})
	}
	return cancelled, warns
}
