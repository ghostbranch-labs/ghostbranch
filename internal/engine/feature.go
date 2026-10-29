package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type FeatureStartResult struct {
	Name   string `json:"name"`
	Branch string `json:"branch"`
	Base   string `json:"base"`
	// BaseBranch is the pair's base branch: what a Parent of "main"
	// stands for, and where an unstacked feature starts.
	BaseBranch string `json:"base_branch"`
	// Parent is the feature this one is stacked on, or the placeholder
	// "main" (feature.MainParent) for the base branch.
	Parent       string     `json:"parent"`
	Checkout     [][]string `json:"checkout"`      // argvs that get the branch into a checkout, in order
	CheckoutHint string     `json:"checkout_hint"` // Checkout as one shell line
	// Local is what feature start did in the checkout it ran in; the CLI
	// fills it after the branch exists. nil for a dry run, and for the
	// engine on its own, which never touches a checkout.
	Local   *LocalCheckout   `json:"local"`
	Preview *preview.Preview `json:"preview"`
	// gateFirst is GateFirst's answer.
	gateFirst string
}

// LocalCheckout is what feature start did in the checkout it ran in: it
// switched to the new branch, only fetched it, or left the checkout alone,
// and why.
type LocalCheckout struct {
	Status string `json:"status"` // LocalSwitched, LocalFetchedOnly or LocalSkipped
	Reason string `json:"reason"` // one of the Reason constants; "" when switched
	Remote string `json:"remote"` // the checkout's remote for the shadow repo; "" when none was looked at
	// For the human text and next, never in JSON: the shadow repo
	// (owner/name); the URL a remote named shadow has when it is another
	// repo's (ReasonRemoteDiffers); git's message when a git step failed;
	// whether fetch said the repo wasn't found; whether feature start added
	// the remote, or failed to; whether the uncommitted work is only
	// untracked files; the detached HEAD's commit and how many commits only
	// it reaches (ReasonDetachedCommits); and the GitHub login Ghostbranch
	// signed in as, when known. URLs and git's messages have no userinfo.
	Shadow          string `json:"-"`
	HaveURL         string `json:"-"`
	GitError        string `json:"-"`
	NotFound        bool   `json:"-"`
	Added           bool   `json:"-"`
	AddFailed       bool   `json:"-"`
	UntrackedOnly   bool   `json:"-"`
	DetachedHead    string `json:"-"`
	DetachedCommits int    `json:"-"`
	Login           string `json:"-"`
}

// LocalCheckout.Status values.
const (
	LocalSwitched    = "switched"
	LocalFetchedOnly = "fetched_only"
	LocalSkipped     = "skipped"
)

// LocalCheckout.Reason values.
const (
	ReasonNotACheckout        = "not_a_checkout"
	ReasonDirtyTree           = "dirty_tree"
	ReasonRemoteDiffers       = "remote_differs"
	ReasonNoCheckoutFlag      = "no_checkout_flag"
	ReasonBranchExistsLocally = "branch_exists_locally"
	ReasonFetchFailed         = "fetch_failed"
	ReasonSwitchFailed        = "switch_failed"
	ReasonDetachedCommits     = "detached_commits"
)

// GateFirst is the feature below the new one, in its stack, that next
// gates first; "" when every one below has a fresh green gate.
func (r *FeatureStartResult) GateFirst() string { return r.gateFirst }

// SetCheckout sets Checkout and CheckoutHint, its one-line form.
func (r *FeatureStartResult) SetCheckout(argvs [][]string) {
	r.Checkout = argvs
	lines := make([]string, len(argvs))
	for i, a := range argvs {
		lines[i] = strings.Join(a, " ")
	}
	r.CheckoutHint = strings.Join(lines, " && ")
}

// FeatureStartOptions are feature start's options beyond the feature's name.
type FeatureStartOptions struct {
	// From is the feature to stack the new one on: it starts at that
	// feature's tip in the shadow repo and records it as its parent, with
	// the tip as forked_at. "", the base branch's real name, or "main" (its
	// alias on every pair) starts from the shadow's copy of the base.
	From string
}

