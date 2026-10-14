package engine

// Handing a published feature off to the public repo (6.5): its shadow
// branch moves to refs/ghostbranch/archive/<name>, and its PR is fixed by
// pushing to the PR's branch.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// What happened to a published feature's shadow branch (data.after_publish).
const (
	AfterArchived   = "archived"
	AfterKept       = "kept"
	AfterIncomplete = "incomplete"
)

// The steps of an archive, as ARCHIVE_INCOMPLETE names them.
const (
	stepPush   = "push"
	stepGatePR = "gate_pr"
	stepRecord = "record"
)

// FeatureArchiveResult is a feature handed off by feature archive.
type FeatureArchiveResult struct {
	Feature string `json:"feature"`
	// Changed is false when the feature was already archived.
	Changed bool `json:"changed"`
	// Archived is the record's archive; null under --plan before it exists.
	Archived *feature.Archived `json:"archived"`
	// Moved is every shadow ref the archive created or deleted, with its
	// SHA; never null.
	Moved  map[string]string `json:"moved"`
	GatePR *int              `json:"gate_pr"` // the gate draft PR closed; null when none was open
	// PublicPR and Branch are the PR and its public branch: push fixes there.
	PublicPR int              `json:"public_pr"`
	Branch   string           `json:"branch"`
	Preview  *preview.Preview `json:"preview"`
}

// archiveDone is what archiveOne did.
type archiveDone struct {
	archived *feature.Archived
	moved    map[string]string
	gatePR   int
}

// archiveStepError is an archive that stopped at step; err is that step's
// error.
type archiveStepError struct {
	step string
	err  error
}

func (a *archiveStepError) Error() string { return a.step + ": " + a.err.Error() }
func (a *archiveStepError) Unwrap() error { return a.err }

// stepErr unwraps an archive's error to the step's own error.
func stepErr(err error) error {
	var se *archiveStepError
	if errors.As(err, &se) {
		return se.err
	}
	return err
}

// archiveIncomplete is the ARCHIVE_INCOMPLETE warning for a feature whose
// archive stopped with err after its publish.
func archiveIncomplete(name string, err error) contract.Warning {
	step := stepPush
	var se *archiveStepError
	if errors.As(err, &se) {
		step = se.step
	}
	ce := contract.From(stepErr(err))
	return contract.Warning{Code: contract.ArchiveIncomplete,
		Message: fmt.Sprintf("published, but archiving feature %s stopped at %s (%s); finish it with ghostbranch feature archive %s",
			name, step, ce.Message, name),
		Details: map[string]any{"feature": name, "step": step, "code": string(ce.Code)}}
}

// archiveTarget is what archiving a feature changes in the shadow repo, as
// the mirror has it: the branch's head, the archive ref and the gate
// branch (each with whether it exists), the commit the archive ref ends at,
// the gate's open draft PR (nil when none), and the one atomic push's
// options and refspecs (R6). pushArch is whether that push writes the
// archive ref.
type archiveTarget struct {
	head, arch, gate, target string
	hasBranch, hasArch       bool
	hasGate, pushArch        bool
	gatePR                   *forge.PR
	opt                      gitx.PushOptions
	specs                    []string
}

