// Package leak scans exactly what will be published: the tree, its paths,
// commit metadata, branch name and PR text. Secret values never leave this
// package: findings carry Redacted, a hash of the match, and a hint that
// only the console shows.
package leak

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"regexp"
	"runtime/debug"
	"sort"
	"strings"
	"sync"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
)

const Redacted = "[REDACTED]"

// BlockRule is the rule name of a block-pattern finding.
const BlockRule = "block-pattern"

// Rules is what the overlay config asks the scan to check.
type Rules struct {
	Overlay       overlay.Set
	BlockPatterns []string
	// LargeFile is the size above which a new or changed blob warns; 0 never warns.
	LargeFile int64
}

// Finding is one leak. Where is "tree" for file content, "path" for a file
// name, otherwise the text field. Only tree and path findings can be
// waived; text the person typed is changed instead.
type Finding struct {
	ID       string `json:"id"`
	Rule     string `json:"rule"`
	Where    string `json:"where"`
	Path     string `json:"path,omitempty"`
	Line     int    `json:"line,omitempty"`
	Secret   string `json:"secret,omitempty"`
	Match    string `json:"match,omitempty"`
	Waivable bool   `json:"waivable"`
	// Commits are the feature commits (source SHAs) that introduce or
	// change this finding (a later commit that only carries it unchanged is
	// not listed). The engine fills it; empty for the branch name, PR text
	// and a squash tree.
	Commits []string `json:"commits,omitempty"`
	// MatchSHA256 binds a waiver to the exact content. Hint is a secret's
	// first and last four characters, for the person at the console.
	// Neither is ever serialized.
	MatchSHA256 string `json:"-"`
	Hint        string `json:"-"`
}

// File is one published blob. Changed is true when it is new or differs
// from public main; only changed files raise warnings.
type File struct {
	Path    string
	Content []byte
	Changed bool
}

type Text struct {
	Where   string
	Content string
}

// Input is everything that will become public. Paths lists every published
// path; Files holds the blobs to scan.
type Input struct {
	Paths []string
	Files []File
	Texts []Text
}

// Report is a scan's result: findings block a publish, warnings never do.
type Report struct {
	Findings []Finding
	Warnings []contract.Warning
}

type Scanner interface {
	Scan(Input) (Report, error)
	Version() string
}

// Gitleaks scans with Gitleaks' default rules plus the block patterns, and
// flags any overlay path still present in the published tree.
type Gitleaks struct {
	det      *detect.Detector
	rules    Rules
	patterns []*regexp.Regexp
}

// gitleaks builds its default detector through a global viper instance, so
// detectors are built one at a time.
var newDetectorMu sync.Mutex

func NewGitleaks(r Rules) (*Gitleaks, error) {
	newDetectorMu.Lock()
	d, err := detect.NewDetectorDefaultConfig()
	newDetectorMu.Unlock()
	if err != nil {
		return nil, err
	}
	dropPathAllowlists(d)
	g := &Gitleaks{det: d, rules: r}
	for _, p := range r.BlockPatterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("block pattern %q: %w", p, err)
		}
		g.patterns = append(g.patterns, re)
	}
	return g, nil
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

