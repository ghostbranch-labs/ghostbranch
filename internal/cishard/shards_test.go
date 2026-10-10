// Package cishard checks the CI workflow's test shards. It has no code of
// its own: its test reads .github/workflows/ci.yml and the engine's test
// files, and fails if an engine test would run in no shard or in two.
//
// It lives outside internal/engine on purpose. Inside it, the check would
// itself be put in a shard by name, and a pattern change that left it out
// would switch it off silently. Here it runs in the "other" shard (every
// package but the engine), and it reads the engine's files with go/parser
// rather than importing them, so it costs no engine build.
package cishard_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// The paths are relative to this package's directory, where go test runs.
var (
	ciFile    = filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	engineDir = filepath.Join("..", "engine")
)

const (
	enginePkg = "./internal/engine"
	allPkgs   = "./..." // the shard that runs every package but the engine
)

// shard is one entry of the test job's matrix.
type shard struct {
	Shard string `yaml:"shard"`
	Pkg   string `yaml:"pkg"`
	Run   string `yaml:"run"`
}

// readShards returns the test job's matrix entries from ci.yml.
func readShards(t *testing.T) []shard {
	t.Helper()
	b, err := os.ReadFile(ciFile)
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]struct {
			Strategy struct {
				Matrix struct {
					Include []shard `yaml:"include"`
				} `yaml:"matrix"`
			} `yaml:"strategy"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(b, &wf); err != nil {
		t.Fatalf("parse %s: %v", ciFile, err)
	}
	job, ok := wf.Jobs["test"]
	if !ok {
		t.Fatalf("%s has no test job", ciFile)
	}
	return job.Strategy.Matrix.Include
}

// isTestFunc follows go test's naming rule: the prefix, then nothing or a
// character that isn't a lower-case letter.
func isTestFunc(name, prefix string) bool {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	if rest == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(rest)
	return !unicode.IsLower(r)
}

// engineTests lists the top-level functions that go test's -run selects
// among in internal/engine: TestXxx (not TestMain), FuzzXxx and ExampleXxx.
func engineTests(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(engineDir, "*_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var names []string
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range af.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Recv != nil {
				continue
			}
			n := fn.Name.Name
			if n != "TestMain" && (isTestFunc(n, "Test") || isTestFunc(n, "Fuzz") || isTestFunc(n, "Example")) {
				names = append(names, n)
			}
		}
	}
	// A wrong path would find nothing, and nothing would fail.
	if len(names) < 100 {
		t.Fatalf("found %d tests in %s; is the path right?", len(names), engineDir)
	}
	slices.Sort(names)
	return names
}

// engineShards checks the matrix's shape and compiles the engine shards'
// patterns. The matrix must have exactly one shard of every package but
// the engine (pkg ./..., no pattern) and at least two engine shards (pkg
// ./internal/engine). Each engine shard has a pattern without "/", which
// go test -run applies to top-level names alone.
func engineShards(shards []shard) ([]string, []*regexp.Regexp, error) {
	var names []string
	var res []*regexp.Regexp
	seen, others := map[string]bool{}, 0
	for _, s := range shards {
		if s.Shard == "" || seen[s.Shard] {
			return nil, nil, fmt.Errorf("shard name %q is empty or repeated", s.Shard)
		}
		seen[s.Shard] = true
		switch s.Pkg {
		case allPkgs:
			if s.Run != "" {
				return nil, nil, fmt.Errorf("shard %s runs every package but the engine and must not filter them (run %q)", s.Shard, s.Run)
			}
			others++
		case enginePkg:
			if s.Run == "" || strings.Contains(s.Run, "/") {
				return nil, nil, fmt.Errorf("engine shard %s: run %q must be a non-empty pattern without /", s.Shard, s.Run)
			}
			re, err := regexp.Compile(s.Run)
			if err != nil {
				return nil, nil, fmt.Errorf("engine shard %s: %v", s.Shard, err)
			}
			names, res = append(names, s.Shard), append(res, re)
		default:
			return nil, nil, fmt.Errorf("shard %s: pkg %q is neither %s nor %s", s.Shard, s.Pkg, enginePkg, allPkgs)
		}
	}
	if others != 1 {
		return nil, nil, fmt.Errorf("want one shard of every package but the engine (pkg %s), got %d", allPkgs, others)
	}
	if len(res) < 2 {
		return nil, nil, fmt.Errorf("want the engine split into at least two shards, got %d", len(res))
	}
	return names, res, nil
}

// shardsOf names the engine shards whose pattern selects test.
func shardsOf(test string, names []string, res []*regexp.Regexp) []string {
	var in []string
	for i, re := range res {
		if re.MatchString(test) {
			in = append(in, names[i])
		}
	}
	return in
}

// Every engine test runs in exactly one CI shard. A test added later, or a
// pattern changed, can't drop a test out of CI or run it twice.
func TestEveryEngineTestIsInExactlyOneShard(t *testing.T) {
	t.Parallel()
	names, res, err := engineShards(readShards(t))
	if err != nil {
		t.Fatalf("%s: %v", ciFile, err)
	}
	for _, test := range engineTests(t) {
		if in := shardsOf(test, names, res); len(in) != 1 {
			t.Errorf("%s is in %d engine shards %v; it must be in exactly one: change the run patterns in %s", test, len(in), in, ciFile)
		}
	}
}

// The check itself catches a gap, an overlap and a malformed matrix.
func TestTheShardCheckCatchesGapsOverlapsAndBadShards(t *testing.T) {
	t.Parallel()
	other := shard{Shard: "other", Pkg: allPkgs}
	names, res, err := engineShards([]shard{{"a-f", enginePkg, "^Test[A-F]"}, {"f-z", enginePkg, "^Test[F-Z]"}, other})
	if err != nil {
		t.Fatal(err)
	}
	for test, want := range map[string]int{"TestApply": 1, "TestFold": 2, "TestZed": 1, "Test_x": 0, "Test": 0, "FuzzX": 0} {
		if got := len(shardsOf(test, names, res)); got != want {
			t.Errorf("%s is in %d shards, want %d", test, got, want)
		}
	}
	for name, bad := range map[string][]shard{
		"no other shard":       {{"a", enginePkg, "^Test[A-M]"}, {"b", enginePkg, "^Test[N-Z]"}},
		"two other shards":     {{"a", enginePkg, "^Test[A-M]"}, {"b", enginePkg, "^Test[N-Z]"}, other, {"other2", allPkgs, ""}},
		"one engine shard":     {{"a", enginePkg, "^Test"}, other},
		"empty pattern":        {{"a", enginePkg, ""}, {"b", enginePkg, "^Test"}, other},
		"subtest pattern":      {{"a", enginePkg, "^Test[A-M]/x"}, {"b", enginePkg, "^Test[N-Z]"}, other},
		"bad regexp":           {{"a", enginePkg, "^Test[A-M"}, {"b", enginePkg, "^Test[N-Z]"}, other},
		"repeated name":        {{"a", enginePkg, "^Test[A-M]"}, {"a", enginePkg, "^Test[N-Z]"}, other},
		"filtered other shard": {{"a", enginePkg, "^Test[A-M]"}, {"b", enginePkg, "^Test[N-Z]"}, {"other", allPkgs, "^Test"}},
		"unknown package":      {{"a", enginePkg, "^Test[A-M]"}, {"b", enginePkg, "^Test[N-Z]"}, other, {"cli", "./internal/cli", ""}},
	} {
		if _, _, err := engineShards(bad); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if !isTestFunc("TestX", "Test") || !isTestFunc("Test", "Test") || !isTestFunc("Test_x", "Test") || isTestFunc("Testing", "Test") {
		t.Error("isTestFunc doesn't follow go test's naming rule")
	}
}