// archiveTargets reads what archiving name would change. archiveOne pushes
// it and previewArchive renders it, so the preview refuses exactly what the
// run refuses (P7): a record naming another branch is INTERNAL, no branch
// and no archive ref is FEATURE_NOT_FOUND (R13, P20), and an archive ref
// the branch doesn't hold is FEATURE_MOVED (R17). Errors are
// *archiveStepError (step push, or gate_pr for the gate PR's lookup).
func (e *Engine) archiveTargets(ctx context.Context, pc *pairCtx, name string) (*archiveTarget, error) {
	g := pc.git
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, &archiveStepError{step: stepPush, err: err}
	}
	if m.Branch != name {
		return nil, &archiveStepError{step: stepPush, err: contract.New(contract.Internal,
			"feature %s's metadata names the branch %q, not %q; refusing to archive another branch", name, m.Branch, name).
			WithDetails(map[string]any{"feature": name, "branch": m.Branch})}
	}
	read := func(ref string) (string, bool, error) {
		sha, ok, err := g.Resolve(ctx, ref)
		if err != nil {
			return "", false, &archiveStepError{step: stepPush, err: internalErr(err, "read "+ref)}
		}
		return sha, ok, nil
	}
	t := &archiveTarget{}
	if t.head, t.hasBranch, err = read(shadowRef(name)); err != nil {
		return nil, err
	}
	if t.arch, t.hasArch, err = read(archiveRef(name)); err != nil {
		return nil, err
	}
	if t.gate, t.hasGate, err = read(shadowRef(gateBranch(name))); err != nil {
		return nil, err
	}
	t.target = t.head
	switch {
	case !t.hasBranch && !t.hasArch:
		return nil, &archiveStepError{step: stepPush, err: contract.New(contract.FeatureNotFound,
			"feature %s's branch is gone from %s and it has no archive ref, so there is nothing to archive; look with ghostbranch feature show %s",
			name, pc.pair.Shadow, name).WithField("feature")}
	case !t.hasBranch:
		t.target = t.arch
	case t.hasArch && t.arch != t.head:
		// The branch moved after an archive that stopped partway: archive
		// the new head, but only when it holds the old one (R17).
		in, err := g.IsAncestor(ctx, t.arch, t.head)
		if err != nil {
			return nil, &archiveStepError{step: stepPush, err: internalErr(err, "compare the archive of "+name+" with its branch")}
		}
		if !in {
			return nil, &archiveStepError{step: stepPush, err: contract.New(contract.FeatureMoved,
				"feature %s's branch (%s) doesn't hold its archive (%s), so moving the archive would lose commits; nothing was changed: a person looks with ghostbranch feature show %s",
				name, short(t.head), short(t.arch), name).WithField("feature").
				WithDetails(map[string]any{"feature": name, "ref": remoteArchiveRef(name)})}
		}
	}
	if t.gatePR, err = e.Forge.FindOpenPR(ctx, pc.pair.Shadow, gateBranch(name), "main"); err != nil {
		return nil, &archiveStepError{step: stepGatePR, err: forgeErr(err, "find the gate PR of feature %s", name)}
	}
	t.opt = gitx.PushOptions{Atomic: true}
	if !t.hasArch || t.arch != t.target {
		// arch is "" when absent: the lease then requires the ref not to exist.
		t.pushArch = true
		t.opt.Leases = append(t.opt.Leases, gitx.Lease{Ref: remoteArchiveRef(name), Expect: t.arch})
		t.specs = append(t.specs, t.target+":"+remoteArchiveRef(name))
	}
	if t.hasBranch {
		t.opt.Leases = append(t.opt.Leases, gitx.Lease{Ref: "refs/heads/" + name, Expect: t.head})
		t.specs = append(t.specs, ":refs/heads/"+name)
	}
	if t.hasGate {
		t.opt.Leases = append(t.opt.Leases, gitx.Lease{Ref: "refs/heads/" + gateBranch(name), Expect: t.gate})
		t.specs = append(t.specs, ":refs/heads/"+gateBranch(name))
	}
	return t, nil
}

