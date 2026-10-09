package engine

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// WaivedFinding is a finding a person waived, with their reason.
type WaivedFinding struct {
	leak.Finding
	Reason string `json:"reason"`
}

// splitFindings separates findings a stored waiver covers from the ones
// that block, using the waivers on disk right now. A waiver applies only to
// a waivable finding with the same id, rule, place and match hash. An
// unreadable waivers file applies no waiver and says so.
func (e *Engine) splitFindings(p workspace.Pair, fs []leak.Finding) ([]leak.Finding, []WaivedFinding, []contract.Warning) {
	blocking, waived := []leak.Finding{}, []WaivedFinding{}
	if len(fs) == 0 {
		return blocking, waived, nil
	}
	var ws map[string]waiver.Waiver
	var warns []contract.Warning
	if e.Waivers != nil {
		var err error
		if ws, err = e.Waivers.For(p.Public); err != nil {
			msg := fmt.Sprintf("no waiver applied because %s could not be read (%v); a person fixes it or runs ghostbranch waivers revoke --all",
				e.Waivers.Path(), err)
			if contract.From(err).Code == contract.UpgradeNeeded {
				msg = fmt.Sprintf("no waiver applied because %s was written by a newer Ghostbranch; upgrade ghostbranch on this machine (the waivers in it are kept)",
					e.Waivers.Path())
			}
			warns = append(warns, contract.Warning{Code: contract.WaiversUnreadable, Message: msg})
		}
	}
	for _, f := range fs {
		w, ok := ws[f.ID]
		if ok && f.Waivable && w.Rule == f.Rule && w.Where == f.Where && w.Path == f.Path && w.MatchSHA256 == f.MatchSHA256 {
			waived = append(waived, WaivedFinding{Finding: f, Reason: w.Reason})
			continue
		}
		blocking = append(blocking, f)
	}
	return blocking, waived, warns
}

func findingIDs(ws []WaivedFinding) []string {
	var out []string
	for _, w := range ws {
		out = append(out, w.ID)
	}
	return out
}

// Location is where a finding is: path:line, the path, or the text field.
func Location(f leak.Finding) string {
	switch {
	case f.Path != "" && f.Line > 0:
		return fmt.Sprintf("%s:%d", f.Path, f.Line)
	case f.Path != "":
		return f.Path
	}
	return f.Where
}

func planWaived(ws []WaivedFinding) []plan.Waived {
	out := []plan.Waived{}
	for _, w := range ws {
		out = append(out, plan.Waived{ID: w.ID, Rule: w.Rule, Where: w.Where, Path: w.Path, Reason: w.Reason})
	}
	return out
}

var findingIDPattern = regexp.MustCompile(`^f-[0-9a-f]{12}$`)

type WaiversAddOptions struct {
	Feature string
	IDs     []string
	Reason  string
}

type WaiversAddResult struct {
	Feature string           `json:"feature"`
	Waivers []waiver.Waiver  `json:"waivers"`
	Preview *preview.Preview `json:"preview"`
}

type WaiversListResult struct {
	Pair    string          `json:"pair"`
	Waivers []waiver.Waiver `json:"waivers"`
}

type WaiversRevokeResult struct {
	Revoked []waiver.Waiver  `json:"revoked"`
	Preview *preview.Preview `json:"preview"`
}

