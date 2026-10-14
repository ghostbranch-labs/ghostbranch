package config_test

import (
	"bytes"
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

func TestSpecKeysNotBuiltYetWarnPerKey(t *testing.T) {
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
`
	_, warns, err := config.Parse([]byte(y))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 2 {
		t.Fatalf("want 2 warnings (agents.default_tier, agents.overrides.claude-code), got %v", warns)
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
		"unknown key":       base + "promte:\n  target: pr\n",
		"bad version":       "version: 2\npublic: a/b\nshadow: a/b-shadow\n",
		"missing public":    "version: 1\nshadow: a/b-shadow\n",
		"merge target":      base + "promote:\n  target: merge\n",
		"rebase mode":       base + "promote:\n  default_mode: rebase\n",
		"secrets disabled":  base + "leak_scan:\n  secrets: false\n",
		"bad duration":      base + "gate:\n  ci_timeout: soon\n",
		"strip not list":    base + "leak_scan:\n  strip: .ghostbranch/**\n",
		"not yaml":          "version: [\n",
		"empty marker":      base + "leak_scan:\n  markers:\n    begin: \"\"\n",
		"spaced marker":     base + "leak_scan:\n  markers:\n    begin: \"BEGIN INTERNAL\"\n",
		"same markers":      base + "leak_scan:\n  markers:\n    begin: X\n    end: X\n",
		"slash marker":      base + "leak_scan:\n  markers:\n    begin: \"//\"\n",
		"punct marker":      base + "leak_scan:\n  markers:\n    end: \"<!--}>\"\n",
		"star marker":       base + "leak_scan:\n  markers:\n    begin: \"*/\"\n",
		"bad regex":         base + "leak_scan:\n  block_patterns: [\"ok\", \"(unclosed\"]\n",
		"pattern not text":  base + "leak_scan:\n  block_patterns: [3]\n",
		"patterns not list": base + "leak_scan:\n  block_patterns: JIRA\n",
		"bad size":          base + "leak_scan:\n  large_file: huge\n",
	}
	for name, y := range cases {
		_, _, err := config.Parse([]byte(y))
		if ce := contract.From(err); ce == nil || ce.Code != contract.ConfigInvalid {
			t.Errorf("%s: want CONFIG_INVALID, got %v", name, err)
		}
	}
	_, _, err := config.Parse([]byte(base + "promte:\n  target: pr\n"))
	if ce := contract.From(err); ce.Field != "promte.target" || ce.Details["unknown_key"] != "promte.target" {
		t.Errorf("unknown key error must name the key, also in details.unknown_key, got %+v", ce)
	}
	// A bad value is no unknown key.
	_, _, err = config.Parse([]byte(base + "leak_scan:\n  large_file: huge\n"))
	if ce := contract.From(err); ce.Details["unknown_key"] != nil {
		t.Errorf("a bad value has details.unknown_key: %+v", ce)
	}
	_, _, err = config.Parse([]byte(base + "leak_scan:\n  markers:\n    end: \"#;%\"\n"))
	if ce := contract.From(err); ce == nil || ce.Field != "leak_scan.markers.end" || !strings.Contains(ce.Message, "must contain a letter or digit") {
		t.Errorf("a punctuation-only marker must be named, got %+v", ce)
	}
	_, _, err = config.Parse([]byte(base + "leak_scan:\n  markers:\n    begin: \"@@1\"\n"))
	if err != nil {
		t.Errorf("a marker with a digit is valid, got %v", err)
	}
	_, _, err = config.Parse([]byte(base + "leak_scan:\n  block_patterns: [\"ok\", \"(unclosed\"]\n"))
	if contract.From(err).Field != "leak_scan.block_patterns[1]" {
		t.Errorf("a bad pattern must be named by index, got %+v", contract.From(err))
	}
}

func TestLeakScanKeys(t *testing.T) {
	y := "version: 1\npublic: a/b\nshadow: a/b-shadow\n" + `leak_scan:
  markers:
    begin: "@@INTERNAL"
    end: "@@END"
  block_patterns: ["internal\\.example\\.com", "JIRA-[0-9]+"]
  large_file: 512KB
`
	cfg, warns, err := config.Parse([]byte(y))
	if err != nil || len(warns) != 0 {
		t.Fatalf("%v %v", warns, err)
	}
	ls := cfg.LeakScan
	if ls.Markers != (config.Markers{Begin: "@@INTERNAL", End: "@@END"}) ||
		!reflect.DeepEqual(ls.BlockPatterns, []string{`internal\.example\.com`, "JIRA-[0-9]+"}) || ls.LargeFile != 512*1024 {
		t.Fatalf("leak_scan = %+v", ls)
	}
	d := config.Default().LeakScan
	if d.Markers.Begin != "BEGIN-INTERNAL" || d.Markers.End != "END-INTERNAL" || d.LargeFile != 5<<20 || d.BlockPatterns == nil {
		t.Fatalf("defaults = %+v", d)
	}
}

func TestParseSize(t *testing.T) {
	good := map[any]int64{1048576: 1048576, "5MB": 5 << 20, "512kb": 512 << 10, "1 GB": 1 << 30, "300": 300}
	for in, want := range good {
		if got, err := config.ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%v) = %d %v, want %d", in, got, err, want)
		}
	}
	for _, in := range []any{"0", "-1", "5TB", "", "5.5MB", 0, -3, 1.5, nil} {
		if _, err := config.ParseSize(in); err == nil {
			t.Errorf("ParseSize(%v) succeeded", in)
		}
	}
}

func TestPromoteModesAndAuthors(t *testing.T) {
	base := string(config.Render("acme/widget", "acme/widget-shadow"))
	cfg, warns, err := config.Parse([]byte(base))
	if err != nil || len(warns) != 0 || cfg.Promote.DefaultMode != config.ModePreserve || cfg.Promote.AuthorEmail != config.AuthorNoreply {
		t.Fatalf("rendered config: %+v %v %v", cfg.Promote, warns, err)
	}
	for _, mode := range []string{"squash", "preserve", "curated"} {
		c, _, err := config.Parse([]byte(strings.Replace(base, "default_mode: preserve", "default_mode: "+mode, 1)))
		if err != nil || c.Promote.DefaultMode != mode {
			t.Fatalf("%s: %+v %v", mode, c.Promote, err)
		}
	}
	bad := strings.Replace(base, "default_mode: preserve", "default_mode: rebase", 1)
	if _, _, err := config.Parse([]byte(bad)); contract.From(err) == nil || contract.From(err).Field != "promote.default_mode" {
		t.Fatalf("bad mode: %v", err)
	}
}

func TestAuthorMapKeepsDottedEmails(t *testing.T) {
	src := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), "author_email: noreply",
		"author_email: map\n  author_map:\n    \"Jane.Doe@Corp.example\": \"Jane Doe <123+jane@users.noreply.github.com>\"", 1)
	cfg, _, err := config.Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Promote.AuthorMap["jane.doe@corp.example"]; got != "Jane Doe <123+jane@users.noreply.github.com>" {
		t.Fatalf("author_map = %#v", cfg.Promote.AuthorMap)
	}
}

func TestAuthorConfigRejects(t *testing.T) {
	base := string(config.Render("acme/widget", "acme/widget-shadow"))
	for name, repl := range map[string]string{
		"unknown mode":   "author_email: someone",
		"map but empty":  "author_email: map",
		"bad identity":   "author_email: map\n  author_map:\n    \"a@b.example\": \"no email here\"",
		"key not email":  "author_email: map\n  author_map:\n    \"nobody\": \"N <n@b.example>\"",
		"case duplicate": "author_email: map\n  author_map:\n    \"A@b.example\": \"N <n@b.example>\"\n    \"a@b.example\": \"M <m@b.example>\"",
		"vtab in key":    "author_email: map\n  author_map:\n    \"a\\v@b.example\": \"N <n@b.example>\"",
		"newline in val": "author_email: map\n  author_map:\n    \"a@b.example\": \"N\\n <n@b.example>\"",
	} {
		_, _, err := config.Parse([]byte(strings.Replace(base, "author_email: noreply", repl, 1)))
		if ce := contract.From(err); ce == nil || ce.Code != contract.ConfigInvalid || !strings.HasPrefix(ce.Field, "promote.author_") {
			t.Fatalf("%s: want CONFIG_INVALID on promote.author_*, got %v", name, err)
		}
	}
}

func TestParseIdentity(t *testing.T) {
	for in, want := range map[string][2]string{
		"Jane Doe <jane@x.example>": {"Jane Doe", "jane@x.example"},
		"<jane@x.example>":          {"", ""},
		"Jane <not-an-email>":       {"", ""},
		"Jane <a@b> trailing":       {"", ""},
		"Ja<ne <a@b.example>":       {"", ""},
		"  <a@b.example>":           {"", ""},
		"  <a@b.example>":           {"", ""},
		"N\x00 <a@b.example>":       {"", ""},
		"N <a@b.example\x7f>":       {"", ""},
		"N\x1b[31m <a@b.example>":   {"", ""},
		"N M <a@b.example>":         {"", ""},
		"N <a@b.example >":          {"", ""},
	} {
		n, e, ok := config.ParseIdentity(in)
		if ok != (want[0] != "") || n != want[0] || e != want[1] {
			t.Fatalf("ParseIdentity(%q) = %q %q %v", in, n, e, ok)
		}
	}
}

func parseWith(t *testing.T, extra string) (config.Config, []contract.Warning, error) {
	t.Helper()
	return config.Parse([]byte(string(config.Render("acme/widget", "acme/widget-shadow")) + extra))
}

func TestSyncAndCIDefaults(t *testing.T) {
	cfg, warns, err := config.Parse(config.Render("acme/widget", "acme/widget-shadow"))
	if err != nil || len(warns) != 0 {
		t.Fatalf("Render does not parse cleanly: %v %v", warns, err)
	}
	if cfg.Sync.Schedule != config.ScheduleOff || !reflect.DeepEqual(cfg.Sync.Branches, []string{"main"}) || cfg.CI.RewriteRepoGuards != "rewrite" {
		t.Fatalf("defaults = %+v %+v", cfg.Sync, cfg.CI)
	}
	if !reflect.DeepEqual(cfg.Sync, config.Default().Sync) || cfg.CI != config.Default().CI {
		t.Fatal("Render and Default disagree")
	}
}

func TestSyncSchedule(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		ok       bool
	}{
		{`"off"`, "off", true},
		{`off`, "off", true},
		{`"23 */6 * * *"`, "23 */6 * * *", true},
		{`"0  3 * * 1-5"`, "0 3 * * 1-5", true},
		{`"*/15 * * * *"`, "*/15 * * * *", true},
		{`"* * * * *"`, "", false},
		{`"*/4 * * * *"`, "", false},
		{`"0 0 * *"`, "", false},
		{`"0 0 * * * *"`, "", false},
		{`6h`, "", false},
	} {
		raw := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), `schedule: "off"`, "schedule: "+tc.in, 1)
		got, _, err := config.Parse([]byte(raw))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v", tc.in, err)
		}
		if tc.ok && got.Sync.Schedule != tc.want {
			t.Fatalf("%s: schedule = %q", tc.in, got.Sync.Schedule)
		}
		if !tc.ok && contract.From(err).Field != "sync.schedule" {
			t.Fatalf("%s: field = %q", tc.in, contract.From(err).Field)
		}
	}
}

func TestSyncBranches(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{`[main]`, true},
		{`[main, release/1.x, dev_2]`, true},
		{`[dev]`, false},        // must include main
		{`[main, main]`, false}, // duplicate
		{`[]`, false},
		{`main`, false}, // not a list
		{`[main, "-x"]`, false},
		{`[main, "a..b"]`, false},
		{`[main, "x.lock"]`, false},
		{`[main, "a.lock/b"]`, false}, // git refuses a component ending in .lock
		{`[main, "v1.lockstep/x"]`, true},
		{`[main, "a/.b"]`, false},
	} {
		raw := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), "branches: [main]", "branches: "+tc.in, 1)
		_, _, err := config.Parse([]byte(raw))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: err = %v", tc.in, err)
		}
	}
}

// Review Focus 3: values rendered into the sync workflow's YAML and bash
// never carry metacharacters.
func TestSyncValuesRejectInjection(t *testing.T) {
	for _, branches := range []string{`[main, "x; rm -rf ~"]`, `[main, "a\"b"]`, `[main, "a b"]`, `[main, "a$(id)"]`, "[main, \"a\\nb\"]"} {
		raw := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), "branches: [main]", "branches: "+branches, 1)
		if _, _, err := config.Parse([]byte(raw)); contract.From(err).Field != "sync.branches" {
			t.Fatalf("branches %s accepted: %v", branches, err)
		}
	}
	for _, sched := range []string{`"0 0 * * *\"\n"`, "\"0 0 * * *\\n\"", `"0 0 * * $(id)"`, `"0 0 * * 1;2"`} {
		raw := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), `schedule: "off"`, "schedule: "+sched, 1)
		if _, _, err := config.Parse([]byte(raw)); contract.From(err).Field != "sync.schedule" {
			t.Fatalf("schedule %s accepted: %v", sched, err)
		}
	}
}

func TestRewriteRepoGuardsMode(t *testing.T) {
	for mode, ok := range map[string]bool{"rewrite": true, "warn": true, "off": true, "yes": false} {
		raw := strings.Replace(string(config.Render("acme/widget", "acme/widget-shadow")), "rewrite_repo_guards: rewrite", "rewrite_repo_guards: "+mode, 1)
		cfg, _, err := config.Parse([]byte(raw))
		if (err == nil) != ok || (ok && cfg.CI.RewriteRepoGuards != mode) {
			t.Fatalf("%s: %+v %v", mode, cfg.CI, err)
		}
	}
}

func TestIgnoredKeysWarnPerKey(t *testing.T) {
	_, warns, err := parseWith(t, "agents:\n  default_tier: read\n  allow: true\n")
	if err != nil {
		t.Fatal(err)
	}
	var msgs []string
	for _, w := range warns {
		if w.Code != contract.ConfigKeyIgnored {
			t.Fatalf("unexpected warning %+v", w)
		}
		msgs = append(msgs, w.Message)
	}
	want := []string{"agents.allow is not supported in this version and is ignored",
		"agents.default_tier is not supported in this version and is ignored"}
	if !reflect.DeepEqual(msgs, want) {
		t.Fatalf("warnings = %q", msgs)
	}
}

func TestUnknownSyncAndCIKeysAreErrors(t *testing.T) {
	t.Parallel()
	for y, key := range map[string]string{
		"version: 1\npublic: o/r\nshadow: o/r-shadow\nci:\n  rewrite_repo_guard: warn\n": "ci.rewrite_repo_guard",
		"version: 1\npublic: o/r\nshadow: o/r-shadow\nsync:\n  schedul: \"0 * * * *\"\n": "sync.schedul",
		"version: 1\npublic: o/r\nshadow: o/r-shadow\nci:\n  runners: github-hosted\n":   "ci.runners",
	} {
		_, _, err := config.Parse([]byte(y))
		ce := contract.From(err)
		if ce == nil || ce.Code != contract.ConfigInvalid || ce.Details["unknown_key"] != key {
			t.Errorf("%q: want CONFIG_INVALID with unknown_key %q, got %v", y, key, err)
		}
	}
}

func TestEmptySyncAndCIMapsAreFine(t *testing.T) {
	t.Parallel()
	base := "version: 1\npublic: o/r\nshadow: o/r-shadow\n"
	for _, extra := range []string{
		"sync: {}\nci: {}\n",
		"sync:\nci:\n",
		"ci:\n  # rewrite_repo_guards: warn\n",
		"sync:\n  # schedule: off\n",
	} {
		cfg, warns, err := config.Parse([]byte(base + extra))
		if err != nil || len(warns) != 0 {
			t.Fatalf("%q: %v %+v", extra, err, warns)
		}
		if !reflect.DeepEqual(cfg.Sync, config.Default().Sync) || cfg.CI != config.Default().CI {
			t.Fatalf("%q: empty section changed the defaults: %+v %+v", extra, cfg.Sync, cfg.CI)
		}
	}
}

func TestParseReturnsNoConfigWithAnError(t *testing.T) {
	t.Parallel()
	for name, y := range map[string]string{
		"bad version": "version: 2\npublic: a/b\nshadow: a/b-shadow\n",
		"bad mode":    "version: 1\npublic: a/b\nshadow: a/b-shadow\npromote:\n  default_mode: rebase\n",
	} {
		cfg, _, err := config.Parse([]byte(y))
		if err == nil || !reflect.DeepEqual(cfg, config.Config{}) {
			t.Errorf("%s: Parse = %+v, %v; want no config with the error", name, cfg, err)
		}
	}
}

// A scalar where a section belongs is a plain CONFIG_INVALID that says
// what to write instead, never an unknown key (which reads as "upgrade").
func TestAScalarSyncOrCIIsAClearError(t *testing.T) {
	t.Parallel()
	const (
		syncMsg = "sync must be a section; to turn scheduled sync off, remove sync.schedule"
		ciMsg   = "ci must be a section; remove it, or put its keys under ci:"
	)
	base := "version: 1\npublic: o/r\nshadow: o/r-shadow\n"
	for extra, want := range map[string]struct{ field, msg string }{
		"sync: off\n":    {"sync", syncMsg},
		"sync: [main]\n": {"sync", syncMsg},
		"ci: rewrite\n":  {"ci", ciMsg},
	} {
		_, _, err := config.Parse([]byte(base + extra))
		ce := contract.From(err)
		if ce == nil || ce.Code != contract.ConfigInvalid || ce.Field != want.field || ce.Message != want.msg || ce.Details["unknown_key"] != nil {
			t.Errorf("%q: got %+v", extra, ce)
		}
	}
}

func TestRenderScheduleParses(t *testing.T) {
	t.Parallel()
	if !bytes.Equal(config.Render("acme/widget", "acme/widget-shadow"), config.RenderSchedule("acme/widget", "acme/widget-shadow", config.ScheduleOff)) {
		t.Fatal("Render changed")
	}
	cfg, _, err := config.Parse(config.RenderSchedule("acme/widget", "acme/widget-shadow", "23 */6 * * *"))
	if err != nil || cfg.Sync.Schedule != "23 */6 * * *" {
		t.Fatalf("%+v %v", cfg.Sync, err)
	}
}

// F-25: a section left empty (bare, comment-only or {}) means its
// defaults, as sync: and ci: already did; before, it read as an unknown
// key and every command said the pair was set up by a newer Ghostbranch.
func TestEmptySectionsMeanTheDefaults(t *testing.T) {
	t.Parallel()
	base := "version: 1\npublic: o/r\nshadow: o/r-shadow\n"
	want := config.Default()
	want.Public, want.Shadow = "o/r", "o/r-shadow"
	for _, extra := range []string{
		"promote:\n", "promote: {}\n", "promote:\n  # default_mode: squash\n",
		"leak_scan:\n", "leak_scan: {}\n", "leak_scan:\n  # secrets: false\n",
		"leak_scan:\n  markers:\n", "leak_scan:\n  markers: {}\n", "leak_scan:\n  markers:\n    # begin: X-BEGIN\n",
		"gate:\n", "gate: {}\n", "gate:\n  # ci_appear_timeout: 10m\n  # ci_timeout: 60m\n",
		"promote:\n  author_map:\n", "promote:\n  author_map: {}\n",
		"promote:\nleak_scan:\n  markers:\ngate:\nsync:\nci:\n",
	} {
		cfg, warns, err := config.Parse([]byte(base + extra))
		if err != nil || len(warns) != 0 {
			t.Errorf("%q: %v %+v", extra, err, warns)
			continue
		}
		if !reflect.DeepEqual(cfg, want) {
			t.Errorf("%q: got %+v\nwant %+v", extra, cfg, want)
		}
	}
	// A key the section doesn't know is still an unknown key.
	for y, key := range map[string]string{
		base + "gate:\n  ci_timout: 5m\n":              "gate.ci_timout",
		base + "leak_scan:\n  markers:\n    bgin: X\n": "leak_scan.markers.bgin",
		base + "promote:\n  mode: squash\n":            "promote.mode",
	} {
		_, _, err := config.Parse([]byte(y))
		if ce := contract.From(err); ce == nil || ce.Code != contract.ConfigInvalid || ce.Details["unknown_key"] != key {
			t.Errorf("%q: want CONFIG_INVALID with unknown_key %q, got %v", y, key, err)
		}
	}
	// A scalar or a list where a section belongs is not empty: it stays an
	// error, as before (spec § 3.2 A6).
	for _, extra := range []string{"gate: 5\n", "promote: []\n", "leak_scan: off\n", "leak_scan:\n  markers: [a, b]\n"} {
		_, _, err := config.Parse([]byte(base + extra))
		if ce := contract.From(err); ce == nil || ce.Code != contract.ConfigInvalid {
			t.Errorf("%q: want CONFIG_INVALID, got %v", extra, err)
		}
	}
}