// archiveOne hands feature name off (spec § 4.1 step 4, R6): one atomic
// push to the shadow repo creates refs/ghostbranch/archive/<name> at the
// branch's head and deletes the branch and its gate branch, each leased on
// what the mirror holds; then the gate PR is closed and the record marked.
// Every step is safe to run again (§ 4.5): an archive ref at the head is
// done, a missing branch is fine once the archive ref exists, a closed
// gate PR is done, and a marked record is done. A failed step is an
// *archiveStepError, returned with what was done before it (for the
// ledger). It never touches the public repo.
func (e *Engine) archiveOne(ctx context.Context, pc *pairCtx, name string) (*archiveDone, []contract.Warning, error) {
	g := pc.git
	m, metaSHA, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, nil, &archiveStepError{step: stepRecord, err: err}
	}
	if m.Archived != nil {
		return &archiveDone{archived: m.Archived, moved: map[string]string{}}, nil, nil
	}
	t, err := e.archiveTargets(ctx, pc, name)
	if err != nil {
		return nil, nil, err
	}
	d := &archiveDone{moved: map[string]string{}}
	var warns []contract.Warning
	if len(t.specs) > 0 {
		st := e.startStep("Archiving %s's shadow branch", name)
		if err := e.push(ctx, pc, remoteShadow, t.opt, t.specs...); err != nil {
			st.Fail()
			return nil, nil, &archiveStepError{step: stepPush, err: movedErr(name, pc.pair.Shadow, "archived", err)}
		}
		st.Done(fmt.Sprintf("Archived %s at %s", name, remoteArchiveRef(name)))
		if t.pushArch {
			// Only a ref this push wrote is moved (P16): on a resume the
			// archive ref was there already.
			d.moved[remoteArchiveRef(name)] = t.target
			warns = addWarnings(warns, e.updateMirror(ctx, g, archiveRef(name), t.target)...)
		}
		if t.hasBranch {
			d.moved["refs/heads/"+name] = t.head
			warns = addWarnings(warns, e.deleteMirror(ctx, g, shadowRef(name))...)
		}
		if t.hasGate {
			d.moved["refs/heads/"+gateBranch(name)] = t.gate
			warns = addWarnings(warns, e.deleteMirror(ctx, g, shadowRef(gateBranch(name)))...)
		}
	}
	if pr := t.gatePR; pr != nil {
		// GitHub closes a PR whose head branch went; a closed PR is done.
		err := e.Forge.ClosePullRequest(ctx, pc.pair.Shadow, pr.Number)
		if err != nil && !errors.Is(err, forge.ErrNotFound) && !e.prClosed(ctx, pc.pair.Shadow, pr.Number) {
			return d, warns, &archiveStepError{step: stepGatePR, err: forgeErr(err, "close gate PR #%d of feature %s in %s", pr.Number, name, pc.pair.Shadow)}
		}
		d.gatePR = pr.Number
	}
	m.SchemaVersion = feature.SchemaVersion
	m.Archived = &feature.Archived{At: e.now(), Ref: remoteArchiveRef(name), Head: t.target}
	_, mw, err := e.updateMeta(ctx, pc, m, metaSHA)
	warns = addWarnings(warns, mw...)
	if err != nil {
		return d, warns, &archiveStepError{step: stepRecord, err: err}
	}
	d.archived = m.Archived
	return d, warns, nil
}

// archiveBlockers is why names can't be archived together (R8): kids are
// the features whose record names one of names as its parent, abandoned
// ones included, unless they are among names or already archived; and
// unreadable are the other records that can't be read, any of which may be
// such a child. Both sorted; both empty when nothing blocks.
func (e *Engine) archiveBlockers(ctx context.Context, pc *pairCtx, names []string) (kids, unreadable []string, err error) {
	recs, bad, err := e.recordsAndUnreadable(ctx, pc)
	if err != nil {
		return nil, nil, err
	}
	in := map[string]bool{}
	for _, n := range names {
		in[n] = true
	}
	for n, r := range recs {
		if !in[n] && in[parentFeature(r.meta)] && r.meta.Archived == nil {
			kids = append(kids, n)
		}
	}
	for _, n := range bad {
		if !in[n] {
			unreadable = append(unreadable, n)
		}
	}
	sort.Strings(kids)
	return kids, unreadable, nil
}

// blockedWhy says why Archive is unavailable for n features, from
// archiveBlockers; "" when nothing blocks.
func blockedWhy(kids, unreadable []string, n int) string {
	this, it := "this feature", "it"
	if n > 1 {
		this = "these features"
	}
	switch {
	case len(kids) == 1:
		return fmt.Sprintf("%s is built on %s; publish or restack %s first", kids[0], this, it)
	case len(kids) > 1:
		return fmt.Sprintf("%s are built on %s; publish or restack them first", strings.Join(kids, ", "), this)
	case len(unreadable) > 0:
		return fmt.Sprintf("the records of %s can't be read, and one may be built on %s", strings.Join(unreadable, ", "), this)
	}
	return ""
}

