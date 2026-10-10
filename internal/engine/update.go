package engine

// Updating an open PR: the next publish of a feature whose PR is open is
// built on that PR. When the feature only gained commits they are appended
// onto the PR's head H; otherwise the branch is replaced. Commits someone
// made on GitHub stop the update: Ghostbranch never drops them.

import (
	"cmp"
	"context"
	"errors"
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
}

// prHead reads the open PR m's record names: its branch head H and base
// branch as GitHub has them now (the mirror was fetched when the operation
// opened), the commits on it, those Ghostbranch didn't publish, and the
// published ones it lost. Style is left "". It returns nil when m has no
// publish record, its PR isn't open (merged or closed: promote acts as
// before), or its branch is gone.
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
	foreign, restores, err := e.comparePublished(ctx, pc, *m, base, H)
	if err != nil {
		return nil, err
	}
	return &prUpdate{PR: n, Branch: branch, Base: pr.Base, Head: H, Public: onPR, Foreign: foreign, Restores: restores}, nil
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
// can't tell apart.
func (e *Engine) foreignCommits(ctx context.Context, pc *pairCtx, m feature.Meta, base, head string) ([]ForeignCommit, error) {
	foreign, _, err := e.comparePublished(ctx, pc, m, base, head)
	return foreign, err
}

