package engine

// Promoting a stack: features a ← b ← c published as one PR each, b's on
// a's public branch, under one plan and one approval, registered as a
// native GitHub stack where GitHub offers one.

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

// stackParent is m's parent: a feature's name, or the placeholder
// feature.MainParent for the base branch.
func stackParent(m feature.Meta) string { return m.ParentName() }

// publishedBranch is the public branch m was published to: the recorded
// one, or, in a record from before 0.11, the feature's name.
func publishedBranch(m feature.Meta) string {
	if m.Promoted != nil && m.Promoted.Branch != "" {
		return m.Promoted.Branch
	}
	return m.Name
}

// stackBranch is one branch of a stack promote or apply, bottom first.
type stackBranch struct {
	name    string
	meta    feature.Meta
	metaSHA string
	head    string
	// base is its PR's base branch: the pair's base branch by its real
	// name, or the public branch below it.
	base string
	// pubParent is the public commit its first public commit sits on:
	// public main, the bottom's published parent's head, or the tip of the
	// branch below in this plan (set as the plan is built or checked).
	pubParent string
	// onto is the rebased commit its gate's chain starts on: public main,
	// or, for a stacked gate, the commit it replayed its own commits onto
	// (its gate's Base).
	onto    string
	T       string           // its gated published tree
	entries []gitx.TreeEntry // T's entries
	guards  workflow.GuardReport
	// parentPR and parentStack are the bottom's public parent's PR and
	// native stack (0 when none).
	parentPR, parentStack int
}

// notAStack is NOT_A_STACK naming the branch that breaks the list.
func notAStack(reason, name string, list []string, format string, args ...any) *contract.Error {
	return contract.New(contract.NotAStack, format, args...).WithField("features").
		WithDetails(map[string]any{"reason": reason, "feature": name, "features": list})
}

// wantStack is the list that would be a stack ending at top: its
// unpublished features from the bottom of its stack up to top. It is nil
// when a record can't be read or the parents loop.
func (e *Engine) wantStack(ctx context.Context, pc *pairCtx, top string) []string {
	out := []string{top}
	seen := map[string]bool{top: true}
	m, _, _, err := e.readMetaAny(ctx, pc, top)
	for err == nil {
		p := stackParent(m)
		if p == feature.MainParent {
			return out
		}
		if seen[p] {
			return nil
		}
		seen[p] = true
		if m, _, _, err = e.readMetaAny(ctx, pc, p); err != nil {
			return nil
		}
		if m.Promoted != nil {
			return out
		}
		out = append([]string{p}, out...)
	}
	return nil
}

// withWant adds details.want to a NOT_A_STACK whose list could be a stack
// in another order or with the missing branches: the stack ending at its
// top-most listed feature.
func (e *Engine) withWant(ctx context.Context, pc *pairCtx, ce *contract.Error, names []string) error {
	switch ce.Details["reason"] {
	case "order", "gap", "parent_not_public":
		// The top-most listed feature is the one with the longest stack below it.
		top, best := "", -1
		for _, n := range names {
			if l := len(e.wantStack(ctx, pc, n)); l > best {
				top, best = n, l
			}
		}
		if want := e.wantStack(ctx, pc, top); want != nil && !slices.Equal(want, names) {
			ce.Details["want"] = want
		}
	}
	return ce
}

// staleGate is NOT_A_STACK stale_gate for branch name.
func staleGate(name string, names []string) *contract.Error {
	return notAStack("stale_gate", name, names,
		"%s's gate is missing, not green, or stale (%s or a branch below it moved since): gate %s, then the branches above it, then promote again",
		name, name, name)
}

// resolveStack checks that names is a stack promote can publish, bottom
// first, and returns its branches with their gates' published trees.
// Anything else is NOT_A_STACK for the lowest branch that breaks it:
//   - duplicate: a feature listed twice;
//   - order: a branch listed before its parent;
//   - gap: a branch whose parent isn't the branch listed before it;
//   - parent_not_public, parent_merged, parent_moved: the bottom's parent
//     isn't main, or a feature published with an open PR, unmoved since
//     (publicParent);
//   - stale_gate: a branch's gate is missing, not green, not fresh, built
//     on another stack than the one there is now (gateStackFresh), or not
//     built on the branch below it: its base tree must be the published
//     tree of the branch below (or of the public parent's gate). Levels
//     link by tree, never by SHA: a parent gated again on the same content
//     has a new rebased tip with the same tree.
//
// ups holds the branches whose open PRs the plan updates. An appending
// bottom branch's gate must have replayed only its new commits onto the PR
// head the plan saw (Gate.Base); it publishes on that head. A replacing
// branch whose gate was built to append is stale_gate.
//
// It reads the mirror and, for a public parent, its PR; it writes nothing.
func (e *Engine) resolveStack(ctx context.Context, pc *pairCtx, cfg config.Config, O, pm string, names []string, ups map[string]levelUpdate) ([]stackBranch, error) {
	g := pc.git
	pos := map[string]int{}
	for i, n := range names {
		if _, dup := pos[n]; dup {
			return nil, notAStack("duplicate", n, names, "%s is listed twice", n)
		}
		pos[n] = i
	}
	// stale is stale_gate for name; with PRs of this stack open, the new
	// plan updates them all (details.updating), which next says.
	stale := func(name string) error {
		ce := staleGate(name, names)
		if len(ups) > 0 {
			ce.Details["updating"] = true
		}
		return ce
	}
	out := make([]stackBranch, len(names))
	for i, n := range names {
		m, sha, err := e.readMeta(ctx, pc, n)
		if err != nil {
			return nil, err
		}
		head, ok, err := g.Resolve(ctx, shadowRef(m.Branch))
		if err != nil {
			return nil, internalErr(err, "read feature branch "+m.Branch)
		}
		if !ok {
			return nil, contract.New(contract.FeatureNotFound, "feature branch %q is gone from %s", m.Branch, pc.pair.Shadow).WithField("feature")
		}
		out[i] = stackBranch{name: n, meta: m, metaSHA: sha, head: head}
	}
	wnames := guardNames(pc)
	for i := range out {
		b := &out[i]
		p := stackParent(b.meta)
		if j, listed := pos[p]; listed && j > i {
			return nil, e.withWant(ctx, pc, notAStack("order", b.name, names,
				"%s is listed before its parent %s: list the stack bottom first", b.name, p), names)
		}
		// belowTree is the published tree b's gate must have built on.
		var belowTree string
		switch {
		case i > 0 && p != names[i-1]:
			ce := notAStack("gap", b.name, names, "%s's parent is %s, not %s: list each branch right after its parent, from the bottom of the stack",
				b.name, p, names[i-1])
			ce.Details["parent"] = p
			return nil, e.withWant(ctx, pc, ce, names)
		case i > 0:
			b.base, belowTree = targetBranch(out[i-1].meta), out[i-1].T
		case p == feature.MainParent:
			b.base, b.pubParent = pc.base, pm
		default:
			t, err := e.publicParent(ctx, pc, b, p, pm, names)
			if err != nil {
				if ce := contract.From(err); ce.Code == contract.NotAStack {
					return nil, e.withWant(ctx, pc, ce, names)
				}
				return nil, err
			}
			belowTree = t
		}
		gt := b.meta.Gate
		if gt == nil {
			return nil, stale(b.name)
		}
		// A stacked gate links to the branch below by tree; a gate on main
		// has no base tree.
		up, updating := ups[b.name]
		switch {
		case updating && up.append && i == 0:
			// An append's gate replayed only the new commits, onto the PR's
			// head the plan saw: the PR gains exactly them, on that head.
			if gt.Base != up.head {
				return nil, stale(b.name)
			}
			b.pubParent = up.head
		case updating && gt.Base == up.head:
			// A gate built to append, for a plan that replaces.
			return nil, stale(b.name)
		case gt.BaseTree != belowTree:
			return nil, stale(b.name)
		}
		stackFresh, err := e.gateStackFresh(ctx, pc, b.name, b.meta)
		if err != nil {
			return nil, err
		}
		b.onto = cmp.Or(gt.Base, pm)
		fresh, T, entries, guards := e.reusableGate(ctx, pc, cfg, b.meta, b.name, b.head, pm, b.onto, O, wnames)
		if !stackFresh || !fresh {
			return nil, stale(b.name)
		}
		starts, err := chainStartsOn(ctx, g, gt.Commits, b.onto)
		if err != nil {
			return nil, err
		}
		if !starts {
			return nil, stale(b.name)
		}
		b.T, b.entries, b.guards = T, entries, guards
	}
	return out, nil
}

