package engine

// feature pull: bring the commits someone made on a published feature's
// open PR on GitHub into its shadow branch, so the next update keeps them.
// Ghostbranch never overwrites them, and never resolves a conflict.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// PullOptions are feature pull's inputs.
type PullOptions struct {
	Feature string
}

// PullResult is what feature pull brought in.
type PullResult struct {
	Feature string `json:"feature"`
	From    string `json:"from"` // the feature branch's head before
	To      string `json:"to"`   // its head now; From when it didn't move
	// Adopted are the PR's commits Ghostbranch didn't publish, now in the
	// feature, oldest first; never null.
	Adopted []feature.AdoptedCommit `json:"adopted"`
	// Children are the live features stacked directly on it, when it moved
	// (To != From): they sit on its old commits until feature restack
	// moves them; never null.
	Children []string         `json:"children"`
	Preview  *preview.Preview `json:"preview"`
}

// FeaturePull reads the feature's open PR and the commits on its branch
// Ghostbranch didn't publish (prHead, through comparePublished: patch-id
// and message), brings them into the shadow branch (pullHead), and records
// them, in one atomic push leased on the branch head and the record it
// read. With nothing foreign it changes nothing. It touches only the
// shadow repo.
func (e *Engine) FeaturePull(ctx context.Context, p workspace.Pair, opt PullOptions) (res *PullResult, warns []contract.Warning, err error) {
	name := opt.Feature
	defer func() {
		summary := ""
		if res != nil && res.Preview == nil && err == nil {
			summary = fmt.Sprintf("adopted %d commit(s) from the public PR", len(res.Adopted))
			if res.From != res.To {
				summary += fmt.Sprintf("; %s moved %s -> %s", name, short(res.From), short(res.To))
			}
		}
		e.record("feature pull", &p, name, "", summary, err)
	}()
	if err := feature.ValidateName(name, "feature"); err != nil {
		return nil, nil, err
	}
	pc, err := e.open(ctx, "feature pull", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	if _, _, warns, err = e.readConfig(ctx, pc); err != nil {
		return nil, warns, err
	}
	g := pc.git
	m, metaSHA, err := e.readMeta(ctx, pc, name)
	if err != nil {
		return nil, warns, err
	}
	head, ok, err := g.Resolve(ctx, shadowRef(m.Branch))
	if err != nil {
		return nil, warns, internalErr(err, "read feature branch "+m.Branch)
	}
	if !ok {
		return nil, warns, contract.New(contract.FeatureNotFound, "feature branch %q is gone from %s", m.Branch, p.Shadow).WithField("feature")
	}
	// The open PR as GitHub has it now: its head H, its base, the commits on
	// it, and those Ghostbranch didn't publish.
	u, err := e.prHead(ctx, pc, &m)
	if err != nil {
		return nil, warns, err
	}
	if u == nil {
		return nil, warns, contract.New(contract.BadFlag,
			"feature pull brings in commits from a feature's open public PR, and %s has none (never published, its PR merged or closed, or its branch gone)", name).WithField("feature")
	}
	// An update Ghostbranch pushed but never recorded is its own, not
	// someone else's: adopting it would rewrite the feature with copies of
	// its own commits. apply of that plan finishes the record.
	if u.Unrecorded != "" {
		return nil, warns, prChangedErr(name, u)
	}
	res = &PullResult{Feature: name, From: head, To: head, Adopted: []feature.AdoptedCommit{}, Children: []string{}}
	if len(u.Foreign) == 0 {
		return res, warns, nil
	}
	newHead, adoptedHead, err := e.pullHead(ctx, pc, m, head, u)
	if err != nil {
		return res, warns, err
	}
	ids, err := g.PatchIDs(ctx, u.Public)
	if err != nil {
		return res, warns, internalErr(err, "compute the PR's patch-ids")
	}
	// The record: the PR's head and patch-ids as GitHub has them now, so
	// nothing on it is foreign any more, and the commits adopted. The base
	// stays as recorded: a base changed on GitHub still makes the next
	// update a replace.
	nm := m
	nm.SchemaVersion = feature.SchemaVersion
	pp := *m.Promoted
	pp.PublishedHead, pp.PatchIDs = u.Head, ids
	if adoptedHead != "" {
		pp.FeatureHead = adoptedHead
	}
	adopted := make([]feature.AdoptedCommit, len(u.Foreign))
	for i, f := range u.Foreign {
		adopted[i] = feature.AdoptedCommit{SHA: f.SHA, Author: f.Author, Subject: f.Subject}
	}
	pp.Adopted = append(append([]feature.AdoptedCommit(nil), pp.Adopted...), adopted...)
	nm.Promoted = &pp
	res.To, res.Adopted = newHead, adopted
	if newHead != head {
		recs, err := e.records(ctx, pc)
		if err != nil {
			return res, warns, err
		}
		res.Children = childrenOf(recs, name, false)
	}
	branchRef, metaRemote := "refs/heads/"+m.Branch, remoteMetaRef(name)
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"feature_head": head})
		if newHead != head {
			res.Preview.Add(preview.PushRef(p.Shadow, branchRef, head, newHead))
		}
		res.Preview.Add(preview.PushRef(p.Shadow, metaRemote, metaSHA, ""))
		return res, warns, nil
	}
	sha, err := e.saveMeta(ctx, pc, nm, metaSHA)
	if err != nil {
		return res, warns, err
	}
	// The branch is leased even when it doesn't move (a by-hand merge already
	// holds the PR): a feature branch pushed to since it was read means
	// what was read no longer stands, and nothing changes.
	popt := gitx.PushOptions{Atomic: true, Leases: []gitx.Lease{{Ref: metaRemote, Expect: metaSHA}, {Ref: branchRef, Expect: head}}}
	specs := []string{sha + ":" + metaRemote, newHead + ":" + branchRef}
	if err := e.push(ctx, pc, remoteShadow, popt, specs...); err != nil {
		res.To, res.Adopted, res.Children = head, []feature.AdoptedCommit{}, []string{}
		if ce := contract.From(err); ce.Code == contract.RefMoved {
			out := contract.New(contract.RefMoved, "feature %s changed in %s while its PR's commits were being pulled in, so nothing changed; run feature pull again", name, p.Shadow).
				WithField("feature").WithDetails(map[string]any{"feature": name, "refs": ce.Details["refs"]})
			out.Err = err
			return res, warns, out
		}
		return res, warns, err
	}
	if newHead != head {
		warns = addWarnings(warns, e.updateMirror(ctx, g, shadowRef(m.Branch), newHead)...)
	}
	warns = addWarnings(warns, e.updateMirror(ctx, g, metaRef(name), sha)...)
	return res, warns, nil
}

