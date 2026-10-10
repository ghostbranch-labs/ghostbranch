package engine

// The publish chain: what each history mode publishes, as public commits
// built from the gate's rebased chain, and the checks every commit passes.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

// link is one public commit before it is written.
type link struct {
	sources []string         // feature branch commits it publishes, in order
	tree    string           // its published (stripped) tree
	entries []gitx.TreeEntry // that tree's entries
	message string           // squash: the -m message; preserve: the original; curated: the group's
	author  gitx.Identity    // the original author (preserve, curated); zero for squash
}

// chainInput is everything deriveChain needs.
type chainInput struct {
	mode     string               // config.ModeSquash | ModePreserve | ModeCurated
	pm       string               // public main
	baseTree string               // the published tree the chain starts from; "" means public main's (pm's) tree
	T        string               // the gated published tree (stripped tip)
	tEntries []gitx.TreeEntry     // T's entries
	commits  []feature.GateCommit // the gate's rebased chain, oldest first
	message  string               // squash only
	groups   []plan.Group         // curated only, From resolved to full source SHAs
}

// replayFunc cherry-picks commits onto onto in a throwaway worktree (Engine.replay).
type replayFunc func(commits []string, onto string) ([]string, error)

// deriveChain builds the public commits for in.mode from the gate's rebased
// chain. A commit (or curated group) whose published tree equals the one
// before it publishes nothing and is dropped; dropped lists its sources.
// No commit at all is NOTHING_TO_PUBLISH. The last link's tree is always
// in.T, the tree the gate's CI tested.
func deriveChain(ctx context.Context, g *gitx.Git, cfg config.Config, in chainInput, replay replayFunc) ([]link, []string, error) {
	base := in.baseTree
	if base == "" {
		var err error
		if base, err = g.TreeOf(ctx, in.pm); err != nil {
			return nil, nil, internalErr(err, "read public main's tree")
		}
	}
	var links []link
	dropped := []string{}
	switch in.mode {
	case config.ModeSquash:
		if in.T != base {
			links = []link{{sources: sourcesOf(in.commits), tree: in.T, entries: in.tEntries, message: in.message}}
		} else {
			dropped = sourcesOf(in.commits) // the squash publishes nothing: every commit is empty
		}
	case config.ModePreserve:
		prev := base
		for _, c := range in.commits {
			tree, entries, err := publishedTree(ctx, g, c.Rebased, cfg)
			if err != nil {
				if ce := contract.From(err); ce.Code == contract.MarkerInvalid {
					if ce.Details == nil {
						ce.Details = map[string]any{}
					}
					ce.Details["commit"] = c.Source
				}
				return nil, nil, err
			}
			if tree == prev {
				dropped = append(dropped, c.Source)
				continue
			}
			msg, err := g.CommitMessage(ctx, c.Rebased)
			if err != nil {
				return nil, nil, internalErr(err, "read a commit message")
			}
			author, err := g.CommitAuthor(ctx, c.Rebased)
			if err != nil {
				return nil, nil, internalErr(err, "read a commit author")
			}
			links = append(links, link{sources: []string{c.Source}, tree: tree, entries: entries, message: msg, author: author})
			prev = tree
		}
	case config.ModeCurated:
		// Replay the gate's rebased commits in the groups' order on public
		// main, in a throwaway worktree. Each group publishes the tree at
		// its last commit. Those intermediate trees never ran CI: they are
		// stripped here and scanned with every other link.
		rebasedOf := map[string]string{}
		for _, c := range in.commits {
			rebasedOf[c.Source] = c.Rebased
		}
		var order []string
		ends := make([]int, len(in.groups)) // index in order of each group's last commit
		for i, gr := range in.groups {
			if len(gr.From) == 0 {
				return nil, nil, contract.New(contract.Internal, "curate group %d names no commits", i)
			}
			for _, s := range gr.From {
				r, ok := rebasedOf[s]
				if !ok {
					return nil, nil, contract.New(contract.Internal, "curate group %d names %s, which the gate did not replay", i, short(s))
				}
				order = append(order, r)
			}
			ends[i] = len(order) - 1
		}
		if replay == nil {
			return nil, nil, contract.New(contract.Internal, "curated mode needs a replay")
		}
		replayed, err := replay(order, in.pm)
		if err != nil {
			var ce *gitx.ConflictError
			if errors.As(err, &ce) {
				src, group := "", -1
				for i, r := range order {
					if r == ce.Commit {
						src = sourceOfRebased(in.commits, r)
						group = groupOf(ends, i)
						break
					}
				}
				return nil, nil, contract.New(contract.CurateConflict,
					"replaying the curated groups in order: commit %s (group %d) conflicts in %s", short(src), group, strings.Join(ce.Files, ", ")).
					WithDetails(map[string]any{"commit": src, "group": group, "files": ce.Files})
			}
			return nil, nil, err
		}
		if len(replayed) != len(order) {
			return nil, nil, contract.New(contract.Internal, "the curated replay made %d commits for %d", len(replayed), len(order))
		}
		prev := base
		for i, gr := range in.groups {
			tipSHA := replayed[ends[i]]
			tree, entries, err := publishedTree(ctx, g, tipSHA, cfg)
			if err != nil {
				if ce := contract.From(err); ce.Code == contract.MarkerInvalid {
					if ce.Details == nil {
						ce.Details = map[string]any{}
					}
					ce.Details["commit"] = gr.From[len(gr.From)-1]
				}
				return nil, nil, err
			}
			if tree == prev {
				dropped = append(dropped, gr.From...)
				continue
			}
			author, err := g.CommitAuthor(ctx, rebasedOf[gr.From[0]])
			if err != nil {
				return nil, nil, internalErr(err, "read a commit author")
			}
			links = append(links, link{sources: gr.From, tree: tree, entries: entries, message: gr.Message, author: author})
			prev = tree
		}
		if len(links) > 0 && links[len(links)-1].tree != in.T {
			last := links[len(links)-1].tree
			return nil, nil, contract.New(contract.CurateTreeMismatch,
				"the curated order ends on tree %s, but the gate's CI tested %s; keep commits that touch the same lines in order", short(last), short(in.T)).
				WithDetails(map[string]any{"tree": last, "gated_tree": in.T})
		}
	default:
		return nil, nil, contract.New(contract.Internal, "unknown history mode %q", in.mode)
	}
	if len(links) == 0 {
		return nil, nil, contract.New(contract.NothingToPublish,
			"nothing would be published: every commit is empty once overlay paths and internal sections are stripped").
			WithDetails(map[string]any{"dropped_empty": dropped})
	}
	if last := links[len(links)-1].tree; last != in.T {
		return nil, nil, contract.New(contract.Internal, "the %s chain ends at tree %s, not the gated tree %s", in.mode, short(last), short(in.T))
	}
	return links, dropped, nil
}

