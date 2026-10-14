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
	Name            string            `yaml:"name"`
	ID              string            `yaml:"id"`
	If              string            `yaml:"if"`
	Uses            string            `yaml:"uses"`
	Run             string            `yaml:"run"`
	With            map[string]string `yaml:"with"`
	Env             map[string]string `yaml:"env"`
	ContinueOnError string            `yaml:"continue-on-error"`
}

// publishWorkflow is the part of publish.yml these tests read.
type publishWorkflow struct {
	On          map[string]yaml.Node `yaml:"on"`
	Permissions map[string]string    `yaml:"permissions"`
	Concurrency struct {
		Group            string `yaml:"group"`
		CancelInProgress string `yaml:"cancel-in-progress"`
	} `yaml:"concurrency"`
	Jobs map[string]publishJobDef `yaml:"jobs"`
}

// publishJobDef is the part of a publish.yml job these tests read.
type publishJobDef struct {
	If              string            `yaml:"if"`
	Environment     string            `yaml:"environment"`
	ContinueOnError string            `yaml:"continue-on-error"`
	Permissions     map[string]string `yaml:"permissions"`
	Defaults        struct {
		Run struct {
			Shell string `yaml:"shell"`
		} `yaml:"run"`
	} `yaml:"defaults"`
	Steps []publishStep `yaml:"steps"`
}

// readPublish reads .github/workflows/publish.yml.
func readPublish(t *testing.T) publishWorkflow {
	t.Helper()
	raw, err := os.ReadFile("../../.github/workflows/publish.yml")
	if err != nil {
		t.Fatal(err)
	}
	var wf publishWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.Jobs) != 1 {
		t.Fatalf("publish.yml has %d jobs; the checks below assume one", len(wf.Jobs))
	}
	if _, ok := wf.Jobs["publish"]; !ok {
		t.Fatal("publish.yml has no publish job")
	}
	return wf
}

func (s publishStep) label() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Uses
}

// publishJob reads publish.yml's one job.
func publishJob(t *testing.T) publishJobDef {
	t.Helper()
	return readPublish(t).Jobs["publish"]
}

// TestPublishTriggerPermissionsAndConcurrency: only a published release
// starts it, with read-only contents and an OIDC token, in the release
// environment, and runs one at a time in publish order, so the tap and the
// bucket end on the newer of two releases. A running publish is never
// cancelled.
func TestPublishTriggerPermissionsAndConcurrency(t *testing.T) {
	wf := readPublish(t)
	var types []string
	if rel, ok := wf.On["release"]; !ok || len(wf.On) != 1 {
		t.Errorf("on: has %d triggers; want release only", len(wf.On))
	} else if err := rel.Decode(&struct {
		Types *[]string `yaml:"types"`
	}{&types}); err != nil || strings.Join(types, ",") != "published" {
		t.Errorf("on.release.types = %v (%v); want [published]", types, err)
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		t.Errorf("workflow permissions = %v; want contents: read", wf.Permissions)
	}
	job := wf.Jobs["publish"]
	if p := job.Permissions; len(p) != 2 || p["contents"] != "read" || p["id-token"] != "write" {
		t.Errorf("job permissions = %v; want exactly contents: read and id-token: write", p)
	}
	if job.Environment != "release" {
		t.Errorf("environment = %q; want release", job.Environment)
	}
	if wf.Concurrency.Group != "publish" || wf.Concurrency.CancelInProgress != "false" {
		t.Errorf("concurrency = %+v; want the one serial publish group, never cancelling", wf.Concurrency)
	}
}