// chainStartsOn reports whether a gate's rebased chain runs from onto, one
// parent at a time. reusableGate checks that it ends at the candidate's
// parent. A commit missing from the mirror is not a chain; a git failure
// is INTERNAL.
func chainStartsOn(ctx context.Context, g *gitx.Git, commits []feature.GateCommit, onto string) (bool, error) {
	prev := onto
	for _, c := range commits {
		parent, ok, err := g.Resolve(ctx, c.Rebased+"^")
		if err != nil {
			return false, internalErr(err, "read a gated commit's parent")
		}
		if !ok || parent != prev {
			return false, nil
		}
		prev = c.Rebased
	}
	return true, nil
}

// publicParent bases the bottom branch b on its already-published parent
// p: p's PR must be open, p's branch unmoved since it was published, its
// public branch still at the published head, and its gate the one whose
// tree it published. It returns that tree, which b's gate must have built
// on. A missing ref or a mismatch is parent_moved, and so is public main
// having moved since p was gated (details.public_main_moved); a git
// failure is INTERNAL.
func (e *Engine) publicParent(ctx context.Context, pc *pairCtx, b *stackBranch, p, pm string, names []string) (string, error) {
	g := pc.git
	pmeta, _, _, err := e.readMetaAny(ctx, pc, p)
	if ce := contract.From(err); ce != nil && ce.Code == contract.FeatureNotFound {
		return "", notAStack("parent_not_public", b.name, names, "%s's parent %s is not a feature any more: run ghostbranch feature restack %s --onto %s", b.name, p, b.name, pc.base)
	}
	if err != nil {
		return "", err
	}
	if pmeta.Promoted == nil {
		ce := notAStack("parent_not_public", b.name, names,
			"%s's parent %s isn't public and isn't in the list: promote the stack from its bottom, each branch right after its parent", b.name, p)
		ce.Details["parent"] = p
		return "", ce
	}
	pr, err := e.Forge.GetPR(ctx, pc.pair.Public, pmeta.Promoted.PublicPR)
	if err != nil {
		return "", forgeErr(err, "read public PR #%d of %s", pmeta.Promoted.PublicPR, p)
	}
	switch {
	case pr.Merged:
		return "", notAStack("parent_merged", b.name, names,
			"%s's parent %s merged: run ghostbranch sync, which closes %s and moves %s onto %s; then gate %s again", b.name, p, p, b.name, pc.base, b.name)
	case pr.State != "open":
		return "", notAStack("parent_not_public", b.name, names, "%s's parent %s's public PR #%d is closed", b.name, p, pr.Number)
	}
	moved := notAStack("parent_moved", b.name, names,
		"%s's parent %s changed since it was published (its branch, its public branch or its gate): run ghostbranch sync, then restack and gate what moved",
		b.name, p)
	branch := publishedBranch(pmeta)
	phead, ok, err := g.Resolve(ctx, shadowRef(pmeta.Branch))
	if err != nil {
		return "", internalErr(err, "read feature branch "+pmeta.Branch)
	}
	tip, tipOK, err := g.Resolve(ctx, publicRef(branch))
	if err != nil {
		return "", internalErr(err, "read public branch "+branch)
	}
	if !ok || !tipOK || phead != pmeta.Promoted.FeatureHead || pmeta.Promoted.PublishedHead == "" || tip != pmeta.Promoted.PublishedHead ||
		pmeta.Gate == nil || pmeta.Gate.FeatureHead != pmeta.Promoted.FeatureHead || pmeta.Gate.PublishedTree == "" {
		return "", moved
	}
	tree, err := g.TreeOf(ctx, tip)
	if err != nil {
		return "", internalErr(err, "read public branch "+branch+"'s tree")
	}
	if tree != pmeta.Gate.PublishedTree {
		return "", moved
	}
	// Public main moved since p was gated and published: b's gate can only
	// build on p replayed onto the new main, which is not the tree p's PR
	// published, so gating b again would never link. Only p's merge (and
	// sync moving b onto main) gets b out of this.
	if pmeta.Gate.PublicMain != pm {
		ce := notAStack("parent_moved", b.name, names,
			"public %s moved since %s was published: promote %s first, which updates its PR onto the new %s (ghostbranch promote %s --plan), then gate and promote %s; or wait for %s to merge, run ghostbranch sync (it moves %s onto %s), then gate and promote %s",
			pc.base, p, p, pc.base, p, b.name, p, b.name, pc.base, b.name)
		ce.Details["parent"] = p
		ce.Details["public_main_moved"] = true
		return "", ce
	}
	b.base, b.pubParent = branch, tip
	b.parentPR, b.parentStack = pmeta.Promoted.PublicPR, pmeta.Promoted.Stack
	return tree, nil
}

