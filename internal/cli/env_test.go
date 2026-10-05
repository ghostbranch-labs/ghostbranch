package cli_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

type testEnv struct {
	t      *testing.T
	root   string
	forge  *fake.Forge
	pubGit *gitx.Git
	vars   map[string]string
	env    cli.Env
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
		Now:          clock.Now,
		Sleep:        clock.Sleep,
		Rand:         &testutil.SeqReader{},
		NewForge:     func(string) (forge.Forge, error) { return f, nil },
		GHToken:      func() (string, error) { return "", errors.New("gh is not logged in") },
		CwdRemotes:   func() []string { return nil },
		NewScanner:   func(s overlay.Set) (leak.Scanner, error) { return leak.NewGitleaks(s) },
		PollInterval: time.Second, SettleWindow: 5 * time.Second,
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

func withLeakyFeature(te *testEnv) {
	initOnly(te)
	te.mustRun("feature", "start", "feat-x")
	te.shadowCommit("feat-x", map[string]string{"config.env": "TOKEN=" + testutil.FakeGitHubToken() + "\n"})
}
