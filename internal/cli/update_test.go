package cli_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeReleases struct {
	mu         sync.Mutex
	tag        string
	advisories string
	delay      time.Duration
	hits       int
	auth       []string
	agents     []string
}

func (f *fakeReleases) count() int { f.mu.Lock(); defer f.mu.Unlock(); return f.hits }

// newFakeReleases serves GitHub's latest-release and advisory endpoints for
// the update checker. An empty tag answers 404 (no release yet).
func newFakeReleases(t *testing.T, tag, advisories string) (*fakeReleases, string) {
	t.Helper()
	f := &fakeReleases{tag: tag, advisories: advisories}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits++
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.agents = append(f.agents, r.Header.Get("User-Agent"))
		tag, adv, delay := f.tag, f.advisories, f.delay
		f.mu.Unlock()
		if delay > 0 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(delay):
			}
		}
		switch r.URL.Path {
		case "/repos/ghostbranch-labs/ghostbranch/releases/latest":
			if tag == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprintf(w, `{"tag_name":%q,"draft":false,"prerelease":false}`, tag)
		case "/repos/ghostbranch-labs/ghostbranch/security-advisories":
			fmt.Fprint(w, adv)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv.URL
}

const securityAdvisory = `[{"ghsa_id":"GHSA-2222-3333-4444","summary":"s","withdrawn_at":null,
 "vulnerabilities":[{"vulnerable_version_range":"< 0.13.1","patched_versions":"0.13.1"}]}]`

// releaseBuild makes te a release build pointed at the fake server, on a
// terminal, waiting long enough that the look always lands in this run.
func (te *testEnv) releaseBuild(version, url string) {
	te.env.Build = func() (string, string) { return version, "0123456789ab" }
	te.vars["GHOSTBRANCH_UPDATE_URL"] = url
	te.env.UpdateWait = 10 * time.Second
	te.env.StderrTerminal = true
}

const page013 = "https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.0"

func TestOrdinaryReminderAfterTheOutput(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	code, out, errOut := te.run("version")
	if code != 0 || !strings.HasPrefix(out, "ghostbranch v0.12.0") {
		t.Fatalf("%d %q", code, out)
	}
	if want := "! Ghostbranch 0.13.0 is out (you have 0.12.0).\n  Upgrade: " + page013 + "\n"; !strings.HasSuffix(errOut, want) {
		t.Fatalf("stderr %q\nwant suffix %q", errOut, want)
	}
}

func TestSecurityReminder(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "v0.13.1", securityAdvisory)
	te.releaseBuild("v0.13.0", url)
	_, _, errOut := te.run("version")
	if want := "✗ Ghostbranch 0.13.1 fixes a security issue (GHSA-2222-3333-4444); you have 0.13.0.\n  Upgrade now: https://github.com/ghostbranch-labs/ghostbranch/releases/tag/v0.13.1\n"; !strings.HasSuffix(errOut, want) {
		t.Fatalf("%q", errOut)
	}
}

// The reminder follows a failing command's error and next steps.
func TestTheReminderFollowsAFailingCommand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	code, _, errOut := te.run("gate", "no-such-feature")
	i, j := strings.Index(errOut, "[FEATURE_NOT_FOUND]"), strings.Index(errOut, "! Ghostbranch 0.13.0 is out")
	if code != 2 || i < 0 || j < i {
		t.Fatalf("%d %q", code, errOut)
	}
}

// Agents: UPDATE_AVAILABLE in the warnings, on success and on errors; no
// reminder on stderr under --json.
func TestUpdateAvailableInJSON(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, url := newFakeReleases(t, "v0.13.1", securityAdvisory)
	te.releaseBuild("v0.13.0", url)
	code, out, errOut := te.run("version", "--json")
	if code != 0 || !strings.Contains(out, `"code": "UPDATE_AVAILABLE"`) || !strings.Contains(out, `"security": true`) ||
		strings.Contains(errOut, "Ghostbranch 0.13.1") {
		t.Fatalf("%d\nstdout %s\nstderr %q", code, out, errOut)
	}
	code, env := te.runJSON("gate", "no-such-feature")
	ws, _ := env["warnings"].([]any)
	if code != 2 || len(ws) == 0 || ws[len(ws)-1].(map[string]any)["code"] != "UPDATE_AVAILABLE" {
		t.Fatalf("error envelope: %d %v", code, env)
	}
}

func TestNoReminderWhenStderrIsNotATerminal(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	te.env.StderrTerminal = false
	if _, _, errOut := te.run("version"); strings.Contains(errOut, "Ghostbranch 0.13.0") {
		t.Fatalf("%q", errOut)
	}
}

func TestOffSwitchesNeverLook(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		version string
		vars    map[string]string
	}{
		{"opt out", "v0.12.0", map[string]string{"GHOSTBRANCH_NO_UPDATE_CHECK": "1"}},
		{"CI", "v0.12.0", map[string]string{"CI": "true"}},
		{"GitHub Actions", "v0.12.0", map[string]string{"GITHUB_ACTIONS": "true"}},
		{"development build", "v0.0.0-20261011120000-abcdef123456", nil},
		{"dirty", "v0.12.0+dirty", nil},
		{"devel", "(devel)", nil},
	} {
		te := newTestEnv(t)
		f, url := newFakeReleases(t, "v0.13.0", "[]")
		te.releaseBuild(c.version, url)
		for k, v := range c.vars {
			te.vars[k] = v
		}
		code, out, errOut := te.run("version", "--json")
		if code != 0 || f.count() != 0 || strings.Contains(out+errOut, "UPDATE_AVAILABLE") {
			t.Errorf("%s: %d hits\n%s", c.name, f.count(), out)
		}
	}
}

