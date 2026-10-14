package engine

// Stacks: a feature stacked on another records it as its parent, with the
// parent's commit it forked from. These helpers read the records of a
// pair's stacks for fold, restack, the abandon and close refusals, the
// gate (Task 5) and sync (Task 7).

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
// and such a feature only closes at a console. A caller that would strand
// a child it can't see uses readableRecords instead.
func (e *Engine) records(ctx context.Context, pc *pairCtx) (map[string]stackRecord, error) {
	out, _, err := e.recordsAndUnreadable(ctx, pc)
	return out, err
}

// recordsAndUnreadable is records, plus the names of the records left out
// because they don't parse, sorted.
func (e *Engine) recordsAndUnreadable(ctx context.Context, pc *pairCtx) (map[string]stackRecord, []string, error) {
	const prefix = "refs/shadow-meta/features/"
	refs, err := pc.git.Refs(ctx, strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return nil, nil, internalErr(err, "list features")
	}
	out := make(map[string]stackRecord, len(refs))
	unreadable := []string{}
	for ref := range refs {
		name := strings.TrimPrefix(ref, prefix)
		m, sha, newer, err := e.readMetaAny(ctx, pc, name)
		if err != nil {
			var bad *unreadableRecord
			if errors.As(err, &bad) {
				unreadable = append(unreadable, name)
				continue
			}
			return nil, nil, err
		}
		head, ok, err := pc.git.Resolve(ctx, shadowRef(m.Branch))
		if err != nil {
			return nil, nil, internalErr(err, "read feature branch "+m.Branch)
		}
		if !ok {
			head = ""
		}
		out[name] = stackRecord{name: name, meta: m, metaSHA: sha, head: head, newer: newer}
	}
	sort.Strings(unreadable)
	return out, unreadable, nil
}

// readableRecords is records for an op that deletes name's branch (close,
// fold): it refuses while another feature's record can't be read, as that
// feature may be stacked on name and would be stranded on a deleted
// branch. name's own record may be the unreadable one (a console close).
func (e *Engine) readableRecords(ctx context.Context, pc *pairCtx, name, op string) (map[string]stackRecord, error) {
	recs, unreadable, err := e.recordsAndUnreadable(ctx, pc)
	if err != nil {
		return nil, err
	}
	others := slices.DeleteFunc(unreadable, func(n string) bool { return n == name })
	if len(others) == 0 {
		return recs, nil
	}
	return nil, unreadableChildren(name, op, others)
}

// unreadableChildren is HAS_CHILDREN for an op ("close", "fold" or
// "archive") on name while others' records can't be read: any of them may
// be a child, so none is known by name.
func unreadableChildren(name, op string, others []string) error {
	done := map[string]string{"close": "closed", "fold": "folded", "archive": "archived"}[op]
	return contract.New(contract.HasChildren,
		"%s: %s corrupt, so it can't be told whether %s stacked on %s, and %s can't be %s until %s dealt with; a person closes %s at the console (%s), or %s fixed",
		unreadableWhat(others), itOrThey(others, "it is", "they are"), itOrThey(others, "it is", "they are"), name, name, done,
		itOrThey(others, "it is", "they are"), itOrThey(others, "it", "each"), consoleCloses(others),
		itOrThey(others, "its record is", "their records are")).WithField("feature").
		WithDetails(map[string]any{"feature": name, "op": op, "children": []string{}, "unreadable": others})
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
	switch {
	case reason == reasonParentGone:
		msg = fmt.Sprintf("feature %s is stacked on %s, which is gone from the shadow repo; move it onto main with ghostbranch feature restack %s --onto main",
			child, parent, child)
	case reason == reasonForkPointLost && parent == feature.MainParent:
		// Moved onto main off a parent: only its own commits were left.
		msg = fmt.Sprintf("feature %s no longer contains its fork point %s from the feature it was moved off (it was rebased by hand); once it sits on main's tip, run ghostbranch feature restack %s --onto main to record it there",
			child, short(forkedAt), child)
	case reason == reasonForkPointLost:
		msg = fmt.Sprintf("feature %s no longer contains its fork point %s from %s (it was rebased by hand); run ghostbranch feature restack %s to put it on %s's tip",
			child, short(forkedAt), parent, parent, parent)
	default:
		msg = fmt.Sprintf("feature %s was rewritten after %s was started from it, so it no longer contains the fork point %s; run ghostbranch feature restack %s",
			parent, child, short(forkedAt), parent)
	}
	return contract.New(contract.ParentRewritten, "%s", msg).WithField("feature").
		WithDetails(map[string]any{"feature": child, "parent": parent, "forked_at": forkedAt, "reason": reason})
}

// hasChildren is HAS_CHILDREN: name can't be op'd ("abandon", "close" or
// "archive") while kids are stacked on it.
func hasChildren(name string, kids []string, op string) error {
	d := map[string]any{"feature": name, "children": kids, "op": op}
	if op == "archive" {
		// Archiving deletes the branch they are built on (R8, P6).
		return contract.New(contract.HasChildren,
			"feature %s has %d feature(s) stacked on it (%s), so it can't be archived; first, for each of them: publish it with %s (ghostbranch promote %s <child> --plan) and archive it, move it onto main (ghostbranch feature restack <child> --onto main), or, if it is abandoned, close it (ghostbranch feature close <child>, at the console)",
			name, len(kids), strings.Join(kids, ", "), name, name).
			WithField("feature").WithDetails(d)
	}
	done := map[string]string{"abandon": "abandoned", "close": "closed"}[op]
	return contract.New(contract.HasChildren,
		"feature %s has %d feature(s) stacked on it (%s), so it can't be %s; first, for each of them: fold it into %s (ghostbranch feature fold <child>), %s it, or move it onto main (ghostbranch feature restack <child> --onto main)",
		name, len(kids), strings.Join(kids, ", "), done, name, op).
		WithField("feature").WithDetails(d)
}
