package workflow

import (
	"reflect"
	"testing"
)

var names = Names{Public: "acme/widget", Shadow: "acme/widget-shadow"}

func TestIsWorkflowPath(t *testing.T) {
	for p, want := range map[string]bool{
		".github/workflows/ci.yml": true, ".github/workflows/ci.yaml": true,
		".github/workflows/sub/ci.yml": false, ".github/workflows/ci.json": false,
		"ci.yml": false, ".github/workflows/": false,
	} {
		if IsWorkflowPath(p) != want {
			t.Errorf("%s: want %v", p, want)
		}
	}
}

// Review Focus 4 is the "Acme/Widget" case.
func TestGuardForms(t *testing.T) {
	other := Names{Public: "acme/widget", Shadow: "octo/widget-shadow"}
	for _, tc := range []struct {
		expr              string
		n                 Names
		guard, rewritable bool
		want              string
	}{
		{"github.repository == 'acme/widget'", names, true, true,
			"(github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow')"},
		{"${{ github.repository == 'acme/widget' }}", names, true, true,
			"${{ (github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow') }}"},
		{"'acme/widget' == github.repository", names, true, true,
			"(github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow')"},
		{"github.repository == 'Acme/Widget'", names, true, true,
			"(github.repository == 'Acme/Widget' || github.repository == 'acme/widget-shadow')"},
		{"GitHub.Repository != 'acme/widget' && github.event_name == 'push'", names, true, true,
			"(GitHub.Repository != 'acme/widget' && GitHub.Repository != 'acme/widget-shadow') && github.event_name == 'push'"},
		{"github.event.repository.full_name == 'acme/widget'", names, true, true,
			"(github.event.repository.full_name == 'acme/widget' || github.event.repository.full_name == 'acme/widget-shadow')"},
		{"github.repository_owner == 'acme'", names, false, false, ""}, // same owner: already matches
		{"github.repository_owner == 'acme'", other, true, true,
			"(github.repository_owner == 'acme' || github.repository_owner == 'octo')"},
		{"startsWith(github.repository, 'acme/')", names, false, false, ""},
		{"startsWith(github.repository, 'acme/')", other, true, false, ""},
		{"contains(fromJSON('[\"acme/widget\"]'), github.repository)", names, true, false, ""},
		{"github.repository == 'other/repo'", names, false, false, ""},
		{"github.event_name == 'push'", names, false, false, ""},
		// already widened: handled for that subject and operator
		{"github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow'", names, false, false, ""},
		{"(github.repository != 'acme/widget' && github.repository != 'acme/widget-shadow')", names, false, false, ""},
		{"github.repository == 'acme/widget' || github.event.repository.full_name == 'acme/widget-shadow'", names, true, true,
			"(github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow') || github.event.repository.full_name == 'acme/widget-shadow'"},
		// the public name counts only as a whole name
		{"github.repository == 'acme/widget-docs'", names, false, false, ""},
		{"github.repository == 'acme/widget.github'", names, false, false, ""},
		// a URL or API path to the public repo still guards on it
		{"github.event.repository.html_url == 'https://github.com/acme/widget'", names, true, false, ""},
		{"contains(github.event.repository.url, 'repos/acme/widget')", names, true, false, ""},
		{"github.repositoryUrl == 'https://github.com/acme/widget.git'", names, true, false, ""},
		// the push guard the audit advises only skips pushes: not a repo guard
		{"github.event_name != 'push' || github.repository == 'acme/widget'", names, false, false, ""},
		{"${{ github.repository == 'Acme/Widget' || github.event_name != 'push' }}", names, false, false, ""},
		{"(github.event_name != 'push') || (github.event.repository.full_name == 'acme/widget')", names, false, false, ""},
		{"GitHub.Event_Name != 'push' || 'acme/widget' == GitHub.Repository", names, false, false, ""},
		{"'push' != github.event_name || github.repository == 'acme/widget'", names, false, false, ""},
		{"matrix.os == 'linux' && (github.event_name != 'push' || github.repository == 'acme/widget')", names, false, false, ""},
		{"github.repository == 'acme/widget' && (github.event_name != 'push' || github.repository == 'acme/widget')", names, true, true,
			"(github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow') && (github.event_name != 'push' || github.repository == 'acme/widget')"},
		// other forms still guard
		{"github.event_name == 'push' || github.repository == 'acme/widget'", names, true, true,
			"github.event_name == 'push' || (github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow')"},
		{"github.event_name != 'push' && github.repository == 'acme/widget'", names, true, true,
			"github.event_name != 'push' && (github.repository == 'acme/widget' || github.repository == 'acme/widget-shadow')"},
	} {
		got, guard, ok := guardExpr(tc.expr, tc.n)
		if guard != tc.guard || ok != tc.rewritable || (ok && got != tc.want) {
			t.Errorf("%s:\n got %q guard=%v ok=%v\nwant %q guard=%v ok=%v", tc.expr, got, guard, ok, tc.want, tc.guard, tc.rewritable)
		}
	}
}

const sample = `name: ci
on:
  push:
    branches: [main, 'release/**']
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - name: Publish coverage
        if: github.repository == 'acme/widget'
        env:
          TOKEN: ${{ secrets.CODECOV_TOKEN }}
        run: echo ${{ secrets['NPM_TOKEN'] }} ${{ secrets.GITHUB_TOKEN }}
  deploy:
    if: ${{ github.repository == 'acme/widget' && github.ref == 'refs/heads/main' }}
    environment: production
    runs-on: ubuntu-latest
    steps:
      - id: ship
        if: contains(fromJSON('["acme/widget"]'), github.repository)
        run: echo ${{ secrets.deploy_key }}
  reuse:
    uses: ./.github/workflows/lib.yml
    secrets: inherit
`

