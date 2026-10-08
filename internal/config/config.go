// Package config parses .ghostbranch.yml, which lives only on the shadow
// repo's overlay branch.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// Config is the subset of .ghostbranch.yml this version honours.
type Config struct {
	Version  int
	Public   string
	Shadow   string
	Promote  Promote
	LeakScan LeakScan
	Gate     Gate
	Sync     Sync
	CI       CI
}

// ScheduleOff turns scheduled sync off: no sync workflow is installed.
const ScheduleOff = "off"

// Sync is how shadow branches follow public. Schedule is ScheduleOff or a
// five-field cron for the opt-in sync workflow.
type Sync struct {
	Schedule string
	Branches []string
}

// CI is how the gate treats the public repo's workflows.
type CI struct {
	RewriteRepoGuards string // rewrite | warn | off
}

// History modes and author modes for promote.
const (
	ModeSquash   = "squash"
	ModePreserve = "preserve"
	ModeCurated  = "curated"

	AuthorNoreply = "noreply"
	AuthorKeep    = "keep"
	AuthorMap     = "map"
)

type Promote struct {
	Target      string
	DefaultMode string
	AuthorEmail string
	// AuthorMap keys are lower-cased emails; values are "Name <email>".
	AuthorMap map[string]string
}

// Markers are the tokens that open and close an internal section.
type Markers struct {
	Begin string
	End   string
}

type LeakScan struct {
	Strip         []string
	Secrets       bool
	Markers       Markers
	BlockPatterns []string
	LargeFile     int64
}

type Gate struct {
	CIAppearTimeout time.Duration
	CITimeout       time.Duration
}

// Default is the configuration init writes.
func Default() Config {
	return Config{
		Version: 1,
		Promote: Promote{Target: "pr", DefaultMode: ModePreserve, AuthorEmail: AuthorNoreply, AuthorMap: map[string]string{}},
		LeakScan: LeakScan{Strip: []string{".ghostbranch/**"}, Secrets: true,
			Markers: Markers{Begin: "BEGIN-INTERNAL", End: "END-INTERNAL"}, BlockPatterns: []string{}, LargeFile: 5 << 20},
		Gate: Gate{CIAppearTimeout: 10 * time.Minute, CITimeout: 60 * time.Minute},
		Sync: Sync{Schedule: ScheduleOff, Branches: []string{"main"}}, CI: CI{RewriteRepoGuards: "rewrite"},
	}
}

var honoured = map[string]bool{
	"version": true, "public": true, "shadow": true,
	"promote.target": true, "promote.default_mode": true,
	"promote.author_email": true, "promote.author_map": true,
	"leak_scan.strip": true, "leak_scan.secrets": true,
	"leak_scan.markers.begin": true, "leak_scan.markers.end": true,
	"leak_scan.block_patterns": true, "leak_scan.large_file": true,
	"gate.ci_appear_timeout": true, "gate.ci_timeout": true,
	"sync.schedule": true, "sync.branches": true, "ci.rewrite_repo_guards": true,
}

// ignoredRoots are product-spec keys that later sub-projects implement.
var ignoredRoots = []string{"sync", "ci", "agents"}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// replaced are keys earlier versions honoured; they are ignored with a
// pointer to what replaced them.
var replaced = map[string]string{
	"promote.require_human": "promote.require_human is replaced by agent grants (ghostbranch agents allow) and is ignored",
}

// Parse reads and validates a config file.
func Parse(data []byte) (Config, []contract.Warning, error) {
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return Config{}, nil, contract.Wrap(contract.ConfigInvalid, err, "parse .ghostbranch.yml")
	}
	leaves := map[string]any{}
	flatten("", raw, leaves)
	keys := make([]string, 0, len(leaves))
	for k := range leaves {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	cfg := Default()
	var warns []contract.Warning
	for _, k := range keys {
		switch root := ignoredRoot(k); {
		case replaced[k] != "":
			warns = append(warns, contract.Warning{Code: contract.ConfigKeyIgnored, Message: replaced[k]})
		case honoured[k]:
			if err := set(&cfg, k, leaves[k]); err != nil {
				return Config{}, nil, err
			}
		case root != "":
			warns = append(warns, contract.Warning{Code: contract.ConfigKeyIgnored,
				Message: fmt.Sprintf("%s is not supported in this version and is ignored", k)})
		default:
			return Config{}, nil, contract.New(contract.ConfigInvalid, "unknown key %q in .ghostbranch.yml", k).WithField(k)
		}
	}
	return cfg, warns, validate(cfg)
}