// stackMessage is a squashed stack branch's commit message, from the first
// of these that applies:
//   - -m or the editor, when the person listed only this feature (own);
//   - its --title-for title;
//   - for an update whose PR holds one published commit, that commit's
//     message as the PR has it (prev);
//   - for a branch of one commit, that commit's message.
func (e *Engine) stackMessage(ctx context.Context, g *gitx.Git, opt PromoteOptions, b *stackBranch, title, prev string, own bool) (string, error) {
	if own {
		return e.message(opt)
	}
	if title != "" {
		return title, nil
	}
	if prev != "" {
		return prev, nil
	}
	if cs := b.meta.Gate.Commits; len(cs) == 1 {
		msg, err := g.CommitMessage(ctx, cs[0].Rebased)
		if err != nil {
			return "", internalErr(err, "read a commit message")
		}
		return strings.TrimSpace(msg), nil
	}
	return "", contract.New(contract.MissingInput, "%s has %d commits, so its squash commit needs a message: pass --title-for %s=<title>",
		b.name, len(b.meta.Gate.Commits), b.name).WithField("title-for").WithDetails(map[string]any{"feature": b.name})
}

// promoteStack is promote for a stack: several features, bottom first, one
// whose parent is a feature, or one with open PRs stacked on it (above: the
// open PRs above the listed top, bottom first, which update in the same
// plan). It never gates: every branch needs a fresh green gate built on the
// branch below (NOT_A_STACK names the first that hasn't one). It builds
// each branch's public commits on the public commit below it, so each PR
// publishes only its own branch's commits, scans each as it will be
// published, saves one plan listing every PR with its base, and applies it
// once approved. A branch whose PR is open is an update of that PR
// (stackUpdates). It returns the plan ID.
func (e *Engine) promoteStack(ctx context.Context, pc *pairCtx, cfg config.Config, O string, opt PromoteOptions, above []string, res *PromoteResult) (string, []contract.Warning, error) {
	g := pc.git
	listed := opt.Features
	if len(listed) == 0 {
		listed = []string{opt.Feature}
	}
	// -m, --title and --body belong to the one feature the person listed:
	// the open PRs above it, added to the plan, never take them.
	alone := len(listed) == 1
	names := append(slices.Clone(listed), above...)
	if opt.Branch != "" {
		return "", nil, contract.New(contract.BadFlag, "--branch: a stacked feature publishes to a branch named after it, so the PR above it can name it as its base").WithField("branch")
	}
	if opt.Detach {
		return "", nil, contract.New(contract.BadFlag, "--detach: a stack promote runs no gate; gate each branch, bottom first, then promote").WithField("detach")
	}
	warns := e.behind(ctx, pc)
	pm, err := g.MustResolve(ctx, publicRef(pc.base))
	if err != nil {
		return "", warns, internalErr(err, "read public "+pc.base)
	}
	modes := make([]string, len(names))
	for i, n := range names {
		modes[i] = cmp.Or(opt.Modes[n], opt.Mode, cfg.Promote.DefaultMode)
	}
	// An open PR on a branch of the plan that the plan doesn't update would
	// be left on that branch's old commits.
	if err := e.openOutside(ctx, pc, names); err != nil {
		return "", warns, err
	}
	updates, err := e.stackUpdates(ctx, pc, names, modes, cfg.Promote.DefaultMode, pm, O)
	if err != nil {
		return "", warns, err
	}
	if err := e.restackedAbove(ctx, pc, names, updates); err != nil {
		return "", warns, err
	}
	branches, err := e.resolveStack(ctx, pc, cfg, O, pm, names, levelUpdates(names, updates))
	if err != nil {
		return "", warns, err
	}
	user, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return "", warns, forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: user.Login, Email: forge.NoreplyEmail(user), When: e.now()}
	set := overlay.NewSet(cfg.LeakScan.Strip...)
	wnames := guardNames(pc)
	entries := make([]plan.Entry, len(branches))
	prs := make([]plan.StackPR, len(branches))
	planWarnings := []contract.Warning{}
	var firstGroups []plan.Group
	var needTitle []string
	version := ""
	planStep := e.startStep("Building the publish plan for %s", strings.Join(names, ", "))
	for i := range branches {
		b := &branches[i]
		if i > 0 {
			b.pubParent = entries[i-1].Publish.Commit
		}
		up := updates[i]
		if up != nil && up.Base != b.base {
			ce := notAStack("parent_moved", b.name, names,
				"PR #%d of %s is based on %s on GitHub, but %s now goes on %s: run ghostbranch sync (GitHub retargets a PR when the branch below it merges), or change the PR's base on GitHub, then promote again",
				up.PR, b.name, up.Base, b.name, b.base)
			ce.Details["parent"] = stackParent(b.meta)
			return "", warns, ce
		}
		mode := modes[i]
		own := alone && i == 0
		if own && mode != config.ModeSquash && strings.TrimSpace(opt.Message) != "" {
			return "", warns, messageNotSquash(mode)
		}
		commits := b.meta.Gate.Commits
		var groups []plan.Group
		if mode == config.ModeCurated {
			// Only a stack of one may be curated (ValidatePromoteOptions).
			if len(names) > 1 {
				return "", warns, contract.New(contract.BadFlag,
					"%s's mode is curated (promote.default_mode): a curated branch is promoted on its own; pass --mode-for %s=squash or preserve", b.name, b.name).WithField("mode-for")
			}
			if err := curateFlags(mode, opt.Curate); err != nil {
				return "", warns, err
			}
			if opt.Groups == nil {
				if opt.Groups, err = LoadCurate(opt.Curate); err != nil {
					return "", warns, err
				}
			}
			if groups, err = ResolveGroups(opt.Groups, sourcesOf(commits)); err != nil {
				return "", warns, err
			}
			firstGroups = groups
		}
		typedTitle := strings.TrimSpace(opt.Titles[b.name])
		typedBody := ""
		if own {
			typedTitle = cmp.Or(typedTitle, strings.TrimSpace(opt.Title))
			typedBody = strings.TrimSpace(opt.Body)
		}
		title, body := typedTitle, typedBody
		var msg string
		if mode == config.ModeSquash {
			prev := ""
			if up != nil && b.meta.Promoted != nil && len(b.meta.Promoted.PatchIDs) == 1 {
				m, err := g.CommitMessage(ctx, up.Head)
				if err != nil {
					return "", warns, internalErr(err, "read the message of "+b.name+"'s published commit")
				}
				prev = strings.TrimSpace(m)
			}
			if msg, err = e.stackMessage(ctx, g, opt, b, typedTitle, prev, own); err != nil {
				if ce := contract.From(err); ce.Code != contract.MissingInput || ce.Field != "title-for" {
					return "", warns, err
				}
				// A stand-in, so the branches above can still be built and
				// every missing title named at once; nothing is saved.
				needTitle, msg = append(needTitle, b.name), "(title needed)"
			}
		}
		parentTree, err := g.TreeOf(ctx, b.pubParent)
		if err != nil {
			return "", warns, internalErr(err, "read the public commit below "+b.name)
		}
		links, dropped, err := deriveChain(ctx, g, cfg, chainInput{mode: mode, pm: pm, baseTree: parentTree, onto: b.onto,
			T: b.T, tEntries: b.entries, commits: commits, message: msg, groups: groups},
			func(cs []string, onto string) ([]string, error) { return e.replay(ctx, pc, cs, onto) })
		if err != nil {
			if ce := contract.From(err); ce.Code == contract.MarkerInvalid {
				err = namedMarkerErr(err, b.name)
				contract.From(err).Details["mode"] = mode
			}
			return "", warns, err
		}
		if up != nil {
			// Commits adopted from the PR keep their authors: as they are in
			// preserve, as Co-authored-by trailers on a squash commit.
			switch mode {
			case config.ModePreserve:
				if err := keepAdoptedAuthors(ctx, g, links, adoptedOf(b.meta)); err != nil {
					return "", warns, err
				}
			case config.ModeSquash:
				if len(links) == 1 {
					if links[0].message, err = withCoAuthors(ctx, g, links[0].message, adoptedOf(b.meta), who); err != nil {
						return "", warns, err
					}
				}
			}
		}
		authors, err := chainAuthors(cfg.Promote, mode, links, who)
		if err != nil {
			return "", warns, err
		}
		chain, err := writeChain(ctx, g, links, authors, who, b.pubParent)
		if err != nil {
			return "", warns, err
		}
		if err := assertChain(ctx, g, chain, b.T, b.meta.Gate.Candidate, b.pubParent, set, wnames); err != nil {
			return "", warns, err
		}
		// A preserve replace keeps every commit feature pull adopted from
		// GitHub, or stops: the approval summary lists those squash folds.
		if up != nil && up.Style == plan.StyleReplace && mode == config.ModePreserve {
			missing, err := missingAdopted(ctx, g, b.meta.Promoted.Adopted, commitSHAs(chain))
			if err != nil {
				return "", warns, err
			}
			if len(missing) > 0 {
				return "", warns, adoptedMissingErr(b.name, up.Branch, up.PR, up.Head, missing)
			}
		}
		switch {
		case mode == config.ModeSquash:
			title, body = prTexts(msg, title, body)
		case len(links) == 1:
			title, body = prTexts(links[0].message, title, body)
		case body == "":
			lines := make([]string, len(chain))
			for j, c := range chain {
				lines[j] = "- " + c.Subject
			}
			body = strings.Join(lines, "\n")
		}
		if title == "" && up != nil {
			// An update keeps its PR's title on GitHub. The plan records one
			// for the scan and the summary.
			title = chain[0].Subject
		}
		if title == "" {
			if !slices.Contains(needTitle, b.name) {
				needTitle = append(needTitle, b.name)
			}
			title = "(title needed)"
		}
		steps := make([]scanStep, len(links))
		for j, l := range links {
			var sources []string
			if mode != config.ModeSquash {
				sources = l.sources
			}
			steps[j] = scanStep{sources: sources, entries: l.entries, texts: []leak.Text{
				{Where: "commit_message", Content: l.message},
				{Where: "author", Content: identityText(authors[j])},
				{Where: "committer", Content: identityText(who)}}}
		}
		// A PR on public main scans its first tree whole, as a promote of one
		// feature does; a PR on a public branch below it scans what it
		// changes from that branch, which the PR below published and scanned.
		texts := []leak.Text{{Where: "branch", Content: targetBranch(b.meta)}, {Where: "pr_title", Content: title}, {Where: "pr_body", Content: body}}
		var rep leak.Report
		var v string
		if up != nil {
			// An update scans what isn't public yet, from the PR's head as it
			// is now (updateScan): each new commit's change and message, its
			// author and committer, nothing public twice.
			rep, v, err = e.updateScan(ctx, pc, cfg, up.Head, chain[len(chain)-1].Commit,
				scanOpts{identities: true, sources: commitSources(chain, mode), texts: texts})
		} else {
			rep, v, err = e.scanChainFrom(ctx, g, cfg, b.pubParent, b.pubParent == pm, steps, texts)
		}
		if err != nil {
			return "", warns, err
		}
		version = v
		blocking, waived, ww := e.splitFindings(pc.pair, rep.Findings)
		warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
		res.Waived = append(res.Waived, waived...)
		if len(blocking) > 0 {
			res.Findings = blocking
			ce := contract.From(leakErr(b.name, blocking))
			ce.Details["mode"] = mode
			return "", warns, ce
		}
		planWarnings = mergeWarnings(planWarnings, rep.Warnings)
		pub := plan.Publish{Commit: chain[len(chain)-1].Commit, Tree: b.T, Parent: b.pubParent, TargetBranch: targetBranch(b.meta),
			PR: plan.PR{Title: title, Body: body}, Commits: chain, DroppedEmpty: dropped}
		spr := plan.StackPR{Feature: b.name, Base: b.base}
		if up != nil {
			pub.Update, pub.Style, pub.ReplaceReason, pub.ExpectedHead = true, up.Style, up.Reason, up.Head
			if up.Style == plan.StyleReplace {
				pub.Restores = up.Restores // the published commits GitHub lost, which this replace puts back
			}
			spr.Number, spr.Update, spr.Style = up.PR, true, up.Style
		}
		entries[i] = plan.Entry{Feature: b.name, Mode: mode,
			Fingerprint: plan.Fingerprint{PublicMain: pm, FeatureHead: b.head, Overlay: O, GateCandidate: b.meta.Gate.Candidate},
			Publish:     pub, Typed: plan.Typed{Title: typedTitle != "", Body: typedBody != ""}, CI: plan.CI{RepoGuards: b.guards}}
		prs[i] = spr
	}
	if len(needTitle) > 0 {
		field := "title-for"
		if alone && len(needTitle) == 1 && needTitle[0] == names[0] {
			field = "title"
		}
		flags := make([]string, len(needTitle))
		for i, n := range needTitle {
			flags[i] = "--title-for " + n + "=<title>"
			if field == "title" {
				flags[i] = "--title <title>"
			}
		}
		d := map[string]any{"feature": needTitle[0], "features": needTitle}
		if len(names) > len(listed) {
			d["stack"] = names
		}
		return "", warns, contract.New(contract.MissingInput, "the PR(s) of %s need a title (each has more than one commit and no one message names it): pass %s",
			strings.Join(needTitle, ", "), strings.Join(flags, " ")).WithField(field).WithDetails(d)
	}

	planID, err := plan.NewID(e.now(), e.Rand)
	if err != nil {
		return "", warns, internalErr(err, "make a plan id")
	}
	first := entries[0]
	planStep.Done("")
	pl := plan.Plan{SchemaVersion: plan.SchemaVersion, ID: planID, Kind: "promote", Actor: e.Actor, CreatedAt: e.now(),
		Pair: plan.Pair{Public: pc.pair.Public, Shadow: pc.pair.Shadow}, Feature: first.Feature, Mode: first.Mode, Curate: firstGroups,
		Fingerprint: first.Fingerprint, Publish: first.Publish, Typed: first.Typed,
		Leak: plan.Leak{Findings: []map[string]any{}, Waived: planWaived(res.Waived), Warnings: planWarnings, ScannerVersion: version},
		CI:   first.CI, Chain: entries, PRs: prs}
	res.Plan, res.PRs = &pl, prs
	top := entries[len(entries)-1].Publish.Commit
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm})
		pv.Add(preview.PushRef(pc.pair.Shadow, remotePlanRef(planID), "", top),
			preview.WriteFile(filepath.Join(e.WS.PlansDir(), planID+".json")))
		if !opt.PlanOnly {
			for i, en := range entries {
				if err := e.previewStackPublish(ctx, pc, pv, en, branches[i].meta, prs[i].Base, prs[i].Number); err != nil {
					return planID, warns, err
				}
			}
		}
		res.Preview = pv
		return planID, warns, nil
	}
	if res.PlanPath, err = plan.Save(e.WS.PlansDir(), pl); err != nil {
		return planID, warns, internalErr(err, "save the plan")
	}
	// The plan ref holds the top commit, and through its parents every
	// commit of the stack.
	if err := e.push(ctx, pc, remoteShadow, gitx.PushOptions{Leases: []gitx.Lease{{Ref: remotePlanRef(planID)}}}, top+":"+remotePlanRef(planID)); err != nil {
		_ = os.Remove(res.PlanPath) // a plan whose commits never reached the shadow repo can't be applied
		res.PlanPath = ""
		return planID, warns, err
	}
	warns = addWarnings(warns, e.updateMirror(ctx, g, planRef(planID), top)...)
	if opt.PlanOnly {
		return planID, warns, nil
	}
	ar, aw, err := e.apply(ctx, pc, pl, applyOpts{yes: opt.Yes, archive: opt.Archive})
	res.Applied = ar
	if ar != nil {
		res.PRs, res.StackNumber = ar.PRs, ar.StackNumber
	}
	return planID, addWarnings(warns, aw...), err
}

