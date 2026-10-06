package engine

import (
	"fmt"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
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
			warns = append(warns, contract.Warning{Code: contract.WaiversUnreadable,
				Message: fmt.Sprintf("no waiver applied because %s could not be read (%v); a person fixes it or runs ghostbranch waivers revoke --all",
					e.Waivers.Path(), err)})
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

// location is where a finding is: path:line, the path, or the text field.
func location(f leak.Finding) string {
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
