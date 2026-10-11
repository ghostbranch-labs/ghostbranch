package engine

// Updating an open PR: the next publish of a feature whose PR is open is
// built on that PR. When the feature only gained commits they are appended
// onto the PR's head H; otherwise the branch is replaced. Commits someone
// made on GitHub stop the update: Ghostbranch never drops them.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
)

// ForeignCommit is a commit on a published feature's public branch that
// Ghostbranch didn't publish (and that isn't GitHub's own restack).
type ForeignCommit struct{ SHA, Author, Subject string }

// prUpdate describes how an open PR would be updated.
type prUpdate struct {
	PR     int
	Branch string
	Base   string // the PR's base branch now
	Head   string // H: the public branch head read from GitHub now
	Style  string // plan.StyleAppend | plan.StyleReplace; "" from prHead
	Reason string // a plan.Reason… constant for a replace; "" for append
	// Foreign lists the commits on the PR that Ghostbranch didn't publish;
	// non-empty means refuse with PUBLIC_BRANCH_CHANGED.
	Foreign []ForeignCommit
	// Restores lists the published commits (base..publishedTip) that H no
	// longer holds, oldest first: someone removed them on GitHub, and an
	// update puts them back.
	Restores []string
	// Public lists the commits on the PR now (base..H, first parent),
	// oldest first.
	Public []string
	// Unrecorded is the plan whose public head H is when an update of this
	// PR landed but its record was never written (plannedHead); "" when
	// none. Its commits are Ghostbranch's, so none is foreign, but nothing
	// may build on the PR until apply of that plan finishes the record.
	Unrecorded string
}

// changed reports whether u's PR holds what an update must not build on:
// commits Ghostbranch didn't publish, or an update whose record is missing.
func (u *prUpdate) changed() bool { return len(u.Foreign) > 0 || u.Unrecorded != "" }

// prHead reads the open PR m's record names: its branch head H and base
// branch as GitHub has them now (the mirror was fetched when the operation
// opened), the commits on it, those Ghostbranch didn't publish, and the
// published ones it lost. Style is left "". It returns nil when m has no
// publish record, its PR isn't open (merged or closed: promote acts as
// before), or its branch is gone. A head that is an unrecorded update's
// (plannedHead) sets Unrecorded, with nothing foreign.
func (e *Engine) prHead(ctx context.Context, pc *pairCtx, m *feature.Meta) (*prUpdate, error) {
	if m.Promoted == nil || m.Promoted.PublicPR == 0 {
		return nil, nil
	}
	g, n := pc.git, m.Promoted.PublicPR
	pr, err := e.Forge.GetPR(ctx, pc.pair.Public, n)
	if err != nil {
		return nil, forgeErr(err, "read public PR #%d", n)
	}
	if pr.State != prOpen || pr.Merged {
		return nil, nil
	}
	branch := publishedBranch(*m)
	H, ok, err := g.Resolve(ctx, publicRef(branch))
	if err != nil {
		return nil, internalErr(err, "read public branch "+branch)
	}
	if !ok {
		return nil, nil // GitHub closes a PR whose branch is deleted
	}
	base, ok, err := g.Resolve(ctx, publicRef(pr.Base))
	if err != nil {
		return nil, internalErr(err, "read public branch "+pr.Base)
	}
	if !ok {
		return nil, contract.New(contract.Internal, "public PR #%d's base branch %s is not in %s", n, pr.Base, pc.pair.Public)
	}
	onPR, err := g.CommitsBetween(ctx, base, H)
	if err != nil {
		return nil, internalErr(err, "list the commits on public PR #"+strconv.Itoa(n))
	}
	foreign, restores, planned, err := e.comparePublished(ctx, pc, *m, base, H)
	if err != nil {
		return nil, err
	}
	return &prUpdate{PR: n, Branch: branch, Base: pr.Base, Head: H, Public: onPR, Foreign: foreign, Restores: restores, Unrecorded: planned}, nil
}

// foreignCommits lists the commits on m's public branch at head, over its
// PR's base commit base (first parent, oldest first), that Ghostbranch
// didn't publish. A commit is Ghostbranch's own only when both its stable
// patch-id and its message match a published commit's: the commits
// base..published, where published is the head Ghostbranch last knew
// (publishedTip). Each published commit vouches for one commit on the
// branch. GitHub's restack keeps both, so it is not foreign. A pushed fix,
// a suggestion, an "Update branch" merge or a commit reworded on GitHub
// is. Without a known published head that is in the mirror, nothing can
// vouch: every commit is foreign, and Ghostbranch never drops what it
// can't tell apart. A head that is the public head of a saved plan of the
// feature (plannedHead) is Ghostbranch's own update whose record was never
// written: nothing on it is foreign, and the plan is returned instead.
func (e *Engine) foreignCommits(ctx context.Context, pc *pairCtx, m feature.Meta, base, head string) ([]ForeignCommit, string, error) {
	foreign, _, planned, err := e.comparePublished(ctx, pc, m, base, head)
	return foreign, planned, err
}

