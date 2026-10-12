package updatecheck

import (
	"fmt"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// Notice is one reminder, for a person or an agent.
type Notice struct {
	Current, Latest, URL string
	Security             bool
	Advisories           []string
	Upgrade              string
}

func (n Notice) issues() string {
	if len(n.Advisories) == 0 {
		return "a security issue"
	}
	if len(n.Advisories) > 1 {
		return fmt.Sprintf("security issues (%s and %d more)", n.Advisories[0], len(n.Advisories)-1)
	}
	return fmt.Sprintf("a security issue (%s)", n.Advisories[0])
}

// Lines is the person's reminder: the mark ("!" or "✗"), the first line, and
// the upgrade line, printed indented under it (spec § 3.5).
func (n Notice) Lines() (mark, first, second string) {
	if n.Security {
		return "✗", fmt.Sprintf("Ghostbranch %s fixes %s; you have %s.", n.Latest, n.issues(), n.Current), "Upgrade now: " + n.Upgrade
	}
	return "!", fmt.Sprintf("Ghostbranch %s is out (you have %s).", n.Latest, n.Current), "Upgrade: " + n.Upgrade
}

// Warning is the agent's UPDATE_AVAILABLE (spec § 3.6).
func (n Notice) Warning() contract.Warning {
	msg := fmt.Sprintf("Ghostbranch %s is out (this is %s)", n.Latest, n.Current)
	if n.Security {
		msg += "; it fixes " + n.issues()
	}
	adv := append([]string{}, n.Advisories...)
	return contract.Warning{Code: contract.UpdateAvailable, Message: msg, Details: map[string]any{
		"current": n.Current, "latest": n.Latest, "url": n.URL, "security": n.Security, "advisories": adv, "upgrade": n.Upgrade}}
}

// Status is doctor's update_check (spec § 3.8).
type Status struct {
	Enabled    bool       `json:"enabled"`
	OffReason  string     `json:"off_reason"`
	CheckedAt  *time.Time `json:"checked_at"`
	Current    string     `json:"current"`
	Latest     string     `json:"latest"`
	Security   bool       `json:"security"`
	Advisories []string   `json:"advisories"`
	Upgrade    string     `json:"upgrade"`
	Error      string     `json:"error"`
	Override   string     `json:"override"`
	ago        string
}

// StatusOf describes the checker for doctor: off and why, or what the cache
// holds now. As in NoticeFor, nothing comes from the cache file as is: the
// latest version must parse, and the error and IDs are cleaned.
func StatusOf(off string, current Version, c Cache, override, upgrade string, now time.Time) Status {
	s := Status{Enabled: off == "", OffReason: off, Advisories: []string{}, Override: clean(override)}
	if off != "" {
		return s
	}
	s.Current, s.Upgrade, s.Error = current.String(), upgrade, clean(c.LastError)
	if !c.CheckedAt.IsZero() {
		at := c.CheckedAt
		s.CheckedAt = &at
		s.ago = ago(now.Sub(at))
	}
	if c.Latest != nil {
		if v, ok := ParseRelease(c.Latest.Version); ok {
			s.Latest = v.String()
		}
	}
	s.Advisories = currentAdvisories(c, current)
	s.Security = len(s.Advisories) > 0
	return s
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd ago", int(d/(24*time.Hour)))
	}
}

// Line is doctor's human line.
func (s Status) Line() string {
	parts := []string{"update check: on"}
	switch {
	case !s.Enabled:
		return "update check: off (" + s.OffReason + ")"
	case s.Error != "":
		parts = append(parts, "last look failed: "+s.Error)
	case s.CheckedAt == nil:
		parts = append(parts, "not looked yet")
	case s.Latest == "":
		parts = append(parts, "no release published yet")
	default:
		parts = append(parts, "last looked "+s.ago)
		cur, ok1 := ParseRelease(s.Current)
		lat, ok2 := ParseRelease(s.Latest)
		if ok1 && ok2 && Compare(lat, cur) > 0 {
			parts = append(parts, fmt.Sprintf("%s available (you have %s)", s.Latest, s.Current))
			if s.Security {
				parts = append(parts, "fixes "+strings.Join(s.Advisories, ", "))
			}
		} else {
			parts = append(parts, "up to date")
		}
	}
	if s.Override != "" {
		parts = append(parts, EnvURL+"="+clean(s.Override)+" (test-only)")
	}
	return strings.Join(parts, " · ")
}
