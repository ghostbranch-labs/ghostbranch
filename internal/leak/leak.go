// Package leak scans exactly what will be published: the tree, commit
// metadata, branch name and PR text. Secret values never leave this
// package; findings carry Redacted instead.
package leak

import (
	"runtime/debug"
	"sort"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"

	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
)

const Redacted = "[REDACTED]"

// Finding is one leak. Where is "tree" for files, otherwise the text field.
type Finding struct {
	Rule   string `json:"rule"`
	Where  string `json:"where"`
	Path   string `json:"path,omitempty"`
	Line   int    `json:"line,omitempty"`
	Secret string `json:"secret,omitempty"`
}

type File struct {
	Path    string
	Content []byte
}

type Text struct {
	Where   string
	Content string
}

// Input is everything that will become public. Paths lists every published
// path (for the strip check); Files holds the blobs to scan.
type Input struct {
	Paths []string
	Files []File
	Texts []Text
}

type Scanner interface {
	Scan(Input) ([]Finding, error)
	Version() string
}

// Gitleaks scans with Gitleaks' default rules, and flags any overlay path
// still present in the published tree.
type Gitleaks struct {
	det *detect.Detector
	set overlay.Set
}

func NewGitleaks(set overlay.Set) (*Gitleaks, error) {
	d, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, err
	}
	dropPathAllowlists(d)
	return &Gitleaks{det: d, set: set}, nil
}

// dropPathAllowlists removes every path allowlist entry from the detector's
// global and per-rule allowlists. The default config skips go.mod, vendor/,
// node_modules, images and more by path, but every published path must be
// scanned. Commit, regex and stopword allowlist parts are kept. Allowlists are
// rebuilt rather than edited because gitleaks compiles Paths on Validate.
func dropPathAllowlists(d *detect.Detector) {
	d.Config.Allowlists = withoutPaths(d.Config.Allowlists)
	for id, r := range d.Config.Rules {
		r.Allowlists = withoutPaths(r.Allowlists)
		d.Config.Rules[id] = r
	}
}

func withoutPaths(in []*config.Allowlist) []*config.Allowlist {
	var out []*config.Allowlist
	for _, a := range in {
		if len(a.Paths) == 0 {
			out = append(out, a)
			continue
		}
		if a.MatchCondition == config.AllowlistMatchAnd {
			continue // path was a required condition; without it, drop the whole entry
		}
		c := &config.Allowlist{
			Description: a.Description, MatchCondition: a.MatchCondition,
			Commits: a.Commits, RegexTarget: a.RegexTarget, Regexes: a.Regexes, StopWords: a.StopWords,
		}
		if c.Validate() != nil {
			continue // nothing but paths was left
		}
		out = append(out, c)
	}
	return out
}

func (g *Gitleaks) Scan(in Input) ([]Finding, error) {
	var out []Finding
	for _, p := range in.Paths {
		if g.set.Match(p) {
			out = append(out, Finding{Rule: "overlay-path", Where: "tree", Path: p})
		}
	}
	for _, f := range in.Files {
		text, enc := Decode(f.Content)
		if enc == Binary {
			continue // printable runs are scanned from Task 5 on
		}
		for _, lf := range g.detect(text, f.Path) {
			// gitleaks StartLine is 0-based; findings are 1-based.
			out = append(out, Finding{Rule: lf.RuleID, Where: "tree", Path: f.Path, Line: lf.StartLine + 1, Secret: Redacted})
		}
	}
	for _, t := range in.Texts {
		for _, lf := range g.detect(t.Content, "") {
			// gitleaks StartLine is 0-based; findings are 1-based.
			out = append(out, Finding{Rule: lf.RuleID, Where: t.Where, Line: lf.StartLine + 1, Secret: Redacted})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Where != b.Where {
			return a.Where < b.Where
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Rule < b.Rule
	})
	return out, nil
}

// detect runs the detector on one fragment. Detector.Detect in gitleaks
// v8.30.1 takes the deprecated detect.Fragment alias, so the deprecation is
// confined here until the API moves to sources.Fragment in v9.
func (g *Gitleaks) detect(raw, path string) []report.Finding {
	return g.det.Detect(detect.Fragment{Raw: raw, FilePath: path}) //nolint:staticcheck // detect.Fragment is deprecated until gitleaks v9; Detect still requires it
}

// Version names the scanner and its rule set, for plans.
func (g *Gitleaks) Version() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/zricethezav/gitleaks/v8" {
				return "gitleaks " + d.Version
			}
		}
	}
	return "gitleaks (unknown version)"
}