// previewArchive adds to pv what archiveOne would do for name, from the
// same archiveTargets (P7): it refuses what the run would refuse, with the
// step's own error.
func (e *Engine) previewArchive(ctx context.Context, pc *pairCtx, pv *preview.Preview, name string) error {
	sh := pc.pair.Shadow
	t, err := e.archiveTargets(ctx, pc, name)
	if err != nil {
		return stepErr(err)
	}
	metaSHA, _, err := pc.git.Resolve(ctx, metaRef(name))
	if err != nil {
		return internalErr(err, "read feature metadata")
	}
	if t.pushArch {
		pv.Add(preview.PushRef(sh, remoteArchiveRef(name), t.arch, t.target))
	}
	if t.hasBranch {
		pv.Add(preview.DeleteRef(sh, "refs/heads/"+name, t.head))
	}
	if t.hasGate {
		pv.Add(preview.DeleteRef(sh, "refs/heads/"+gateBranch(name), t.gate))
	}
	if t.gatePR != nil {
		pv.Add(preview.ClosePR(sh, t.gatePR.Number))
	}
	pv.Add(preview.PushRef(sh, remoteMetaRef(name), metaSHA, ""))
	return nil
}

// publicPROpen reports whether m's public PR is open, with its branch.
func (e *Engine) publicPROpen(ctx context.Context, pc *pairCtx, m feature.Meta) (bool, error) {
	if m.Promoted == nil || m.Promoted.PublicPR == 0 {
		return false, nil
	}
	pr, err := e.Forge.GetPR(ctx, pc.pair.Public, m.Promoted.PublicPR)
	if err != nil {
		return false, forgeErr(err, "read public PR #%d", m.Promoted.PublicPR)
	}
	if pr.Merged || pr.State != prOpen {
		return false, nil
	}
	_, ok, err := pc.git.Resolve(ctx, publicRef(publishedBranch(m)))
	if err != nil {
		return false, internalErr(err, "read public branch "+publishedBranch(m))
	}
	return ok, nil
}

// FeatureArchive hands a published feature off to the public repo (spec
// § 4.3.5): only when its PR is open and nothing is built on it (R8, R13).
// It deletes nothing (the archive keeps the head), so it needs no console.
// An archived feature is exit 0 with changed false.
func (e *Engine) FeatureArchive(ctx context.Context, p workspace.Pair, name string) (res *FeatureArchiveResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		var refs map[string]string
		if res != nil && res.Preview == nil {
			refs = res.Moved
			switch {
			case err != nil:
			case res.Changed:
				summary = "archived " + name + " at " + remoteArchiveRef(name)
			default:
				summary = "already archived; nothing changed"
			}
		}
		e.recordWith("feature archive", &p, name, "", summary, err, func(ent *ledger.Entry) {
			if len(refs) > 0 {
				ent.Refs = refs
			}
		})
	}()
	if err := feature.ValidateName(name, "feature"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature archive", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	m, _, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, warns, err
	}
	res = &FeatureArchiveResult{Feature: name, Archived: m.Archived, Moved: map[string]string{}}
	if m.Promoted != nil {
		res.PublicPR, res.Branch = m.Promoted.PublicPR, publishedBranch(m)
	}
	if m.Archived != nil {
		return res, warns, nil
	}
	if open, err := e.publicPROpen(ctx, pc, m); err != nil {
		return nil, warns, err
	} else if !open {
		return nil, warns, e.noOpenPRFor(ctx, pc, m, name, "feature archive")
	}
	// The same rule as apply's hand-off (R8): one source of truth.
	kids, unreadable, err := e.archiveBlockers(ctx, pc, []string{name})
	switch {
	case err != nil:
		return nil, warns, err
	case len(kids) > 0:
		return nil, warns, hasChildren(name, kids, "archive")
	case len(unreadable) > 0:
		return nil, warns, unreadableChildren(name, "archive", unreadable)
	}
	if e.DryRun {
		res.Preview = preview.New(nil)
		return res, warns, e.previewArchive(ctx, pc, res.Preview, name)
	}
	d, aw, err := e.archiveOne(ctx, pc, name)
	warns = addWarnings(warns, aw...)
	if d != nil {
		res.Moved = d.moved
		if d.gatePR != 0 {
			n := d.gatePR
			res.GatePR = &n
		}
	}
	if err != nil {
		return res, warns, stepErr(err)
	}
	res.Archived, res.Changed = d.archived, true
	return res, warns, nil
}

