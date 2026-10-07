package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/managed"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// UpgradeFile is one managed overlay path and what upgrade does to it.
type UpgradeFile struct {
	Path               string         `json:"path"`
	Status             managed.Status `json:"status"`
	Action             string         `json:"action"` // add | update | remove | unchanged
	ReplacesLocalEdits bool           `json:"replaces_local_edits"`
}

// UpgradeResult is the upgrade command's data.
type UpgradeResult struct {
	Public  string           `json:"public"`
	Shadow  string           `json:"shadow"`
	Changed bool             `json:"changed"`
	Files   []UpgradeFile    `json:"files"`
	Commit  string           `json:"commit"`
	Audit   *AuditResult     `json:"audit"`
	Preview *preview.Preview `json:"preview"`
}

// Upgrade brings the pair's managed overlay files up to this binary's
// templates and the overlay config: it adds, updates or removes them in one
// overlay commit, leased on the overlay branch. It writes only overlay
// paths in the shadow repo.
func (e *Engine) Upgrade(ctx context.Context, p workspace.Pair) (res *UpgradeResult, warns []contract.Warning, err error) {
	defer func() {
		summary := ""
		if res != nil && err == nil {
			summary = "overlay already current"
			if res.Changed {
				var changed []string
				for _, f := range res.Files {
					if f.Action != "unchanged" {
						changed = append(changed, f.Action+" "+f.Path)
					}
				}
				summary = fmt.Sprintf("%s in %s", strings.Join(changed, ", "), short(res.Commit))
			}
		}
		e.record("upgrade", &p, "", "", summary, err)
	}()
	pc, err := e.open(ctx, "upgrade", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	g := pc.git
	cfg, O, warns, err := e.readConfig(ctx, pc)
	if err != nil {
		return nil, warns, err
	}
	warns = addWarnings(warns, e.behind(ctx, pc)...)
	files, err := e.managedFiles(ctx, g, O, cfg)
	if err != nil {
		return nil, warns, err
	}
	want := managed.Render(managedInput(cfg))
	res = &UpgradeResult{Public: p.Public, Shadow: p.Shadow, Files: []UpgradeFile{}}
	var changes []UpgradeFile
	for _, f := range files {
		uf := UpgradeFile{Path: f.Path, Status: f.Status, Action: "unchanged"}
		_, wanted := want[f.Path]
		switch f.Status {
		case managed.Missing:
			uf.Action = "add"
		case managed.Outdated, managed.Modified:
			uf.Action = "update"
			if !wanted {
				uf.Action = "remove"
			}
			uf.ReplacesLocalEdits = f.Status == managed.Modified
		}
		res.Files = append(res.Files, uf)
		if uf.Action != "unchanged" {
			changes = append(changes, uf)
		}
	}
	if len(changes) == 0 {
		if e.DryRun {
			res.Preview = preview.New(map[string]string{"overlay": O})
		} else {
			res.Audit, warns = e.auditInto(ctx, pc, cfg, O, warns)
		}
		return res, warns, nil
	}
	res.Changed = true
	if e.DryRun {
		pv := preview.New(map[string]string{"overlay": O})
		for _, c := range changes {
			pv.Add(preview.OverlayFile(p.Shadow, c.Path, c.Action))
		}
		pv.Add(preview.PushRef(p.Shadow, "refs/heads/"+OverlayBranch, O, ""))
		res.Preview = pv
		return res, warns, nil
	}
	entries, err := g.ListTree(ctx, O)
	if err != nil {
		return res, warns, internalErr(err, "list the overlay branch")
	}
	byPath := map[string]gitx.TreeEntry{}
	for _, en := range entries {
		byPath[en.Path] = en
	}
	for _, c := range changes {
		if c.Action == "remove" {
			delete(byPath, c.Path)
			continue
		}
		sha, err := g.HashObject(ctx, want[c.Path])
		if err != nil {
			return res, warns, internalErr(err, "write "+c.Path)
		}
		byPath[c.Path] = gitx.TreeEntry{Mode: "100644", Type: "blob", SHA: sha, Path: c.Path}
	}
	list := make([]gitx.TreeEntry, 0, len(byPath))
	for _, en := range byPath {
		list = append(list, en)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Path < list[j].Path })
	tree, err := g.MakeTree(ctx, list)
	if err != nil {
		return res, warns, internalErr(err, "write the overlay tree")
	}
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		return res, warns, forgeErr(err, "look up the current user")
	}
	who := gitx.Identity{Name: u.Login, Email: forge.NoreplyEmail(u), When: e.now()}
	c, err := g.CommitTree(ctx, tree, []string{O}, "ghostbranch: upgrade overlay ("+managed.Describe()+")", who, who)
	if err != nil {
		return res, warns, internalErr(err, "write the overlay commit")
	}
	e.progressf("upgrading the overlay branch of %s", p.Shadow)
	lease := gitx.PushOptions{Leases: []gitx.Lease{{Ref: "refs/heads/" + OverlayBranch, Expect: O}}}
	if err := e.push(ctx, pc, remoteShadow, lease, c+":refs/heads/"+OverlayBranch); err != nil {
		return res, warns, err
	}
	_ = g.UpdateRef(ctx, shadowRef(OverlayBranch), c)
	res.Commit = c
	res.Audit, warns = e.auditInto(ctx, pc, cfg, c, warns)
	return res, warns, nil
}
