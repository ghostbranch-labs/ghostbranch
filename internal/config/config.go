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
}

type Promote struct {
	Target      string
	DefaultMode string
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
		Promote: Promote{Target: "pr", DefaultMode: "squash"},
		LeakScan: LeakScan{Strip: []string{".ghostbranch/**"}, Secrets: true,
			Markers: Markers{Begin: "BEGIN-INTERNAL", End: "END-INTERNAL"}, BlockPatterns: []string{}, LargeFile: 5 << 20},
		Gate: Gate{CIAppearTimeout: 10 * time.Minute, CITimeout: 60 * time.Minute},
	}
}

var honoured = map[string]bool{
	"version": true, "public": true, "shadow": true,
	"promote.target": true, "promote.default_mode": true,
	"leak_scan.strip": true, "leak_scan.secrets": true,
	"leak_scan.markers.begin": true, "leak_scan.markers.end": true,
	"leak_scan.block_patterns": true, "leak_scan.large_file": true,
	"gate.ci_appear_timeout": true, "gate.ci_timeout": true,
}

// ignoredRoots are product-spec keys that later sub-projects implement.
var ignoredRoots = []string{"sync", "ci", "agents"}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// replaced are keys earlier versions honoured; they are ignored with a
// pointer to what replaced them.
var replaced = map[string]string{
	"promote.require_human": "promote.require_human is replaced by agent grants (ghostbranch agents allow) and is ignored",
	"promote.author_email":  "promote.author_email arrives with preserve mode and is ignored",
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
	warned := map[string]bool{}
	for _, k := range keys {
		switch root := ignoredRoot(k); {
		case replaced[k] != "":
			warns = append(warns, contract.Warning{Code: contract.ConfigKeyIgnored, Message: replaced[k]})
		case honoured[k]:
			if err := set(&cfg, k, leaves[k]); err != nil {
				return Config{}, nil, err
			}
		case root != "":
			if !warned[root] {
				warned[root] = true
				warns = append(warns, contract.Warning{Code: contract.ConfigKeyIgnored,
					Message: fmt.Sprintf("%s is not supported in this version and is ignored", root)})
			}
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
	}
	return err
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
	case c.Promote.DefaultMode != "squash":
		return invalid("promote.default_mode", "only squash is supported in this version")
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
  default_mode: squash

leak_scan:
  strip: [".ghostbranch/**"]
  markers:
    begin: BEGIN-INTERNAL
    end: END-INTERNAL
  block_patterns: []
  large_file: 5MB
  secrets: true

gate:
  ci_appear_timeout: 10m
  ci_timeout: 60m
`, public, shadow))
}
