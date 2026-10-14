package main

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// publishStep is the part of a publish.yml step these tests read.
type publishStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

func (s publishStep) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Uses
}

// publishJob reads .github/workflows/publish.yml's one job.
func publishJob(t *testing.T) (job struct {
	If       string `yaml:"if"`
	Defaults struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	} `yaml:"defaults"`
	Steps []publishStep `yaml:"steps"`
}) {
	t.Helper()
	raw, err := os.ReadFile("../../.github/workflows/publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf struct {
		Jobs map[string]yaml.Node `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("publish.yml has %d jobs; the checks below assume one", len(wf.Jobs))
	}
	node := wf.Jobs["publish"]
	if err := node.Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

// TestPublishJobGuardAndShell: the job never runs outside the real repo, and
// every step runs under bash's pipefail, so a failed gh api in a pipe fails
// the job instead of routing a test build to the tap.
func TestPublishJobGuardAndShell(t *testing.T) {
	job := publishJob(t)
	if job.If != "github.repository == 'ghostbranch-labs/ghostbranch'" {
		t.Errorf("job if = %q; want the repo guard", job.If)
	}
	if job.Defaults.Run.Shell != "bash" {
		t.Errorf("defaults.run.shell = %q; want bash (pipefail)", job.Defaults.Run.Shell)
	}
}

// TestPublishVerifiesBeforeAnythingElse: only checkout, the toolchain setup
// and the download come before the verify step, which checks every file
// against checksums.txt and checksums.txt against release.yml's exact
// identity. The release is downloaded once, so npm builds from the files
// that were verified.
func TestPublishVerifiesBeforeAnythingElse(t *testing.T) {
	job := publishJob(t)
	verify := -1
	for i, s := range job.Steps {
		if s.Name == "verify before distributing" {
			verify = i
			break
		}
	}
	if verify < 0 {
		t.Fatal("no verify before distributing step")
	}
	v := job.Steps[verify].Run
	for _, want := range []string{"sha256sum --strict -c checksums.txt", "cosign verify-blob", `--certificate-identity "$IDENTITY"`} {
		if !strings.Contains(v, want) {
			t.Errorf("the verify step lacks %q", want)
		}
	}
	if strings.Contains(v, "regexp") {
		t.Error("the verify step uses a regexp identity; it must be exact")
	}
	for _, s := range job.Steps[:verify] {
		switch {
		case strings.HasPrefix(s.Uses, "actions/checkout@"),
			strings.HasPrefix(s.Uses, "actions/setup-go@"),
			strings.HasPrefix(s.Uses, "sigstore/cosign-installer@"),
			s.Name == "download the release":
		default:
			t.Errorf("step %q runs before the verify step", s.label())
		}
	}
	downloads := 0
	for _, s := range job.Steps {
		downloads += strings.Count(s.Run, "release download")
		if strings.Contains(s.Run, "build.mjs") && !strings.Contains(s.Run, "--assets assets ") {
			t.Errorf("step %q builds npm packages from somewhere other than the verified assets/", s.label())
		}
	}
	if downloads != 1 {
		t.Errorf("publish.yml downloads the release %d times; want once, before the verify step", downloads)
	}
}

// TestPublishRoutesWithEqualsFlags: Go's bool flags take only -flag=value;
// "-prerelease false" stops parsing and drops every later flag. The stable
// count leaves out the release being published.
func TestPublishRoutesWithEqualsFlags(t *testing.T) {
	job := publishJob(t)
	var route string
	for _, s := range job.Steps {
		if s.ID == "route" {
			route = s.Run
		}
	}
	line := regexp.MustCompile(`(?m)^.*render route.*$`).FindString(route)
	if line == "" {
		t.Fatal("the route step doesn't run render route")
	}
	for _, flag := range []string{"-prerelease", "-stable-exists"} {
		if !strings.Contains(line, flag+"=") {
			t.Errorf("render route passes %s without =: %s", flag, line)
		}
	}
	if regexp.MustCompile(`-(prerelease|stable-exists)\s`).MatchString(line) {
		t.Errorf("render route passes a bool flag with a space: %s", line)
	}
	if !strings.Contains(route, ".tag_name != env.TAG") {
		t.Error("the stable count doesn't leave out the release being published")
	}
}

// TestPublishPushesOnlyWhenRouted: the push to the tap and the bucket is the
// only step that reads the route's tap output, and it runs only when that is
// true and the repo is public.
func TestPublishPushesOnlyWhenRouted(t *testing.T) {
	job := publishJob(t)
	var readers []string
	for _, s := range job.Steps {
		if strings.Contains(s.If+s.Run, "steps.route.outputs.tap") {
			readers = append(readers, s.label())
			if !strings.Contains(s.If, "steps.route.outputs.tap == 'true'") || !strings.Contains(s.If, "!github.event.repository.private") {
				t.Errorf("step %q if = %q", s.label(), s.If)
			}
		}
		if strings.Contains(s.Run, "push origin") && s.Name != "push the formula and the manifest" {
			t.Errorf("step %q pushes", s.label())
		}
	}
	if len(readers) != 1 || readers[0] != "push the formula and the manifest" {
		t.Errorf("steps reading the tap output: %q; want only the push step", readers)
	}
}

// TestPublishAppTokenReachesOnlyTheTapAndBucket: the push token is limited to
// the two repos, and a check that can fail lists every repository the App is
// installed on and refuses anything but those two.
func TestPublishAppTokenReachesOnlyTheTapAndBucket(t *testing.T) {
	job := publishJob(t)
	tokens := map[string]publishStep{}
	for _, s := range job.Steps {
		if strings.HasPrefix(s.Uses, "actions/create-github-app-token@") {
			tokens[s.ID] = s
			if _, ok := s.With["app-id"]; ok {
				t.Errorf("token %q uses the deprecated app-id; want client-id", s.ID)
			}
		}
	}
	if len(tokens) != 2 {
		t.Fatalf("publish.yml mints %d App tokens; want app and reach", len(tokens))
	}
	if got := tokens["app"].With["repositories"]; got != "homebrew-tap,scoop-bucket" {
		t.Errorf("the push token's repositories = %q", got)
	}
	reach := tokens["reach"].With
	if _, ok := reach["repositories"]; ok || reach["permission-metadata"] != "read" || len(reach) != 4 {
		t.Errorf("the reach token must be read-only and unscoped: %v", reach)
	}
	var check string
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "installation/repositories") {
			check = s.Run
		}
	}
	for _, want := range []string{
		`want="ghostbranch-labs/homebrew-tap ghostbranch-labs/scoop-bucket"`,
		`GH_TOKEN="$REACH_TOKEN"`,
		`if [ "$got" != "$want" ]; then`,
		"exit 1",
	} {
		if !strings.Contains(check, want) {
			t.Errorf("the access check lacks %q", want)
		}
	}
}

// TestPublishPinsEveryAction: every uses: is a full commit SHA with its tag.
func TestPublishPinsEveryAction(t *testing.T) {
	raw, err := os.ReadFile("../../.github/workflows/publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	pinned := regexp.MustCompile(`^\s*- uses: [^ ]+@[0-9a-f]{40} # v\S+$`)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "uses:") && !pinned.MatchString(strings.TrimRight(line, "\r")) {
			t.Errorf("not pinned: %s", line)
		}
	}
}