// comparePublished matches the commits on m's public branch at head with
// the published ones, as foreignCommits describes. It returns the foreign
// commits, and the published commits no commit on the branch matched
// (someone removed them on GitHub), both oldest first. When head is the
// public head of a saved plan of m (plannedHead), that plan vouches for
// every commit on the branch: it returns only the plan's id.
func (e *Engine) comparePublished(ctx context.Context, pc *pairCtx, m feature.Meta, base, head string) ([]ForeignCommit, []string, string, error) {
	g := pc.git
	published := e.publishedTip(m)
	if published == head {
		return nil, nil, "", nil
	}
	if id, err := e.plannedHead(ctx, pc, m, head, published); err != nil || id != "" {
		return nil, nil, id, err
	}
	type mark struct{ id, msg string }
	marks := func(commits []string) ([]mark, error) {
		ids, err := g.PatchIDs(ctx, commits)
		if err != nil {
			return nil, err
		}
		out := make([]mark, len(commits))
		for i, c := range commits {
			msg, err := g.CommitMessage(ctx, c)
			if err != nil {
				return nil, err
			}
			out[i] = mark{ids[i], strings.TrimSpace(msg)}
		}
		return out, nil
	}
	// own maps each published commit's mark to the published commits that
	// carry it, oldest first; a match on the branch takes the oldest.
	own := map[mark][]string{}
	var pubCommits []string
	if published != "" {
		if _, ok, err := g.Resolve(ctx, published); err != nil {
			return nil, nil, "", internalErr(err, "read the published head")
		} else if ok {
			if pubCommits, err = g.CommitsBetween(ctx, base, published); err != nil {
				return nil, nil, "", internalErr(err, "list the published commits")
			}
			ms, err := marks(pubCommits)
			if err != nil {
				return nil, nil, "", internalErr(err, "read the published commits")
			}
			for i, mk := range ms {
				own[mk] = append(own[mk], pubCommits[i])
			}
		}
	}
	onPR, err := g.CommitsBetween(ctx, base, head)
	if err != nil {
		return nil, nil, "", internalErr(err, "list the commits on the public branch")
	}
	ms, err := marks(onPR)
	if err != nil {
		return nil, nil, "", internalErr(err, "read the commits on the public branch")
	}
	var foreign []ForeignCommit
	matched := map[string]bool{}
	for i, c := range onPR {
		if left := own[ms[i]]; len(left) > 0 {
			matched[left[0]] = true
			own[ms[i]] = left[1:]
			continue
		}
		author, _, err := g.CommitIdentities(ctx, c)
		if err != nil {
			return nil, nil, "", internalErr(err, "read a commit's author")
		}
		foreign = append(foreign, ForeignCommit{SHA: c, Author: author, Subject: subject(ms[i].msg)})
	}
	var restores []string
	for _, c := range pubCommits {
		if !matched[c] {
			restores = append(restores, c)
		}
	}
	return foreign, restores, "", nil
}

// publishedTip is the public head m's record says Ghostbranch published:
// published_head, or, in a record from before 0.11, the tip of the plan it
// names when that plan file is here; "" when neither is known.
func (e *Engine) publishedTip(m feature.Meta) string {
	if m.Promoted == nil {
		return ""
	}
	if h := m.Promoted.PublishedHead; h != "" {
		return h
	}
	if id := m.Promoted.PlanID; plan.ValidID(id) {
		if pl, _, err := plan.Load(e.WS.PlansDir(), id); err == nil && pl.ID == id && pl.Feature == m.Name {
			return pl.Publish.Commit
		}
	}
	return ""
}

// plannedHead names the saved plan whose public head for m is head: an
// update of m's open PR that apply pushed but whose record was never
// written (apply stopped after the push, the record's push failed, or the
// other machine is asked). Plan refs live in the shadow repo, so every
// machine sees them; the plan file only on the machine that made it. A
// plan qualifies when its ref is at head (a plan of one feature, or the
// top of a stack plan) or holds it (a lower branch of a stack plan), it is
// newer than the plan m's record names, and head isn't already in the
// published history (published, a branch reset on GitHub to an older
// head). When the plan file is here, it must name head as m's own public
// head. Of several, the oldest wins: the one that made head. "" when none.
func (e *Engine) plannedHead(ctx context.Context, pc *pairCtx, m feature.Meta, head, published string) (string, error) {
	if m.Promoted == nil || head == "" {
		return "", nil
	}
	g := pc.git
	if published != "" {
		if _, ok, err := g.Resolve(ctx, published); err != nil {
			return "", internalErr(err, "read the published head")
		} else if ok {
			if in, err := g.IsAncestor(ctx, head, published); err != nil {
				return "", internalErr(err, "compare the public branch with the published head")
			} else if in {
				return "", nil
			}
		}
	}
	out, err := g.Run(ctx, "for-each-ref", "--contains", head, "--format=%(refname)", planRef(""))
	if err != nil {
		return "", internalErr(err, "list the plans holding the public branch's head")
	}
	var ids []string
	for _, ref := range strings.Split(out, "\n") {
		id := strings.TrimPrefix(ref, planRef(""))
		if id == ref || !plan.ValidID(id) || (plan.ValidID(m.Promoted.PlanID) && id <= m.Promoted.PlanID) {
			continue
		}
		ids = append(ids, id)
	}
	slices.Sort(ids) // plan ids start with their UTC time: oldest first
	for _, id := range ids {
		pl, _, err := plan.Load(e.WS.PlansDir(), id)
		if err != nil || pl.ID != id {
			return id, nil // no plan file here: the ref alone says it
		}
		if pl.Feature == m.Name && pl.Publish.Commit == head {
			return id, nil
		}
		for _, en := range pl.Chain {
			if en.Feature == m.Name && en.Publish.Commit == head {
				return id, nil
			}
		}
	}
	return "", nil
}

