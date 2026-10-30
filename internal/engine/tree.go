package engine

import (
	"bytes"
	"context"
	"path"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/strip"
	"github.com/ghostbranch-labs/ghostbranch/internal/workflow"
)

// publishedTree is rev's tree minus overlay and strip paths, with internal
// sections cut out of its files: exactly the tree promote publishes. It
// returns the tree SHA and its entries, or MARKER_INVALID.
func publishedTree(ctx context.Context, g *gitx.Git, rev string, cfg config.Config) (string, []gitx.TreeEntry, error) {
	all, err := g.ListTree(ctx, rev)
	if err != nil {
		return "", nil, internalErr(err, "list the feature tree")
	}
	m := strip.Markers{Begin: cfg.LeakScan.Markers.Begin, End: cfg.LeakScan.Markers.End}
	keep, err := strip.Tree(ctx, g, all, overlay.NewSet(cfg.LeakScan.Strip...), m)
	if err != nil {
		return "", nil, err
	}
	t, err := g.MakeTree(ctx, keep)
	if err != nil {
		return "", nil, internalErr(err, "write the published tree")
	}
	return t, keep, nil
}

// guardedEntries returns a copy of tEntries in which each public workflow's
// repo guards are rewritten (mode rewrite), the gate's report, and every
// guard found. tEntries itself is never changed: it is what publishes.
func (e *Engine) guardedEntries(ctx context.Context, g *gitx.Git, tEntries []gitx.TreeEntry, names workflow.Names, mode string) ([]gitx.TreeEntry, workflow.GuardReport, []workflow.Guard, error) {
	rep := workflow.GuardReport{Mode: mode, Rewritten: []workflow.Guard{}, Unrewritable: []workflow.Guard{}, Skipped: []workflow.Guard{}}
	out := append([]gitx.TreeEntry(nil), tEntries...)
	if mode == "off" {
		return out, rep, nil, nil
	}
	var idx []int
	var shas []string
	for i, en := range out {
		if en.Type == "blob" && workflow.IsWorkflowPath(en.Path) {
			idx, shas = append(idx, i), append(shas, en.SHA)
		}
	}
	if len(idx) == 0 {
		return out, rep, nil, nil
	}
	blobs, err := g.ReadBlobs(ctx, shas)
	if err != nil {
		return nil, rep, nil, internalErr(err, "read workflow files")
	}
	var all []workflow.Guard
	for _, i := range idx {
		src := blobs[out[i].SHA]
		nb, guards, err := workflow.Rewrite(out[i].Path, src, names)
		if err != nil {
			continue // invalid YAML: GitHub reports it; the file is left as is
		}
		all = append(all, guards...)
		for _, gd := range guards {
			switch {
			case !gd.Rewritable:
				rep.Unrewritable = append(rep.Unrewritable, gd)
			case mode == "rewrite":
				rep.Rewritten = append(rep.Rewritten, gd)
			default: // warn: left as it is, so its job skips in the shadow repo
				rep.Skipped = append(rep.Skipped, gd)
			}
		}
		if mode == "rewrite" && !bytes.Equal(nb, src) {
			sha, err := g.HashObject(ctx, nb)
			if err != nil {
				return nil, rep, nil, internalErr(err, "write a rewritten workflow")
			}
			out[i].SHA = sha
		}
	}
	return out, rep, all, nil
}

// candidateTree is C's tree: T with its repo guards rewritten for the
// shadow repo (guardedEntries), plus the overlay branch's files, so CI runs
// on what publishes, with private workflows and config present. Clashes
// are checked against R, which still holds every public and feature path.
func (e *Engine) candidateTree(ctx context.Context, g *gitx.Git, R string, tEntries []gitx.TreeEntry, O string, names workflow.Names, mode string) (string, workflow.GuardReport, []workflow.Guard, error) {
	var rep workflow.GuardReport
	rEntries, err := g.ListTree(ctx, R)
	if err != nil {
		return "", rep, nil, internalErr(err, "list the rebased tree")
	}
	oEntries, err := g.ListTree(ctx, O)
	if err != nil {
		return "", rep, nil, internalErr(err, "list the overlay")
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
		return "", rep, nil, contract.New(contract.OverlayCollision,
			"the feature already has paths the overlay adds: %s", strings.Join(clash, ", ")).
			WithDetails(map[string]any{"paths": clash})
	}
	cEntries, rep, guards, err := e.guardedEntries(ctx, g, tEntries, names, mode)
	if err != nil {
		return "", rep, nil, err
	}
	tree, err := g.MakeTree(ctx, append(cEntries, oEntries...))
	if err != nil {
		return "", rep, nil, internalErr(err, "write the candidate tree")
	}
	return tree, rep, guards, nil
}

// candidate builds C: one commit on R whose tree is candidateTree. It
// returns C with the guard report and every guard found.
func (e *Engine) candidate(ctx context.Context, g *gitx.Git, R string, tEntries []gitx.TreeEntry, O, name string, names workflow.Names, mode string) (string, workflow.GuardReport, []workflow.Guard, error) {
	tree, rep, guards, err := e.candidateTree(ctx, g, R, tEntries, O, names, mode)
	if err != nil {
		return "", rep, nil, err
	}
	who := e.bot()
	c, err := g.CommitTree(ctx, tree, []string{R}, "ghostbranch: gate candidate for "+name, who, who)
	if err != nil {
		return "", rep, nil, internalErr(err, "write the candidate commit")
	}
	return c, rep, guards, nil
}

