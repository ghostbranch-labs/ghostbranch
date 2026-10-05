// Package leak scans exactly what will be published: the tree, commit
// metadata, branch name and PR text. Secret values never leave this
// package; findings carry Redacted instead.
package leak

import (
	"bytes"
	"runtime/debug"
	"sort"
	"unicode/utf16"

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
		text, ok := decodeText(f.Content)
		if !ok {
			continue
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

// sniffLen is how much of a blob is inspected to classify it, like git's
// own binary detection.
const sniffLen = 8000

// decodeText returns a blob's content as text to scan, and false for a binary
// blob that should be skipped. A UTF-8 BOM is stripped. UTF-16 text, which is
// NUL-interleaved and would otherwise look binary, is transcoded to UTF-8 so
// it is scanned with the same paths and line numbers. Other blobs with a NUL
// in their first sniffLen bytes are binary; scanning printable runs inside
// them is deferred to sub-project 3.
func decodeText(b []byte) (string, bool) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		b = b[3:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return decodeUTF16(b[2:], false), true
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return decodeUTF16(b[2:], true), true
	}
	sample := b
	if len(sample) > sniffLen {
		sample = sample[:sniffLen]
	}
	if bytes.IndexByte(sample, 0) < 0 {
		return string(b), true
	}
	if bigEndian, ok := looksUTF16(sample); ok {
		return decodeUTF16(b, bigEndian), true
	}
	return "", false
}

// looksUTF16 detects BOM-less UTF-16 by its alternating NULs. Text that is
// mostly ASCII or Latin-1 has a NUL in the high byte of nearly every code
// unit: the odd-indexed bytes for little-endian, the even-indexed ones for
// big-endian. The sample is UTF-16 when over 40% of the bytes on one side
// are NUL and at most 5% on the other side are. The 40% floor leaves room for
// non-Latin characters mixed in; the 5% ceiling rejects binaries, whose NULs
// fall on both sides (and UTF-32, whose NULs fill both sides).
func looksUTF16(sample []byte) (bigEndian, ok bool) {
	units := len(sample) / 2
	if units == 0 {
		return false, false
	}
	var evenNUL, oddNUL int
	for i := 0; i+1 < len(sample); i += 2 {
		if sample[i] == 0 {
			evenNUL++
		}
		if sample[i+1] == 0 {
			oddNUL++
		}
	}
	mostly := func(n int) bool { return n*10 > units*4 } // > 40%
	hardly := func(n int) bool { return n*20 <= units }  // <= 5%
	switch {
	case mostly(oddNUL) && hardly(evenNUL):
		return false, true
	case mostly(evenNUL) && hardly(oddNUL):
		return true, true
	}
	return false, false
}

// decodeUTF16 transcodes UTF-16 to UTF-8. A trailing odd byte is dropped and
// unpaired surrogates become U+FFFD; neither stops the rest being scanned.
func decodeUTF16(b []byte, bigEndian bool) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		lo, hi := b[2*i], b[2*i+1]
		if bigEndian {
			lo, hi = hi, lo
		}
		u[i] = uint16(lo) | uint16(hi)<<8
	}
	return string(utf16.Decode(u))
}