// openAbove is the line of published features above names' top whose
// public PRs are open, bottom first. When promote updates the top's open
// PR, those PRs update in the same plan. Nothing is above a top that was
// never published. Features above that were never published are left out:
// they publish later, on the new top. A branch with two such children
// can't be one plan: NOT_A_STACK open_above names them.
func (e *Engine) openAbove(ctx context.Context, pc *pairCtx, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	recs, err := e.records(ctx, pc)
	if err != nil {
		return nil, err
	}
	top := names[len(names)-1]
	if r, ok := recs[top]; !ok || r.meta.Promoted == nil {
		return nil, nil
	}
	seen := map[string]bool{}
	for _, n := range names {
		seen[n] = true
	}
	var out []string
	for cur := top; ; {
		open, err := e.openChildren(ctx, pc, recs, cur, seen)
		if err != nil {
			return nil, err
		}
		switch len(open) {
		case 0:
			return out, nil
		case 1:
			seen[open[0]] = true
			out = append(out, open[0])
			cur = open[0]
		default:
			return nil, openAboveErr(cur, open, append(slices.Clone(names), out...))
		}
	}
}

// openChildren lists the published features stacked on cur whose public
// PRs are open, leaving out those in skip.
func (e *Engine) openChildren(ctx context.Context, pc *pairCtx, recs map[string]stackRecord, cur string, skip map[string]bool) ([]string, error) {
	var open []string
	for _, c := range childrenOf(recs, cur, false) {
		cm := recs[c].meta
		if skip[c] || cm.Promoted == nil || cm.Promoted.PublicPR == 0 {
			continue
		}
		pr, err := e.Forge.GetPR(ctx, pc.pair.Public, cm.Promoted.PublicPR)
		if err != nil {
			return nil, forgeErr(err, "read public PR #%d of %s", cm.Promoted.PublicPR, c)
		}
		if !pr.Merged && pr.State == prOpen {
			open = append(open, c)
		}
	}
	return open, nil
}

// foreignDetails is PUBLIC_BRANCH_CHANGED's details.foreign: each commit
// Ghostbranch didn't publish, oldest first, as {sha, author, subject}. The
// errors (update, feature pull) and sync's warning share it.
func foreignDetails(fs []ForeignCommit) []map[string]any {
	out := make([]map[string]any, len(fs))
	for i, f := range fs {
		out[i] = map[string]any{"sha": f.SHA, "author": f.Author, "subject": f.Subject}
	}
	return out
}

// openAboveErr is NOT_A_STACK open_above: cur has open PRs stacked on it
// (open) that plan list can't update together, as one plan updates one line
// of PRs. details.children names them.
func openAboveErr(cur string, open, list []string) *contract.Error {
	ce := notAStack("open_above", cur, list,
		"%s has %d open PRs stacked on it that this plan can't update (%s): one plan updates one line of PRs, and an update of %s would leave the others on its old commits. A person closes or merges on GitHub the PRs of every line but the one to update, then run ghostbranch promote %s --plan again",
		cur, len(open), strings.Join(open, ", "), cur, cur)
	ce.Details["children"] = open
	return ce
}

// hasOpenAbove reports whether any published feature stacked above name
// has an open public PR (openAbove). Two open lines above it count too:
// the gate still builds a replace, and promote refuses the plan later.
func (e *Engine) hasOpenAbove(ctx context.Context, pc *pairCtx, name string) (bool, error) {
	above, err := e.openAbove(ctx, pc, []string{name})
	if ce := contract.From(err); ce != nil && ce.Code == contract.NotAStack && ce.Details["reason"] == "open_above" {
		return true, nil
	}
	return len(above) > 0, err
}

// setStyle decides how u updates m's open PR, which holds no foreign
// commits. Squash and curated always replace. A changed base branch, a
// pending restack or open PRs above (a stack level always replaces), and
// published commits removed on GitHub (Restores: the update puts them
// back) replace. Otherwise it appends when the commits on the PR are, by
// patch-id and message and in order, the first commits of publish, and the
// private history wasn't rewritten since the publish (rewritten, from
// historyRewritten): a clean rebase onto a moved main keeps every patch-id
// and message, yet the PR must move onto the new base, so it is a replace
// (rebase).
//
// u is changed only on success: until then Style stays "", which
// gateUpdateOf never reports or records.
func setStyle(ctx context.Context, g *gitx.Git, u *prUpdate, m *feature.Meta, mode string, above, rewritten bool, publish []string) error {
	reason := forcedReason(u, m, mode, above)
	if reason == "" {
		r, err := prefixReason(ctx, g, u.Public, publish)
		if err != nil {
			return err
		}
		if reason = r; reason == "" && rewritten {
			reason = plan.ReasonRebase
		}
	}
	u.Style, u.Reason = plan.StyleReplace, reason
	if reason == "" {
		u.Style = plan.StyleAppend
	}
	return nil
}

// forcedReason is why u must replace m's open PR's branch whatever the
// new history is (setStyle): squash or curated mode, a changed base
// branch, a pending restack or open PRs above, or published commits
// removed on GitHub. It is "" when the new history decides (prefixReason).
func forcedReason(u *prUpdate, m *feature.Meta, mode string, above bool) string {
	switch {
	case mode == config.ModeSquash:
		return plan.ReasonSquash
	case mode == config.ModeCurated:
		return plan.ReasonCurated
	case u.Base != cmp.Or(m.Promoted.Base, feature.MainParent):
		return plan.ReasonBaseChanged
	case m.Promoted.NeedsUpdate || above:
		return plan.ReasonRestack
	case len(u.Restores) > 0:
		return plan.ReasonRebase
	}
	return ""
}

