// Package config parses .ghostbranch.yml, which lives only on the shadow
// repo's overlay branch.
package config

import (
	"fmt"
	"regexp"
	"sort"
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
	Target       string
	DefaultMode  string
	RequireHuman bool
}

type LeakScan struct {
	Strip   []string
	Secrets bool
}

type Gate struct {
	CIAppearTimeout time.Duration
	CITimeout       time.Duration
}

// Default is the configuration init writes.
func Default() Config {
	return Config{
		Version:  1,
		Promote:  Promote{Target: "pr", DefaultMode: "squash", RequireHuman: true},
		LeakScan: LeakScan{Strip: []string{".ghostbranch/**"}, Secrets: true},
		Gate:     Gate{CIAppearTimeout: 10 * time.Minute, CITimeout: 60 * time.Minute},
	}
}

var honoured = map[string]bool{
	"version": true, "public": true, "shadow": true,
	"promote.target": true, "promote.default_mode": true, "promote.require_human": true,
	"leak_scan.strip": true, "leak_scan.secrets": true,
	"gate.ci_appear_timeout": true, "gate.ci_timeout": true,
}

// ignoredRoots are product-spec keys that later sub-projects implement.
var ignoredRoots = []string{"sync", "ci", "agents", "promote.author_email", "leak_scan.markers", "leak_scan.block_patterns"}

var repoName = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

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
	case "promote.require_human":
		cfg.Promote.RequireHuman, err = boolean()
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
	case "gate.ci_appear_timeout":
		cfg.Gate.CIAppearTimeout, err = dur()
	case "gate.ci_timeout":
		cfg.Gate.CITimeout, err = dur()
	}
	return err
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
  require_human: true

leak_scan:
  strip: [".ghostbranch/**"]
  secrets: true

gate:
  ci_appear_timeout: 10m
  ci_timeout: 60m
`, public, shadow))
}