// comparePublished matches the commits on m's public branch at head with
// the published ones, as foreignCommits describes. It returns the foreign
// commits, and the published commits no commit on the branch matched
// (someone removed them on GitHub), both oldest first.
func (e *Engine) comparePublished(ctx context.Context, pc *pairCtx, m feature.Meta, base, head string) ([]ForeignCommit, []string, error) {
	g := pc.git
	published := e.publishedTip(m)
	if published == head {
		return nil, nil, nil
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
			return nil, nil, internalErr(err, "read the published head")
		} else if ok {
			if pubCommits, err = g.CommitsBetween(ctx, base, published); err != nil {
				return nil, nil, internalErr(err, "list the published commits")
			}
			ms, err := marks(pubCommits)
			if err != nil {
				return nil, nil, internalErr(err, "read the published commits")
			}
			for i, mk := range ms {
				own[mk] = append(own[mk], pubCommits[i])
			}
		}
	}
	onPR, err := g.CommitsBetween(ctx, base, head)
	if err != nil {
		return nil, nil, internalErr(err, "list the commits on the public branch")
	}
	ms, err := marks(onPR)
	if err != nil {
		return nil, nil, internalErr(err, "read the commits on the public branch")
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
			return nil, nil, internalErr(err, "read a commit's author")
		}
		foreign = append(foreign, ForeignCommit{SHA: c, Author: author, Subject: subject(ms[i].msg)})
	}
	var restores []string
	for _, c := range pubCommits {
		if !matched[c] {
			restores = append(restores, c)
		}
	}
	return foreign, restores, nil
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
		var open []string
		for _, c := range childrenOf(recs, cur, false) {
			cm := recs[c].meta
			if seen[c] || cm.Promoted == nil || cm.Promoted.PublicPR == 0 {
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
		switch len(open) {
		case 0:
			return out, nil
		case 1:
			seen[open[0]] = true
			out = append(out, open[0])
			cur = open[0]
		default:
			list := append(slices.Clone(names), out...)
			ce := notAStack("open_above", cur, list,
				"%s has %d open PRs stacked on it (%s), and one plan updates one line of PRs: promote each line, listed from its bottom (for example ghostbranch promote %s %s)",
				cur, len(open), strings.Join(open, ", "), strings.Join(list, " "), open[0])
			ce.Details["children"] = open
			return nil, ce
		}
	}
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

// openUpdate is prHead plus the update's style for mode, given whether
// open PRs sit above m (above) and publish, the public commits a publish
// would make now in publish order. It returns nil when m has no open PR (a
// first publish), and Style "" when the PR holds foreign commits.
func (e *Engine) openUpdate(ctx context.Context, pc *pairCtx, m *feature.Meta, mode string, above bool, publish []string) (*prUpdate, error) {
	u, err := e.prHead(ctx, pc, m)
	if err != nil || u == nil || len(u.Foreign) > 0 {
		return u, err
	}
	return u, setStyle(ctx, pc.git, u, m, mode, above, publish)
}

// setStyle decides how u updates m's open PR, which holds no foreign
// commits. Squash and curated always replace. A changed base branch, a
// pending restack or open PRs above (a stack level always replaces), and
// published commits removed on GitHub (Restores: the update puts them
// back) replace. Otherwise it appends when the commits on the PR are, by
// patch-id and message and in order, the first commits of publish.
//
// u is changed only on success: until then Style stays "", which
// gateUpdateOf never reports or records.
func setStyle(ctx context.Context, g *gitx.Git, u *prUpdate, m *feature.Meta, mode string, above bool, publish []string) error {
	var reason string
	switch {
	case mode == config.ModeSquash:
		reason = plan.ReasonSquash
	case mode == config.ModeCurated:
		reason = plan.ReasonCurated
	case u.Base != cmp.Or(m.Promoted.Base, feature.MainParent):
		reason = plan.ReasonBaseChanged
	case m.Promoted.NeedsUpdate || above:
		reason = plan.ReasonRestack
	case len(u.Restores) > 0:
		reason = plan.ReasonRebase
	default:
		r, err := prefixReason(ctx, g, u.Public, publish)
		if err != nil {
			return err
		}
		reason = r
	}
	u.Style, u.Reason = plan.StyleReplace, reason
	if reason == "" {
		u.Style = plan.StyleAppend
	}
	return nil
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

// readUpdate reads, before anything is built, the open PR an unstacked
// published feature's gate updates (prHead) into b.update. Commits someone
// made on GitHub stop the gate here, first: PUBLIC_BRANCH_CHANGED.
func (e *Engine) readUpdate(ctx context.Context, pc *pairCtx, b *built, name string) error {
	if b.meta.Promoted == nil || b.parent != "" {
		return nil
	}
	u, err := e.prHead(ctx, pc, &b.meta)
	if err != nil || u == nil {
		return err
	}
	if len(u.Foreign) > 0 {
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
			// no mapping is AUTHOR_UNMAPPED here, not only at promote.
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
	if err := setStyle(ctx, g, u, &b.meta, b.scannedAs, above, commitSHAs(chain)); err != nil {
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
// prHead reads it now; nil when no PR is open) and promote mode. Either
// both are nil, or the record names the same PR, head and base, with a
// style mode still gives. Squash and curated replace for their own reason,
// and a preserve promote may append, so a replace gate scanned for another
// mode doesn't stand for it.
func gateFitsUpdate(gt *feature.Gate, u *prUpdate, mode string) bool {
	if gt == nil {
		return false
	}
	if u == nil {
		return gt.Update == nil
	}
	w := gt.Update
	if w == nil || w.PR != u.PR || w.Head != u.Head || w.Base != u.Base {
		return false
	}
	switch mode {
	case config.ModeSquash:
		return w.Reason == plan.ReasonSquash
	case config.ModeCurated:
		return w.Reason == plan.ReasonCurated
	}
	return w.Reason != plan.ReasonSquash && w.Reason != plan.ReasonCurated
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
// holds commits Ghostbranch didn't publish, which an update would drop.
func prChangedErr(name string, u *prUpdate) error {
	foreign := make([]map[string]any, len(u.Foreign))
	shas := make([]string, len(u.Foreign))
	for i, f := range u.Foreign {
		foreign[i] = map[string]any{"sha": f.SHA, "author": f.Author, "subject": f.Subject}
		shas[i] = short(f.SHA)
	}
	return contract.New(contract.PRChangedOnGitHub,
		"public PR #%d's branch %s holds %d commit(s) Ghostbranch didn't publish (%s); an update would drop them, so nothing was built. Bring them into %s with ghostbranch feature pull %s, then gate and promote again",
		u.PR, u.Branch, len(u.Foreign), strings.Join(shas, ", "), name, name).WithField("feature").
		WithDetails(map[string]any{"feature": name, "branch": u.Branch, "public_pr": u.PR, "public_head": u.Head, "foreign": foreign})
}