// prefixReason is "" when public is, by patch-id and message and in order,
// a prefix of publish. Otherwise it says why the history changed: rebase (a
// patch differs, or commits are gone) or reworded (only a message differs).
func prefixReason(ctx context.Context, g *gitx.Git, public, publish []string) (string, error) {
	if len(publish) < len(public) {
		return plan.ReasonRebase, nil
	}
	have, err := g.PatchIDs(ctx, public)
	if err != nil {
		return "", internalErr(err, "compute the PR's patch-ids")
	}
	want, err := g.PatchIDs(ctx, publish[:len(public)])
	if err != nil {
		return "", internalErr(err, "compute the new history's patch-ids")
	}
	if !slices.Equal(have, want) {
		return plan.ReasonRebase, nil
	}
	for i := range public {
		a, err := g.CommitMessage(ctx, public[i])
		if err != nil {
			return "", internalErr(err, "read a commit message")
		}
		b, err := g.CommitMessage(ctx, publish[i])
		if err != nil {
			return "", internalErr(err, "read a commit message")
		}
		if strings.TrimSpace(a) != strings.TrimSpace(b) {
			return plan.ReasonReworded, nil
		}
	}
	return "", nil
}

// historyRewritten reports whether the feature's private history was
// rewritten since its last publish: the head the publish was planned from
// (published, publishedHead) is no longer an ancestor of the feature's head.
// A rebase, an amend or a squash does that; new commits on top, a merge of
// main, or commits feature pull replayed on top don't. A published head
// the mirror doesn't have was rewritten away (the mirror holds every
// ancestor of the feature's head). An unknown published head ("", a record
// from before 0.6 with no plan file here) can't tell: it is not rewritten,
// and the patch-id prefix alone decides.
func historyRewritten(ctx context.Context, g *gitx.Git, published, head string) (bool, error) {
	if published == "" || published == head {
		return false, nil
	}
	if _, ok, err := g.Resolve(ctx, published); err != nil {
		return false, internalErr(err, "read the feature's published head")
	} else if !ok {
		return true, nil
	}
	in, err := g.IsAncestor(ctx, published, head)
	if err != nil {
		return false, internalErr(err, "compare the feature with its published head")
	}
	return !in, nil
}

// readUpdate reads, before anything is built, the open PR an unstacked
// published feature's gate updates (prHead) into b.update. Commits someone
// made on GitHub, or an update Ghostbranch pushed but never recorded, stop
// the gate here, first: PUBLIC_BRANCH_CHANGED.
func (e *Engine) readUpdate(ctx context.Context, pc *pairCtx, b *built, name string) error {
	if b.meta.Promoted == nil || b.parent != "" {
		return nil
	}
	u, err := e.prHead(ctx, pc, &b.meta)
	if err != nil || u == nil {
		return err
	}
	if u.changed() {
		return prChangedErr(name, u)
	}
	b.update = u
	return nil
}

// buildUpdate turns b, built on public main, into the update of the open
// PR readUpdate read (b.update; nothing to do when nil). It writes the
// chain b would publish on public main, as promote writes it but with the
// original authors, and decides append or replace from that chain
// (setStyle). For an append it rebuilds b on the PR's head H: the new
// public commits are cherry-picked onto H, so CI runs on exactly what the
// PR will hold, and new commits that don't apply there make it a replace
// (conflict). A replace keeps b as built. Either way it sets what the
// update scan covers: scanTip, the tip of what the update publishes, and
// scanSources, the feature commits each public commit publishes.
func (e *Engine) buildUpdate(ctx context.Context, pc *pairCtx, b *built, name string) error {
	u := b.update
	if u == nil {
		return nil
	}
	g := pc.git
	// Squash and curated always replace: one commit of the gated tree
	// stands for what they publish.
	chainMode := config.ModeSquash
	if b.scannedAs == config.ModePreserve {
		chainMode = config.ModePreserve
	}
	var chain []plan.Commit
	links, _, err := deriveChain(ctx, g, b.cfg, chainInput{mode: chainMode, pm: b.pm, T: b.T, tEntries: b.entries, commits: b.commits}, nil)
	switch {
	case err == nil:
		if chainMode == config.ModePreserve && b.cfg.Promote.AuthorEmail != config.AuthorNoreply {
			// As a first publish's gate does (scanHistory): an author with
			// no mapping is AUTHOR_UNMAPPED here, not only at promote. A
			// commit adopted from the PR keeps its author: never unmapped.
			if err := keepAdoptedAuthors(ctx, g, links, adoptedOf(b.meta)); err != nil {
				return err
			}
			if _, err := chainAuthors(b.cfg.Promote, config.ModePreserve, links, gitx.Identity{}); err != nil {
				return err
			}
		}
		authors := make([]gitx.Identity, len(links))
		for i, l := range links {
			if authors[i] = l.author; authors[i].Name == "" {
				authors[i] = e.bot()
			}
		}
		if chain, err = writeChain(ctx, g, links, authors, e.bot(), b.pm); err != nil {
			return err
		}
	case contract.From(err).Code != contract.NothingToPublish:
		return namedMarkerErr(err, name)
	}
	above, err := e.hasOpenAbove(ctx, pc, name)
	if err != nil {
		return err
	}
	rewritten, err := historyRewritten(ctx, g, e.publishedHead(b.meta), b.head)
	if err != nil {
		return err
	}
	if err := setStyle(ctx, g, u, &b.meta, b.scannedAs, above, rewritten, commitSHAs(chain)); err != nil {
		return err
	}
	b.scanTip, b.scanSources = b.pm, map[string][]string{}
	if n := len(chain); n > 0 {
		b.scanTip = chain[n-1].Commit
	}
	if chainMode == config.ModePreserve {
		for _, c := range chain {
			b.scanSources[c.Commit] = c.Sources
		}
	}
	if u.Style != plan.StyleAppend {
		e.progressf("public PR #%d of %s: replacing its branch (history changed: %s)", u.PR, name, u.Reason)
		return nil
	}
	added := chain[len(u.Public):]
	e.progressf("public PR #%d of %s: appending %d commit(s) onto its head %s", u.PR, name, len(added), short(u.Head))
	appended, err := e.replay(ctx, pc, commitSHAs(added), u.Head)
	if err != nil {
		var ce *gitx.ConflictError
		if !errors.As(err, &ce) {
			return contract.From(err)
		}
		u.Style, u.Reason = plan.StyleReplace, plan.ReasonConflict
		e.progressf("public PR #%d of %s: the new commits don't apply on its head; replacing its branch instead", u.PR, name)
		return nil
	}
	// The append: what is built, recorded and scanned is H plus the new
	// public commits.
	commits := make([]feature.GateCommit, len(added))
	scanSources := map[string][]string{}
	R := u.Head
	for i, c := range added {
		commits[i] = feature.GateCommit{Source: c.Sources[0], Rebased: appended[i]} // preserve: one source per commit
		scanSources[appended[i]] = c.Sources
		R = appended[i]
	}
	T, entries, err := publishedTree(ctx, g, R, b.cfg)
	if err != nil {
		return namedMarkerErr(err, name)
	}
	infos, err := gateCommitInfos(ctx, g, b.cfg, u.Head, commits)
	if err != nil {
		return err
	}
	b.base, b.R, b.T, b.entries, b.commits, b.infos = u.Head, R, T, entries, commits, infos
	b.scanTip, b.scanSources = R, scanSources
	return nil
}

