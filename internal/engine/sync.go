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
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type SyncResult struct {
	Public           string           `json:"public"`
	Shadow           string           `json:"shadow"`
	Action           string           `json:"action"`
	PublicMain       string           `json:"public_main"`
	ShadowMainBefore string           `json:"shadow_main_before"`
	ShadowMainAfter  string           `json:"shadow_main_after"` // where sync left shadow main; when its push failed, the SHA sync read
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
	// Reparented are the children moved onto main in the push that closed
	// it; never null.
	Reparented []string `json:"reparented"`
	// GatePRLeftOpen is the gate PR the close couldn't close, 0 when none;
	// for human output only.
	GatePRLeftOpen int `json:"-"`
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
	Status string `json:"status"` // up_to_date | created | fast_forwarded | diverged | push_failed
}

// Sync fast-forwards each branch in sync.branches in the shadow repo to the
// public branch, creating it when missing. It never force-pushes: a diverged
// branch is reported and left alone while the others still fast-forward.
// The top-level main fields describe main. A push with the person's token
// starts the shadow repo's push-triggered workflows, re-testing what public
// CI already tested, so sync then cancels those runs unless o.NoCancel.
// Last, unless o.NoClose, it surveys the features: it records GitHub's
// restack of the public branches of a stack above a merged PR (the same
// changes on a new base) and flags any other change to them
// (PUBLIC_BRANCH_CHANGED), then closes the features whose public PR merged
// and whose branch hasn't moved, bottom of a stack first, moving each
// closing feature's children onto main in the push that closes it (see
// closeMerged). --no-close skips all of that, the survey too. A branch
// whose push fails is reported (SYNC_PUSH_FAILED) and the rest still go;
// the first failure is the sync's error, after the cancels and closes. A
// dry run reports each branch's target in To.
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
	pm := pubs[pc.base] // sync.branches always holds the base branch
	sm, _, err := g.Resolve(ctx, shadowRef(pc.base))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow "+pc.base)
	}
	res = &SyncResult{Public: p.Public, Shadow: p.Shadow, PublicMain: pm, ShadowMainBefore: sm, ShadowMainAfter: sm, Branches: []SyncBranch{}, CancelledRuns: []CancelledRun{},
		Closed: []ClosedFeature{}, Attention: []FeatureReport{}}
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"public_main": pm, "shadow_main": sm})
	}
	var diverged []string
	var pushed []pushedCommit
	var pushErr error // the first branch push that failed: sync's error once the rest have gone
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
				br.To = pb // what the run would push
			} else {
				st := e.startStep("Fast-forwarding shadow %s", b)
				from := short(sb)
				if sb == "" {
					from = "(new)" // a branch the shadow repo doesn't have yet
				}
				e.notef(progress.Info, "shadow %s: %s → %s", b, from, short(pb))
				opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + b, Expect: sb}}}
				if err := e.push(ctx, pc, remoteShadow, opt, pb+":refs/heads/"+b); err != nil {
					// Sync goes on to the next branch: end this step as failed
					// before the next one would mark it done.
					st.Fail()
					if ctx.Err() != nil {
						return res, warns, err // interrupted: push nothing more
					}
					// The other branches still go; the first failure is sync's error.
					br.Status = "push_failed"
					msg := fmt.Sprintf("sync could not push shadow %s: %v", b, err)
					if len(cfg.Sync.Branches) > 1 {
						msg = fmt.Sprintf("sync could not push shadow %s, and still synced the other branches: %v", b, err)
					}
					warns = append(warns, contract.Warning{Code: contract.SyncPushFailed, Message: msg,
						Details: map[string]any{"branch": b, "code": string(contract.From(err).Code)}})
					if pushErr == nil {
						pushErr = err
					}
				} else {
					st.Done(fmt.Sprintf("Fast-forwarded shadow %s", b))
					warns = addWarnings(warns, e.updateMirror(ctx, g, shadowRef(b), pb)...)
					br.To = pb
					pushed = addPushed(pushed, pb, b)
				}
			}
		}
		if b == pc.base {
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
	if pushErr != nil {
		return res, warns, pushErr
	}
	return res, warns, nil
}