func flatten(prefix string, m map[string]any, out map[string]any) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if key == "promote.author_map" {
			out[key] = v // one leaf: its keys are emails, which contain dots
			continue
		}
		if sub, ok := v.(map[string]any); ok && len(sub) > 0 {
			flatten(key, sub, out)
			continue
		}
		out[key] = v
	}
}

func ignoredRoot(key string) string {
	for _, r := range ignoredRoots {
		if key == r || strings.HasPrefix(key, r+".") {
			return r
		}
	}
	return ""
}

func invalid(key, format string, args ...any) error {
	return contract.New(contract.ConfigInvalid, "%s: %s", key, fmt.Sprintf(format, args...)).WithField(key)
}

func set(cfg *Config, key string, v any) error {
	str := func() (string, error) {
		s, ok := v.(string)
		if !ok {
			return "", invalid(key, "must be a string")
		}
		return s, nil
	}
	boolean := func() (bool, error) {
		b, ok := v.(bool)
		if !ok {
			return false, invalid(key, "must be true or false")
		}
		return b, nil
	}
	dur := func() (time.Duration, error) {
		s, ok := v.(string)
		d, err := time.ParseDuration(s)
		if !ok || err != nil || d <= 0 {
			return 0, invalid(key, "must be a positive duration such as 10m")
		}
		return d, nil
	}
	token := func() (string, error) {
		s, ok := v.(string)
		if !ok || s == "" || strings.ContainsAny(s, " \t\r\n") {
			return "", invalid(key, "must be a non-empty string without spaces")
		}
		if strings.Trim(s, "/*#-;%<!{}>") == "" {
			// A token of comment punctuation would match the wrappers
			// around every marker, or ordinary comment lines.
			return "", invalid(key, "must contain a letter or digit")
		}
		return s, nil
	}
	var err error
	switch key {
	case "version":
		n, ok := v.(int)
		if !ok {
			return invalid(key, "must be an integer")
		}
		cfg.Version = n
	case "public":
		cfg.Public, err = str()
	case "shadow":
		cfg.Shadow, err = str()
	case "promote.target":
		cfg.Promote.Target, err = str()
	case "promote.default_mode":
		cfg.Promote.DefaultMode, err = str()
	case "promote.author_email":
		cfg.Promote.AuthorEmail, err = str()
	case "promote.author_map":
		m, ok := v.(map[string]any)
		if !ok {
			return invalid(key, "must map author emails to \"Name <email>\"")
		}
		cfg.Promote.AuthorMap = map[string]string{}
		for k, raw := range m {
			field := key + "." + k
			if _, e, okKey := ParseIdentity("x <" + k + ">"); !okKey || e != k {
				return invalid(field, "%q is not an email address", k)
			}
			s, isStr := raw.(string)
			if _, _, okVal := ParseIdentity(s); !isStr || !okVal {
				return invalid(field, "must be \"Name <email>\"")
			}
			lk := strings.ToLower(k)
			if _, dup := cfg.Promote.AuthorMap[lk]; dup {
				return invalid(field, "duplicates another key (emails match case-insensitively)")
			}
			cfg.Promote.AuthorMap[lk] = s
		}
	case "leak_scan.secrets":
		cfg.LeakScan.Secrets, err = boolean()
	case "leak_scan.strip":
		list, ok := v.([]any)
		if !ok {
			return invalid(key, "must be a list of path patterns")
		}
		cfg.LeakScan.Strip = nil
		for _, item := range list {
			s, ok := item.(string)
			if !ok || s == "" {
				return invalid(key, "must be a list of path patterns")
			}
			cfg.LeakScan.Strip = append(cfg.LeakScan.Strip, s)
		}
	case "leak_scan.markers.begin":
		cfg.LeakScan.Markers.Begin, err = token()
	case "leak_scan.markers.end":
		cfg.LeakScan.Markers.End, err = token()
	case "leak_scan.block_patterns":
		list, ok := v.([]any)
		if !ok {
			return invalid(key, "must be a list of regular expressions")
		}
		cfg.LeakScan.BlockPatterns = []string{}
		for i, item := range list {
			field := fmt.Sprintf("%s[%d]", key, i)
			s, ok := item.(string)
			if !ok || s == "" {
				return invalid(field, "must be a non-empty string")
			}
			if _, rerr := regexp.Compile(s); rerr != nil {
				return invalid(field, "is not a valid Go regular expression: %v", rerr)
			}
			cfg.LeakScan.BlockPatterns = append(cfg.LeakScan.BlockPatterns, s)
		}
	case "leak_scan.large_file":
		n, serr := ParseSize(v)
		if serr != nil {
			return invalid(key, "%v", serr)
		}
		cfg.LeakScan.LargeFile = n
	case "gate.ci_appear_timeout":
		cfg.Gate.CIAppearTimeout, err = dur()
	case "gate.ci_timeout":
		cfg.Gate.CITimeout, err = dur()
	case "sync.schedule":
		s, ok := v.(string)
		if !ok {
			return invalid(key, `must be "off" or five cron fields such as "23 */6 * * *"`)
		}
		norm, serr := checkSchedule(s)
		if serr != nil {
			return invalid(key, "%v", serr)
		}
		cfg.Sync.Schedule = norm
	case "sync.branches":
		list, ok := v.([]any)
		if !ok || len(list) == 0 {
			return invalid(key, "must be a list of branch names that includes main")
		}
		seen := map[string]bool{}
		cfg.Sync.Branches = nil
		for _, item := range list {
			b, ok := item.(string)
			if !ok || !validBranch(b) {
				return invalid(key, "%v is not a branch name Ghostbranch can sync", item)
			}
			if seen[b] {
				return invalid(key, "lists %s twice", b)
			}
			seen[b] = true
			cfg.Sync.Branches = append(cfg.Sync.Branches, b)
		}
		if !seen["main"] {
			return invalid(key, "must include main")
		}
	case "ci.rewrite_repo_guards":
		var s string
		if s, err = str(); err == nil {
			if s != "rewrite" && s != "warn" && s != "off" {
				return invalid(key, "must be rewrite, warn or off")
			}
			cfg.CI.RewriteRepoGuards = s
		}
	}
	return err
}