// scanOpts is what updateScan scans beside each new commit's tree change
// and message.
type scanOpts struct {
	identities bool                // also scan each new commit's author and committer
	sources    map[string][]string // public commit -> the feature commits it publishes, for findings
	texts      []leak.Text         // scanned once beside the commits (branch, PR texts)
}

// updateScan scans what an update of an open PR at head (H) publishes
// that isn't public yet. That is every commit in tip that no public branch
// has, oldest first. For each one it scans the entries it changes from its
// parent, except those whose content H already holds at that path, plus
// its message (and, with identities, its author and committer). In append
// mode that is exactly the new commits. In replace mode it is the net
// change since H, commit by commit, so no intermediate tree goes public
// unscanned. Nothing already public is scanned again, nothing new is
// skipped. It returns the report and the scanner version.
func (e *Engine) updateScan(ctx context.Context, pc *pairCtx, cfg config.Config, head, tip string, o scanOpts) (leak.Report, string, error) {
	g := pc.git
	sc, err := e.NewScanner(rules(cfg))
	if err != nil {
		return leak.Report{}, "", internalErr(err, "start the leak scanner")
	}
	public, err := blobsByPath(ctx, g, head)
	if err != nil {
		return leak.Report{}, "", err
	}
	commits, err := unpublished(ctx, g, tip)
	if err != nil {
		return leak.Report{}, "", internalErr(err, "list the commits going public")
	}
	rep := leak.Report{Findings: []leak.Finding{}, Warnings: []contract.Warning{}}
	byID := map[string]int{}
	add := func(fs []leak.Finding, sources []string) {
		for _, f := range fs {
			i, seen := byID[f.ID]
			if !seen {
				f.Commits = nil
				i = len(rep.Findings)
				byID[f.ID] = i
				rep.Findings = append(rep.Findings, f)
			}
			for _, s := range sources {
				if !slices.Contains(rep.Findings[i].Commits, s) {
					rep.Findings[i].Commits = append(rep.Findings[i].Commits, s)
				}
			}
		}
	}
	for _, c := range commits {
		prev := map[string]string{}
		if parent, ok, err := g.Resolve(ctx, c+"^"); err != nil {
			return leak.Report{}, "", internalErr(err, "read a public commit's parent")
		} else if ok {
			if prev, err = blobsByPath(ctx, g, parent); err != nil {
				return leak.Report{}, "", err
			}
		}
		es, err := g.ListTree(ctx, c)
		if err != nil {
			return leak.Report{}, "", internalErr(err, "list a public commit's tree")
		}
		var changed []gitx.TreeEntry
		for _, en := range es {
			if prev[en.Path] != en.SHA && public[en.Path] != en.SHA {
				changed = append(changed, en)
			}
		}
		msg, err := g.CommitMessage(ctx, c)
		if err != nil {
			return leak.Report{}, "", internalErr(err, "read a public commit's message")
		}
		texts := []leak.Text{{Where: "commit_message", Content: msg}}
		if o.identities {
			author, committer, err := g.CommitIdentities(ctx, c)
			if err != nil {
				return leak.Report{}, "", internalErr(err, "read a public commit's author and committer")
			}
			texts = append(texts, leak.Text{Where: "author", Content: author}, leak.Text{Where: "committer", Content: committer})
		}
		r, err := scanWith(ctx, g, sc, changed, public, texts)
		if err != nil {
			return leak.Report{}, "", err
		}
		add(r.Findings, o.sources[c])
		rep.Warnings = mergeWarnings(rep.Warnings, r.Warnings)
	}
	if len(o.texts) > 0 {
		r, err := scanWith(ctx, g, sc, nil, public, o.texts)
		if err != nil {
			return leak.Report{}, "", err
		}
		add(r.Findings, nil)
		rep.Warnings = mergeWarnings(rep.Warnings, r.Warnings)
	}
	return rep, sc.Version(), nil
}