func TestHelpAndCompletionNeverLook(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	for _, args := range [][]string{{"--help"}, {}, {"help", "gate"}, {"completion", "bash"}, {"__complete", "ga"}, {"__completeNoDesc", "ga"}} {
		te.run(args...)
	}
	// None of these reaches render, so nothing would wait for a look that
	// leaked through: give one time to reach the server before counting.
	time.Sleep(500 * time.Millisecond)
	if f.count() != 0 {
		t.Fatalf("%d hits", f.count())
	}
	// doctor starts no background look: its own fresh look is the only one
	// (a latest request, then advisories for the newer release), not two.
	te.run("doctor")
	if f.count() != 2 {
		t.Fatalf("doctor made %d requests, want one look's 2", f.count())
	}
}

// guard check, the pre-push hook, never looks: it runs inside every git
// push, so it neither reminds nor waits there.
func TestGuardCheckNeverLooks(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	te.env.Stdin = strings.NewReader("refs/heads/main 1111111111111111111111111111111111111111 refs/heads/main 0000000000000000000000000000000000000000\n")
	_, out, errOut := te.run("guard", "check", "origin", "https://github.com/acme/widget.git")
	time.Sleep(500 * time.Millisecond) // as above: room for a leaked look to land
	if f.count() != 0 || strings.Contains(out+errOut, "0.13.0") {
		t.Fatalf("%d hits\nstdout %q\nstderr %q", f.count(), out, errOut)
	}
}

// Reminders are kept per audience, and from 1.0 on a person is reminded once
// per release: a piped human run records nothing, the first terminal run
// reminds, the second does not, and an agent still gets its warning.
func TestRemindersAreKeptPerAudienceFromOnePointZero(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "v1.3.0", "[]")
	te.releaseBuild("v1.2.0", url)
	te.env.StderrTerminal = false
	if _, _, errOut := te.run("version"); strings.Contains(errOut, "Ghostbranch 1.3.0") {
		t.Fatalf("piped run reminded: %q", errOut)
	}
	te.env.StderrTerminal = true
	if _, _, errOut := te.run("version"); !strings.Contains(errOut, "! Ghostbranch 1.3.0 is out (you have 1.2.0).") {
		t.Fatalf("first terminal run (the piped one recorded nothing): %q", errOut)
	}
	if _, _, errOut := te.run("version"); strings.Contains(errOut, "Ghostbranch 1.3.0") {
		t.Fatalf("second run reminded again: %q", errOut)
	}
	if _, out, _ := te.run("version", "--json"); !strings.Contains(out, `"code": "UPDATE_AVAILABLE"`) {
		t.Fatalf("agent audience got no warning: %s", out)
	}
}

// The look is saved: a second command within 4 hours asks nothing, and
// below 1.0 it reminds again.
func TestTheLookIsSavedAndReused(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	te.run("version")
	n := f.count()
	_, _, errOut := te.run("version")
	if f.count() != n || !strings.Contains(errOut, "! Ghostbranch 0.13.0 is out") {
		t.Fatalf("hits %d → %d\n%q", n, f.count(), errOut)
	}
	if _, err := os.Stat(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "update-check.json")); err != nil {
		t.Fatal(err)
	}
}

// Privacy: the stored token never reaches the update server.
func TestTheLookCarriesNoToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.13.1", securityAdvisory)
	te.releaseBuild("v0.13.0", url)
	te.run("version") // GHOSTBRANCH_TOKEN is set in te.vars
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.auth) != 2 {
		t.Fatalf("%d requests", len(f.auth))
	}
	for i := range f.auth {
		if f.auth[i] != "" || f.agents[i] != "ghostbranch" {
			t.Fatalf("request %d: auth %q agent %q", i, f.auth[i], f.agents[i])
		}
	}
}

// Review Focus 2: a slow server costs at most the wait; the command's exit
// code and output are unchanged.
func TestASlowLookNeverHoldsTheCommand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	f, url := newFakeReleases(t, "v0.13.0", "[]")
	f.delay = 2 * time.Second
	te.releaseBuild("v0.12.0", url)
	te.env.UpdateWait = 100 * time.Millisecond
	start := time.Now()
	code, out, errOut := te.run("version")
	if code != 0 || time.Since(start) > 1500*time.Millisecond || strings.Contains(errOut, "Ghostbranch 0.13.0") || !strings.HasPrefix(out, "ghostbranch v0.12.0") {
		t.Fatalf("%d after %s\n%q", code, time.Since(start), errOut)
	}
	// The look goes on in the background and saves when the server answers.
	// Wait for that, so it doesn't write into the temp dir while the test's
	// cleanup removes it.
	cache := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "update-check.json")
	for deadline := time.Now().Add(6 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if _, err := os.Stat(cache); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the background look never saved")
		}
	}
}

// Review Focus 3: a cache that can't be written is silent.
func TestAnUnwritableCacheIsSilent(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "v0.13.0", "[]")
	te.releaseBuild("v0.12.0", url)
	if err := os.MkdirAll(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "update-check.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, _, errOut := te.run("version")
	if code != 0 || !strings.Contains(errOut, "! Ghostbranch 0.13.0 is out") || strings.Contains(errOut, "update-check") {
		t.Fatalf("%d %q", code, errOut)
	}
}

func TestNoReleaseYetIsSilent(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	_, url := newFakeReleases(t, "", "[]")
	te.releaseBuild("v0.12.0", url)
	if code, out, errOut := te.run("version", "--json"); code != 0 || strings.Contains(out+errOut, "UPDATE_AVAILABLE") {
		t.Fatalf("%d %s %q", code, out, errOut)
	}
}
