// Package updatecheck tells people and agents about newer Ghostbranch
// releases (spec 6.4a § 3). It asks GitHub anonymously for the latest
// release and, only when that is newer, for the repo's security advisories;
// it caches the answer under GHOSTBRANCH_HOME and works out when to remind
// whom. It never downloads or installs anything, and it never calls through
// the forge: it reads Ghostbranch's own releases, without the user's token.
package updatecheck

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Version is a release version: MAJOR.MINOR.PATCH with an optional
// pre-release. Build metadata is never part of a release.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

var (
	releasePattern = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:-([0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*))?$`)
	// A Go pseudo-version's pre-release ends in a 14-digit time and a
	// 12-hex commit: 20261011120000-abcdef123456, 0.2026…, beta.0.2026….
	pseudoPattern = regexp.MustCompile(`(?:^|\.)(?:0\.)?[0-9]{14}-[0-9a-f]{12}$|^[0-9]{14}-[0-9a-f]{12}$`)
)

// ParseRelease reads a release version, with or without the leading v. Go
// pseudo-versions, (devel), (unknown) and anything with build metadata
// (+dirty) are not releases, so those builds never look for updates.
func ParseRelease(s string) (Version, bool) {
	m := releasePattern.FindStringSubmatch(s)
	if m == nil || (m[4] != "" && pseudoPattern.MatchString(m[4])) {
		return Version{}, false
	}
	maj, _ := strconv.Atoi(m[1]) // at most 9 digits: no overflow
	mnr, _ := strconv.Atoi(m[2])
	pat, _ := strconv.Atoi(m[3])
	return Version{Major: maj, Minor: mnr, Patch: pat, Pre: m[4]}, true
}

// String is the version without the v: 0.13.0, 1.0.0-rc.1.
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// BelowOne reports whether v comes before 1.0.0, pre-releases of 1.0.0
// included: the strict schedule applies to it (spec § 3.3).
func (v Version) BelowOne() bool { return Compare(v, Version{Major: 1}) < 0 }

// Compare orders a and b by semver precedence: -1, 0 or 1.
func Compare(a, b Version) int {
	for _, d := range [3][2]int{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		if d[0] != d[1] {
			return cmpInt(d[0], d[1])
		}
	}
	return comparePre(a.Pre, b.Pre)
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// comparePre orders pre-releases: none ranks above any; numeric identifiers
// compare as numbers and rank below alphanumeric ones; a longer list wins a
// tie.
func comparePre(a, b string) int {
	switch {
	case a == b:
		return 0
	case a == "":
		return 1
	case b == "":
		return -1
	}
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aNum := numeric(as[i])
		bn, bNum := numeric(bs[i])
		switch {
		case aNum && bNum:
			if an != bn {
				return cmpInt(an, bn)
			}
		case aNum:
			return -1
		case bNum:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return cmpInt(len(as), len(bs))
}

func numeric(s string) (int, bool) {
	if s == "" || strings.Trim(s, "0123456789") != "" || len(s) > 9 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	return n, err == nil
}

// Affected reports whether v is inside a GitHub advisory's
// vulnerable_version_range: comma-separated constraints such as "< 0.13.1"
// or ">= 0.10.0, < 0.13.1", each an operator (<, <=, >, >=, =) and a full
// version. ok is false when the range can't be read; the caller then counts
// the advisory as affecting this version (fail toward warning).
func Affected(rng string, v Version) (affected, ok bool) {
	if strings.TrimSpace(rng) == "" {
		return false, false
	}
	in := true
	for _, part := range strings.Split(rng, ",") {
		part = strings.TrimSpace(part)
		op := ""
		for _, o := range []string{"<=", ">=", "<", ">", "="} {
			if strings.HasPrefix(part, o) {
				op = o
				break
			}
		}
		if op == "" {
			return false, false
		}
		w, good := ParseRelease(strings.TrimSpace(strings.TrimPrefix(part, op)))
		if !good {
			return false, false
		}
		c := Compare(v, w)
		switch op {
		case "<":
			in = in && c < 0
		case "<=":
			in = in && c <= 0
		case ">":
			in = in && c > 0
		case ">=":
			in = in && c >= 0
		case "=":
			in = in && c == 0
		}
	}
	return in, true
}