// unpublished lists the commits in tip that no public branch has, parents
// before children.
func unpublished(ctx context.Context, g *gitx.Git, tip string) ([]string, error) {
	out, err := g.Run(ctx, "rev-list", "--reverse", "--topo-order", tip, "--not", "--remotes=public")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return []string{}, nil
	}
	return strings.Split(out, "\n"), nil
}

// gateFitsUpdate reports whether gate record gt was built for update u (as
// prHead reads it now; nil when no PR is open) of m, for promote mode, with
// above telling whether open PRs sit above m now. Either both are nil, or
// the record names the same PR, head and base, with the style and reason
// they give now: a reason forced now (forcedReason) must be the recorded
// one, and with none forced the recorded reason must be one the new
// history alone gives (append, rebase, reworded or conflict), which the
// gate decided on the same feature head and public main. So a gate built
// to append never stands for an update that must replace (CI never ran on
// that branch), nor one scanned for another mode.
func gateFitsUpdate(gt *feature.Gate, u *prUpdate, m feature.Meta, mode string, above bool) bool {
	if gt == nil {
		return false
	}
	if u == nil {
		return gt.Update == nil
	}
	w := gt.Update
	if w == nil || w.PR != u.PR || w.Head != u.Head || w.Base != u.Base || m.Promoted == nil {
		return false
	}
	if forced := forcedReason(u, &m, mode, above); forced != "" {
		return w.Style == plan.StyleReplace && w.Reason == forced
	}
	switch {
	case w.Style == plan.StyleAppend:
		return w.Reason == ""
	case w.Style == plan.StyleReplace:
		return w.Reason == plan.ReasonRebase || w.Reason == plan.ReasonReworded || w.Reason == plan.ReasonConflict
	}
	return false
}

// updateFits reads m's open PR now and reports whether gate record gt still
// stands for its update in mode (gateFitsUpdate), with that update (nil
// when no PR is open). Foreign commits on the PR, or an update of it whose
// record is missing (Unrecorded), make it not fit: the gate would refuse
// them.
func (e *Engine) updateFits(ctx context.Context, pc *pairCtx, gt *feature.Gate, m feature.Meta, mode string) (bool, *prUpdate, error) {
	u, err := e.prHead(ctx, pc, &m)
	if err != nil || (u != nil && u.changed()) {
		return false, u, err
	}
	above := false
	if u != nil {
		if above, err = e.hasOpenAbove(ctx, pc, m.Name); err != nil {
			return false, u, err
		}
	}
	return gateFitsUpdate(gt, u, m, mode, above), u, nil
}

// gateUpdateOf is u as a gate records and reports it. It is nil for nil,
// and for an update whose style isn't decided yet (Style ""): a gate that
// stopped between reading the PR and choosing append or replace (a
// conflict onto main, a broken marker, an unmapped author) built no update.
func gateUpdateOf(u *prUpdate) *feature.GateUpdate {
	if u == nil || u.Style == "" {
		return nil
	}
	return &feature.GateUpdate{PR: u.PR, Head: u.Head, Base: u.Base, Style: u.Style, Reason: u.Reason}
}

// prChangedErr is PUBLIC_BRANCH_CHANGED as an error: the open PR's branch
// holds commits Ghostbranch didn't publish, which an update would drop, or
// (u.Unrecorded) an update Ghostbranch pushed whose record is missing.
func prChangedErr(name string, u *prUpdate) error {
	if u.Unrecorded != "" {
		return unrecordedErr(name, u)
	}
	foreign := foreignDetails(u.Foreign)
	shas := make([]string, len(u.Foreign))
	for i, f := range u.Foreign {
		shas[i] = short(f.SHA)
	}
	return contract.New(contract.PRChangedOnGitHub,
		"public PR #%d's branch %s holds %d commit(s) Ghostbranch didn't publish (%s); an update would drop them, so nothing was built. Bring them into %s with ghostbranch feature pull %s, then gate and promote again",
		u.PR, u.Branch, len(u.Foreign), strings.Join(shas, ", "), name, name).WithField("feature").
		WithDetails(map[string]any{"feature": name, "branch": u.Branch, "public_pr": u.PR, "public_head": u.Head, "foreign": foreign})
}

// unrecordedErr is PUBLIC_BRANCH_CHANGED reason unrecorded_update: the open
// PR's branch is at the public head of plan u.Unrecorded, whose apply
// pushed it but never wrote the record. Nothing is foreign; apply of that
// plan finds the push landed and writes the record.
func unrecordedErr(name string, u *prUpdate) error {
	id := u.Unrecorded
	return contract.New(contract.PRChangedOnGitHub,
		"public PR #%d's branch %s is at %s, the update plan %s pushed, but the record of that update was never written (its apply stopped after the push); nothing was built. Finish it with ghostbranch apply %s, on the machine that made the plan",
		u.PR, u.Branch, short(u.Head), id, id).WithField("feature").
		WithDetails(map[string]any{"feature": name, "branch": u.Branch, "public_pr": u.PR, "public_head": u.Head,
			"foreign": foreignDetails(nil), "reason": "unrecorded_update", "plan": id})
}