// sourceOfRebased is the feature commit the gate rebased into r.
func sourceOfRebased(cs []feature.GateCommit, r string) string {
	for _, c := range cs {
		if c.Rebased == r {
			return c.Source
		}
	}
	return ""
}

// groupOf is the group whose commits hold index i of the replay order,
// given each group's last index in ends.
func groupOf(ends []int, i int) int {
	for gi, end := range ends {
		if i <= end {
			return gi
		}
	}
	return -1
}

func sourcesOf(cs []feature.GateCommit) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Source
	}
	return out
}

// publicAuthor is the author a preserved or curated commit goes public as,
// under promote.author_email. Original author dates are kept. false means
// map mode found no entry for the email.
func publicAuthor(p config.Promote, orig, publisher gitx.Identity) (gitx.Identity, bool) {
	switch p.AuthorEmail {
	case config.AuthorKeep:
		return orig, true
	case config.AuthorMap:
		v, ok := p.AuthorMap[strings.ToLower(orig.Email)]
		if !ok {
			return gitx.Identity{}, false
		}
		name, email, _ := config.ParseIdentity(v) // validated when the config was read
		return gitx.Identity{Name: name, Email: email, When: orig.When}, true
	}
	return gitx.Identity{Name: publisher.Name, Email: publisher.Email, When: orig.When}, true
}