func (g *Gitleaks) Scan(in Input) (Report, error) {
	var out []Finding
	for _, p := range in.Paths {
		if g.rules.Overlay.Match(p) {
			out = append(out, newFinding("overlay-path", "tree", p, 0, p, false))
		}
		for _, m := range g.blocked(p) {
			f := newFinding(BlockRule, "path", p, 0, m, true)
			f.Match = m
			out = append(out, f)
		}
	}
	var workflows, large, binaries []string
	for _, f := range in.Files {
		text, enc := Decode(f.Content)
		out = append(out, g.text(text, "tree", f.Path, true)...)
		if enc != Plain {
			// The decoder's heuristics can misread a blob, so the printable
			// runs of anything that is not plain text are scanned too.
			out = append(out, g.text(Runs(f.Content), "tree", f.Path, false)...)
		}
		if !f.Changed {
			continue
		}
		if strings.HasPrefix(f.Path, ".github/workflows/") {
			workflows = append(workflows, f.Path)
		}
		if g.rules.LargeFile > 0 && int64(len(f.Content)) > g.rules.LargeFile {
			large = append(large, f.Path)
		}
		if enc == Binary {
			binaries = append(binaries, f.Path)
		}
	}
	for _, t := range in.Texts {
		out = append(out, g.text(t.Content, t.Where, "", true)...)
	}
	rep := Report{Findings: dedupe(out), Warnings: []contract.Warning{}}
	warn := func(code contract.WarningCode, paths []string, what string) {
		if len(paths) == 0 {
			return
		}
		shown := paths
		if len(shown) > 5 {
			shown = append(append([]string{}, shown[:5]...), fmt.Sprintf("and %d more", len(paths)-5))
		}
		rep.Warnings = append(rep.Warnings, contract.Warning{Code: code,
			Message: fmt.Sprintf("%s: %s", what, strings.Join(shown, ", ")), Details: map[string]any{"paths": paths}})
	}
	warn(contract.WorkflowChanged, workflows, "workflow files are new or changed")
	warn(contract.LargeFile, large, fmt.Sprintf("new or changed files are larger than %d bytes", g.rules.LargeFile))
	warn(contract.BinaryFile, binaries, "new or changed binary files would be published")
	return rep, nil
}

// text scans one piece of text for secrets and block patterns. Without
// lines (printable runs), findings carry no line number.
func (g *Gitleaks) text(s, where, path string, lines bool) []Finding {
	if s == "" {
		return nil
	}
	waivable := where == "tree"
	var out []Finding
	for _, lf := range g.detect(s, path) {
		line := 0
		if lines {
			line = lf.StartLine + 1 // gitleaks StartLine is 0-based
		}
		f := newFinding(lf.RuleID, where, path, line, lf.Secret, waivable)
		f.Secret, f.Hint = Redacted, hint(lf.Secret)
		out = append(out, f)
	}
	for i, ln := range strings.Split(s, "\n") {
		for _, m := range g.blocked(strings.TrimRight(ln, "\r")) {
			line := 0
			if lines {
				line = i + 1
			}
			f := newFinding(BlockRule, where, path, line, m, waivable)
			f.Match = m
			out = append(out, f)
		}
	}
	return out
}

func (g *Gitleaks) blocked(s string) []string {
	var out []string
	for _, re := range g.patterns {
		for _, m := range re.FindAllString(s, -1) {
			if m != "" { // a pattern that can match nothing must not block everything
				out = append(out, m)
			}
		}
	}
	return out
}

func newFinding(rule, where, path string, line int, match string, waivable bool) Finding {
	sum := sha256.Sum256([]byte(match))
	f := Finding{Rule: rule, Where: where, Path: path, Line: line, Waivable: waivable, MatchSHA256: hex.EncodeToString(sum[:])}
	f.ID = FindingID(rule, where, path, f.MatchSHA256)
	return f
}

// FindingID is f- plus 12 hex digits of SHA-256 over the rule, where, path
// and the match's SHA-256. Lines are left out, so an id survives edits
// elsewhere in the file; a changed match is a new id.
func FindingID(rule, where, path, matchSHA256 string) string {
	sum := sha256.Sum256([]byte(rule + "\x00" + where + "\x00" + path + "\x00" + matchSHA256))
	return "f-" + hex.EncodeToString(sum[:6])
}

// hint shows a secret's first and last four characters, or only "…" when
// it is too short to show any of it safely.
func hint(secret string) string {
	r := []rune(secret)
	if len(r) < 12 {
		return "…"
	}
	return string(r[:4]) + "…" + string(r[len(r)-4:])
}

// dedupe sorts findings and keeps one per id, preferring one with a line.
func dedupe(fs []Finding) []Finding {
	line := func(f Finding) int {
		if f.Line == 0 {
			return math.MaxInt
		}
		return f.Line
	}
	sort.SliceStable(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.Where != b.Where:
			return a.Where < b.Where
		case a.Path != b.Path:
			return a.Path < b.Path
		case line(a) != line(b):
			return line(a) < line(b)
		case a.Rule != b.Rule:
			return a.Rule < b.Rule
		}
		return a.ID < b.ID
	})
	out := []Finding{}
	seen := map[string]bool{}
	for _, f := range fs {
		if !seen[f.ID] {
			seen[f.ID] = true
			out = append(out, f)
		}
	}
	return out
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
