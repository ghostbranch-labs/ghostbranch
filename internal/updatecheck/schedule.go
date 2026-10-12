package updatecheck

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

// EnvOff turns the check off when set to anything non-empty. EnvURL replaces
// the API base URL: test-only, for the owner's hands-on check and the tests.
const (
	EnvOff = "GHOSTBRANCH_NO_UPDATE_CHECK"
	EnvURL = "GHOSTBRANCH_UPDATE_URL"
)

// ciVars are the CI systems' markers; any of them set turns the check off.
var ciVars = []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "CIRCLECI", "JENKINS_URL", "TF_BUILD"}

// Audience is who a reminder is for.
type Audience int

const (
	// Person: the stderr reminder on a terminal.
	Person Audience = iota
	// Agent: UPDATE_AVAILABLE in --json warnings.
	Agent
)

// OffReason says why this binary never looks: EnvOff, "CI" or "development
// build"; "" when it looks.
func OffReason(getenv func(string) string, version string) string {
	if getenv(EnvOff) != "" {
		return EnvOff
	}
	for _, k := range ciVars {
		if getenv(k) != "" {
			return "CI"
		}
	}
	if _, ok := ParseRelease(version); !ok {
		return "development build"
	}
	return ""
}

// BaseURL is the API origin: EnvURL when set (override is its value), else
// GitHub's. An override that isn't an absolute http(s) URL is an error, and
// no look is made.
func BaseURL(getenv func(string) string) (base, override string, err error) {
	v := strings.TrimSpace(getenv(EnvURL))
	if v == "" {
		return DefaultBaseURL, "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", v, errors.New(EnvURL + " isn't an http(s) URL")
	}
	return strings.TrimRight(v, "/"), v, nil
}

// Interval is how often to look: every 4 hours below 1.0.0, daily from it.
func Interval(current Version) time.Duration {
	if current.BelowOne() {
		return 4 * time.Hour
	}
	return 24 * time.Hour
}

// Due reports whether to look now: never looked, a look by another binary, a
// look in the future (clock skew), or one at least Interval old.
func Due(c Cache, current Version, now time.Time) bool {
	switch {
	case c.CheckedAt.IsZero(), c.BinaryVersion != current.String(), c.CheckedAt.After(now):
		return true
	}
	return now.Sub(c.CheckedAt) >= Interval(current)
}

// currentAdvisories is the cached advisory IDs, cleaned, when they were
// matched against current; none (never nil) when another binary matched them.
func currentAdvisories(c Cache, current Version) []string {
	out := []string{}
	if c.BinaryVersion != current.String() {
		return out
	}
	for _, a := range c.Advisories {
		out = append(out, clean(a.ID))
	}
	return out
}

// NoticeFor is the reminder due for aud now, or nil (spec § 3.3): a security
// fix for this version every time; below 1.0.0 every time; from 1.0.0 once
// per 24 h per audience. The URL is rebuilt from the version and the IDs
// cleaned: nothing printed comes from the cache file as is. Upgrade is left
// for the caller.
func NoticeFor(c Cache, current Version, aud Audience, now time.Time) *Notice {
	if c.Latest == nil {
		return nil
	}
	latest, ok := ParseRelease(c.Latest.Version)
	if !ok || Compare(latest, current) <= 0 {
		return nil
	}
	n := &Notice{Current: current.String(), Latest: latest.String(), URL: ReleaseURL("v" + latest.String()), Advisories: currentAdvisories(c, current)}
	n.Security = len(n.Advisories) > 0
	if n.Security || current.BelowOne() {
		return n
	}
	last := c.RemindedPersonAt
	if aud == Agent {
		last = c.RemindedAgentAt
	}
	if last.IsZero() || last.After(now) || now.Sub(last) >= 24*time.Hour {
		return n
	}
	return nil
}
