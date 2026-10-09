package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
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
	Closed           []ClosedFeature  `json:"closed"`          // merged features sync closed; never null
	Surveyed         bool             `json:"surveyed"`        // the features were surveyed: false with --no-close or when the survey failed
	ActiveFeatures   int              `json:"active_features"` // features not abandoned and not closable by sync; 0 unless Surveyed
	Attention        []FeatureReport  `json:"attention"`       // features a person must look at; never null
	Preview          *preview.Preview `json:"preview"`
}

// SyncOptions are sync's flags. NoCancel leaves the workflow runs the
// sync's pushes start in the shadow repo running. NoClose leaves merged
// features in place.
type SyncOptions struct {
	NoCancel bool
	NoClose  bool
}

// ClosedFeature is a merged feature sync closed: its public PR and every
// shadow ref deleted, with the SHA it pointed at.
type ClosedFeature struct {
	Name     string            `json:"name"`
	PublicPR int               `json:"public_pr"`
	Deleted  map[string]string `json:"deleted"`
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
// Last, unless o.NoClose, it closes the features whose public PR merged and
// whose branch hasn't moved (see closeMerged).
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
			if n := len(res.Closed); n > 0 {
				summary += fmt.Sprintf("; closed %d merged feature(s)", n)
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
	res = &SyncResult{Public: p.Public, Shadow: p.Shadow, PublicMain: pm, ShadowMainBefore: sm, ShadowMainAfter: sm, Branches: []SyncBranch{}, CancelledRuns: []CancelledRun{},
		Closed: []ClosedFeature{}, Attention: []FeatureReport{}}
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
				warns = addWarnings(warns, e.updateMirror(ctx, g, shadowRef(b), pb)...)
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
	// The closes read only public main and the feature refs, so a diverged
	// shadow branch doesn't hold them up.
	warns = addWarnings(warns, e.closeMerged(ctx, pc, &p, res, o.NoClose)...)
	if len(diverged) > 0 {
		pronoun := "them"
		if len(diverged) == 1 {
			pronoun = "it"
		}
		return res, warns, contract.New(contract.SyncDiverged,
			"shadow %s has commits that are not in public; nothing was pushed to %s", strings.Join(diverged, ", "), pronoun).
			WithDetails(map[string]any{"branches": diverged, "shadow_main": sm, "public_main": pm})
	}
	return res, warns, nil
}

// closeMerged is sync's auto-close, under sync's pair lock. It surveys the
// pair's features, which costs one GetPR per published feature: the only
// way to learn which public PRs merged, and acceptable for the handful of
// features a pair has. It sets res.ActiveFeatures and res.Attention, then
// closes each feature that is ClosableWithoutConsole through closeFeature,
// the same code as feature close: nothing that needs a person is ever
// touched here. noClose skips all of it, the survey too. In a dry run the
// closes' ops join res.Preview. Nothing here fails the sync: a survey that
// fails closes nothing, and a close that fails (or, in a dry run, can't be
// previewed) is a FEATURE_NOT_CLOSED warning. The ledger records each
// close, with every ref it deleted, and a feature whose refs were deleted
// is in res.Closed even if its gate PR then wouldn't close.
func (e *Engine) closeMerged(ctx context.Context, pc *pairCtx, p *workspace.Pair, res *SyncResult, noClose bool) []contract.Warning {
	if noClose {
		return nil
	}
	rs, warns, err := e.survey(ctx, pc)
	if err != nil {
		ce := contract.From(err)
		return addWarnings(warns, contract.Warning{Code: contract.FeatureNotClosed,
			Message: fmt.Sprintf("couldn't survey features, so nothing was closed: %s: %s", ce.Code, ce.Message),
			Details: map[string]any{"error_code": string(ce.Code)}})
	}
	res.Surveyed = true
	for _, r := range rs {
		closable := ClosableWithoutConsole(r)
		if !r.Abandoned && !closable {
			res.ActiveFeatures++
		}
		switch r.State {
		case feature.StateMergedMoved, feature.StatePRClosed, feature.StateBranchGone:
			if !closable {
				res.Attention = append(res.Attention, r)
			}
		}
	}
	for _, r := range rs {
		if !ClosableWithoutConsole(r) {
			continue
		}
		cr, cw, err := e.closeFeature(ctx, pc, r)
		warns = addWarnings(warns, cw...)
		if e.DryRun {
			// A close that couldn't be previewed warns, as the real one would.
			if err != nil {
				warns = addWarnings(warns, notClosed(r.Name, err, nil))
				continue
			}
			res.Preview.Add(cr.Preview.Ops...)
			continue
		}
		summary := ""
		var refs map[string]string
		if cr != nil {
			refs = cr.Deleted
			if err == nil {
				summary = closeLedgerSummary(cr)
			}
		}
		e.recordWith("feature close", p, r.Name, "", summary, err, func(ent *ledger.Entry) {
			if len(refs) > 0 {
				ent.Refs = refs
			}
		})
		deleted := cr != nil && len(cr.Deleted) > 0
		if err == nil || deleted {
			pr := 0
			if r.PublicPR != nil {
				pr = *r.PublicPR
			}
			e.progressf("closed merged feature %s (PR #%d)", r.Name, pr)
			res.Closed = append(res.Closed, ClosedFeature{Name: r.Name, PublicPR: pr, Deleted: cr.Deleted})
		}
		if err != nil {
			var gone map[string]string
			if deleted {
				gone = cr.Deleted
			}
			warns = addWarnings(warns, notClosed(r.Name, err, gone))
		}
	}
	return warns
}

// notClosed is the FEATURE_NOT_CLOSED warning for a merged feature sync
// couldn't close (or, under --plan, couldn't preview closing), with the refs
// it deleted when there are any.
func notClosed(name string, err error, deleted map[string]string) contract.Warning {
	ce := contract.From(err)
	details := map[string]any{"feature": name, "error_code": string(ce.Code)}
	if len(deleted) > 0 {
		details["deleted"] = deleted
	}
	return contract.Warning{Code: contract.FeatureNotClosed,
		Message: fmt.Sprintf("couldn't close merged feature %s: %s: %s", name, ce.Code, ce.Message),
		Details: details}
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
// started job at least a minute. GitHub only accepts a cancel, so it then
// checks that each run did stop (verifyCancels), and lists only those.
// It never fails the sync, which has already pushed: trouble is a
// CI_NOT_CANCELLED warning. Once a cancel is forbidden, every other would
// be too, so it stops looking.
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
	var stuck []CancelledRun
	forceDenied := false
	var verifyErr error
	for _, pu := range pushed {
		sha := pu.sha
		if !e.pushStartsWorkflows(ctx, pc, sha, pu.branches) {
			continue
		}
		e.progressf("looking for workflow runs the push of %s started in %s", short(sha), repo)
		start := e.Now()
		seen := map[int64]bool{}
		requested := map[int64]CancelledRun{}
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
				case err == nil || errors.Is(err, forge.ErrConflict):
					// Checked below: a 409 may mean it finished, or not.
					requested[r.ID] = CancelledRun{ID: r.ID, Workflow: r.Workflow, URL: r.URL}
				case errors.Is(err, forge.ErrForbidden):
					denied++
				case failure == nil:
					failure = err
				}
			}
			if denied > 0 || (len(seen) > 0 && fresh == 0) || e.Now().Sub(start) >= e.CIAppearWindow {
				break
			}
			if err := e.Sleep(ctx, interval); err != nil {
				// Interrupted: runs may have started that it never saw.
				if failure == nil {
					failure = err
				}
				break
			}
		}
		if len(requested) > 0 {
			done, left, forbidden, err := e.verifyCancels(ctx, repo, sha, requested, interval)
			cancelled = append(cancelled, done...)
			stuck = append(stuck, left...)
			forceDenied = forceDenied || forbidden
			verifyErr = err
		}
		if listDenied || denied > 0 || failure != nil || verifyErr != nil {
			break
		}
	}
	const fix = ", and the token can't cancel them; give it Actions write, or add the guard the audit suggests"
	var warns []contract.Warning
	if len(stuck) > 0 {
		var names []string
		for _, r := range stuck {
			names = append(names, fmt.Sprintf("%d (%s)", r.ID, r.URL))
		}
		list := strings.Join(names, ", ")
		var msg string
		switch {
		case verifyErr != nil && ctx.Err() != nil:
			msg = fmt.Sprintf("sync was interrupted while checking that workflow run(s) %s were cancelled; check the shadow repo's Actions tab", list)
		case verifyErr != nil:
			msg = fmt.Sprintf("sync could not check that workflow run(s) %s were cancelled (%v); check the shadow repo's Actions tab", list, verifyErr)
		case forceDenied:
			msg = fmt.Sprintf("workflow run(s) %s kept running after sync cancelled them", list) + fix
		default:
			msg = fmt.Sprintf("workflow run(s) %s kept running after sync cancelled and force-cancelled them; cancel them in the shadow repo's Actions tab", list)
		}
		warns = append(warns, contract.Warning{Code: contract.CINotCancelled, Message: msg})
	}
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