// entryPlan is stack plan pl's branch i seen as a plan of one feature, for
// the checks apply runs on one feature.
func entryPlan(pl plan.Plan, i int) plan.Plan {
	en := pl.Chain[i]
	out := pl
	out.Feature, out.Mode, out.Fingerprint, out.Publish, out.Typed, out.CI = en.Feature, en.Mode, en.Fingerprint, en.Publish, en.Typed, en.CI
	if i > 0 {
		out.Curate = nil
	}
	out.Chain = nil
	return out
}

// stackStale is PLAN_STALE for a stack plan: the details of its branch i,
// plus the stack's features, so a re-plan names the whole stack.
func stackStale(pl plan.Plan, i int, moved []string, format string, args ...any) error {
	d := stale(entryPlan(pl, i), moved)
	d["features"] = chainNames(pl)
	return contract.New(contract.PlanStale, format, args...).WithDetails(d)
}

// chainNames are a stack plan's features, bottom first.
func chainNames(pl plan.Plan) []string {
	names := make([]string, len(pl.Chain))
	for j, en := range pl.Chain {
		names[j] = en.Feature
	}
	return names
}

// applyStack publishes a stack plan, bottom first. For every branch it
// re-checks what apply checks for one feature, on the shadow repo's
// objects: the stack still resolves (resolveStack); the fingerprint and the
// gate; the public commits, derived again on the commit below; and a fresh
// scan of what the branch publishes. One approval covers every PR, and its
// summary lists each with its base, commits and files. Then, bottom first,
// it pushes each branch and opens its PR on the branch below, recording the
// PR in that feature's record at once. Last, it registers the native GitHub
// stack (stackUp). Every step is safe to run again: an apply that stopped
// resumes at the first PR it hadn't opened.
func (e *Engine) applyStack(ctx context.Context, pc *pairCtx, pl plan.Plan, ao applyOpts) (res *ApplyResult, warns []contract.Warning, err error) {
	g := pc.git
	n := len(pl.Chain)
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	if len(pl.PRs) != n {
		return nil, warns, stackStale(pl, 0, []string{"prs"}, "plan %s lists %d PRs for %d branches; make a new plan with ghostbranch promote", pl.ID, len(pl.PRs), n)
	}
	names := chainNames(pl)
	for _, en := range pl.Chain {
		if def := pc.public.DefaultBranch; def != "" && strings.EqualFold(en.Publish.TargetBranch, def) {
			return nil, warns, contract.New(contract.NameInvalid, "plan %s would publish to the public default branch %q", pl.ID, def).WithField("plan")
		}
	}
	// Every branch an update whose push already landed: only the records
	// are left to write, whatever moved since (finishLanded). A plan with a
	// first publish, or a branch not yet pushed, is checked in full below.
	allLanded := true
	for i, en := range pl.Chain {
		m, _, err := e.readMeta(ctx, pc, en.Feature)
		if err != nil {
			return nil, warns, err
		}
		ok, err := e.landedUpdate(ctx, pc, pl, en.Publish, m, i)
		if err != nil {
			return nil, warns, err
		}
		if !ok {
			allLanded = false
			break
		}
	}
	if allLanded {
		return e.finishLanded(ctx, pc, cfg, pl, ao.yes, warns) // never archives (R14)
	}
	pm, err := g.MustResolve(ctx, publicRef(pc.base))
	if err != nil {
		return nil, warns, internalErr(err, "read public "+pc.base)
	}
	branches, err := e.resolveStack(ctx, pc, cfg, O, pm, names, planUpdates(pl))
	if err != nil {
		if ce := contract.From(err); ce.Code == contract.NotAStack {
			return nil, warns, stackStale(pl, 0, []string{"stack"}, "the stack of plan %s no longer stands (%s); make a new plan with ghostbranch promote", pl.ID, ce.Message)
		}
		return nil, warns, err
	}
	planTip, _, err := g.Resolve(ctx, planRef(pl.ID))
	if err != nil {
		return nil, warns, internalErr(err, "read plan "+pl.ID+"'s commit")
	}
	res = &ApplyResult{PlanID: pl.ID, Waived: []WaivedFinding{}, PRs: append([]plan.StackPR(nil), pl.PRs...)}
	// The plan file records the PRs opened and the stack, also when the
	// apply stops part-way; the feature records are what later commands read.
	defer func() {
		if e.DryRun || res == nil || !slices.ContainsFunc(res.PRs, func(p plan.StackPR) bool { return p.Number != 0 }) {
			return
		}
		saved := pl
		saved.PRs, saved.StackNumber = res.PRs, res.StackNumber
		if _, serr := plan.Save(e.WS.PlansDir(), saved); serr != nil {
			warns = addWarnings(warns, mirrorNotUpdated("couldn't record the PRs in the local plan file "+pl.ID+".json", serr, map[string]any{"plan_id": pl.ID}))
		}
	}()
	shown := make([][]plan.Commit, n)
	authors := make([][]string, n)
	files := make([][]string, n)
	exists := make([]bool, n)
	// landed: an update whose push already landed (a resumed apply).
	landed := make([]bool, n)
	ids := make([][]string, n)
	var scanWarnings []contract.Warning
	for i, en := range pl.Chain {
		b := &branches[i]
		if i > 0 {
			b.pubParent = pl.Chain[i-1].Publish.Commit
		}
		ep := entryPlan(pl, i)
		if pl.PRs[i].Feature != en.Feature || pl.PRs[i].Base != b.base || en.Publish.TargetBranch != targetBranch(b.meta) {
			return nil, warns, stackStale(pl, i, []string{"prs"}, "plan %s bases %s's PR on %s, but it now goes on %s; make a new plan with ghostbranch promote",
				pl.ID, en.Feature, pl.PRs[i].Base, b.base)
		}
		if en.Publish.Update {
			// The plan file is local and unsigned: an update must name the
			// feature's own PR, and the bottom on main the update its gate built.
			if err := updateLevelStands(pl, i, b); err != nil {
				return nil, warns, stackLevelErr(err, names)
			}
		}
		gateC, _, err := g.Resolve(ctx, shadowRef(gateBranch(en.Feature)))
		if err != nil {
			return nil, warns, internalErr(err, "read the gate branch of "+en.Feature)
		}
		live := plan.Fingerprint{PublicMain: pm, FeatureHead: b.head, Overlay: O, GateCandidate: gateC}
		if moved := en.Fingerprint.Moved(live); len(moved) > 0 {
			return nil, warns, stackStale(pl, i, moved, "the repos changed since plan %s was made (%s of %s); make a new plan with ghostbranch promote",
				pl.ID, strings.Join(moved, ", "), en.Feature)
		}
		P, links, err := e.verifyEntry(ctx, pc, ep, b.meta, cfg, live, b.onto, b.pubParent, planTip, i < n-1)
		if err != nil {
			if ce := contract.From(err); ce.Code == contract.PlanStale {
				ce.Details["features"] = names
			}
			return nil, warns, err
		}
		steps, au, sh, err := storedSteps(ctx, g, ep, links)
		if err != nil {
			return nil, warns, err
		}
		authors[i], shown[i] = au, sh
		var rep leak.Report
		if en.Publish.Update {
			// An update scans what isn't public yet, from the PR head the plan
			// saw, with the commits as stored.
			rep, _, err = e.updateScan(ctx, pc, cfg, en.Publish.ExpectedHead, P,
				scanOpts{identities: true, sources: commitSources(en.Publish.Commits, en.Mode), texts: planTexts(ep)})
		} else {
			rep, _, err = e.scanChainFrom(ctx, g, cfg, b.pubParent, b.pubParent == pm, steps, planTexts(ep))
		}
		if err != nil {
			return nil, warns, err
		}
		blocking, waived, ww := e.splitFindings(pc.pair, rep.Findings)
		warns = addWarnings(addWarnings(warns, rep.Warnings...), ww...)
		if len(blocking) > 0 {
			ce := contract.From(leakErr(en.Feature, blocking))
			ce.Details["mode"], ce.Details["public"], ce.Details["branch"] = en.Mode, pl.Pair.Public, en.Publish.TargetBranch
			ce.Details["feature_head"] = en.Fingerprint.FeatureHead
			return nil, warns, ce
		}
		res.Waived = append(res.Waived, waived...)
		scanWarnings = mergeWarnings(scanWarnings, rep.Warnings)
		existing, ok, err := g.Resolve(ctx, publicRef(en.Publish.TargetBranch))
		if err != nil {
			return nil, warns, internalErr(err, "read public branch "+en.Publish.TargetBranch)
		}
		filesFrom := b.pubParent
		switch {
		case en.Publish.Update:
			// The lease: the PR's branch must still be where the plan saw it
			// (or already at P: the push landed), and the PR open on its base.
			if landed[i], err = e.updateLive(ctx, pc, levelPlan(pl, i), b.meta, existing, ok, P); err != nil {
				return nil, warns, stackLevelErr(err, names)
			}
			exists[i], filesFrom = landed[i], en.Publish.ExpectedHead
		case ok && existing != P:
			return res, warns, branchExists(en.Publish.TargetBranch, existing, "")
		default:
			exists[i] = ok
		}
		if files[i], err = g.DiffNames(ctx, filesFrom, P); err != nil {
			return nil, warns, internalErr(err, "list the files going public")
		}
		if ids[i], err = g.PatchIDs(ctx, commitSHAs(en.Publish.Commits)); err != nil {
			return nil, warns, internalErr(err, "compute the published commits' patch-ids")
		}
	}
	top := n - 1
	res.PublicBranch = pl.Chain[top].Publish.TargetBranch
	if e.DryRun {
		pv := preview.New(map[string]string{"public_main": pm})
		for i, en := range pl.Chain {
			if err := e.previewStackPublish(ctx, pc, pv, en, branches[i].meta, pl.PRs[i].Base, pl.PRs[i].Number); err != nil {
				return res, warns, err
			}
		}
		ch, err := e.handOffChoice(ctx, pc, names, ao.archive)
		if err != nil {
			return res, warns, err
		}
		if ch != nil && archives(ch, ch.Default) {
			res.handOff = names
			for i := len(names) - 1; i >= 0; i-- { // top first, as handOff runs
				if err := e.previewArchive(ctx, pc, pv, names[i]); err != nil {
					return res, warns, err
				}
			}
		}
		res.Preview = pv
		return res, warns, nil
	}
	// One approval for every PR: a console code, or the pair's grant, which
	// names no features and so covers the whole chain.
	folded := make([][]feature.AdoptedCommit, n)
	for i, en := range pl.Chain {
		if folded[i], err = e.foldedAdopted(ctx, pc, en.Feature, branches[i].meta, en.Mode, en.Publish, pl.PRs[i].Number); err != nil {
			return res, warns, stackLevelErr(err, names)
		}
	}
	ch, err := e.handOffChoice(ctx, pc, names, ao.archive)
	if err != nil {
		return nil, warns, err
	}
	appr, pick, err := e.authorizeChoosing(ctx, pc.pair, pl.ID, stackSummary(pl, shown, authors, files, res.Waived, scanWarnings, folded), ao.yes, ch)
	if err != nil {
		return nil, warns, err
	}
	res.Approval = appr
	if archives(ch, pick) {
		res.handOff = names
	}
	for i, en := range pl.Chain {
		branch, base, P := en.Publish.TargetBranch, pl.PRs[i].Base, en.Publish.Commit
		if en.Publish.Update {
			lp := levelPlan(pl, i)
			if landed[i] {
				res.Resumed = true
			} else if err := e.pushUpdate(ctx, pc, lp, P); err != nil {
				// The branches below are updated; this one is left as it is.
				return res, warns, stackLevelErr(err, names)
			}
			// The record first, so the next read of the PR never takes these
			// commits for someone else's; then the texts the person typed.
			pr, mw, err := e.recordUpdate(ctx, pc, lp, P)
			warns = addWarnings(warns, mw...)
			if err != nil {
				return res, warns, err
			}
			res.PRs[i].Number, res.PRs[i].URL = pr.Number, pr.URL
			if err := e.editUpdated(ctx, pc, lp); err != nil {
				return res, warns, err
			}
			continue
		}
		if exists[i] {
			res.Resumed = true
		} else {
			step := e.startStep("Publishing %s to %s (branch %s)", en.Feature, pc.pair.Public, branch)
			e.notef(progress.Info, "commit %s → %s:%s", short(P), pc.pair.Public, branch)
			opt := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + branch}}}
			if err := e.push(ctx, pc, remotePublic, opt, P+":refs/heads/"+branch); err != nil {
				if contract.From(err).Code == contract.RefMoved {
					return res, warns, contract.New(contract.BranchExists, "public branch %q appeared while publishing", branch).WithField("branch")
				}
				return res, warns, err
			}
			step.Done(fmt.Sprintf("Published %s to %s (branch %s)", en.Feature, pc.pair.Public, branch))
		}
		pr, err := e.Forge.FindOpenPR(ctx, pc.pair.Public, branch, base)
		if err != nil {
			return res, warns, forgeErr(err, "find the public PR of %s", en.Feature)
		}
		if pr == nil {
			created, err := e.Forge.CreatePR(ctx, pc.pair.Public, forge.NewPR{Title: en.Publish.PR.Title, Body: en.Publish.PR.Body, Head: branch, Base: base})
			if err != nil {
				return res, warns, forgeErr(err, "open the public PR of %s", en.Feature)
			}
			pr = &created
		} else {
			res.Resumed = true
		}
		res.PRs[i].Number, res.PRs[i].URL = pr.Number, pr.URL
		mw, err := e.recordPublished(ctx, pc, en.Feature, feature.Promoted{PublicPR: pr.Number, PlanID: pl.ID,
			FeatureHead: en.Fingerprint.FeatureHead, Branch: branch, Base: base, PublishedHead: P, PatchIDs: ids[i]})
		warns = addWarnings(warns, mw...)
		if err != nil {
			return res, warns, err
		}
	}
	res.PublicPR, res.PRURL = res.PRs[top].Number, res.PRs[top].URL
	st, err := e.stackUp(ctx, pc, branches[0], res.PRs)
	if err != nil {
		return res, warns, err
	}
	if st != nil {
		num := st.Number
		res.StackNumber = &num
		// Every feature whose PR is in the stack records it: the bottom's
		// public parent too, when its PR leads a new stack.
		owners := make([]string, 0, n+1)
		if branches[0].parentPR != 0 {
			owners = append(owners, stackParent(branches[0].meta))
		}
		for _, p := range res.PRs {
			owners = append(owners, p.Feature)
		}
		for _, name := range owners {
			m, sha, err := e.readMeta(ctx, pc, name)
			if err != nil {
				return res, warns, err
			}
			if m.Promoted == nil || m.Promoted.Stack == num {
				continue
			}
			m.SchemaVersion = feature.SchemaVersion
			m.Promoted.Stack = num
			_, mw, err := e.updateMeta(ctx, pc, m, sha)
			warns = addWarnings(warns, mw...)
			if err != nil {
				return res, warns, err
			}
		}
	}
	return res, warns, nil
}