var identity = regexp.MustCompile(`^([^<>\p{Cc}\p{Zl}\p{Zp}]*[^<>\s\p{Cc}\p{Zl}\p{Zp}])[ \t]*<([^<>\s\p{Z}\p{Cc}@]+@[^<>\s\p{Z}\p{Cc}@]+)>$`)

// ParseIdentity splits "Name <email>". The name must be non-empty after
// trimming, and neither part may hold '<', '>', a line break or a control
// character.
func ParseIdentity(s string) (name, email string, ok bool) {
	m := identity.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	name = strings.TrimSpace(m[1])
	if name == "" {
		return "", "", false
	}
	return name, m[2], true
}

var sizePattern = regexp.MustCompile(`(?i)^([0-9]+)\s*(KB|MB|GB)?$`)

var errSize = errors.New("must be a positive size such as 5MB, 512KB or 1048576")

// ParseSize reads leak_scan.large_file: a byte count, or a whole number
// with KB, MB or GB (powers of 1024).
func ParseSize(v any) (int64, error) {
	var n int64
	unit := ""
	switch x := v.(type) {
	case int:
		n = int64(x)
	case string:
		m := sizePattern.FindStringSubmatch(strings.TrimSpace(x))
		if m == nil {
			return 0, errSize
		}
		p, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return 0, errSize
		}
		n, unit = p, strings.ToUpper(m[2])
	default:
		return 0, errSize
	}
	shift := map[string]uint{"": 0, "KB": 10, "MB": 20, "GB": 30}[unit]
	if n <= 0 || n > (int64(1)<<62)>>shift {
		return 0, errSize
	}
	return n << shift, nil
}