// chainAuthors is each link's public author. Squash publishes as the
// publisher, dated as the publisher's identity is. Unmapped emails in map
// mode are AUTHOR_UNMAPPED, listing each email once, sorted.
func chainAuthors(p config.Promote, mode string, links []link, publisher gitx.Identity) ([]gitx.Identity, error) {
	out := make([]gitx.Identity, len(links))
	missing := map[string]bool{}
	for i, l := range links {
		if mode == config.ModeSquash {
			out[i] = publisher
			continue
		}
		id, ok := publicAuthor(p, l.author, publisher)
		if !ok {
			missing[strings.ToLower(l.author.Email)] = true
			continue
		}
		out[i] = id
	}
	if len(missing) > 0 {
		emails := make([]string, 0, len(missing))
		for e := range missing {
			emails = append(emails, e)
		}
		sort.Strings(emails)
		return nil, contract.New(contract.AuthorUnmapped, "promote.author_map has no entry for: %s", strings.Join(emails, ", ")).
			WithDetails(map[string]any{"emails": emails})
	}
	return out, nil
}

// writeChain writes the public commits on public main, oldest first.
func writeChain(ctx context.Context, g *gitx.Git, links []link, authors []gitx.Identity, committer gitx.Identity, pm string) ([]plan.Commit, error) {
	if len(authors) != len(links) {
		return nil, contract.New(contract.Internal, "%d authors for %d public commits", len(authors), len(links))
	}
	out := make([]plan.Commit, 0, len(links))
	parent := pm
	for i, l := range links {
		sha, err := g.CommitTree(ctx, l.tree, []string{parent}, l.message, authors[i], committer)
		if err != nil {
			return nil, internalErr(err, "write a public commit")
		}
		out = append(out, plan.Commit{Commit: sha, Tree: l.tree, Parent: parent, Sources: l.sources,
			Author: plan.Identity{Name: authors[i].Name, Email: authors[i].Email, Date: authors[i].When}, Subject: subject(l.message)})
		parent = sha
	}
	return out, nil
}

// subject is the message's first line, trimmed.
func subject(msg string) string {
	first, _, _ := strings.Cut(msg, "\n")
	return strings.TrimSpace(first)
}

// identityText is id as "Name <email>".
func identityText(id gitx.Identity) string { return id.Name + " <" + id.Email + ">" }

// scanStep is one commit's worth of what goes public.
type scanStep struct {
	sources []string         // set on findings; nil for squash
	entries []gitx.TreeEntry // the step's full published tree
	texts   []leak.Text      // its message, author, committer
}

// scanChain scans everything a chain publishes. The first step's whole tree
// is scanned, as a squash scan always has; each later step adds only the
// entries that differ from the step before. Each step's texts are scanned
// with it. A finding seen in several steps is one finding whose Commits
// lists every step's sources, first seen first. texts (branch, PR) are
// scanned once and carry no commits. Warnings of one code are merged into
// one, listing every path.
func (e *Engine) scanChain(ctx context.Context, g *gitx.Git, cfg config.Config, pm string, steps []scanStep, texts []leak.Text) (leak.Report, string, error) {
	return e.scanChainFrom(ctx, g, cfg, pm, true, steps, texts)
}

