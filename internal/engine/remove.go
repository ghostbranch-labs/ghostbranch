package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// RemoveResult is what remove took off this machine. Grants and Waivers
// count what was revoked; Plans are the plan files removed and Dir the
// pair's directory (mirror, work dirs, lock).
type RemoveResult struct {
	Public  string           `json:"public"`
	Shadow  string           `json:"shadow"`
	Grants  int              `json:"grants"`
	Waivers int              `json:"waivers"`
	Plans   []string         `json:"plans"`
	Dir     string           `json:"dir"`
	Preview *preview.Preview `json:"preview"`
}

// Remove unregisters p from this machine: it revokes the pair's agent
// grants and waivers, removes its plan files and its directory, and drops
// it from workspace.json. It touches nothing remote, and the ledger keeps
// its history. It drops grants and waivers, so it needs a person at a
// console; --plan does not.
func (e *Engine) Remove(ctx context.Context, p workspace.Pair) (res *RemoveResult, warns []contract.Warning, err error) {
	var waived []string
	var revoked []grant.Grant
	defer func() {
		summary := ""
		if err == nil && res != nil && res.Preview == nil {
			summary = fmt.Sprintf("removed %s from this machine: %d grant(s), %d waiver(s), %d plan file(s); the shadow repo %s is untouched",
				p.Public, res.Grants, res.Waivers, len(res.Plans), p.Shadow)
		}
		// Each revoked grant gets its own entry, as agents revoke writes, even
		// when remove then failed: the grants are gone.
		for _, g := range revoked {
			pair := workspace.Pair{Public: g.Public, Shadow: g.Shadow}
			ref := grantRef(g)
			e.recordWith("grant_revoked", &pair, "", "", "agents may no longer publish to "+g.Public, nil, func(ent *ledger.Entry) { ent.Grant = ref })
		}
		e.recordWith("remove", &p, "", "", summary, err, func(ent *ledger.Entry) { ent.Waivers = waived })
	}()
	reg, ok := e.WS.Find(p.Public)
	if !ok {
		return nil, nil, contract.New(contract.PairAmbiguous, "no repo %q in workspace %q", p.Public, e.WS.Name).WithField("repo")
	}
	p = reg
	// Before the console, the lock and any change: a refused remove
	// changes nothing.
	if err := e.checkPairDir(p); err != nil {
		return nil, nil, err
	}
	gstore, err := e.grants()
	if err != nil {
		return nil, nil, err
	}
	wstore, err := e.waivers()
	if err != nil {
		return nil, nil, err
	}
	if !e.DryRun && e.Approver == nil {
		return nil, nil, noConsole("removing a pair from this machine")
	}
	if !e.DryRun {
		// No other operation on the pair while its files go; waivers add
		// takes this lock too. The lock file sits in the workspace's locks
		// folder, outside the pair folder removed below, so it stays, and
		// the deferred unlock releases only this run's own hold.
		unlock, err := workspace.Lock(e.WS.LockPath(p), e.Now(), "remove")
		if err != nil {
			return nil, nil, err
		}
		defer unlock()
	}
	now := e.now()
	gs, err := gstore.Load()
	if err != nil {
		return nil, nil, grantErr(err, "read agent grants")
	}
	var grants []string
	for _, g := range gs {
		if strings.EqualFold(g.Public, p.Public) && g.ValidAt(now) {
			grants = append(grants, g.Shadow)
		}
	}
	byID, err := wstore.For(p.Public)
	if err != nil {
		return nil, nil, grantErr(err, "read waivers")
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	plans, unreadable, err := pairPlans(e.WS.PlansDir(), p.Public)
	if err != nil {
		return nil, nil, internalErr(err, "list plan files")
	}
	for _, u := range unreadable {
		warns = append(warns, planFileUnreadable(u, "if it is a plan of "+p.Public+", delete it by hand"))
	}
	res = &RemoveResult{Public: p.Public, Shadow: p.Shadow, Grants: len(grants), Waivers: len(ids), Plans: plans, Dir: e.WS.PairDir(p)}
	if e.DryRun {
		res.Preview = preview.New(nil)
		for _, sh := range grants {
			res.Preview.Add(preview.Revoke(p.Public, sh))
		}
		for _, id := range ids {
			res.Preview.Add(preview.Unwaive(p.Public, id))
		}
		for _, path := range plans {
			res.Preview.Add(preview.RemoveFile(path))
		}
		res.Preview.Add(preview.RemoveFile(res.Dir), preview.WriteFile(filepath.Join(e.WS.Dir, "workspace.json")))
		return res, warns, nil
	}
	if err := e.confirm(ctx, "", removeSummary(e.WS.Name, res, ids)); err != nil {
		return nil, nil, err
	}
	// Revoke even when none was in force at the look above: agents allow
	// does not take the pair lock, and a grant left behind would come back
	// with the pair.
	rg, err := gstore.Revoke(p.Public, false, now)
	if err != nil {
		return nil, nil, grantErr(err, "revoke agent grants")
	}
	revoked, res.Grants = rg, len(rg)
	if len(ids) > 0 {
		gone, err := wstore.Revoke(p.Public, ids, false, now)
		if err != nil {
			return nil, nil, partway(grantErr(err, "revoke waivers"), p.Public)
		}
		res.Waivers = len(gone)
		for _, w := range gone {
			waived = append(waived, w.ID)
		}
	}
	for _, path := range plans {
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, nil, partway(internalErr(err, "remove plan file "+path), p.Public)
		}
	}
	if err := os.RemoveAll(res.Dir); err != nil {
		return nil, nil, partway(internalErr(err, "remove "+res.Dir), p.Public)
	}
	// Last, so a remove that failed above can simply be run again.
	if err := e.WS.Update(e.Now(), func(w *workspace.Workspace) bool { return w.RemovePair(p.Public) }); err != nil {
		return nil, nil, partway(updateErr(err, "save workspace.json"), p.Public)
	}
	return res, warns, nil
}