// FeatureStart creates a feature branch and its metadata ref in one atomic
// push: from the shadow repo's copy of the base branch, or, with From,
// stacked on another feature's tip. It does not touch the user's checkout,
// and never writes the parent's record.
func (e *Engine) FeatureStart(ctx context.Context, p workspace.Pair, name string, opts ...FeatureStartOptions) (res *FeatureStartResult, warns []contract.Warning, err error) {
	var opt FeatureStartOptions
	if len(opts) > 0 {
		opt = opts[0]
	}
	from := opt.From
	if from == feature.MainParent {
		from = ""
	}
	defer func() {
		summary := ""
		if err == nil && from != "" {
			summary = "stacked on " + from
		}
		e.record("feature start", &p, name, "", summary, err)
	}()
	if err := feature.ValidateName(name, "name", "main"); err != nil {
		return nil, nil, err
	}
	if from != "" {
		if err := feature.ValidateName(from, "from", "main"); err != nil {
			return nil, nil, err
		}
	}
	pc, err := e.open(ctx, "feature start", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	// The name was checked before the pair was read; the base branch's name
	// is reserved too, as the shadow repo has a branch of that name. --from
	// the base by its real name starts from it, as --from main does.
	if err := feature.ValidateName(name, "name", pc.base); err != nil {
		return nil, nil, err
	}
	if from == pc.base {
		from = ""
	}
	_, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	// A feature started from a stale shadow base branch would be based on
	// old code: refuse, before anything is created (--plan too).
	mn, err := compareMains(ctx, pc.git, pc.base)
	if err != nil {
		return nil, warns, internalErr(err, "compare shadow "+pc.base+" with public "+pc.base)
	}
	if mn.Behind > 0 {
		return nil, warns, contract.New(contract.ShadowBehindError,
			"shadow %s is %d commit(s) behind public %s, so a feature started now would be based on old code; run ghostbranch sync --repo %s, then start the feature again", pc.base, mn.Behind, pc.base, p.Public).
			WithDetails(map[string]any{"public_main": mn.Public, "shadow_main": mn.Shadow})
	}
	if err := e.checkNameFree(ctx, pc, name); err != nil {
		return nil, warns, err
	}
	shadowMain, err := pc.git.MustResolve(ctx, shadowRef(pc.base))
	if err != nil {
		return nil, warns, internalErr(err, "read shadow "+pc.base)
	}
	base, parent, forkedAt := shadowMain, feature.MainParent, ""
	if from != "" {
		tip, err := e.parentTip(ctx, pc, from)
		if err != nil {
			return nil, warns, err
		}
		base, parent, forkedAt = tip, from, tip
	}
	res = &FeatureStartResult{Name: name, Branch: name, Base: base, BaseBranch: pc.base, Parent: parent}
	res.SetCheckout([][]string{{"git", "fetch", remoteShadow}, {"git", "switch", name}})
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"shadow_main": shadowMain})
		res.Preview.Add(preview.PushRef(p.Shadow, "refs/heads/"+name, "", base), preview.PushRef(p.Shadow, remoteMetaRef(name), "", ""))
		return res, warns, nil
	}
	// The record carries the parent and fork point only when stacked, so a
	// feature from main gets the same record as before 0.11.
	m := feature.Meta{SchemaVersion: feature.SchemaVersion, Name: name, Branch: name, CreatedBy: e.Actor, CreatedAt: e.now(),
		Base: base, ForkedAt: forkedAt}
	if from != "" {
		m.Parent = from
	}
	metaSHA, err := e.saveMeta(ctx, pc, m, "")
	if err != nil {
		return nil, warns, err
	}
	opt2 := gitx.PushOptions{Atomic: true, Leases: []gitx.Lease{{Ref: "refs/heads/" + name}, {Ref: remoteMetaRef(name)}}}
	if err := e.push(ctx, pc, remoteShadow, opt2, base+":refs/heads/"+name, metaSHA+":"+remoteMetaRef(name)); err != nil {
		if contract.From(err).Code == contract.RefMoved {
			return nil, warns, contract.New(contract.BranchExists, "feature %q was just created elsewhere", name).WithField("name")
		}
		return nil, warns, err
	}
	warns = addWarnings(warns, e.updateMirror(ctx, pc.git, shadowRef(name), base)...)
	warns = addWarnings(warns, e.updateMirror(ctx, pc.git, metaRef(name), metaSHA)...)
	if from != "" {
		if pm, err := pc.git.MustResolve(ctx, publicRef(pc.base)); err == nil {
			res.gateFirst = e.firstToGate(ctx, pc, from, pm, O)
		}
	}
	return res, warns, nil
}