// archivedErr is FEATURE_ARCHIVED for a feature handed off to the public
// repo, nil otherwise. Its next is the push hint (Task 10).
func archivedErr(name string, m feature.Meta) error {
	if m.Archived == nil {
		return nil
	}
	pr, branch := 0, name
	if m.Promoted != nil {
		pr, branch = m.Promoted.PublicPR, publishedBranch(m)
	}
	return contract.New(contract.FeatureArchived,
		"feature %s was handed off to the public repo: its shadow branch is archived at %s; fix or change PR #%d by pushing to its branch %s, and ghostbranch sync closes the feature when it merges",
		name, m.Archived.Ref, pr, branch).WithField("feature").
		WithDetails(map[string]any{"feature": name, "public_pr": pr, "branch": branch, "archive_ref": m.Archived.Ref})
}

// handOffChoice is the Archive or Keep choice for names (one plan), nil
// when archive is false (Keep without asking, R3). Archive is unavailable,
// with the reason, while something blocks it (R8); Keep is then the default.
func (e *Engine) handOffChoice(ctx context.Context, pc *pairCtx, names []string, archive bool) (*approval.Choice, error) {
	if !archive {
		return nil, nil
	}
	kids, unreadable, err := e.archiveBlockers(ctx, pc, names)
	if err != nil {
		return nil, err
	}
	ch := approval.Choice{Title: "After publishing, this feature:", Options: []approval.Option{
		{Key: "a", Name: "Archive", Text: "the private branch is archived; make fixes or changes to this PR by pushing to it directly"},
		{Key: "k", Name: "Keep", Text: "the private branch stays; make fixes or changes to this PR through gate and promote, as today"}}}
	if len(names) > 1 {
		ch.Title = "After publishing, these features:"
		ch.Options[0].Text = "the private branches are archived; make fixes or changes to these PRs by pushing to them directly"
		ch.Options[1].Text = "the private branches stay; make fixes or changes to these PRs through gate and promote, as today"
	}
	if why := blockedWhy(kids, unreadable, len(names)); why != "" {
		ch.Options[0].Unavailable, ch.Default = why, 1
	}
	return &ch, nil
}

// archives reports whether pick, an answer to ch, hands off.
func archives(ch *approval.Choice, pick int) bool {
	return ch != nil && pick == 0 && ch.Options[0].Unavailable == ""
}

// handOff archives res's features after a successful publish, top first
// (names is bottom first), and stops at the first that fails (P5): that one
// is ARCHIVE_INCOMPLETE (incomplete), and every level below it stays kept
// with its branch untouched, so no live feature is ever left on an
// archived parent (R8). It never undoes the publish. res.Archived stays
// bottom first.
func (e *Engine) handOff(ctx context.Context, pc *pairCtx, res *ApplyResult, names []string) []contract.Warning {
	var warns []contract.Warning
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		d, aw, err := e.archiveOne(ctx, pc, name)
		warns = addWarnings(warns, aw...)
		if d != nil {
			for ref, sha := range d.moved {
				if res.moved == nil {
					res.moved = map[string]string{}
				}
				res.moved[ref] = sha
			}
		}
		if err != nil {
			res.AfterPublish[name] = AfterIncomplete
			return addWarnings(warns, archiveIncomplete(name, err))
		}
		res.AfterPublish[name] = AfterArchived
		res.Archived = append([]string{name}, res.Archived...)
	}
	return warns
}