// TestPublishFailsClosed: nothing can let a failed check carry on. No step
// or job continues on error, no if: runs after a failure, and the verify
// and access steps run unconditionally and never swallow an exit code.
func TestPublishFailsClosed(t *testing.T) {
	job := publishJob(t)
	if job.ContinueOnError != "" {
		t.Errorf("the job has continue-on-error: %s", job.ContinueOnError)
	}
	risky := regexp.MustCompile(`always\(\)|failure\(\)|cancelled\(\)`)
	swallow := regexp.MustCompile(`\|\|\s*(true|:)|set \+e|set \+o pipefail`)
	for _, s := range job.Steps {
		if s.ContinueOnError != "" {
			t.Errorf("step %q has continue-on-error: %s", s.label(), s.ContinueOnError)
		}
		if risky.MatchString(s.If) {
			t.Errorf("step %q runs after a failure: if: %s", s.label(), s.If)
		}
		if swallow.MatchString(s.Run) {
			t.Errorf("step %q swallows a failure: %s", s.label(), swallow.FindString(s.Run))
		}
		if (s.Name == "verify before distributing" || strings.Contains(s.Run, "installation/repositories")) && s.If != "" {
			t.Errorf("step %q is conditional: if: %s", s.label(), s.If)
		}
	}
}

// TestPublishJobGuardAndShell: the job never runs outside the real repo, and
// every step runs under bash's pipefail, so a failed gh api in a pipe fails
// the job instead of routing a test build to the tap.
func TestPublishJobGuardAndShell(t *testing.T) {
	job := publishJob(t)
	// Two forms only: the original guard, and what Ghostbranch's gate writes
	// in its place on a candidate (workflow.widen: the guard also allows the
	// shadow repo). Anything else, a missing guard or another repo, fails.
	const guard = "github.repository == 'ghostbranch-labs/ghostbranch'"
	const gated = "(github.repository == 'ghostbranch-labs/ghostbranch' || github.repository == 'ghostbranch-labs/ghostbranch-shadow')"
	if job.If != guard && job.If != gated {
		t.Errorf("job if = %q; want %q, or the gate's rewrite %q", job.If, guard, gated)
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
	for _, want := range []string{
		"sha256sum --strict -c checksums.txt",
		"cosign verify-blob",
		`--certificate-identity "$IDENTITY" \`,
		`--certificate-oidc-issuer https://token.actions.githubusercontent.com \`,
		`--certificate-github-workflow-sha "$(git rev-parse HEAD)" \`,
	} {
		if !strings.Contains(v, want) {
			t.Errorf("the verify step lacks %q", want)
		}
	}
	if strings.Contains(v, "regexp") {
		t.Error("the verify step uses a regexp identity; it must be exact")
	}
	const identity = "https://github.com/${{ github.repository }}/.github/workflows/release.yml@refs/tags/${{ github.event.release.tag_name }}"
	if got := job.Steps[verify].Env["IDENTITY"]; got != identity {
		t.Errorf("IDENTITY = %q; want %q", got, identity)
	}
	for _, s := range job.Steps[:verify] {
		if ref, ok := s.With["ref"]; ok && !strings.HasPrefix(ref, "refs/tags/") {
			t.Errorf("step %q checks out ref %q, which may resolve to a branch", s.label(), ref)
		}
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

// TestPublishNPMWithProvenance: npm publishes only from a public repo, with
// provenance, under the routed dist-tag, using an exact npm 11.x.
func TestPublishNPMWithProvenance(t *testing.T) {
	job := publishJob(t)
	var publish string
	for _, s := range job.Steps {
		if strings.Contains(s.Run, "npm publish") || strings.HasPrefix(s.Uses, "actions/setup-node@") {
			if s.If != "${{ !github.event.repository.private }}" {
				t.Errorf("step %q if = %q; want the public-repo check only", s.label(), s.If)
			}
		}
		if strings.Contains(s.Run, "npm publish") {
			publish = s.Run
		}
	}
	line := regexp.MustCompile(`(?m)^\s*npm publish .*$`).FindString(publish)
	for _, want := range []string{"--provenance", `--tag "$NPM_TAG"`, "--access public"} {
		if !strings.Contains(line, want) {
			t.Errorf("npm publish lacks %s: %q", want, line)
		}
	}
	if !regexp.MustCompile(`npm install -g npm@11\.\d+\.\d+\n`).MatchString(publish) {
		t.Error("npm isn't pinned to an exact 11.x")
	}
}
