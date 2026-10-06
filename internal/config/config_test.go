package config_test

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

func TestRenderRoundTripsToDefaults(t *testing.T) {
	cfg, warns, err := config.Parse(config.Render("acme/widget", "acme/widget-shadow"))
	if err != nil || len(warns) != 0 {
		t.Fatalf("Parse(Render) = %v, %v", warns, err)
	}
	want := config.Default()
	want.Public, want.Shadow = "acme/widget", "acme/widget-shadow"
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v\nwant %+v", cfg, want)
	}
	if cfg.Gate.CIAppearTimeout != 10*time.Minute || cfg.Gate.CITimeout != 60*time.Minute {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
}

func TestRequireHumanIsReplacedByGrants(t *testing.T) {
	_, warns, err := config.Parse([]byte("version: 1\npublic: acme/widget\nshadow: acme/widget-shadow\npromote:\n  require_human: false\n"))
	if err != nil || len(warns) != 1 || warns[0].Code != contract.ConfigKeyIgnored || !strings.Contains(warns[0].Message, "agents allow") {
		t.Fatalf("%v %v", warns, err)
	}
}

func TestSpecKeysNotBuiltYetWarnOncePerSection(t *testing.T) {
	y := `version: 1
public: a/b
shadow: a/b-shadow
sync:
  schedule: "*/30 * * * *"
  branches: [main]
agents:
  default_tier: read
  overrides:
    claude-code: shadow-write
promote:
  author_email: noreply
`
	_, warns, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 3 {
		t.Fatalf("want 3 warnings (sync, agents, promote.author_email), got %v", warns)
	}
	for _, w := range warns {
		if w.Code != contract.ConfigKeyIgnored {
			t.Fatalf("bad warning code %v", w)
		}
	}
}

func TestInvalidConfigs(t *testing.T) {
	base := "version: 1\npublic: a/b\nshadow: a/b-shadow\n"
	cases := map[string]string{
		"unknown key":      base + "promte:\n  target: pr\n",
		"bad version":      "version: 2\npublic: a/b\nshadow: a/b-shadow\n",
		"missing public":   "version: 1\nshadow: a/b-shadow\n",
		"merge target":     base + "promote:\n  target: merge\n",
		"preserve mode":    base + "promote:\n  default_mode: preserve\n",
		"secrets disabled": base + "leak_scan:\n  secrets: false\n",
		"bad duration":     base + "gate:\n  ci_timeout: soon\n",
		"strip not list":   base + "leak_scan:\n  strip: .ghostbranch/**\n",
		"not yaml":         "version: [\n",
	}
	for name, y := range cases {
		_, _, err := config.Parse([]byte(y))
		if ce := contract.From(err); ce == nil || ce.Code != contract.ConfigInvalid {
			t.Errorf("%s: want CONFIG_INVALID, got %v", name, err)
		}
	}
	_, _, err := config.Parse([]byte(base + "promte:\n  target: pr\n"))
	if contract.From(err).Field != "promte.target" {
		t.Errorf("unknown key error must name the key, got %+v", contract.From(err))
	}
}