var (
	cronField  = regexp.MustCompile(`^[0-9A-Za-z*/,-]+$`)
	fastMinute = regexp.MustCompile(`^\*(/[1-4])?$`)
	branchPart = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

// checkSchedule returns s with single spaces, or why GitHub's schedule (or
// the workflow it is written into) would not take it.
func checkSchedule(s string) (string, error) {
	if s == ScheduleOff {
		return s, nil
	}
	if strings.ContainsAny(s, "\r\n") {
		// strings.Fields would swallow a line break as spacing.
		return "", errors.New("must be on one line")
	}
	f := strings.Fields(s)
	if len(f) != 5 {
		return "", errors.New(`must be "off" or five cron fields, such as "23 */6 * * *"`)
	}
	for _, x := range f {
		if !cronField.MatchString(x) {
			return "", fmt.Errorf("cron field %q has characters a schedule does not take", x)
		}
	}
	if fastMinute.MatchString(f[0]) {
		return "", errors.New("runs more often than every 5 minutes, GitHub's shortest schedule; use a minute such as 23 or */15")
	}
	return strings.Join(f, " "), nil
}

// validBranch accepts branch names made of letters, digits, '.', '_' and
// '-' in '/'-separated parts, within git's ref-name rules. Anything else
// could break the YAML or bash the sync workflow is rendered into.
func validBranch(b string) bool {
	if b == "" || strings.HasPrefix(b, "-") || strings.HasSuffix(b, ".") || strings.HasSuffix(b, ".lock") || strings.Contains(b, "..") {
		return false
	}
	for _, part := range strings.Split(b, "/") {
		if !branchPart.MatchString(part) || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}

func validate(c Config) error {
	switch {
	case c.Version != 1:
		return invalid("version", "must be 1")
	case !repoName.MatchString(c.Public):
		return invalid("public", "must be owner/repo")
	case !repoName.MatchString(c.Shadow):
		return invalid("shadow", "must be owner/repo")
	case c.Promote.Target != "pr":
		return invalid("promote.target", "only pr is supported in this version")
	case c.Promote.DefaultMode != ModeSquash && c.Promote.DefaultMode != ModePreserve && c.Promote.DefaultMode != ModeCurated:
		return invalid("promote.default_mode", "must be squash, preserve or curated")
	case c.Promote.AuthorEmail != AuthorNoreply && c.Promote.AuthorEmail != AuthorKeep && c.Promote.AuthorEmail != AuthorMap:
		return invalid("promote.author_email", "must be noreply, keep or map")
	case c.Promote.AuthorEmail == AuthorMap && len(c.Promote.AuthorMap) == 0:
		return invalid("promote.author_map", "must list at least one email when author_email is map")
	case !c.LeakScan.Secrets:
		return invalid("leak_scan.secrets", "secret scanning cannot be disabled")
	case c.LeakScan.Markers.Begin == c.LeakScan.Markers.End:
		return invalid("leak_scan.markers", "begin and end must differ")
	}
	return nil
}

// Render is the config file init writes for a new pair.
func Render(public, shadow string) []byte {
	return []byte(fmt.Sprintf(`# Ghostbranch configuration. It lives only on this shadow repo's
# "ghostbranch" branch and is never promoted.
version: 1
public: %s
shadow: %s

promote:
  target: pr
  # preserve publishes each feature commit; squash publishes one commit;
  # curated regroups, reorders and rewords commits from a --curate file.
  default_mode: preserve
  # noreply publishes every commit as your GitHub noreply identity; keep
  # publishes the original authors; map uses author_map.
  author_email: noreply

leak_scan:
  strip: [".ghostbranch/**"]
  markers:
    begin: BEGIN-INTERNAL
    end: END-INTERNAL
  block_patterns: []
  large_file: 5MB
  secrets: true

sync:
  # Scheduled sync is off: run ghostbranch sync yourself (commands warn when
  # shadow main falls behind public). To opt in, set a cron such as
  # "23 */6 * * *" (about 120 Actions minutes a month), then run
  # ghostbranch upgrade.
  schedule: "off"
  branches: [main]

ci:
  rewrite_repo_guards: rewrite

gate:
  ci_appear_timeout: 10m
  ci_timeout: 60m
`, public, shadow))
}
