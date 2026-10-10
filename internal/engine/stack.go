package engine

// Stacks: a feature stacked on another records it as its parent, with the
// parent's commit it forked from. These helpers read the records of a
// pair's stacks for fold, restack, the abandon and close refusals, the
// gate (Task 5) and sync (Task 7).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
)

// Why a stacked feature can't build on its parent (PARENT_REWRITTEN's
// details.reason).
const (
	reasonParentGone      = "parent_gone"      // the parent's record or branch is gone
	reasonParentRewritten = "parent_rewritten" // the parent no longer contains forked_at
	reasonForkPointLost   = "fork_point_lost"  // the feature itself no longer contains forked_at
)

// parentFeature is the feature m is stacked on, or "" when its parent is main.
func parentFeature(m feature.Meta) string {
	if m.Parent == "main" {
		return ""
	}
	return m.Parent
}

// stackRecord is one feature record as the stack helpers read it.
type stackRecord struct {
	name    string
	meta    feature.Meta
	metaSHA string
	head    string // "" when the branch is gone
	newer   bool   // written by a newer Ghostbranch: read, never written
}

// records reads every feature record in the mirror, by name. A record that
// doesn't parse is left out: this binary can't see what it is stacked on,
// and such a feature only closes at a console.
func (e *Engine) records(ctx context.Context, pc *pairCtx) (map[string]stackRecord, error) {
	const prefix = "refs/shadow-meta/features/"
	refs, err := pc.git.Refs(ctx, strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, internalErr(err, "list features")
	}
	out := make(map[string]stackRecord, len(refs))
	for ref := range refs {
		name := strings.TrimPrefix(ref, prefix)
		m, sha, newer, err := e.readMetaAny(ctx, pc, name)
		if err != nil {
			var bad *unreadableRecord
			if errors.As(err, &bad) {
				continue
			}
			return nil, err
		}
		head, ok, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
		if err != nil {
			return nil, internalErr(err, "read feature branch "+m.Branch)
		}
		if !ok {
			head = ""
		}
		out[name] = stackRecord{name: name, meta: m, metaSHA: sha, head: head, newer: newer}
	}
	return out, nil
}

// childrenOf lists the features stacked directly on name, sorted; abandoned
// ones only when withAbandoned. Never nil.
func childrenOf(recs map[string]stackRecord, name string, withAbandoned bool) []string {
	out := []string{}
	for n, r := range recs {
		if n != name && parentFeature(r.meta) == name && (withAbandoned || !r.meta.Abandoned) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// descendants lists every feature above name, abandoned ones included,
// depth first in name order: each child, then the features above it, so a
// feature always comes after its parent. A loop in the records is cut.
func descendants(recs map[string]stackRecord, name string) []string {
	out := []string{}
	seen := map[string]bool{name: true}
	var walk func(string)
	walk = func(n string) {
		for _, c := range childrenOf(recs, n, true) {
			if seen[c] {
				continue
			}
			seen[c] = true
			out = append(out, c)
			walk(c)
		}
	}
	walk(name)
	return out
}

// RebaseSteps are the git commands that rebase branch's own commits onto
// onto (a feature, or "main") in a checkout whose remote for the shadow
// repo is remote, then push the result leased. upstream is where branch's
// own commits start: a fork-point SHA, "main" for the shadow repo's main,
// or "" for a plain rebase. Ghostbranch never runs them: a person or their
// agent does, and resolves any conflict.
func RebaseSteps(remote, branch, onto, upstream string) [][]string {
	rebase := []string{"git", "rebase"}
	switch upstream {
	case "":
		rebase = append(rebase, remote+"/"+onto, branch)
	case "main":
		rebase = append(rebase, "--onto", remote+"/"+onto, remote+"/main", branch)
	default:
		rebase = append(rebase, "--onto", remote+"/"+onto, upstream, branch)
	}
	return [][]string{
		{"git", "fetch", remote},
		{"git", "switch", branch},
		rebase,
		{"git", "push", "--force-with-lease", remote, branch},
	}
}

// parentRewritten is PARENT_REWRITTEN: child can't build on parent, for
// reason. forkedAt is child's recorded fork point.
func parentRewritten(child, parent, forkedAt, reason string) error {
	var msg string
	switch reason {
	case reasonParentGone:
		msg = fmt.Sprintf("feature %s is stacked on %s, which is gone from the shadow repo; move it onto main with ghostbranch feature restack %s --onto main",
			child, parent, child)
	case reasonForkPointLost:
		msg = fmt.Sprintf("feature %s no longer contains its fork point %s from %s (it was rebased by hand); run ghostbranch feature restack %s to put it on %s's tip",
			child, short(forkedAt), parent, parent, parent)
	default:
		msg = fmt.Sprintf("feature %s was rewritten after %s was started from it, so it no longer contains the fork point %s; run ghostbranch feature restack %s",
			parent, child, short(forkedAt), parent)
	}
	return contract.New(contract.ParentRewritten, "%s", msg).WithField("feature").
		WithDetails(map[string]any{"feature": child, "parent": parent, "forked_at": forkedAt, "reason": reason})
}

// hasChildren is HAS_CHILDREN: name can't be op'd ("abandon" or "close")
// while kids are stacked on it.
func hasChildren(name string, kids []string, op string) error {
	done := map[string]string{"abandon": "abandoned", "close": "closed"}[op]
	return contract.New(contract.HasChildren,
		"feature %s has %d feature(s) stacked on it (%s), so it can't be %s; first, for each of them: fold it into %s (ghostbranch feature fold <child>), %s it, or move it onto main (ghostbranch feature restack <child> --onto main)",
		name, len(kids), strings.Join(kids, ", "), done, name, op).
		WithField("feature").WithDetails(map[string]any{"feature": name, "children": kids, "op": op})
}