// normalizeIDs lower-cases and de-duplicates finding ids, and checks
// their form.
func normalizeIDs(in []string) ([]string, error) {
	var ids []string
	seen := map[string]bool{}
	for _, id := range in {
		id = strings.ToLower(strings.TrimSpace(id))
		if !findingIDPattern.MatchString(id) {
			return nil, contract.New(contract.BadFlag, "%q is not a finding id such as f-3a9c51d0e2b4", id).WithField("id")
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// ValidateWaiversAdd checks everything about waivers add that needs no
// network, so bad input fails before any token or console is needed.
func ValidateWaiversAdd(opt WaiversAddOptions) ([]string, string, error) {
	if len(opt.IDs) == 0 {
		return nil, "", contract.New(contract.MissingInput, "name the finding ids to waive, from error.details.waivable of the gate").WithField("id")
	}
	ids, err := normalizeIDs(opt.IDs)
	if err != nil {
		return nil, "", err
	}
	reason := strings.TrimSpace(opt.Reason)
	if reason == "" {
		return nil, "", contract.New(contract.MissingInput, "say why these findings are safe to publish: pass --reason").WithField("reason")
	}
	return ids, reason, nil
}

func (e *Engine) waivers() (*waiver.Store, error) {
	if e.Waivers == nil {
		return nil, contract.New(contract.Internal, "waivers are not available")
	}
	return e.Waivers, nil
}

// WaiversAdd waives findings in what the feature would publish now (spec
// §7.2). It never trusts recorded results: it rebases, strips and scans
// again, with no push and no CI, and shows the person what it found. Only a
// person at a console can confirm; --yes and grants never apply.
func (e *Engine) WaiversAdd(ctx context.Context, p workspace.Pair, opt WaiversAddOptions) (res *WaiversAddResult, warns []contract.Warning, err error) {
	reason := strings.TrimSpace(opt.Reason)
	defer func() {
		var ids []string
		summary := ""
		if err == nil && res != nil {
			for _, w := range res.Waivers {
				ids = append(ids, w.ID)
			}
			summary = fmt.Sprintf("waived %d finding(s) in %s", len(ids), opt.Feature)
		}
		e.recordWith("waivers add", &p, opt.Feature, "", summary, err, func(ent *ledger.Entry) {
			ent.Waivers, ent.Reason = ids, reason
			if len(ids) > 0 {
				ent.Approval = &ledger.Approval{Kind: "console"}
			}
		})
	}()
	ids, reason, err := ValidateWaiversAdd(opt)
	if err != nil {
		return nil, nil, err
	}
	s, err := e.waivers()
	if err != nil {
		return nil, nil, err
	}
	if !e.DryRun && e.Approver == nil {
		// An agent's shell: hand it to a person before any network call.
		return nil, nil, noConsole("waiving a leak finding")
	}
	pc, err := e.open(ctx, "waivers add", p, true)
	if err != nil {
		return nil, nil, err
	}
	defer pc.unlock()
	// Scan as preserve whatever the default mode: finding ids don't depend
	// on mode or line, so the preserve scan holds every waivable finding any
	// promote mode can raise.
	b, warns, err := e.build(ctx, pc, opt.Feature, config.ModePreserve, true)
	if err != nil {
		// A broken marker in an older commit only blocks a preserve publish.
		// Under a squash or curated default, scan just the final tree, as
		// that promote would; a marker broken in the tip itself still fails.
		ce := contract.From(err)
		commit, _ := ce.Details["commit"].(string)
		if ce.Code != contract.MarkerInvalid || b == nil || b.cfg.Promote.DefaultMode == config.ModePreserve || commit == "" || commit == b.head {
			return nil, warns, err
		}
		if b, warns, err = e.build(ctx, pc, opt.Feature, config.ModeSquash, true); err != nil {
			return nil, warns, err
		}
	}
	byID := map[string]leak.Finding{}
	for _, f := range b.report.Findings {
		if f.Waivable {
			byID[f.ID] = f
		}
	}
	var picked []leak.Finding
	var unknown []string
	for _, id := range ids {
		if f, ok := byID[id]; ok {
			picked = append(picked, f)
		} else {
			unknown = append(unknown, id)
		}
	}
	if len(unknown) > 0 {
		return nil, warns, contract.New(contract.WaiverUnknown,
			"%s: not a waivable finding in any commit %s would publish now; gate again for the current ids", strings.Join(unknown, ", "), opt.Feature).
			WithField("id").WithDetails(map[string]any{"ids": unknown, "feature": opt.Feature})
	}
	now := e.now()
	res = &WaiversAddResult{Feature: opt.Feature, Waivers: []waiver.Waiver{}}
	for _, f := range picked {
		res.Waivers = append(res.Waivers, waiver.Waiver{ID: f.ID, Public: p.Public, Rule: f.Rule, Where: f.Where, Path: f.Path,
			MatchSHA256: f.MatchSHA256, Reason: reason, CreatedAt: now, CreatedBy: e.Actor})
	}
	if e.DryRun {
		res.Preview = preview.New(map[string]string{"public_main": b.pm, "feature_head": b.head, "overlay": b.O})
		for _, w := range res.Waivers {
			res.Preview.Add(preview.Waive(p.Public, w.ID))
		}
		return res, warns, nil
	}
	// A waivers file that cannot be read could not be saved to either; say
	// so before the person types a code.
	if _, err := s.Load(); err != nil {
		return nil, warns, internalErr(err, "read waivers")
	}
	if err := e.confirm(ctx, "", waiveSummary(p, opt.Feature, picked, ids, reason)); err != nil {
		return nil, warns, err
	}
	for i := range res.Waivers {
		res.Waivers[i].Approval = &ledger.Approval{Kind: "console"}
	}
	if err := s.Add(res.Waivers, now); err != nil {
		return nil, warns, grantErr(err, "save the waivers")
	}
	return res, warns, nil
}

// waiveSummary is what the person sees before typing the code. A secret
// shows only its first and last four characters; a block-pattern match is
// shown whole, because it is not a secret.
func waiveSummary(p workspace.Pair, feat string, fs []leak.Finding, ids []string, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Waive %d leak finding(s) in %s for %s, so they no longer block a publish:\n", len(fs), feat, p.Public)
	for _, f := range fs {
		shown := f.Hint
		if f.Match != "" {
			shown = fmt.Sprintf("%q", f.Match)
		}
		fmt.Fprintf(&b, "  %s  %s  %s  %s\n", f.ID, f.Rule, Location(f), shown)
	}
	fmt.Fprintf(&b, "Reason: %s\n", reason)
	fmt.Fprintf(&b, "Each waiver covers exactly this content in this file; changed content is a new finding.\n")
	fmt.Fprintf(&b, "Undo with: ghostbranch waivers revoke %s --repo %s", strings.Join(ids, " "), p.Public)
	return b.String()
}

// WaiversList lists stored waivers, for one pair or all. It needs no
// network.
func (e *Engine) WaiversList(ctx context.Context, p *workspace.Pair) (*WaiversListResult, []contract.Warning, error) {
	s, err := e.waivers()
	if err != nil {
		return nil, nil, err
	}
	ws, err := s.Load()
	if err != nil {
		return nil, nil, internalErr(err, "read waivers")
	}
	res := &WaiversListResult{Waivers: []waiver.Waiver{}}
	if p != nil {
		res.Pair = p.Public
	}
	for _, w := range ws {
		if p == nil || strings.EqualFold(w.Public, p.Public) {
			res.Waivers = append(res.Waivers, w)
		}
	}
	return res, nil, nil
}

// WaiversRevoke removes waivers by id for one pair, or every waiver with
// all. Taking a waiver away only makes the gate stricter, so it needs no
// console. An id with no waiver for the repo is WAIVER_UNKNOWN, and nothing
// is revoked. With all it also clears a waivers.json that does not decode.
func (e *Engine) WaiversRevoke(ctx context.Context, p *workspace.Pair, ids []string, all bool) (res *WaiversRevokeResult, warns []contract.Warning, err error) {
	defer func() {
		var revoked []string
		summary := ""
		if err == nil && res != nil {
			for _, w := range res.Revoked {
				revoked = append(revoked, w.ID)
			}
			summary = fmt.Sprintf("revoked %d waiver(s)", len(revoked))
		}
		e.recordWith("waivers revoke", p, "", "", summary, err, func(ent *ledger.Entry) { ent.Waivers = revoked })
	}()
	switch {
	case all && len(ids) > 0:
		return nil, nil, contract.New(contract.BadFlag, "name waiver ids or pass --all, not both").WithField("id")
	case !all && len(ids) == 0:
		return nil, nil, contract.New(contract.MissingInput, "name the waiver ids to revoke, or pass --all").WithField("id")
	case !all && p == nil:
		return nil, nil, contract.New(contract.BadFlag, "name a repo with --repo").WithField("repo")
	}
	if ids, err = normalizeIDs(ids); err != nil {
		return nil, nil, err
	}
	s, err := e.waivers()
	if err != nil {
		return nil, nil, err
	}
	public := ""
	if p != nil {
		public = p.Public
	}
	res = &WaiversRevokeResult{Revoked: []waiver.Waiver{}}
	if e.DryRun {
		ws, err := s.Load()
		res.Preview = preview.New(nil)
		switch {
		case err != nil && all && waiver.Corrupt(err):
			// revoke --all replaces a waivers.json that doesn't decode.
			res.Preview.Add(preview.WriteFile(s.Path()))
			return res, nil, nil
		case err != nil:
			return nil, nil, grantErr(err, "read waivers")
		}
		if !all {
			if unknown := waiver.Unknown(ws, public, ids); len(unknown) > 0 {
				return nil, nil, waiver.UnknownError(public, unknown)
			}
		}
		for _, w := range ws {
			if waiver.Matches(w, public, ids, all) {
				res.Revoked = append(res.Revoked, w)
				res.Preview.Add(preview.Unwaive(w.Public, w.ID))
			}
		}
		return res, nil, nil
	}
	if all {
		res.Revoked, err = s.Revoke(public, nil, true, e.now())
	} else {
		res.Revoked, err = s.RevokeKnown(public, ids, e.now())
	}
	if err != nil {
		return nil, nil, grantErr(err, "revoke waivers")
	}
	return res, nil, nil
}