// verifyCancels checks that the runs sync asked GitHub to cancel did stop.
// It looks every interval for up to DefaultCancelVerifyWindow; a run that
// completed as cancelled is done, and one that completed otherwise finished
// on its own and is dropped. It force-cancels the runs still going and
// looks again for up to DefaultForceCancelWindow. What is still going then
// is stuck. forbidden is set when the token can't force-cancel. A failed
// look or an interrupted wait ends it with err, the unsettled runs stuck.
func (e *Engine) verifyCancels(ctx context.Context, repo, sha string, requested map[int64]CancelledRun, interval time.Duration) (done, stuck []CancelledRun, forbidden bool, err error) {
	pending := maps.Clone(requested)
	settle := func(window time.Duration) error {
		start := e.Now()
		for {
			runs, err := e.Forge.PushRuns(ctx, repo, sha)
			if err != nil {
				return err
			}
			for _, r := range runs {
				c, ok := pending[r.ID]
				if !ok || r.Status != "completed" {
					continue
				}
				delete(pending, r.ID)
				if r.Conclusion == "cancelled" {
					e.progressf("cancelled workflow run %s (%d)", c.Workflow, c.ID)
					done = append(done, c)
				} else {
					e.progressf("workflow run %s (%d) finished on its own", c.Workflow, c.ID)
				}
			}
			if len(pending) == 0 || e.Now().Sub(start) >= window {
				return nil
			}
			if err := e.Sleep(ctx, interval); err != nil {
				return err
			}
		}
	}
	unsettled := func() []CancelledRun {
		out := slices.Collect(maps.Values(pending))
		slices.SortFunc(out, func(a, b CancelledRun) int { return cmp.Compare(a.ID, b.ID) })
		return out
	}
	if err := settle(DefaultCancelVerifyWindow); err != nil || len(pending) == 0 {
		return done, unsettled(), false, err
	}
	forced := 0
	for _, r := range unsettled() {
		switch err := e.Forge.ForceCancelRun(ctx, repo, r.ID); {
		case err == nil || errors.Is(err, forge.ErrConflict):
			e.progressf("force-cancelling workflow run %s (%d), which kept running", r.Workflow, r.ID)
			forced++
		case errors.Is(err, forge.ErrForbidden):
			forbidden = true
		default:
			e.progressf("could not force-cancel workflow run %s (%d): %v", r.Workflow, r.ID, err)
		}
		if forbidden {
			break // every other force-cancel would be refused too
		}
	}
	if forced > 0 {
		err = settle(DefaultForceCancelWindow)
	}
	return done, unsettled(), forbidden, err
}