// assertTip checks that the chain's tip P differs from the CI candidate C
// only in overlay paths and in workflows whose only difference is the
// gate's guard rewrite for wnames.
func assertTip(ctx context.Context, g *gitx.Git, P, C string, set overlay.Set, wnames workflow.Names) error {
	paths, err := g.DiffNames(ctx, C, P)
	if err != nil {
		return internalErr(err, "compare the published commit with the CI candidate")
	}
	var outside []string
	for _, n := range paths {
		if set.Match(n) || rewriteOnly(ctx, g, C, P, n, wnames) {
			continue
		}
		outside = append(outside, n)
	}
	if len(outside) > 0 {
		return contract.New(contract.Internal, "published commit differs from the CI candidate outside overlay paths and guard rewrites: %s",
			strings.Join(outside, ", ")).WithDetails(map[string]any{"paths": outside})
	}
	return nil
}

// rewriteOnly reports whether path differs between C and P only by the
// gate's guard rewrite: a workflow present in both, whose content differs,
// and the rewrite of P's blob is exactly C's. Equal content means the
// difference is in the file's mode, which no rewrite makes.
func rewriteOnly(ctx context.Context, g *gitx.Git, C, P, path string, n workflow.Names) bool {
	if !workflow.IsWorkflowPath(path) {
		return false
	}
	c, okC, err1 := g.ShowFile(ctx, C, path)
	p, okP, err2 := g.ShowFile(ctx, P, path)
	if err1 != nil || err2 != nil || !okC || !okP || bytes.Equal(c, p) {
		return false
	}
	out, _, err := workflow.Rewrite(path, p, n)
	return err == nil && bytes.Equal(out, c)
}

// rules is the leak scan's view of the overlay config.
func rules(cfg config.Config) leak.Rules {
	return leak.Rules{Overlay: overlay.NewSet(cfg.LeakScan.Strip...), BlockPatterns: cfg.LeakScan.BlockPatterns, LargeFile: cfg.LeakScan.LargeFile}
}

// scan runs the leak scanner over published entries and texts. base is
// the public base branch's tip (or the commit the published chain starts
// on): files that match it are not new or changed, so they raise no
// warnings. Findings and warnings are never nil.
func (e *Engine) scan(ctx context.Context, g *gitx.Git, cfg config.Config, entries []gitx.TreeEntry, base string, texts []leak.Text) (leak.Report, string, error) {
	sc, err := e.NewScanner(rules(cfg))
	if err != nil {
		return leak.Report{}, "", internalErr(err, "start the leak scanner")
	}
	old, err := blobsByPath(ctx, g, base)
	if err != nil {
		return leak.Report{}, "", err
	}
	rep, err := scanWith(ctx, g, sc, entries, old, texts)
	if err != nil {
		return leak.Report{}, "", err
	}
	return rep, sc.Version(), nil
}

// blobsByPath maps rev's paths to their blob SHAs.
func blobsByPath(ctx context.Context, g *gitx.Git, rev string) (map[string]string, error) {
	es, err := g.ListTree(ctx, rev)
	if err != nil {
		return nil, internalErr(err, "list the files of "+rev)
	}
	out := make(map[string]string, len(es))
	for _, en := range es {
		out[en.Path] = en.SHA
	}
	return out, nil
}

// scanWith scans entries and texts with sc. old maps public main's paths to
// blob SHAs; files that match it raise no warnings. Findings and warnings
// are never nil.
func scanWith(ctx context.Context, g *gitx.Git, sc leak.Scanner, entries []gitx.TreeEntry, old map[string]string, texts []leak.Text) (leak.Report, error) {
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
		return leak.Report{}, internalErr(err, "read files to scan")
	}
	var files []leak.File
	for _, en := range entries {
		if en.Type == "blob" {
			files = append(files, leak.File{Path: en.Path, Content: blobs[en.SHA], Changed: old[en.Path] != en.SHA})
		}
	}
	rep, err := sc.Scan(leak.Input{Paths: paths, Files: files, Texts: texts})
	if err != nil {
		return leak.Report{}, internalErr(err, "leak scan")
	}
	if rep.Findings == nil {
		rep.Findings = []leak.Finding{}
	}
	if rep.Warnings == nil {
		rep.Warnings = []contract.Warning{}
	}
	return rep, nil
}

// addWarnings appends more to dst, skipping any warning that dst already
// holds with the same code and message (a gate that promote ran itself and
// promote's own scan raise the same file warnings).
func addWarnings(dst []contract.Warning, more ...contract.Warning) []contract.Warning {
	for _, w := range more {
		dup := false
		for _, d := range dst {
			if d.Code == w.Code && d.Message == w.Message {
				dup = true
				break
			}
		}
		if !dup {
			dst = append(dst, w)
		}
	}
	return dst
}