// parentTip is the shadow tip of from, the feature a new one stacks on. It
// refuses a parent that isn't an open feature: unknown or closed
// (FEATURE_NOT_FOUND), abandoned or without its branch (BAD_FLAG), archived
// (FEATURE_ARCHIVED), from a newer Ghostbranch (UPGRADE_NEEDED), or
// unreadable (INTERNAL). Each
// refusal names the from field. The parent's record is only read.
func (e *Engine) parentTip(ctx context.Context, pc *pairCtx, from string) (string, error) {
	m, _, newer, err := e.readMetaAny(ctx, pc, from)
	if ce := contract.From(err); ce != nil && ce.Code == contract.FeatureNotFound {
		return "", contract.New(contract.FeatureNotFound,
			"no open feature %q to start from in %s; ghostbranch feature list shows the open features", from, pc.pair.Shadow).WithField("from")
	}
	switch {
	case err != nil:
		return "", contract.Wrap(contract.Internal, err, "can't start from feature %q: its record can't be read", from).WithField("from")
	case newer:
		return "", contract.NewerRecord("feature", m.SchemaVersion).WithField("from")
	case m.Abandoned:
		return "", contract.New(contract.BadFlag,
			"feature %q is abandoned, so nothing can be started from it; start from %s or another feature", from, pc.base).WithField("from")
	case m.Archived != nil:
		// Before the branch lookup: an archived feature's branch is gone
		// on purpose (P25).
		return "", e.stackOnArchivedRefusal(ctx, pc, from, m, "from")
	}
	tip, ok, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
	if err != nil {
		return "", internalErr(err, "read the parent feature's branch")
	}
	if !ok {
		return "", contract.New(contract.BadFlag,
			"feature %q's branch %s is gone from the shadow repo, so nothing can be started from it", from, m.Branch).WithField("from")
	}
	return tip, nil
}

// checkNameFree rejects names that exist, or would clash as a file/folder
// pair (auth vs auth/login), in the shadow repo, the public repo, or among
// features.
func (e *Engine) checkNameFree(ctx context.Context, pc *pairCtx, name string) error {
	spaces := []struct{ prefix, where string }{
		{"refs/remotes/shadow/", "shadow"},
		{"refs/remotes/public/", "public"},
		{"refs/shadow-meta/features/", "feature"},
	}
	for _, s := range spaces {
		refs, err := pc.git.Refs(ctx, strings.TrimSuffix(s.prefix, "/"))
		if err != nil {
			return internalErr(err, "list refs")
		}
		for ref := range refs {
			b := strings.TrimPrefix(ref, s.prefix)
			where := s.where
			if where == "feature" {
				where = "shadow"
			}
			var ce *contract.Error
			switch {
			case b == name:
				ce = contract.New(contract.BranchExists, "%q already exists in the %s repo", name, where)
			case strings.HasPrefix(name, b+"/") || strings.HasPrefix(b, name+"/"):
				ce = contract.New(contract.BranchExists, "%q clashes with the existing branch %q in the %s repo: git cannot have both a branch and a folder of that name", name, b, where)
			default:
				continue
			}
			return ce.WithField("name").
				WithDetails(map[string]any{"conflicts_with": b, "in": s.where})
		}
	}
	return nil
}