// closeMerged is sync's auto-close, under sync's pair lock. It surveys the
// pair's features, which costs one GetPR per published feature: the only
// way to learn which public PRs merged, and acceptable for the handful of
// features a pair has. Once the survey succeeds it records GitHub's restack
// of stacked public branches and flags foreign changes to them
// (refreshPublished; one more GetPR per moved branch), sets res.Surveyed,
// then res.ActiveFeatures and res.Attention (a survey that fails leaves all
// three unset); then it closes each feature that is ClosableWithoutConsole
// through closeFeature, the same code as feature close, parents before
// children: a closing feature's children are re-parented onto main in its
// close's atomic push. Nothing that needs a person is ever touched here.
// While any feature's record can't be read at all, no merged feature is
// closed: that feature may be stacked on it. Each such merged feature is in
// res.Attention with a FEATURE_NOT_CLOSED warning naming the record.
// noClose skips all of it, the survey too.
// In a dry run the closes' ops join res.Preview. Nothing here fails the
// sync: a survey that fails closes nothing, a feature whose record can't be
// read is in res.Attention, and a close that fails (or, in a dry run, can't
// be previewed) is a FEATURE_NOT_CLOSED warning. The ledger records each
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
	// GitHub restacks the public branches above a merged PR: record those
	// (the same changes), and flag anything else, before deciding what to close.
	warns = addWarnings(warns, e.refreshPublished(ctx, pc, rs, res.Preview)...)
	res.Surveyed = true
	// A record that can't be read may be stacked on any merged feature:
	// closing one could strand it on a deleted branch (feature close and
	// fold refuse likewise), so none closes until it is dealt with.
	unread := unreadableReports(rs)
	for _, r := range rs {
		closable := ClosableWithoutConsole(r) && !mergedIntoParent(r, pc.base)
		if !r.Abandoned && !closable {
			res.ActiveFeatures++
		}
		attention := r.flagged || (closable && len(unread) > 0) || mergedIntoParent(r, pc.base)
		switch r.State {
		// A merged feature that isn't closable has a record from a newer
		// Ghostbranch: RECORD_NEWER says why it waits for a person.
		case feature.StateMergedMoved, feature.StatePRClosed, feature.StateBranchGone, feature.StateUnreadable, feature.StateMerged:
			attention = attention || !closable
		}
		if attention {
			res.Attention = append(res.Attention, r)
		}
	}
	// The features this sync closes: a child among them closes too, so it
	// isn't "moved onto main" in any sense a person needs to hear.
	closing := map[string]bool{}
	for _, r := range rs {
		if ClosableWithoutConsole(r) && !mergedIntoParent(r, pc.base) && len(unread) == 0 {
			closing[r.Name] = true
		}
	}
	for _, i := range bottomUp(rs) {
		r := rs[i]
		if !ClosableWithoutConsole(r) {
			continue
		}
		if mergedIntoParent(r, pc.base) {
			_, there, err := pc.git.Resolve(ctx, publicRef(r.prBase))
			warns = addWarnings(warns, mergedIntoParentWarning(r, pc.base, err == nil && !there))
			continue
		}
		if len(unread) > 0 {
			warns = addWarnings(warns, closeBlocked(r.Name, unread))
			continue
		}
		cr, cw, err := e.closeFeature(ctx, pc, r, childReports(rs, r.Name), closing)
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
			if cr.GatePRLeftOpen != 0 {
				e.notef(progress.Warn, "Deleted merged feature %s (PR #%d), but its gate PR #%d is still open", r.Name, pr, cr.GatePRLeftOpen)
			} else {
				e.notef(progress.Info, "Closed merged feature %s (PR #%d)", r.Name, pr)
			}
			res.Closed = append(res.Closed, ClosedFeature{Name: r.Name, PublicPR: pr, Deleted: cr.Deleted, Reparented: cr.Reparented,
				GatePRLeftOpen: cr.GatePRLeftOpen})
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

// mergedIntoParent is whether r is a plain chained PR (no native stack)
// that merged into a base other than the pair's base branch base: a person
// merged it into the public branch of the feature below. Its changes aren't
// in base yet, so sync leaves it open, and doesn't move its children, until
// a person has checked that branch and closes it (mergedIntoParentWarning).
// prBase is GitHub's name, so it is compared with the real branch, never
// with the parent placeholder.
func mergedIntoParent(r FeatureReport, base string) bool {
	if r.PRState != prMerged || r.prBase == "" || r.prBase == base || r.meta == nil {
		return false
	}
	return r.meta.Promoted == nil || r.meta.Promoted.Stack == 0
}

// mergedIntoParentWarning is the FEATURE_NOT_CLOSED warning for a feature
// mergedIntoParent leaves open. baseGone: the public branch its PR merged
// into is gone (the feature below merged and closed). The warning then
// names the base r's record has now (details.record_base), as a person
// can't check a deleted branch.
func mergedIntoParentWarning(r FeatureReport, base string, baseGone bool) contract.Warning {
	pr := 0
	if r.PublicPR != nil {
		pr = *r.PublicPR
	}
	details := map[string]any{"feature": r.Name, "reason": "merged_into_parent_branch", "base": r.prBase}
	if rb := recordBase(r); baseGone && rb != "" && rb != r.prBase {
		details["record_base"] = rb
		return contract.Warning{Code: contract.FeatureNotClosed,
			Message: fmt.Sprintf("sync left merged feature %s open: its PR #%d merged into %s, not %s, and public branch %s is gone now; its record stands on %s: a person checks public branch %s holds its changes, then closes %s (ghostbranch feature close %s)",
				r.Name, pr, r.prBase, base, r.prBase, rb, rb, r.Name, r.Name),
			Details: details}
	}
	return contract.Warning{Code: contract.FeatureNotClosed,
		Message: fmt.Sprintf("sync left merged feature %s open: its PR #%d merged into %s, not %s, so its changes reach %s only with %s's PR; a person checks public branch %s, then closes %s (ghostbranch feature close %s)",
			r.Name, pr, r.prBase, base, base, r.prBase, r.prBase, r.Name, r.Name),
		Details: details}
}

// recordBase is the base branch r's publish record names, "" when there
// is none.
func recordBase(r FeatureReport) string {
	if r.meta == nil || r.meta.Promoted == nil {
		return ""
	}
	return r.meta.Promoted.Base
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
var syncAction = map[string]string{"up_to_date": "noop", "fast_forwarded": "fast_forward", "created": "fast_forward", "diverged": "diverged",
	"push_failed": "push_failed"}

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

// pendingRun is a run sync asked GitHub to cancel, with the commit whose
// push started it: GitHub lists runs per commit.
type pendingRun struct {
	CancelledRun
	sha string
}

// stuckReason is why sync couldn't confirm a run cancelled.
type stuckReason int

const (
	stuckIgnored      stuckReason = iota // kept running after the cancel and the force-cancel
	stuckForceDenied                     // kept running, and the token can't force-cancel
	stuckForceFailed                     // kept running, and its force-cancel failed (err)
	stuckCancelFailed                    // its cancel failed (err)
	stuckUnchecked                       // a look at the runs failed (err)
	stuckInterrupted                     // Ctrl-C before sync could confirm it
)

// stuckRun is a run sync couldn't confirm cancelled, and its own reason.
type stuckRun struct {
	CancelledRun
	why stuckReason
	err error
}

// lookFailure is a pushed commit whose runs sync couldn't list.
type lookFailure struct {
	sha string
	err error
}

// cancelPushRuns cancels the workflow runs that the pushes started in the
// shadow repo. A commit whose push starts no workflow is skipped without
// asking GitHub; that is decided first, so an early stop warns only about
// commits that could have runs. Phase 1 looks for each commit's runs (for
// up to CIAppearWindow, every CIAppearPoll, until runs have appeared and a
// further look finds none new) and cancels each as soon as it is seen,
// since GitHub bills each started job at least a minute. Phase 2 checks
// every cancel at once (verifyCancels), so later commits' cancels never
// wait on earlier commits' checks, and lists only the runs that did stop.
// It never fails the sync, which has already pushed: trouble is a
// CI_NOT_CANCELLED warning with details.runs. Once a listing or a cancel
// is forbidden, every other would be too, so it stops looking.
func (e *Engine) cancelPushRuns(ctx context.Context, pc *pairCtx, pushed []pushedCommit) ([]CancelledRun, []contract.Warning) {
	repo := pc.pair.Shadow
	interval := e.CIAppearPoll
	if interval <= 0 {
		interval = DefaultCIAppearPoll
	}
	var shas []string
	for _, pu := range pushed {
		if e.pushStartsWorkflows(ctx, pc, pu.sha, pu.branches) {
			shas = append(shas, pu.sha)
		}
	}
	requested := map[int64]pendingRun{}
	var asked []string // the commits with requested runs, in push order
	var stuck []stuckRun
	var denied []CancelledRun // active runs seen once the token couldn't cancel
	var lookFailed []lookFailure
	listDenied := false
	var interrupted error
	// A sync that pushed nothing which starts workflows has nothing to look for.
	look := progress.Nop().Step("")
	if len(shas) > 0 {
		look = e.startStep("Looking for CI runs the sync started in %s", repo)
	}
looking:
	for k, sha := range shas {
		look.Update(fmt.Sprintf("Looking for CI runs the sync started in %s (commit %d of %d)", repo, k+1, len(shas)))
		start := e.Now()
		seen := map[int64]bool{}
		for {
			runs, err := e.Forge.PushRuns(ctx, repo, sha)
			switch {
			case err != nil && ctx.Err() != nil:
				interrupted = ctx.Err()
				break looking
			case errors.Is(err, forge.ErrForbidden):
				listDenied = true
				break looking
			case err != nil:
				lookFailed = append(lookFailed, lookFailure{sha: sha, err: err})
				continue looking // this commit only: the next may list fine
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
				run := CancelledRun{ID: r.ID, Workflow: r.Workflow, URL: r.URL}
				if len(denied) > 0 {
					denied = append(denied, run) // the token can't cancel: list it, don't ask
					continue
				}
				switch err := e.Forge.CancelRun(ctx, repo, r.ID); {
				case err == nil || errors.Is(err, forge.ErrConflict):
					// Checked in phase 2: a 409 may mean it finished, or not.
					requested[r.ID] = pendingRun{CancelledRun: run, sha: sha}
					if !slices.Contains(asked, sha) {
						asked = append(asked, sha)
					}
				case errors.Is(err, forge.ErrForbidden):
					denied = append(denied, run)
				case ctx.Err() != nil:
					stuck = append(stuck, stuckRun{CancelledRun: run, why: stuckInterrupted})
				default:
					stuck = append(stuck, stuckRun{CancelledRun: run, why: stuckCancelFailed, err: err})
				}
			}
			switch {
			case len(denied) > 0:
				break looking
			case ctx.Err() != nil:
				interrupted = ctx.Err()
				break looking
			case (len(seen) > 0 && fresh == 0) || e.Now().Sub(start) >= e.CIAppearWindow:
				continue looking
			}
			if err := e.Sleep(ctx, interval); err != nil {
				interrupted = err // runs may have started that it never saw
				break looking
			}
		}
	}
	if interrupted == nil {
		look.Done("Looked for CI runs the sync started")
	}
	cancelled := []CancelledRun{}
	if len(requested) > 0 {
		if interrupted != nil {
			for _, r := range sortedRuns(requested) {
				stuck = append(stuck, stuckRun{CancelledRun: r, why: stuckInterrupted})
			}
		} else {
			check := e.startStep("Checking that the cancelled CI runs stopped")
			done, left := e.verifyCancels(ctx, repo, asked, requested, interval)
			if len(left) > 0 {
				e.notef(progress.Warn, "Couldn't confirm that %d cancelled CI run(s) stopped", len(left))
			}
			check.Done("Checked the cancelled CI runs")
			cancelled = append(cancelled, done...)
			stuck = append(stuck, left...)
		}
	}
	return cancelled, cancelWarnings(stuck, denied, listDenied, lookFailed, interrupted)
}

// verifyCancels checks, in one window for every commit, that the runs sync
// asked GitHub to cancel did stop. Each look lists the runs of each commit
// in shas that still has a run pending. It looks every interval, starting
// one interval after the cancels, for up to DefaultCancelVerifyWindow: a
// run that completed as cancelled is done, and one that completed
// otherwise finished on its own and is dropped. It force-cancels the runs
// still going (the first ErrForbidden stops it asking) and, if any
// force-cancel was accepted, looks again for up to DefaultForceCancelWindow.
// Every run neither done nor dropped is stuck with its own reason: a failed
// force-cancel keeps its error, and a failed look or an interrupt marks
// only the runs it left unconfirmed.
func (e *Engine) verifyCancels(ctx context.Context, repo string, shas []string, requested map[int64]pendingRun, interval time.Duration) (done []CancelledRun, stuck []stuckRun) {
	pending := maps.Clone(requested)
	forceFailed := map[int64]stuckRun{}
	look := func() error {
		for _, sha := range shas {
			if !hasPending(pending, sha) {
				continue
			}
			runs, err := e.Forge.PushRuns(ctx, repo, sha)
			if err != nil {
				return err
			}
			for _, r := range runs {
				p, ok := pending[r.ID]
				if !ok || r.Status != "completed" {
					continue
				}
				delete(pending, r.ID)
				delete(forceFailed, r.ID)
				if r.Conclusion == "cancelled" {
					e.notef(progress.Info, "Cancelled CI run %s (%d)", p.Workflow, p.ID)
					done = append(done, p.CancelledRun)
				} else {
					e.notef(progress.Info, "CI run %s (%d) finished on its own", p.Workflow, p.ID)
				}
			}
		}
		return nil
	}
	settle := func(window time.Duration) error {
		start := e.Now()
		for {
			if err := e.Sleep(ctx, interval); err != nil {
				return err
			}
			if err := look(); err != nil {
				return err
			}
			if len(pending) == 0 || e.Now().Sub(start) >= window {
				return nil
			}
		}
	}
	finish := func(err error) ([]CancelledRun, []stuckRun) {
		for _, r := range sortedRuns(pending) {
			s, failed := forceFailed[r.ID]
			switch {
			case failed: // keeps its own force-cancel failure
			case err != nil && ctx.Err() != nil:
				s = stuckRun{CancelledRun: r, why: stuckInterrupted}
			case err != nil:
				s = stuckRun{CancelledRun: r, why: stuckUnchecked, err: err}
			default:
				s = stuckRun{CancelledRun: r, why: stuckIgnored}
			}
			stuck = append(stuck, s)
		}
		return done, stuck
	}
	if err := settle(DefaultCancelVerifyWindow); err != nil || len(pending) == 0 {
		return finish(err)
	}
	forced, forceDenied := 0, false
	for _, r := range sortedRuns(pending) {
		if forceDenied {
			forceFailed[r.ID] = stuckRun{CancelledRun: r, why: stuckForceDenied}
			continue
		}
		switch ferr := e.Forge.ForceCancelRun(ctx, repo, r.ID); {
		case ferr == nil || errors.Is(ferr, forge.ErrConflict):
			e.notef(progress.Warn, "Force-cancelling CI run %s (%d), which kept running", r.Workflow, r.ID)
			forced++
		case ctx.Err() != nil:
			return finish(ctx.Err())
		case errors.Is(ferr, forge.ErrForbidden):
			forceDenied = true // every other force-cancel would be refused too
			forceFailed[r.ID] = stuckRun{CancelledRun: r, why: stuckForceDenied}
		default:
			e.notef(progress.Warn, "Couldn't force-cancel CI run %s (%d): %v", r.Workflow, r.ID, ferr)
			forceFailed[r.ID] = stuckRun{CancelledRun: r, why: stuckForceFailed, err: ferr}
		}
	}
	if forced == 0 {
		return finish(nil) // none was accepted: nothing to wait for
	}
	return finish(settle(DefaultForceCancelWindow))
}

func hasPending(pending map[int64]pendingRun, sha string) bool {
	for _, p := range pending {
		if p.sha == sha {
			return true
		}
	}
	return false
}

// sortedRuns lists runs by ID.
func sortedRuns(m map[int64]pendingRun) []CancelledRun {
	out := make([]CancelledRun, 0, len(m))
	for _, p := range m {
		out = append(out, p.CancelledRun)
	}
	slices.SortFunc(out, func(a, b CancelledRun) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// runRefs is CI_NOT_CANCELLED's details: each run's id and URL, never null.
func runRefs(runs []CancelledRun) map[string]any {
	refs := make([]map[string]any, 0, len(runs))
	for _, r := range runs {
		refs = append(refs, map[string]any{"id": r.ID, "url": r.URL})
	}
	return map[string]any{"runs": refs}
}

func runList(runs []CancelledRun) string {
	names := make([]string, 0, len(runs))
	for _, r := range runs {
		names = append(names, fmt.Sprintf("%d (%s)", r.ID, r.URL))
	}
	return strings.Join(names, ", ")
}

// cancelWarnings turns what cancelPushRuns couldn't do into CI_NOT_CANCELLED
// warnings: one per reason (and error text) for the runs it knows of, then
// one each for runs it couldn't list or never saw.
func cancelWarnings(stuck []stuckRun, denied []CancelledRun, listDenied bool, lookFailed []lookFailure, interrupted error) []contract.Warning {
	const fix = ", and the token can't cancel them; give it Actions write, or add the guard the audit suggests"
	var warns []contract.Warning
	warn := func(msg string, runs []CancelledRun) {
		warns = append(warns, contract.Warning{Code: contract.CINotCancelled, Message: msg, Details: runRefs(runs)})
	}
	type group struct {
		why  stuckReason
		err  string
		runs []CancelledRun
	}
	var groups []*group
	for _, s := range stuck {
		text := ""
		if s.err != nil {
			text = s.err.Error()
		}
		i := slices.IndexFunc(groups, func(g *group) bool { return g.why == s.why && g.err == text })
		if i < 0 {
			groups = append(groups, &group{why: s.why, err: text})
			i = len(groups) - 1
		}
		groups[i].runs = append(groups[i].runs, s.CancelledRun)
	}
	for _, g := range groups {
		list := runList(g.runs)
		switch g.why {
		case stuckInterrupted:
			warn(fmt.Sprintf("sync was interrupted before it could confirm that workflow run(s) %s were cancelled; check the shadow repo's Actions tab", list), g.runs)
		case stuckUnchecked:
			warn(fmt.Sprintf("sync could not check that workflow run(s) %s were cancelled (%s); check the shadow repo's Actions tab", list, g.err), g.runs)
		case stuckCancelFailed:
			warn(fmt.Sprintf("sync could not cancel workflow run(s) %s (%s); cancel them in the shadow repo's Actions tab", list, g.err), g.runs)
		case stuckForceDenied:
			warn(fmt.Sprintf("workflow run(s) %s kept running after sync cancelled them", list)+fix, g.runs)
		case stuckForceFailed:
			warn(fmt.Sprintf("workflow run(s) %s kept running after sync cancelled them, and sync could not force-cancel them (%s); cancel them in the shadow repo's Actions tab", list, g.err), g.runs)
		default:
			warn(fmt.Sprintf("workflow run(s) %s kept running after sync cancelled and force-cancelled them; cancel them in the shadow repo's Actions tab", list), g.runs)
		}
	}
	if len(denied) > 0 {
		warn(fmt.Sprintf("the push started %d workflow run(s) in the shadow repo", len(denied))+fix, denied)
	}
	if listDenied {
		warn("the push may have started workflow runs in the shadow repo"+fix, nil)
	}
	for _, f := range lookFailed {
		warn(fmt.Sprintf("sync could not list the workflow runs the push of %s started (%v), so it could not cancel them; check the shadow repo's Actions tab", short(f.sha), f.err), nil)
	}
	if interrupted != nil {
		warn("sync was interrupted while looking for the workflow runs its push started, and could not cancel any it had not seen yet; check the shadow repo's Actions tab", nil)
	}
	return warns
}
