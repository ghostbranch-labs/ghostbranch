// Package overlay defines which repo paths belong to the shadow-only
// overlay. Those paths are added to the CI candidate and must never be
// published.
package overlay

import (
	"regexp"
	"strings"
)

// Fixed paths are always overlay paths, whatever the config says.
var Fixed = []string{".ghostbranch.yml", ".ghostbranch/**", ".github/workflows/ghostbranch-*"}

// Set is the fixed overlay patterns plus leak_scan.strip patterns.
type Set struct {
	patterns []string
	res      []*regexp.Regexp
}

// NewSet builds the overlay set from the fixed patterns and extra ones.
func NewSet(extra ...string) Set {
	var s Set
	for _, p := range append(append([]string{}, Fixed...), extra...) {
		s.patterns = append(s.patterns, p)
		s.res = append(s.res, compile(p))
	}
	return s
}

// Match reports whether a repo-relative, slash-separated path is an overlay path.
func (s Set) Match(path string) bool {
	for _, re := range s.res {
		if re.MatchString(path) {
			return true
		}
	}
	return false
}

// Patterns returns every pattern in the set.
func (s Set) Patterns() []string { return append([]string(nil), s.patterns...) }

func compile(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case c == '*' && i+1 < len(pattern) && pattern[i+1] == '*':
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}