// unrecordedWarning is PUBLIC_BRANCH_CHANGED as status's and sync's
// warning for an update that landed unrecorded (plannedHead): the PR's
// branch at tip is plan id's push, nothing on it is foreign, and apply of
// that plan finishes the record.
func unrecordedWarning(name, branch string, pr int, tip, id string) contract.Warning {
	return contract.Warning{Code: contract.PublicBranchChanged,
		Message: fmt.Sprintf("public branch %s of feature %s is at %s, the update plan %s pushed, but that update was never recorded; finish it with ghostbranch apply %s",
			branch, name, short(tip), id, id),
		Details: map[string]any{"feature": name, "branch": branch, "public_head": tip, "public_pr": pr,
			"foreign": foreignDetails(nil), "reason": "unrecorded_update", "plan": id}}
}

// missingAdopted lists the commits feature pull adopted (adopted, from
// Promoted.Adopted) that chain, the public commits an update leaves on the
// PR, doesn't carry. As R6 tells Ghostbranch's own commits, a commit is
// carried when one commit of chain has both its stable patch-id and its
// message, each commit of chain carrying one. An adopted commit is a public
// commit: the mirror fetched it from the PR's branch, and keeps the object
// after a replace. One the mirror doesn't have (another machine whose
// mirror never fetched it) can't be matched by its patch-id, so it counts
// as missing: a preserve update refuses rather than guess. A merge commit
// (GitHub's Update branch) is never missing: its content comes in with the
// branch it merged, and only its object leaves the PR. Neither is a folded
// one (Folded): an earlier squash or curated update already holds it.
func missingAdopted(ctx context.Context, g *gitx.Git, adopted []feature.AdoptedCommit, chain []string) ([]feature.AdoptedCommit, error) {
	carriers, err := adoptedCarriers(ctx, g, adopted, chain)
	if err != nil {
		return nil, err
	}
	var out []feature.AdoptedCommit
	for i, c := range carriers {
		if c == "" {
			out = append(out, adopted[i])
		}
	}
	return out, nil
}

// adoptedCarriers is, for each adopted commit, the commit of chain that
// carries it (missingAdopted's match), "" when none does, and the adopted
// commit's own SHA when it needs no carrier: a folded one, or a merge.
func adoptedCarriers(ctx context.Context, g *gitx.Git, adopted []feature.AdoptedCommit, chain []string) ([]string, error) {
	out := make([]string, len(adopted))
	if len(adopted) == 0 {
		return out, nil
	}
	type mark struct{ id, msg string }
	ids, err := g.PatchIDs(ctx, chain)
	if err != nil {
		return nil, internalErr(err, "compute the update's patch-ids")
	}
	marks := map[mark][]string{}
	for i, c := range chain {
		msg, err := g.CommitMessage(ctx, c)
		if err != nil {
			return nil, internalErr(err, "read a commit message")
		}
		mk := mark{ids[i], strings.TrimSpace(msg)}
		marks[mk] = append(marks[mk], c)
	}
	for i, a := range adopted {
		if a.Folded {
			out[i] = a.SHA
			continue
		}
		if _, ok, err := g.Resolve(ctx, a.SHA); err != nil {
			return nil, internalErr(err, "read adopted commit "+short(a.SHA))
		} else if !ok {
			continue
		}
		parents, err := g.Run(ctx, "rev-list", "--parents", "-n", "1", a.SHA)
		if err != nil {
			return nil, internalErr(err, "read adopted commit "+short(a.SHA))
		}
		if len(strings.Fields(parents)) > 2 {
			out[i] = a.SHA
			continue
		}
		id, err := g.PatchIDs(ctx, []string{a.SHA})
		if err != nil {
			return nil, internalErr(err, "compute adopted commit "+short(a.SHA)+"'s patch-id")
		}
		msg, err := g.CommitMessage(ctx, a.SHA)
		if err != nil {
			return nil, internalErr(err, "read adopted commit "+short(a.SHA))
		}
		if mk := (mark{id[0], strings.TrimSpace(msg)}); len(marks[mk]) > 0 {
			out[i], marks[mk] = marks[mk][0], marks[mk][1:]
		}
	}
	return out, nil
}

// adoptedForeign is adopted commits as foreignDetails takes them.
func adoptedForeign(as []feature.AdoptedCommit) []ForeignCommit {
	out := make([]ForeignCommit, len(as))
	for i, a := range as {
		out[i] = ForeignCommit{SHA: a.SHA, Author: a.Author, Subject: a.Subject}
	}
	return out
}

// adoptedMissingErr is PUBLIC_BRANCH_CHANGED reason adopted_missing: a
// preserve update would replace PR pr's branch with a history that lacks
// commits feature pull adopted from GitHub (missing), as the feature no
// longer has them. Ghostbranch never drops a commit made on GitHub, so the
// person brings them back first.
func adoptedMissingErr(name, branch string, pr int, head string, missing []feature.AdoptedCommit) error {
	shas := make([]string, len(missing))
	for i, a := range missing {
		shas[i] = short(a.SHA)
	}
	return contract.New(contract.PRChangedOnGitHub,
		"feature %s no longer has %d commit(s) made on GitHub that feature pull brought in (%s), and this update replaces public PR #%d's branch %s, so it would drop them; nothing was published. Bring them back into %s (cherry-pick them onto it and push it to the shadow repo), then gate and promote again",
		name, len(missing), strings.Join(shas, ", "), pr, branch, name).WithField("feature").
		WithDetails(map[string]any{"feature": name, "branch": branch, "public_pr": pr, "public_head": head,
			"foreign": foreignDetails(adoptedForeign(missing)), "reason": "adopted_missing"})
}

