package engine

import (
	"context"
	"path"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
)

// publishedTree is rev's tree minus overlay and strip paths: exactly the
// tree promote publishes. It returns the tree SHA and its entries.
func publishedTree(ctx context.Context, g *gitx.Git, rev string, set overlay.Set) (string, []gitx.TreeEntry, error) { //nolint:unused // used by later tasks
	all, err := g.ListTree(ctx, rev)
	if err != nil {
		return "", nil, internalErr(err, "list the feature tree")
	}
	keep := make([]gitx.TreeEntry, 0, len(all))
	for _, en := range all {
		if !set.Match(en.Path) {
			keep = append(keep, en)
		}
	}
	t, err := g.MakeTree(ctx, keep)
	if err != nil {
		return "", nil, internalErr(err, "write the published tree")
	}
	return t, keep, nil
}

// candidate builds C: R plus one commit that adds the overlay branch's
// files, so CI runs with private workflows and config present.
func (e *Engine) candidate(ctx context.Context, g *gitx.Git, R, O, name string) (string, error) { //nolint:unused // used by later tasks
	rEntries, err := g.ListTree(ctx, R)
	if err != nil {
		return "", internalErr(err, "list the rebased tree")
	}
	oEntries, err := g.ListTree(ctx, O)
	if err != nil {
		return "", internalErr(err, "list the overlay")
	}
	files, dirs := map[string]bool{}, map[string]bool{}
	for _, en := range rEntries {
		files[en.Path] = true
		for d := path.Dir(en.Path); d != "."; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	var clash []string
	for _, oe := range oEntries {
		hit := files[oe.Path] || dirs[oe.Path]
		for d := path.Dir(oe.Path); d != "." && !hit; d = path.Dir(d) {
			hit = files[d]
		}
		if hit {
			clash = append(clash, oe.Path)
		}
	}
	if len(clash) > 0 {
		return "", contract.New(contract.OverlayCollision,
			"the feature already has paths the overlay adds: %s", strings.Join(clash, ", ")).
			WithDetails(map[string]any{"paths": clash})
	}
	tree, err := g.MakeTree(ctx, append(append([]gitx.TreeEntry{}, rEntries...), oEntries...))
	if err != nil {
		return "", internalErr(err, "write the candidate tree")
	}
	who := e.bot()
	c, err := g.CommitTree(ctx, tree, []string{R}, "ghostbranch: gate candidate for "+name, who, who)
	if err != nil {
		return "", internalErr(err, "write the candidate commit")
	}
	return c, nil
}

// assertPublish re-checks P before it may be published: its tree is the
// gated tree, its only parent is public main, and it differs from the CI
// candidate only in overlay paths.
func assertPublish(ctx context.Context, g *gitx.Git, P, T, C, publicMain string, set overlay.Set) error { //nolint:unused // used by later tasks
	tree, err := g.TreeOf(ctx, P)
	if err != nil || tree != T {
		return contract.Wrap(contract.Internal, err, "published tree %s does not match the gated tree %s", short(tree), short(T))
	}
	parent, err := g.MustResolve(ctx, P+"^")
	if err != nil || parent != publicMain {
		return contract.Wrap(contract.Internal, err, "published commit's parent %s is not public main %s", short(parent), short(publicMain))
	}
	if _, ok, _ := g.Resolve(ctx, P+"^2"); ok {
		return contract.New(contract.Internal, "published commit must have exactly one parent")
	}
	names, err := g.DiffNames(ctx, C, P)
	if err != nil {
		return internalErr(err, "compare the published commit with the CI candidate")
	}
	var outside []string
	for _, n := range names {
		if !set.Match(n) {
			outside = append(outside, n)
		}
	}
	if len(outside) > 0 {
		return contract.New(contract.Internal, "published commit differs from the CI candidate outside overlay paths: %s",
			strings.Join(outside, ", ")).WithDetails(map[string]any{"paths": outside})
	}
	return nil
}

// scan runs the leak scanner over published entries and texts. It returns
// the findings (never nil) and the scanner version.
func (e *Engine) scan(ctx context.Context, g *gitx.Git, set overlay.Set, entries []gitx.TreeEntry, texts []leak.Text) ([]leak.Finding, string, error) { //nolint:unused // used by later tasks
	sc, err := e.NewScanner(set)
	if err != nil {
		return nil, "", internalErr(err, "start the leak scanner")
	}
	paths := make([]string, 0, len(entries))
	var shas []string
	seen := map[string]bool{}
	for _, en := range entries {
		paths = append(paths, en.Path)
		if en.Type == "blob" && !seen[en.SHA] {
			seen[en.SHA] = true
			shas = append(shas, en.SHA)
		}
	}
	blobs, err := g.ReadBlobs(ctx, shas)
	if err != nil {
		return nil, "", internalErr(err, "read files to scan")
	}
	var files []leak.File
	for _, en := range entries {
		if en.Type == "blob" {
			files = append(files, leak.File{Path: en.Path, Content: blobs[en.SHA]})
		}
	}
	findings, err := sc.Scan(leak.Input{Paths: paths, Files: files, Texts: texts})
	if err != nil {
		return nil, "", internalErr(err, "leak scan")
	}
	if findings == nil {
		findings = []leak.Finding{}
	}
	return findings, sc.Version(), nil
}