func TestParse(t *testing.T) {
	f, err := Parse(".github/workflows/ci.yml", []byte(sample), names)
	if err != nil {
		t.Fatal(err)
	}
	pub, ship := "Publish coverage", "ship"
	want := []Guard{
		{Path: ".github/workflows/ci.yml", Job: "test", Step: &pub, Expr: "github.repository == 'acme/widget'", Rewritable: true},
		{Path: ".github/workflows/ci.yml", Job: "deploy", Expr: "${{ github.repository == 'acme/widget' && github.ref == 'refs/heads/main' }}", Rewritable: true},
		{Path: ".github/workflows/ci.yml", Job: "deploy", Step: &ship, Expr: `contains(fromJSON('["acme/widget"]'), github.repository)`},
	}
	if !reflect.DeepEqual(f.Guards, want) {
		t.Fatalf("guards = %+v", f.Guards)
	}
	if !reflect.DeepEqual(f.Secrets, []string{"CODECOV_TOKEN", "DEPLOY_KEY", "NPM_TOKEN"}) {
		t.Fatalf("secrets = %v", f.Secrets)
	}
	if !reflect.DeepEqual(f.Inherit, []string{"reuse"}) || !reflect.DeepEqual(f.Environments, []string{"deploy"}) {
		t.Fatalf("inherit = %v, environments = %v", f.Inherit, f.Environments)
	}
	if !f.PushTriggered([]string{"main"}) || !f.PushTriggered([]string{"release/1.x"}) || f.PushTriggered([]string{"dev"}) {
		t.Fatal("push filter")
	}
}

func TestPushTriggers(t *testing.T) {
	for on, want := range map[string]bool{
		"on: push":                 true,
		"on: [push, pull_request]": true,
		"on: pull_request":         false,
		"on:\n  push:\n":           true,
		"on:\n  push:\n    branches-ignore: [main]\n": false,
		"on:\n  push:\n    branches-ignore: [dev]\n":  true,
		"on:\n  push:\n    tags: ['v*']\n":            false,
		"on:\n  push:\n    branches: ['feature/*']\n": false,
		"on:\n  push:\n    branches: ['**']\n":        true,
		"on:\n  push:\n    branches: ['ma?n']\n":      true, // can't evaluate: counted as a match
		"on:\n  schedule:\n    - cron: '0 0 * * *'\n": false,
	} {
		f, err := Parse("w.yml", []byte(on+"\njobs: {}\n"), names)
		if err != nil {
			t.Fatalf("%q: %v", on, err)
		}
		if got := f.PushTriggered([]string{"main"}); got != want {
			t.Errorf("%q: got %v", on, got)
		}
	}
}

// A gate PR runs a workflow triggered on pull_request or
// pull_request_target, or on a push its head branch matches.
func TestRunsOnPR(t *testing.T) {
	for on, want := range map[string]bool{
		"on: pull_request":                             true,
		"on: [pull_request_target]":                    true,
		"on:\n  pull_request:\n    branches: [main]\n": true,
		"on: push": true,
		"on:\n  push:\n    branches: ['ghostbranch-gate/**']\n": true,
		"on:\n  push:\n    branches: [main]\n":                  false,
		"on:\n  push:\n    tags: ['v*']\n":                      false,
		"on: [workflow_dispatch]":                               false,
		"on:\n  schedule:\n    - cron: '0 0 * * *'\n":           false,
		"name: no trigger":                                      false,
	} {
		f, err := Parse("w.yml", []byte(on+"\njobs: {}\n"), names)
		if err != nil {
			t.Fatalf("%q: %v", on, err)
		}
		if got := f.RunsOnPR("ghostbranch-gate/feat-x"); got != want {
			t.Errorf("%q: got %v", on, got)
		}
	}
}

func TestPushGuarded(t *testing.T) {
	const guard = "github.event_name != 'push' || github.repository == 'acme/widget'"
	job := func(id, cond string) string {
		s := "  " + id + ":\n    runs-on: x\n"
		if cond != "" {
			s += "    if: " + cond + "\n"
		}
		return s + "    steps:\n      - run: echo\n"
	}
	for _, tc := range []struct {
		name, jobs string
		want       bool
	}{
		{"every job guarded", job("a", guard) + job("b", "${{ 'acme/widget' == github.repository || github.event_name != 'push' }}"), true},
		{"combined with an existing if", job("a", "matrix.os == 'linux' && ("+guard+")"), true},
		{"one job unguarded", job("a", guard) + job("b", ""), false},
		{"another repo", job("a", "github.event_name != 'push' || github.repository == 'other/repo'"), false},
		{"a plain repo guard", job("a", "github.repository == 'acme/widget'"), false},
		{"a step-level guard only", "  a:\n    runs-on: x\n    steps:\n      - if: " + guard + "\n        run: echo\n", false},
		{"no jobs", "  {}\n", false},
	} {
		f, err := Parse("w.yml", []byte("on: push\njobs:\n"+tc.jobs), names)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if f.PushGuarded != tc.want {
			t.Errorf("%s: PushGuarded = %v", tc.name, f.PushGuarded)
		}
		if !f.PushTriggered([]string{"main"}) {
			t.Errorf("%s: PushTriggered must not change", tc.name)
		}
	}
}

func TestParseBadYAML(t *testing.T) {
	if _, err := Parse("w.yml", []byte("jobs: [\n"), names); err == nil {
		t.Fatal("want an error for invalid YAML")
	}
	f, err := Parse("w.yml", []byte("- just\n- a list\n"), names)
	if err != nil || len(f.Guards) != 0 {
		t.Fatalf("a non-mapping document: %+v %v", f, err)
	}
}