// adoptedIdentity is adopted commit a's author as its public commit has it:
// read from the commit when the mirror has it (ok), else parsed from the
// record's "Name <email>".
func adoptedIdentity(ctx context.Context, g *gitx.Git, a feature.AdoptedCommit) (gitx.Identity, bool, error) {
	if _, ok, err := g.Resolve(ctx, a.SHA); err != nil {
		return gitx.Identity{}, false, internalErr(err, "read adopted commit "+short(a.SHA))
	} else if ok {
		id, err := g.CommitAuthor(ctx, a.SHA)
		if err != nil {
			return gitx.Identity{}, false, internalErr(err, "read adopted commit "+short(a.SHA)+"'s author")
		}
		return id, true, nil
	}
	return recordedIdentity(a), false, nil
}

// recordedIdentity is the public author the record keeps for adopted
// commit a, "Name <email>" as read from the PR when feature pull adopted it.
func recordedIdentity(a feature.AdoptedCommit) gitx.Identity {
	name, email, _ := strings.Cut(a.Author, " <")
	return gitx.Identity{Name: strings.TrimSpace(name), Email: strings.TrimSuffix(strings.TrimSpace(email), ">")}
}

// keepAdoptedAuthors marks each preserve link that publishes a commit
// feature pull adopted from the public PR (adopted), matched as R6 tells
// Ghostbranch's own commits: the stable patch-id and the message of the
// link's one source commit against the adopted commit's, each adopted
// commit marking one link. A marked link keeps the adopted commit's public
// author (link.adopted): its email is already public, and author_email
// (noreply, map) rewrites only the person's own commits. An adopted commit
// the mirror doesn't have can't be matched by patch-id: a link matches it
// only when its subject is the recorded subject exactly and its source's
// author is the recorded public author (feature pull's cherry-picks keep
// it), and then keeps that recorded author. A commit of the person's own
// with the same subject never matches, so author_email still applies to
// it and no private email goes public. Each adopted commit marks one link;
// folded ones count too, as the feature still holds their commits.
func keepAdoptedAuthors(ctx context.Context, g *gitx.Git, links []link, adopted []feature.AdoptedCommit) error {
	if len(adopted) == 0 {
		return nil
	}
	type mark struct{ id, msg string }
	byMark := map[mark][]gitx.Identity{}
	var unread []feature.AdoptedCommit // not in the mirror: matched by subject and author
	for _, a := range adopted {
		id, ok, err := adoptedIdentity(ctx, g, a)
		if err != nil {
			return err
		}
		if !ok {
			unread = append(unread, a)
			continue
		}
		pid, err := g.PatchIDs(ctx, []string{a.SHA})
		if err != nil {
			return internalErr(err, "compute adopted commit "+short(a.SHA)+"'s patch-id")
		}
		msg, err := g.CommitMessage(ctx, a.SHA)
		if err != nil {
			return internalErr(err, "read adopted commit "+short(a.SHA))
		}
		mk := mark{pid[0], strings.TrimSpace(msg)}
		byMark[mk] = append(byMark[mk], id)
	}
	for i := range links {
		l := &links[i]
		if len(l.sources) != 1 {
			continue
		}
		pid, err := g.PatchIDs(ctx, l.sources)
		if err != nil {
			return internalErr(err, "compute a commit's patch-id")
		}
		mk := mark{pid[0], strings.TrimSpace(l.message)}
		if ids := byMark[mk]; len(ids) > 0 {
			l.adopted, byMark[mk] = &ids[0], ids[1:]
			continue
		}
		for j, a := range unread {
			rec := recordedIdentity(a)
			if a.Subject == subject(l.message) && rec.Email != "" &&
				strings.EqualFold(rec.Email, l.author.Email) && rec.Name == l.author.Name {
				l.adopted = &rec
				unread = slices.Delete(unread, j, j+1)
				break
			}
		}
	}
	return nil
}

// adoptedOf is m's adopted commits; none for a record without a publish.
func adoptedOf(m feature.Meta) []feature.AdoptedCommit {
	if m.Promoted == nil {
		return nil
	}
	return m.Promoted.Adopted
}

// withCoAuthors ends a squash update's message with one Co-authored-by
// trailer for each distinct author (by email, any case) of the adopted
// commits it folds, those folded by earlier updates (Folded) included, first
// seen first, after a blank line. The publisher, who authors the squash
// commit, is left out.
func withCoAuthors(ctx context.Context, g *gitx.Git, msg string, adopted []feature.AdoptedCommit, publisher gitx.Identity) (string, error) {
	seen := map[string]bool{strings.ToLower(publisher.Email): true}
	var lines []string
	for _, a := range adopted {
		id, _, err := adoptedIdentity(ctx, g, a)
		if err != nil {
			return "", err
		}
		key := strings.ToLower(id.Email)
		if id.Email == "" || seen[key] {
			continue
		}
		seen[key] = true
		lines = append(lines, "Co-authored-by: "+identityText(id))
	}
	if len(lines) == 0 {
		return msg, nil
	}
	return strings.TrimRight(msg, "\n") + "\n\n" + strings.Join(lines, "\n") + "\n", nil
}
