package managed

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var on = Input{Public: "acme/widget", Schedule: "23 */6 * * *", Branches: []string{"main", "release/1.x"}}
var off = Input{Public: "acme/widget", Schedule: "off", Branches: []string{"main"}}

func TestRenderOnlyWhenScheduled(t *testing.T) {
	if len(Render(off)) != 0 {
		t.Fatal("schedule off must render no files")
	}
	got := Render(on)[SyncPath]
	for _, want := range []string{
		"# ghostbranch-managed: sync v1 sha256=",
		`- cron: "23 */6 * * *"`,
		"workflow_dispatch:",
		"PUBLIC_REPO: acme/widget",
		`BRANCHES: "main release/1.x"`,
		"SYNC_TOKEN: ${{ secrets.GHOSTBRANCH_SYNC_TOKEN }}",
		"DEFAULT_TOKEN: ${{ github.token }}",
		"permissions:\n  contents: write",
	} {
		if !bytes.Contains(got, []byte(want)) {
			t.Fatalf("rendered workflow lacks %q:\n%s", want, got)
		}
	}
	if bytes.Contains(got, []byte("uses:")) {
		t.Fatal("the sync workflow must use no actions")
	}
	if strings.Contains(Script(), "${{") {
		t.Fatal("sync.sh must not contain an Actions expression")
	}
	if !bytes.Equal(got, Render(on)[SyncPath]) {
		t.Fatal("Render is not deterministic")
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Fatal("the rendered workflow must have LF line endings only")
	}
	// Only the two Actions expressions in the env block may remain braced.
	if n := bytes.Count(got, []byte("{{")); n != 2 {
		t.Fatalf("rendered workflow has %d {{ sequences, want 2 (the env expressions):\n%s", n, got)
	}
	if bytes.Contains(got, []byte("{{SCRIPT}}")) || bytes.Contains(got, []byte("{{PUBLIC}}")) ||
		bytes.Contains(got, []byte("{{BRANCHES}}")) || bytes.Contains(got, []byte("{{SCHEDULE}}")) {
		t.Fatal("rendered workflow still has a template placeholder")
	}
}

func TestStatusOf(t *testing.T) {
	cur := Render(on)[SyncPath]
	old := bytes.Replace(cur, []byte("sync v1 "), []byte("sync v0 "), 1)
	old = rehash(old)
	newer := rehash(bytes.Replace(cur, []byte("sync v1 "), []byte("sync v9 "), 1))
	edited := append(append([]byte{}, cur...), []byte("# my tweak\n")...)
	otherCron := Render(Input{Public: on.Public, Schedule: "0 1 * * *", Branches: on.Branches})[SyncPath]
	for _, tc := range []struct {
		name   string
		cur    []byte
		exists bool
		in     Input
		want   Status
	}{
		{"wanted and absent", nil, false, on, Missing},
		{"current", cur, true, on, Current},
		{"older template", old, true, on, Outdated},
		{"config changed", otherCron, true, on, Outdated},
		{"hand-edited", edited, true, on, Modified},
		{"no header", []byte("name: mine\n"), true, on, Modified},
		{"newer binary wrote it", newer, true, on, Newer},
		{"not wanted, absent", nil, false, off, Current},
		{"not wanted, present", cur, true, off, Outdated},
		{"not wanted, hand-edited", edited, true, off, Modified},
	} {
		if got := StatusOf(SyncPath, tc.cur, tc.exists, tc.in); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// rehash recomputes a rendered file's header hash after a test edits the
// header's version, so only the version differs.
func rehash(b []byte) []byte {
	nl := bytes.IndexByte(b, '\n')
	head, body := string(b[:nl]), b[nl+1:]
	i := strings.Index(head, " sha256=")
	return append([]byte(head[:i]+" sha256="+hashOf(body)+"\n"), body...)
}

// The script is embedded in a literal block; parsing the workflow back must
// give it byte for byte, with the env and cron values in place.
func TestRenderedWorkflowParsesBackToTheScript(t *testing.T) {
	var wf struct {
		On struct {
			Schedule []struct {
				Cron string `yaml:"cron"`
			} `yaml:"schedule"`
		} `yaml:"on"`
		Jobs struct {
			Sync struct {
				Steps []struct {
					Run string            `yaml:"run"`
					Env map[string]string `yaml:"env"`
				} `yaml:"steps"`
			} `yaml:"sync"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(Render(on)[SyncPath], &wf); err != nil {
		t.Fatal(err)
	}
	if len(wf.On.Schedule) != 1 || wf.On.Schedule[0].Cron != on.Schedule {
		t.Fatalf("schedule = %+v", wf.On.Schedule)
	}
	steps := wf.Jobs.Sync.Steps
	if len(steps) != 1 || steps[0].Run != Script() {
		t.Fatalf("the parsed run: differs from Script():\n%q\nwant\n%q", steps[0].Run, Script())
	}
	want := map[string]string{"PUBLIC_REPO": "acme/widget", "BRANCHES": "main release/1.x",
		"SYNC_TOKEN": "${{ secrets.GHOSTBRANCH_SYNC_TOKEN }}", "DEFAULT_TOKEN": "${{ github.token }}"}
	if !reflect.DeepEqual(steps[0].Env, want) {
		t.Fatalf("env = %v", steps[0].Env)
	}
}

// An anonymous read of a private repo must fail, never wait at a prompt.
func TestScriptNeverPromptsForCredentials(t *testing.T) {
	if !strings.Contains(Script(), "\nexport GIT_TERMINAL_PROMPT=0\n") {
		t.Fatal("sync.sh must export GIT_TERMINAL_PROMPT=0")
	}
}
