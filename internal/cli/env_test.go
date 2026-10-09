package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type testEnv struct {
	t       *testing.T
	root    string
	forge   *fake.Forge
	pubGit  *gitx.Git
	vars    map[string]string
	env     cli.Env
	finding string
	oldJob  string // a replaced gate job, for <OLD_JOB>
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	root := t.TempDir()
	f := fake.New(filepath.Join(root, "forge"))
	dir, err := f.AddRepo("acme/widget", false)
	if err != nil {
		t.Fatal(err)
	}
	pubGit := gitx.New(dir)
	testutil.Commit(t, pubGit, "main", "initial", map[string]string{
		"README.md": "# widget\n", ".github/workflows/ci.yml": "name: ci\non: [pull_request]\n"})
	clock := testutil.NewClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	te := &testEnv{t: t, root: root, forge: f, pubGit: pubGit,
		vars: map[string]string{"GHOSTBRANCH_HOME": filepath.Join(root, "home"), "GHOSTBRANCH_TOKEN": "test-token"}}
	te.env = cli.Env{
		Stdin:        strings.NewReader(""),
		Getenv:       func(k string) string { return te.vars[k] },
		UserHome:     func() (string, error) { return filepath.Join(root, "home-dir"), nil },
		Now:          clock.Now,
		Sleep:        clock.SleepCtx,
		Rand:         &testutil.SeqReader{},
		NewForge:     func(string) (forge.Forge, error) { return f, nil },
		GHToken:      func() (string, error) { return "", errors.New("gh is not logged in") },
		CwdRemotes:   func() []workspace.Remote { return nil },
		NewScanner:   func(r leak.Rules) (leak.Scanner, error) { return leak.NewGitleaks(r) },
		PollInterval: time.Second, SettleWindow: 5 * time.Second,
		Executable:   func() (string, error) { return filepath.Join(root, "bin", "ghostbranch"), nil },
		Getwd:        func() (string, error) { return root, nil },
		GuardState:   func(string) (bool, bool) { return false, false },
		ShadowRemote: func(string) (string, string, bool) { return "shadow", "shadow", true },
	}
	return te
}

func (te *testEnv) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := te.env
	env.Stdout, env.Stderr = &out, &errb
	code := cli.Run(args, env)
	return code, out.String(), errb.String()
}

func (te *testEnv) runJSON(args ...string) (int, map[string]any) {
	te.t.Helper()
	code, out, errOut := te.run(append(args, "--json")...)
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		te.t.Fatalf("%v: stdout is not one JSON object: %v\nstdout: %s\nstderr: %s", args, err, out, errOut)
	}
	return code, m
}

func (te *testEnv) mustRun(args ...string) {
	te.t.Helper()
	if code, out, errOut := te.run(args...); code != 0 {
		te.t.Fatalf("%v exited %d\nstdout: %s\nstderr: %s", args, code, out, errOut)
	}
}

func (te *testEnv) shadowCommit(branch string, files map[string]string) {
	te.t.Helper()
	testutil.Commit(te.t, gitx.New(te.forge.RepoDir("acme/widget-shadow")), branch, "work", files)
}

func (te *testEnv) shadowCommitMsg(branch, msg string, files map[string]string) {
	te.t.Helper()
	testutil.Commit(te.t, gitx.New(te.forge.RepoDir("acme/widget-shadow")), branch, msg, files)
}