// partway marks an error after remove began revoking: each step is safe to
// repeat, and the pair stays registered until the last one, so running
// remove again finishes it.
func partway(err error, public string) error {
	ce := contract.From(err)
	ce.Message += "; remove stopped partway (the pair's agent grants are already revoked): run ghostbranch remove --repo " + public + " again to finish"
	return ce
}

// checkPairDir refuses a pair whose folder would not be a direct child of
// the workspace's pairs folder. workspace.json is local and unchecked, and
// an entry such as ".." would otherwise send RemoveAll at the workspace
// itself, or further. A key ending in a dot or a space is refused too:
// Windows drops those from a path, so the folder would be another one
// (owner/repo. would be owner/repo's).
func (e *Engine) checkPairDir(p workspace.Pair) error {
	dir, key := e.WS.PairDir(p), p.Key()
	_, _, nameErr := forge.SplitName(p.Public)
	if nameErr != nil || strings.HasSuffix(key, ".") || strings.HasSuffix(key, " ") || strings.ContainsAny(key, `/\`) ||
		filepath.Dir(dir) != filepath.Join(e.WS.Dir, "pairs") || filepath.Base(dir) != key {
		return contract.New(contract.Internal,
			"refusing to remove %q: workspace.json names a repo whose folder %q is not inside %q; fix workspace.json by hand",
			p.Public, dir, filepath.Join(e.WS.Dir, "pairs"))
	}
	return nil
}

// pairPlans lists the plan files in dir made for public, by path, and the
// plan files it couldn't read.
func pairPlans(dir, public string) ([]string, []unreadablePlan, error) {
	files, unreadable, err := planFiles(dir)
	if err != nil {
		return nil, nil, err
	}
	out := []string{}
	for _, f := range files {
		if strings.EqualFold(f.Pair.Public, public) {
			out = append(out, f.Path)
		}
	}
	return out, unreadable, nil
}

// planFile is the part of a plan file that says whose it is, and what it
// publishes where.
type planFile struct {
	ID      string    `json:"id"`
	Pair    plan.Pair `json:"pair"`
	Feature string    `json:"feature"`
	Publish struct {
		Commit       string `json:"commit"`
		TargetBranch string `json:"target_branch"`
	} `json:"publish"`
	Path string `json:"-"`
}

// unreadablePlan is a plan file planFiles couldn't read.
type unreadablePlan struct {
	path string
	err  error
}

// planFiles decodes every plan file in dir, in name order. A file that
// isn't JSON is left out, and so is one that can't be read: that one is
// returned in unreadable, for the caller's PLAN_FILE_UNREADABLE warning.
// Nothing here is checked: the files are local and unsigned.
func planFiles(dir string) ([]planFile, []unreadablePlan, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var out []planFile
	var unreadable []unreadablePlan
	for _, ent := range entries {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, ent.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			unreadable = append(unreadable, unreadablePlan{path: path, err: err})
			continue
		}
		var f planFile
		if json.Unmarshal(b, &f) == nil {
			f.Path = path
			out = append(out, f)
		}
	}
	return out, unreadable, nil
}

// planFileUnreadable is the warning for a plan file that was skipped; what
// says what that may leave behind.
func planFileUnreadable(u unreadablePlan, what string) contract.Warning {
	return contract.Warning{Code: contract.PlanFileUnreadable,
		Message: fmt.Sprintf("plan file %s couldn't be read (%v), so it was skipped; %s", u.path, u.err, what),
		Details: map[string]any{"path": u.path}}
}

// removeSummary is what the person sees before typing the code.
func removeSummary(ws string, r *RemoveResult, waivers []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Remove %s (shadow %s) from workspace %q on this machine:\n", visible(r.Public), visible(r.Shadow), visible(ws))
	fmt.Fprintf(&b, "  revoke   %d agent grant(s)\n", r.Grants)
	fmt.Fprintf(&b, "  revoke   %d waiver(s)", r.Waivers)
	if len(waivers) > 0 {
		fmt.Fprintf(&b, ": %s", strings.Join(waivers, " "))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "  delete   %d plan file(s)\n", len(r.Plans))
	fmt.Fprintf(&b, "  delete   %s (the mirror and work files)\n", visible(r.Dir))
	b.WriteString("Nothing on GitHub changes, and the ledger keeps its history.\n")
	fmt.Fprintf(&b, "Set the pair up here again with: ghostbranch init %s --adopt --shadow %s", visible(r.Public), visible(r.Shadow))
	return b.String()
}

// DefaultLogLimit is how many entries log shows unless told otherwise.
const DefaultLogLimit = 50

// LogOptions filter the ledger. Feature and Op match exactly, Pair (a
// public repo) ignoring case, ActorPrefix as a prefix; a zero Since is no
// limit, and Limit 0 is DefaultLogLimit (the CLI asks for 1 or more).
type LogOptions struct {
	Feature, Op, ActorPrefix, Pair string
	Since                          time.Time
	Limit                          int
}

type LogResult struct {
	Entries []ledger.Entry `json:"entries"` // newest first, never null
}

// Log reads this workspace's ledger, newest first. It needs no pair, no
// token and no network.
func Log(ws *workspace.Workspace, o LogOptions) (*LogResult, error) {
	all, err := ledger.Read(ws.LedgerPath())
	if err != nil {
		return nil, internalErr(err, "read the ledger "+ws.LedgerPath())
	}
	limit := o.Limit
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	res := &LogResult{Entries: []ledger.Entry{}}
	for i := len(all) - 1; i >= 0 && len(res.Entries) < limit; i-- {
		ent := all[i]
		switch {
		case o.Feature != "" && ent.Feature != o.Feature,
			o.Op != "" && ent.Op != o.Op,
			o.Pair != "" && !strings.EqualFold(ent.Pair, o.Pair),
			!strings.HasPrefix(ent.Actor, o.ActorPrefix),
			!o.Since.IsZero() && ent.TS.Before(o.Since):
			continue
		}
		res.Entries = append(res.Entries, ent)
	}
	return res, nil
}

// ParseSince reads log's --since: a positive duration back from now, such
// as 24h, or an RFC 3339 time. "" is no limit.
func ParseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, contract.New(contract.BadFlag,
		"--since %q is neither a duration such as 24h nor an RFC 3339 time such as 2026-10-01T00:00:00Z", s).WithField("since")
}