// scanChainFrom is scanChain from base, a commit or a tree. With firstFull
// the first step's whole tree is scanned, as a squash scan always has.
// Without it, only what the first step changes from base is scanned: for a
// stacked feature, whose base is the published tree below it, which the
// gates below scanned.
func (e *Engine) scanChainFrom(ctx context.Context, g *gitx.Git, cfg config.Config, base string, firstFull bool, steps []scanStep, texts []leak.Text) (leak.Report, string, error) {
	sc, err := e.NewScanner(rules(cfg))
	if err != nil {
		return leak.Report{}, "", internalErr(err, "start the leak scanner")
	}
	old, err := blobsByPath(ctx, g, base)
	if err != nil {
		return leak.Report{}, "", err
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
	var prev map[string]string
	if !firstFull {
		prev = old
	}
	for _, st := range steps {
		entries := st.entries
		if prev != nil {
			entries = nil
			for _, en := range st.entries {
				if prev[en.Path] != en.SHA {
					entries = append(entries, en)
				}
			}
		}
		r, err := scanWith(ctx, g, sc, entries, old, st.texts)
		if err != nil {
			return leak.Report{}, "", err
		}
		add(r.Findings, st.sources)
		rep.Warnings = mergeWarnings(rep.Warnings, r.Warnings)
		prev = make(map[string]string, len(st.entries))
		for _, en := range st.entries {
			prev[en.Path] = en.SHA
		}
	}
	if len(texts) > 0 {
		r, err := scanWith(ctx, g, sc, nil, old, texts)
		if err != nil {
			return leak.Report{}, "", err
		}
		add(r.Findings, nil)
	}
	return rep, sc.Version(), nil
}

// mergeWarnings adds more to dst. A warning whose code dst already has and
// whose details carry paths is merged into that one: the paths are joined
// (each once, in order) and the message's list is rebuilt the way the
// scanner writes it ("<what>: a, b, c, d, e, and N more").
func mergeWarnings(dst, more []contract.Warning) []contract.Warning {
	for _, w := range more {
		newPaths, ok := w.Details["paths"].([]string)
		i := slices.IndexFunc(dst, func(d contract.Warning) bool {
			_, has := d.Details["paths"].([]string)
			return d.Code == w.Code && has
		})
		if !ok || i < 0 {
			dst = addWarnings(dst, w)
			continue
		}
		paths := append([]string(nil), dst[i].Details["paths"].([]string)...)
		for _, p := range newPaths {
			if !slices.Contains(paths, p) {
				paths = append(paths, p)
			}
		}
		shown := paths
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("and %d more", len(paths)-5))
		}
		what, _, _ := strings.Cut(dst[i].Message, ": ")
		details := make(map[string]any, len(dst[i].Details))
		for k, v := range dst[i].Details {
			details[k] = v
		}
		details["paths"] = paths
		dst[i] = contract.Warning{Code: w.Code, Message: what + ": " + strings.Join(shown, ", "), Details: details}
	}
	return dst
}

// assertChain re-checks a chain before it may be published: each commit's
// only parent is the one before it (public main for the first), each tree
// is the planned tree and holds no overlay path, the tip's tree is the
// gated tree T, and the tip differs from the CI candidate C only in overlay
// paths and guard rewrites.
func assertChain(ctx context.Context, g *gitx.Git, commits []plan.Commit, T, C, pm string, set overlay.Set, names workflow.Names) error {
	if len(commits) == 0 {
		return contract.New(contract.Internal, "the chain has no commits")
	}
	parent := pm
	for _, c := range commits {
		if c.Parent != parent {
			return contract.New(contract.Internal, "public commit %s records parent %s, expected %s", short(c.Commit), short(c.Parent), short(parent))
		}
		if got, err := g.MustResolve(ctx, c.Commit+"^"); err != nil || got != parent {
			return contract.Wrap(contract.Internal, err, "public commit %s's parent is %s, expected %s", short(c.Commit), short(got), short(parent))
		}
		// Fail closed: a parent count that can't be read is not one.
		if _, ok, err := g.Resolve(ctx, c.Commit+"^2"); err != nil || ok {
			return contract.Wrap(contract.Internal, err, "public commit %s must have exactly one parent", short(c.Commit))
		}
		if tree, err := g.TreeOf(ctx, c.Commit); err != nil || tree != c.Tree {
			return contract.Wrap(contract.Internal, err, "public commit %s's tree %s is not the planned %s", short(c.Commit), short(tree), short(c.Tree))
		}
		es, err := g.ListTree(ctx, c.Commit)
		if err != nil {
			return internalErr(err, "list a public commit's tree")
		}
		for _, en := range es {
			if set.Match(en.Path) {
				return contract.New(contract.Internal, "public commit %s holds overlay path %s", short(c.Commit), en.Path)
			}
		}
		parent = c.Commit
	}
	tipC := commits[len(commits)-1]
	if tipC.Tree != T {
		return contract.New(contract.Internal, "the chain's tip tree %s is not the gated tree %s", short(tipC.Tree), short(T))
	}
	return assertTip(ctx, g, tipC.Commit, C, set, names)
}