// recordPublished writes p as feature name's publish record, read again so
// the lease is on what is there now. A record already saying p is not
// written again (a resumed apply), and a stack it records for the same PR
// is kept.
func (e *Engine) recordPublished(ctx context.Context, pc *pairCtx, name string, p feature.Promoted) ([]contract.Warning, error) {
	m, sha, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, err
	}
	if p.Stack == 0 {
		p.Stack = stackOfRecord(m, p.PublicPR)
	}
	if m.Promoted != nil && reflect.DeepEqual(*m.Promoted, p) {
		return nil, nil
	}
	m.SchemaVersion = feature.SchemaVersion
	m.Promoted = &p
	_, mw, err := e.updateMeta(ctx, pc, m, sha)
	return mw, err
}

// stackUp registers prs, bottom first, as a native GitHub stack: added to
// the stack the bottom's public parent is in, or to one an earlier run of
// this apply made, or as a new stack. The parent's PR leads a new stack, so
// the stack mirrors the shadow one. A single PR makes none. It returns nil
// when the forge has no native stacks (no Stacker, or
// forge.ErrStacksUnavailable): the PRs stay plain chained PRs, each based on
// the branch below.
func (e *Engine) stackUp(ctx context.Context, pc *pairCtx, bottom stackBranch, prs []plan.StackPR) (*forge.Stack, error) {
	st, ok := e.Forge.(forge.Stacker)
	if !ok {
		return nil, nil
	}
	repo := pc.pair.Public
	want := make([]int, 0, len(prs)+1)
	if bottom.parentPR != 0 {
		want = append(want, bottom.parentPR)
	}
	for _, p := range prs {
		want = append(want, p.Number)
	}
	if len(want) < 2 {
		return nil, nil
	}
	// GitHub answers 404 or 422 both for a repo without native stacks and
	// for a token that may not use them; either way the PRs stay chained,
	// and the host's answer is shown.
	unavailable := func(err error) (*forge.Stack, error) {
		e.notef(progress.Warn, "%s has no native stacked PRs (%v), so the PRs stay chained, each based on the branch below", repo, err)
		return nil, nil
	}
	var have *forge.Stack
	var err error
	for _, n := range want {
		if have, err = st.StackOf(ctx, repo, n); err != nil || have != nil {
			break
		}
	}
	switch {
	case errors.Is(err, forge.ErrStacksUnavailable):
		return unavailable(err)
	case err != nil:
		return nil, forgeErr(err, "read the native stacks of %s", repo)
	}
	if have == nil {
		step := e.startStep("Registering PRs %s as a native stack", prNumbers(want))
		s, err := st.CreateStack(ctx, repo, want)
		switch {
		case errors.Is(err, forge.ErrStacksUnavailable):
			// Deferred, so the note with the host's answer comes first.
			defer step.Done(fmt.Sprintf("Kept PRs %s chained (no native stacked PRs)", prNumbers(want)))
			return unavailable(err)
		case err != nil:
			return nil, forgeErr(err, "register PRs %s as a native stack", prNumbers(want))
		}
		step.Done(fmt.Sprintf("Registered PRs %s as a native stack", prNumbers(want)))
		return &s, nil
	}
	var missing []int
	for _, n := range want {
		if !slices.Contains(have.PRs, n) {
			missing = append(missing, n)
		}
	}
	if len(missing) == 0 {
		return have, nil
	}
	// A native stack grows only at its top: PRs that would go above one
	// lower down (a second branch on the same parent) stay chained.
	if i := slices.Index(want, missing[0]); i > 0 && len(have.PRs) > 0 && want[i-1] != have.PRs[len(have.PRs)-1] {
		e.notef(progress.Warn, "PR #%d isn't the top of native stack #%d (#%d is), and a native stack only grows at its top, so PRs %s stay chained, each based on the branch below",
			want[i-1], have.Number, have.PRs[len(have.PRs)-1], prNumbers(missing))
		return nil, nil
	}
	step := e.startStep("Adding PRs %s to native stack #%d", prNumbers(missing), have.Number)
	s, err := st.AddToStack(ctx, repo, have.Number, missing)
	switch {
	case errors.Is(err, forge.ErrStacksUnavailable):
		// Deferred, so the note with the host's answer comes first.
		defer step.Done(fmt.Sprintf("Kept PRs %s chained (no native stacked PRs)", prNumbers(missing)))
		return unavailable(err)
	case err != nil:
		return nil, forgeErr(err, "add PRs %s to native stack #%d", prNumbers(missing), have.Number)
	}
	step.Done(fmt.Sprintf("Added PRs %s to native stack #%d", prNumbers(missing), have.Number))
	return &s, nil
}

func prNumbers(ns []int) string {
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprintf("#%d", n)
	}
	return strings.Join(parts, ", ")
}