// job is the id of feature name's last gate job, from the candidate its
// metadata in the shadow repo records.
func (te *testEnv) job(name string) string {
	te.t.Helper()
	raw, ok, err := gitx.New(te.forge.RepoDir("acme/widget-shadow")).ShowFile(te.t.Context(), "refs/ghostbranch/features/"+name, "feature.json")
	if err != nil || !ok {
		te.t.Fatalf("read %s's metadata: %v", name, err)
	}
	var m struct {
		Gate *struct {
			Candidate string `json:"candidate"`
		} `json:"gate"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Gate == nil || len(m.Gate.Candidate) < 12 {
		te.t.Fatalf("%s has no gate candidate: %v %s", name, err, raw)
	}
	return "gate/" + name + "/" + m.Gate.Candidate[:12]
}

func (te *testEnv) lastPlanID() string {
	te.t.Helper()
	entries, _ := os.ReadDir(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "plans"))
	var ids []string
	for _, e := range entries {
		ids = append(ids, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		te.t.Fatal("no plans saved")
	}
	return ids[len(ids)-1]
}

// Preludes shared with the golden tests.
func initOnly(te *testEnv) { te.mustRun("init", "acme/widget") }
func withFeature(te *testEnv) {
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"feature.txt": "new\n"})
}
func gatedFeature(te *testEnv) { withFeature(te); te.mustRun("gate", "feat-x") }

// grantAgents writes an agent grant for acme/widget, as `agents allow` would
// after a person confirmed it. The binary has no way to skip that step.
func grantAgents(te *testEnv) {
	te.t.Helper()
	ws := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default")
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	exp := now.Add(6 * time.Hour)
	s := grant.NewStore(filepath.Join(ws, "grants.json"), filepath.Join(ws, "grants.lock"))
	if _, err := s.Put(grant.Grant{ID: "g-0a0b0c", Public: "acme/widget", Shadow: "acme/widget-shadow",
		CreatedAt: now, ExpiresAt: &exp, CreatedBy: "human:octo"}, now); err != nil {
		te.t.Fatal(err)
	}
}

func publishable(te *testEnv) { gatedFeature(te); grantAgents(te) }

func withLeakyFeature(te *testEnv) {
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
}

var codeLine = regexp.MustCompile(`press Enter: ([A-Z0-9]{6})`)

// typist is a fake console with a person at it: it reads the code it is
// shown and types back answer(code).
type typist struct {
	pr       *io.PipeReader
	pw       *io.PipeWriter
	answer   func(code string) string
	seen     strings.Builder
	answered bool
}

func newTypist(answer func(code string) string) *typist {
	pr, pw := io.Pipe()
	return &typist{pr: pr, pw: pw, answer: answer}
}

func (c *typist) Read(p []byte) (int, error) { return c.pr.Read(p) }
func (c *typist) Write(p []byte) (int, error) {
	c.seen.Write(p)
	if m := codeLine.FindStringSubmatch(c.seen.String()); m != nil && !c.answered {
		c.answered = true
		line := c.answer(m[1])
		go func() { _, _ = c.pw.Write([]byte(line)) }()
	}
	return len(p), nil
}
func (c *typist) Close() error { return c.pw.Close() }

// atConsole makes the test env look like a person's terminal.
func atConsole(te *testEnv, answer func(code string) string) *typist {
	ty := newTypist(answer)
	te.env.ConsoleUsable = true
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { return ty, nil }
	return ty
}

func typeTheCode(code string) string { return code + "\n" }

// firstWaivable gates feat-x and returns its first waivable finding id.
func (te *testEnv) firstWaivable() string {
	te.t.Helper()
	_, m := te.runJSON("gate", "feat-x")
	e, _ := m["error"].(map[string]any)
	d, _ := e["details"].(map[string]any)
	ids, _ := d["waivable"].([]any)
	if len(ids) == 0 {
		te.t.Fatalf("no waivable finding: %v", m)
	}
	return ids[0].(string)
}

// leakyWithID is withLeakyFeature with its finding id saved for <FINDING>.
func leakyWithID(te *testEnv) { withLeakyFeature(te); te.finding = te.firstWaivable() }

// waiveLeak waives feat-x's finding the way a person does: by typing the
// code at the console. The binary has no other way.
func waiveLeak(te *testEnv) {
	te.t.Helper()
	leakyWithID(te)
	saved := te.env
	atConsole(te, typeTheCode)
	te.mustRun("waivers", "add", "feat-x", te.finding, "--reason", "test fixture")
	te.env = saved
}

func (te *testEnv) waiversFile() string {
	return filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "waivers.json")
}