// pullHead is the feature head that holds the PR's commits (u), and the
// commit on it that holds exactly what the PR holds ("" when it can't be
// told):
//   - H itself, when the feature head is in the PR's branch (a fast-forward);
//   - the feature head, when it already holds H (merged by hand), with "";
//   - otherwise, when no foreign commit is a merge, the foreign commits
//     replayed onto the published feature head, then the feature's own
//     commits after it, so the next update appends. Without a usable
//     published head (or with merges after it) they go on the feature head.
//
// A merge commit, or a conflict, is pullByHand.
func (e *Engine) pullHead(ctx context.Context, pc *pairCtx, m feature.Meta, head string, u *prUpdate) (string, string, error) {
	g, name, H := pc.git, m.Name, u.Head
	if in, err := g.IsAncestor(ctx, head, H); err != nil {
		return "", "", internalErr(err, "compare "+name+" with its PR")
	} else if in {
		return H, H, nil
	}
	if in, err := g.IsAncestor(ctx, H, head); err != nil {
		return "", "", internalErr(err, "compare "+name+" with its PR")
	} else if in {
		return head, "", nil
	}
	shas := make([]string, len(u.Foreign))
	for i, f := range u.Foreign {
		parents, err := g.Run(ctx, "rev-list", "--parents", "-n", "1", f.SHA)
		if err != nil {
			return "", "", internalErr(err, "read commit "+short(f.SHA))
		}
		if len(strings.Fields(parents)) > 2 {
			return "", "", pullByHand(m, u, "merge_commit", f.SHA, []string{})
		}
		shas[i] = f.SHA
	}
	onto, rest := head, []string(nil)
	pub := m.Promoted.FeatureHead
	if pub != "" && pub != head {
		in, err := g.IsAncestor(ctx, pub, head)
		if err != nil {
			return "", "", internalErr(err, "compare "+name+" with its published head")
		}
		merges, err := g.Run(ctx, "rev-list", "--merges", "--count", pub+".."+head)
		if err != nil {
			return "", "", internalErr(err, "list "+name+"'s commits")
		}
		// Only a linear history after the published head is replayed:
		// replaying drops merge commits.
		if in && strings.TrimSpace(merges) == "0" {
			if rest, err = g.SourceCommits(ctx, pub, head); err != nil {
				return "", "", internalErr(err, "list "+name+"'s commits after its published head")
			}
			onto = pub
		}
	}
	st := e.startStep("Bringing %d commit(s) from PR #%d into %s", len(shas), u.PR, name)
	out, err := e.replay(ctx, pc, append(shas, rest...), onto)
	if err != nil {
		var ce *gitx.ConflictError
		if errors.As(err, &ce) {
			return "", "", pullByHand(m, u, "conflict", ce.Commit, ce.Files)
		}
		return "", "", contract.From(err)
	}
	st.Done(fmt.Sprintf("Brought %d commit(s) from PR #%d into %s", len(shas), u.PR, name))
	adopted := out[len(shas)-1]
	if onto == head && pub != head {
		// Replayed on top of unpublished work: no one commit holds only what
		// the PR holds.
		adopted = ""
	}
	return out[len(out)-1], adopted, nil
}

// pullByHand is PUBLIC_BRANCH_CHANGED for feature pull: the PR's commits
// can't come in by themselves (a merge commit, or a conflict), so the person
// merges the PR's branch into the feature in their checkout, pushes it to
// the shadow repo, and runs feature pull again, which then records it.
func pullByHand(m feature.Meta, u *prUpdate, reason, commit string, files []string) error {
	name := m.Name
	list := foreignDetails(u.Foreign)
	why := "commit " + short(commit) + " is a merge commit (GitHub's Update branch, or a merge someone pushed)"
	if reason == "conflict" {
		why = fmt.Sprintf("replaying commit %s with them conflicts in %s", short(commit), strings.Join(files, ", "))
	}
	steps := []string{
		"git fetch origin " + u.Branch,
		"git switch " + m.Branch,
		"git merge origin/" + u.Branch,
		"git push <shadow remote> " + m.Branch,
		"ghostbranch feature pull " + name,
	}
	return contract.New(contract.PRChangedOnGitHub,
		"PR #%d of %s holds %d commit(s) Ghostbranch didn't publish, and they can't be brought in by themselves: %s. Ghostbranch never resolves conflicts: merge the PR's branch into %s yourself (details.steps), push it to the shadow repo, then run feature pull again",
		u.PR, name, len(u.Foreign), why, name).WithField("feature").
		WithDetails(map[string]any{"feature": name, "branch": u.Branch, "public_pr": u.PR, "public_head": u.Head, "foreign": list,
			"reason": reason, "commit": commit, "files": files, "steps": steps})
}
