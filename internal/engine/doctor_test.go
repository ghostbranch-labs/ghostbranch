package engine_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// doctorOpts are what the CLI passes for the harness's pair, with a fixed
// git and build so results don't depend on the machine.
func doctorOpts() engine.DoctorOptions {
	return engine.DoctorOptions{Repo: publicName, GitVersion: "git version 2.47.1", Version: "v0.0.0-test", Commit: "abc1234"}
}

// asApp gives the harness an App token from GHOSTBRANCH_TOKEN.
func (h *harness) asApp() {
	h.eng.Token, h.eng.TokenSource = "ghu_test", "GHOSTBRANCH_TOKEN"
}

func (h *harness) doctor(opt engine.DoctorOptions) (*engine.DoctorResult, map[string]engine.DoctorCheck) {
	h.t.Helper()
	res, err := h.eng.Doctor(h.ctx, opt)
	if err != nil {
		h.t.Fatalf("doctor: %v", err)
	}
	return res, byID(h.t, res)
}

// byID checks the shape every doctor result has (Review Focus 5): all six
// checks, in order, each with a known status, a message and details, and
// none stopped by an internal error.
func byID(t *testing.T, res *engine.DoctorResult) map[string]engine.DoctorCheck {
	t.Helper()
	ids := make([]string, len(res.Checks))
	m := map[string]engine.DoctorCheck{}
	for i, c := range res.Checks {
		ids[i], m[c.ID] = c.ID, c
		if !slices.Contains([]string{engine.DoctorOK, engine.DoctorWarn, engine.DoctorFail, engine.DoctorSkipped}, c.Status) {
			t.Errorf("%s: status %q", c.ID, c.Status)
		}
		if c.Message == "" || c.Details == nil {
			t.Errorf("%s: message %q, details %v", c.ID, c.Message, c.Details)
		}
		if strings.Contains(c.Message, "internal error") {
			t.Errorf("%s stopped: %s", c.ID, c.Message)
		}
		// next is built from FixArgv; fix is how it reads.
		if c.Fix != strings.Join(c.FixArgv, " ") {
			t.Errorf("%s: fix %q, argv %q", c.ID, c.Fix, c.FixArgv)
		}
	}
	if !reflect.DeepEqual(ids, engine.DoctorCheckIDs) {
		t.Fatalf("checks %v, want %v", ids, engine.DoctorCheckIDs)
	}
	return m
}

func wantStatus(t *testing.T, c engine.DoctorCheck, status string) {
	t.Helper()
	if c.Status != status {
		t.Fatalf("%s: %s (%s), want %s", c.ID, c.Status, c.Message, status)
	}
}

func TestDoctorOnAHealthyPair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.asApp()
	res, c := h.doctor(doctorOpts())
	for _, id := range []string{"tools", "token", "pair", "state", "ci"} {
		wantStatus(t, c[id], engine.DoctorOK)
	}
	wantStatus(t, c["guard"], engine.DoctorSkipped) // no directory given
	if s := res.Summary(); s.OK != 5 || s.Skipped != 1 || len(s.Failed) != 0 {
		t.Fatalf("summary %+v", s)
	}
	d := c["token"].Details
	if d["kind"] != "app" || d["account"] != "octo" || d["can_publish"] != "true" || d["source"] != "GHOSTBRANCH_TOKEN" {
		t.Fatalf("token details %+v", d)
	}
	if c["tools"].Details["git"] != "2.47.1" || !strings.Contains(c["tools"].Message, "v0.0.0-test") {
		t.Fatalf("tools %+v", c["tools"])
	}
}

// offline is GitHub out of reach: every call doctor makes fails as a
// network error would. Its Forge is nil, so a call doctor shouldn't make
// panics, and byID refuses the internal error that becomes.
type offline struct{ forge.Forge }

func (offline) GetRepo(context.Context, string) (forge.Repo, error) {
	return forge.Repo{}, fmt.Errorf("%w: dial tcp: lookup api.github.com: no such host", forge.ErrTransient)
}

func (offline) CurrentUser(context.Context) (forge.User, error) {
	return forge.User{}, fmt.Errorf("%w: dial tcp: lookup api.github.com: no such host", forge.ErrTransient)
}

// Review Focus 5: a half-broken setup still gets every check, each fail or
// skipped with a reason, and doctor never stops partway.
func TestDoctorOnABrokenSetupReportsEveryCheck(t *testing.T) {
	t.Parallel()
	const ok, warn, fail, skip = engine.DoctorOK, engine.DoctorWarn, engine.DoctorFail, engine.DoctorSkipped
	cases := []struct {
		name  string
		setup func(h *harness, opt *engine.DoctorOptions)
		want  map[string]string
	}{
		{"no token", func(h *harness, opt *engine.DoctorOptions) {
			h.eng.Forge, h.eng.Token, h.eng.TokenSource = nil, "", ""
			opt.TokenErr = contract.New(contract.AuthMissing, "no GitHub token: set GHOSTBRANCH_TOKEN, or run ghostbranch login")
		}, map[string]string{"tools": ok, "token": fail, "pair": skip, "guard": skip, "state": ok, "ci": skip}},
		{"offline", func(h *harness, opt *engine.DoctorOptions) { h.asApp(); h.eng.Forge = offline{} },
			map[string]string{"tools": ok, "token": fail, "pair": fail, "guard": skip, "state": ok, "ci": skip}},
		{"no pair", func(h *harness, opt *engine.DoctorOptions) { h.asApp(); opt.Repo = "" },
			map[string]string{"token": ok, "pair": skip, "guard": skip, "state": ok, "ci": skip}},
		{"not in a checkout", func(h *harness, opt *engine.DoctorOptions) { h.asApp(); opt.Dir = h.t.TempDir() },
			map[string]string{"pair": ok, "guard": skip}},
		{"an unreadable record", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
			h.corruptMeta("feat-y")
		}, map[string]string{"pair": ok, "state": fail, "ci": ok}},
		{"no workspace", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			h.eng.WS = nil
			opt.WorkspaceErr = errors.New("read workspace.json: invalid character 'x' looking for beginning of value")
		}, map[string]string{"pair": skip, "guard": skip, "state": fail, "ci": skip}},
		{"git too old", func(h *harness, opt *engine.DoctorOptions) { h.asApp(); opt.GitVersion = "git version 2.30.1" },
			map[string]string{"tools": fail, "pair": ok}},
		{"no git", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			opt.GitVersion, opt.GitErr = "", errors.New(`exec: "git": executable file not found in $PATH`)
		}, map[string]string{"tools": fail}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			opt := doctorOpts()
			tc.setup(h, &opt)
			_, c := h.doctor(opt)
			for id, want := range tc.want {
				wantStatus(t, c[id], want)
			}
			if tc.name == "an unreadable record" && c["state"].Fix != "ghostbranch feature show feat-y --repo acme/widget" {
				t.Fatalf("state fix %q", c["state"].Fix)
			}
		})
	}
}

// noWrites fails the test on any forge call that changes something.
type noWrites struct {
	forge.Forge
	t *testing.T
}

func (n *noWrites) CreatePrivateRepo(context.Context, string) (forge.Repo, error) {
	n.t.Error("doctor created a repo")
	return forge.Repo{}, forge.ErrForbidden
}
func (n *noWrites) SetDefaultBranch(context.Context, string, string) error {
	n.t.Error("doctor set a default branch")
	return forge.ErrForbidden
}
func (n *noWrites) CreatePR(context.Context, string, forge.NewPR) (forge.PR, error) {
	n.t.Error("doctor opened a PR")
	return forge.PR{}, forge.ErrForbidden
}
func (n *noWrites) CancelRun(context.Context, string, int64) error {
	n.t.Error("doctor cancelled a run")
	return forge.ErrForbidden
}
func (n *noWrites) ForceCancelRun(context.Context, string, int64) error {
	n.t.Error("doctor force-cancelled a run")
	return forge.ErrForbidden
}
func (n *noWrites) ClosePullRequest(context.Context, string, int) error {
	n.t.Error("doctor closed a PR")
	return forge.ErrForbidden
}
func (n *noWrites) PushAccess(ctx context.Context, full string) forge.PushAccess {
	return n.Forge.(forge.PushProber).PushAccess(ctx, full)
}
func (n *noWrites) ProbePermissions(ctx context.Context, full string) (forge.RepoPermissions, error) {
	return n.Forge.(forge.PermissionProber).ProbePermissions(ctx, full)
}
func (n *noWrites) OAuthScopes(ctx context.Context) ([]string, bool, error) {
	return n.Forge.(forge.PermissionProber).OAuthScopes(ctx)
}

// snapshot hashes every file under dir, with its mode. The local mirrors
// are left out: doctor fetches into them as status does (a cache of the
// remotes, not state).
func snapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "mirror.git" {
				return filepath.SkipDir
			}
			out[p] = "dir"
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		out[p] = fi.Mode().String() + " " + hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Review Focus 5: doctor writes nothing. No file under the workspace
// changes (records, ledger, locks, plans), no ref moves in either repo, no
// push and no forge write.
func TestDoctorWritesNothing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
	h.gateFeature("feat-x")
	h.grant(time.Hour)
	if _, _, err := h.eng.Promote(h.ctx, h.pair, engine.PromoteOptions{Feature: "feat-x", Mode: "squash", Message: "Add x", PlanOnly: true}); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, h.eng.WS.Dir)
	shadowBefore, publicBefore, pushes := h.shadowRefs(), h.publicRefs(), len(h.pushes)
	h.asApp()
	h.eng.Forge = &noWrites{Forge: h.forge, t: t}
	_, c := h.doctor(doctorOpts())
	wantStatus(t, c["pair"], engine.DoctorOK)
	if after := snapshot(t, h.eng.WS.Dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("doctor changed the workspace:\nbefore %v\nafter  %v", before, after)
	}
	if !reflect.DeepEqual(shadowBefore, h.shadowRefs()) || !reflect.DeepEqual(publicBefore, h.publicRefs()) {
		t.Fatal("doctor moved a ref")
	}
	if len(h.pushes) != pushes {
		t.Fatalf("doctor pushed: %+v", h.pushes[pushes:])
	}
}

// lockedKeychain fails every call as a locked OS keychain does: an error
// that isn't keychain.ErrUnavailable.
type lockedKeychain struct{}

func (lockedKeychain) Get(string, string) (string, error) { return "", errors.New("keychain locked") }
func (lockedKeychain) Set(string, string, string) error   { return errors.New("keychain locked") }
func (lockedKeychain) Delete(string, string) error        { return errors.New("keychain locked") }

// noToken is a run with no env token and the stored login in kc, resolved
// as the CLI does for doctor (no refresh): no token, and Resolve's error.
func noToken(h *harness, opt *engine.DoctorOptions, kc keychain.Store) {
	h.t.Helper()
	tok, _, err := auth.Resolve(h.ctx, auth.Source{Getenv: func(string) string { return "" }, Store: kc, Now: h.clock.Now})
	if tok != "" || err == nil {
		h.t.Fatalf("resolve: %q %v", tok, err)
	}
	h.eng.Forge, h.eng.Token, h.eng.TokenSource = nil, "", ""
	opt.TokenErr = err
}

func TestDoctorTokenCheck(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) // the harness clock
	login := func(expires, refreshExpires time.Time) *auth.Login {
		return &auth.Login{Token: "ghu_test", RefreshToken: "ghr_test", Expires: expires, RefreshExpires: refreshExpires,
			Account: "octo", Kind: "app", SchemaVersion: contract.RecordVersion}
	}
	cases := []struct {
		name    string
		setup   func(h *harness, opt *engine.DoctorOptions)
		status  string
		has     string // the message contains it
		fix     string
		details map[string]any
	}{
		{"an unknown-kind token can't say what it may write", func(h *harness, _ *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource = "test-token", "GHOSTBRANCH_TOKEN"
		}, engine.DoctorWarn, "Pull requests: write on acme/widget-shadow (gate)", "", map[string]any{"kind": "unknown"}},
		{"a fine-grained token missing a read fails", func(h *harness, _ *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource = "github_pat_test", "GHOSTBRANCH_TOKEN"
			h.forge.PermsDenied = map[string]map[string]bool{shadowName: {forge.PermActionsRead: true}}
		}, engine.DoctorFail, "Actions: read on acme/widget-shadow (gate)", "ghostbranch guide", nil},
		{"a classic token without the workflow scope fails", func(h *harness, _ *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource = "ghp_test", "GH_TOKEN"
			h.forge.Scopes = []string{"repo"}
		}, engine.DoctorFail, "Workflows: write on acme/widget-shadow (gate, init, sync)", "ghostbranch guide", nil},
		{"a classic token with repo and workflow is ok", func(h *harness, _ *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource = "ghp_test", "GH_TOKEN"
			h.forge.Scopes = []string{"repo", "workflow"}
		}, engine.DoctorOK, "can publish to acme/widget: true", "", map[string]any{"kind": "classic"}},
		{"a shadow-only token can't publish, which is a warning", func(h *harness, _ *engine.DoctorOptions) {
			h.asApp()
			h.forge.PushDenied = map[string]bool{publicName: true}
		}, engine.DoctorWarn, "can't publish to acme/widget", "", map[string]any{"can_publish": "false"}},
		{"an agent on the person's login", func(h *harness, opt *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource, h.eng.Actor, h.eng.LooksLikeAgent = "ghu_test", "login", "agent:claude-code", true
			opt.Login = login(now.Add(time.Hour), now.Add(90*24*time.Hour))
		}, engine.DoctorWarn, "looks like an agent's session", "", map[string]any{"agent_on_person_login": true}},
		{"a person on their own login", func(h *harness, opt *engine.DoctorOptions) {
			h.eng.Token, h.eng.TokenSource, h.eng.LooksLikeAgent = "ghu_test", "login", false
			opt.Login = login(now.Add(time.Hour), now.Add(90*24*time.Hour))
		}, engine.DoctorOK, "can publish to acme/widget: true", "", map[string]any{"account": "octo"}},
		{"the login is ending soon", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			h.eng.TokenSource = "login"
			opt.Login = login(now.Add(time.Hour), now.Add(3*24*time.Hour))
		}, engine.DoctorWarn, "the stored login ends on 2026-10-08", "ghostbranch login", nil},
		{"the login has expired", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			h.eng.TokenSource = "login"
			opt.Login = login(now.Add(-2*time.Hour), now.Add(-time.Hour))
		}, engine.DoctorFail, "the stored login expired", "ghostbranch login", nil},
		{"an expired access token isn't refreshed or used", func(h *harness, opt *engine.DoctorOptions) {
			h.asApp()
			h.eng.TokenSource = "login"
			opt.Login = login(now.Add(-time.Minute), now.Add(90*24*time.Hour))
			h.forge.UserErr = errors.New("doctor called GitHub with an expired token")
		}, engine.DoctorWarn, "the next command that uses GitHub refreshes it", "", nil},
		{"GitHub refuses the token", func(h *harness, _ *engine.DoctorOptions) {
			h.asApp()
			h.eng.TokenSource = "login"
			h.forge.UserErr = errors.New("GET https://api.github.com/user: 401 Bad credentials []")
		}, engine.DoctorFail, "GitHub refused the token from login", "ghostbranch login", nil},
		{"a corrupt stored login", func(h *harness, opt *engine.DoctorOptions) {
			kc := keychain.NewFake()
			_ = kc.Set(auth.Service, auth.Account, "not json")
			noToken(h, opt, kc)
		}, engine.DoctorFail, "can't be read", "ghostbranch logout", nil},
		// A locked keychain: logout would delete a login that works once
		// it is unlocked, so there is no fix to run.
		{"a keychain that can't be used", func(h *harness, opt *engine.DoctorOptions) {
			noToken(h, opt, lockedKeychain{})
		}, engine.DoctorFail, "unlock it", "", nil},
		{"a stored login from a newer Ghostbranch", func(h *harness, opt *engine.DoctorOptions) {
			kc := keychain.NewFake()
			_ = kc.Set(auth.Service, auth.Account, `{"schema_version":"0.99","token":"ghu_test","account":"octo","kind":"app"}`)
			noToken(h, opt, kc)
		}, engine.DoctorFail, "upgrade ghostbranch on this machine", "", nil},
		{"an expired stored login", func(h *harness, opt *engine.DoctorOptions) {
			kc := keychain.NewFake()
			if err := auth.SaveLogin(kc, *login(now.Add(-2*time.Hour), now.Add(-time.Hour))); err != nil {
				h.t.Fatal(err)
			}
			noToken(h, opt, kc)
		}, engine.DoctorFail, "expired", "ghostbranch login", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			opt := doctorOpts()
			tc.setup(h, &opt)
			_, c := h.doctor(opt)
			tok := c["token"]
			wantStatus(t, tok, tc.status)
			if !strings.Contains(tok.Message, tc.has) || tok.Fix != tc.fix {
				t.Fatalf("message %q fix %q; want it to contain %q, fix %q", tok.Message, tok.Fix, tc.has, tc.fix)
			}
			for k, v := range tc.details {
				if tok.Details[k] != v {
					t.Fatalf("details[%s] = %v, want %v (%+v)", k, tok.Details[k], v, tok.Details)
				}
			}
			// Review Focus 1: no token in what doctor says.
			for _, secret := range []string{"ghu_test", "ghr_test", "github_pat_test", "ghp_test", "test-token"} {
				if strings.Contains(tok.Message, secret) || strings.Contains(fmt.Sprint(tok.Details), secret) {
					t.Fatalf("the token check shows a token: %+v", tok)
				}
			}
		})
	}
}

func TestDoctorTokenGroups(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.init()
	h.eng.Token, h.eng.TokenSource = "github_pat_test", "GHOSTBRANCH_TOKEN"
	_, c := h.doctor(doctorOpts())
	groups, ok := c["token"].Details["groups"].(map[string]engine.GroupAccess)
	if !ok {
		t.Fatalf("groups: %T", c["token"].Details["groups"])
	}
	if g := groups["gate"]; len(g.Missing) != 0 || !slices.Contains(g.Unknown, "Workflows: write on acme/widget-shadow") {
		t.Fatalf("gate %+v", g)
	}
	if g := groups["audit"]; len(g.Missing)+len(g.Unknown)+len(g.Optional) != 0 {
		t.Fatalf("audit %+v (the fake lets every read through)", g)
	}
	if !reflect.DeepEqual(slices.Sorted(maps.Keys(groups)), []string{"audit", "gate", "init", "promote", "sync"}) {
		t.Fatalf("groups %v", groups)
	}
}

func TestDoctorPairCheck(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		setup  func(h *harness, opt *engine.DoctorOptions)
		status string
		fix    string
	}{
		{"shadow main behind", func(h *harness, _ *engine.DoctorOptions) {
			testutil.Commit(h.t, h.pubGit, "main", "upstream", map[string]string{"up.txt": "up\n"})
		}, engine.DoctorWarn, "ghostbranch sync --repo acme/widget"},
		{"managed files outdated", func(h *harness, _ *engine.DoctorOptions) {
			h.setConfig(`schedule: "off"`, `schedule: "23 */6 * * *"`)
		}, engine.DoctorWarn, "ghostbranch upgrade --repo acme/widget"},
		{"a public shadow repo", func(h *harness, _ *engine.DoctorOptions) { h.forge.SetPrivate(shadowName, false) },
			engine.DoctorFail, ""},
		{"--repo names an unregistered repo", func(h *harness, opt *engine.DoctorOptions) { opt.Repo = "acme/other" },
			engine.DoctorFail, "ghostbranch init acme/other --adopt --plan"},
		{"the shadow repo is gone", func(h *harness, _ *engine.DoctorOptions) {
			err := h.eng.WS.Update(h.clock.Now(), func(w *workspace.Workspace) bool {
				for i := range w.Pairs {
					if w.Pairs[i].Public == publicName {
						w.Pairs[i].Shadow = "acme/gone-shadow"
					}
				}
				return true
			})
			if err != nil {
				h.t.Fatal(err)
			}
		}, engine.DoctorFail, "ghostbranch init acme/widget --plan"},
		{"the overlay branch is missing", func(h *harness, _ *engine.DoctorOptions) {
			if err := h.shadowGit().DeleteRef(h.ctx, "refs/heads/ghostbranch"); err != nil {
				h.t.Fatal(err)
			}
		}, engine.DoctorFail, "ghostbranch init acme/widget --adopt --plan"},
		{"a checkout of an unregistered repo", func(h *harness, opt *engine.DoctorOptions) {
			opt.Repo, opt.Remotes = "", []workspace.Remote{{Name: "origin", URL: "https://github.com/acme/other.git"}}
		}, engine.DoctorSkipped, ""},
		{"a checkout of the pair", func(h *harness, opt *engine.DoctorOptions) {
			opt.Repo, opt.Remotes = "", []workspace.Remote{{Name: "origin", URL: "git@github.com:acme/widget.git"}}
		}, engine.DoctorOK, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.init()
			h.asApp()
			opt := doctorOpts()
			tc.setup(h, &opt)
			_, c := h.doctor(opt)
			wantStatus(t, c["pair"], tc.status)
			if c["pair"].Fix != tc.fix {
				t.Fatalf("fix %q, want %q (%s)", c["pair"].Fix, tc.fix, c["pair"].Message)
			}
		})
	}
}

// The guard check reads a real checkout, so git's user config must not
// leak in: t.Setenv keeps this test serial.
func TestDoctorGuardCheck(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(cfg, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	h := newHarness(t)
	h.init()
	h.asApp()
	dir := filepath.Join(t.TempDir(), "checkout")
	git("init", "-q", "-b", "main", dir)
	git("-C", dir, "remote", "add", "origin", "https://github.com/acme/widget.git")
	opt := doctorOpts()
	opt.Dir = dir

	_, c := h.doctor(opt)
	wantStatus(t, c["guard"], engine.DoctorFail)
	if c["guard"].Fix != "ghostbranch guard install" {
		t.Fatalf("fix %q", c["guard"].Fix)
	}

	install := func() {
		t.Helper()
		co, err := guard.Open(h.ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		hook, err := guard.PublicRemotes(h.ctx, co, h.eng.WS)
		if err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(t.TempDir(), "ghostbranch")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		hook.Binary = filepath.ToSlash(bin)
		if _, err := guard.Install(h.ctx, co, hook, false); err != nil {
			t.Fatal(err)
		}
	}
	install()
	_, c = h.doctor(opt)
	wantStatus(t, c["guard"], engine.DoctorOK)

	// A pushurl at the public repo, added after the guard: an older guard
	// misses it, and doctor says so.
	git("-C", dir, "remote", "add", "upstream", "https://example.com/mirror.git")
	git("-C", dir, "config", "--add", "remote.upstream.pushurl", "https://github.com/acme/widget.git")
	_, c = h.doctor(opt)
	wantStatus(t, c["guard"], engine.DoctorFail)
	if !strings.Contains(c["guard"].Message, "upstream") || !strings.Contains(c["guard"].Message, "pushurl") {
		t.Fatalf("message %q", c["guard"].Message)
	}

	install()
	opt.Getenv = func(k string) string {
		if k == "GHOSTBRANCH_HOME" {
			return "/tmp/elsewhere"
		}
		return ""
	}
	_, c = h.doctor(opt)
	wantStatus(t, c["guard"], engine.DoctorWarn)

	other := filepath.Join(t.TempDir(), "other")
	git("init", "-q", "-b", "main", other)
	git("-C", other, "remote", "add", "origin", "https://github.com/acme/other.git")
	opt.Dir = other
	_, c = h.doctor(opt)
	wantStatus(t, c["guard"], engine.DoctorSkipped)
}

func TestDoctorStateCheck(t *testing.T) {
	t.Parallel()
	t.Run("grants from a newer Ghostbranch", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.init()
		h.asApp()
		if err := os.WriteFile(h.eng.WS.GrantsPath(), []byte(`{"schema_version": "0.99", "grants": []}`+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorFail)
		if !strings.Contains(c["state"].Message, "newer Ghostbranch") {
			t.Fatalf("message %q", c["state"].Message)
		}
	})
	t.Run("a gate job pending for over an hour", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.init()
		h.asApp()
		h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
		if _, _, err := h.eng.GateDetached(h.ctx, h.pair, "feat-x"); err != nil {
			t.Fatal(err)
		}
		h.clock.Sleep(2 * time.Hour)
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorWarn)
		if !strings.HasPrefix(c["state"].Fix, "ghostbranch wait gate/feat-x/") || !strings.HasSuffix(c["state"].Fix, " --repo acme/widget") {
			t.Fatalf("fix %q", c["state"].Fix)
		}
	})
	t.Run("a feature from a newer Ghostbranch", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.init()
		h.asApp()
		h.startFeature("feat-x", map[string]string{"x.txt": "x\n"})
		h.newerMeta("feat-x")
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorFail)
	})
	t.Run("a ledger other users can read", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file modes are checked on macOS and Linux")
		}
		h := newHarness(t)
		h.init()
		h.asApp()
		if err := os.Chmod(h.eng.WS.LedgerPath(), 0o644); err != nil {
			t.Fatal(err)
		}
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorWarn)
		if c["state"].Fix != "chmod 600 "+h.eng.WS.LedgerPath() {
			t.Fatalf("fix %q", c["state"].Fix)
		}
	})
	t.Run("state directories other users can open", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file modes are checked on macOS and Linux")
		}
		h := newHarness(t)
		h.init()
		h.asApp()
		ws := h.eng.WS
		pairs := filepath.Join(ws.Dir, "pairs")
		for _, d := range []string{ws.Dir, pairs} {
			if err := os.Chmod(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorWarn)
		if c["state"].Fix != "chmod 700 "+ws.Dir+" "+pairs ||
			!strings.Contains(c["state"].Message, "chmod -R go-rwx "+pairs) {
			t.Fatalf("fix %q, message %q", c["state"].Fix, c["state"].Message)
		}
	})
	for _, c := range []struct {
		name    string
		ownHome bool
	}{{"a custom home other users can open", false}, {"the default home other users can open", true}} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if runtime.GOOS == "windows" {
				t.Skip("file modes are checked on macOS and Linux")
			}
			h := newHarness(t)
			h.eng.WS.OwnHome = c.ownHome
			h.init()
			h.asApp()
			home := h.eng.WS.Home()
			if err := os.Chmod(home, 0o755); err != nil {
				t.Fatal(err)
			}
			_, c2 := h.doctor(doctorOpts())
			wantStatus(t, c2["state"], engine.DoctorWarn)
			msg := c2["state"].Message
			if c2["state"].Fix != "chmod 700 "+home {
				t.Fatalf("fix %q", c2["state"].Fix)
			}
			never := strings.Contains(msg, "ghostbranch never changes the mode of "+home+" itself, because you chose it: run chmod 700 "+home+" if no one else uses it")
			next := strings.Contains(msg, "the next writing ghostbranch command makes "+home+" private")
			if never == c.ownHome || next != c.ownHome {
				t.Fatalf("message %q", msg)
			}
		})
	}
	t.Run("lock folders other users can open", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file modes are checked on macOS and Linux")
		}
		h := newHarness(t)
		h.init()
		h.asApp()
		locks := filepath.Join(h.eng.WS.Dir, "locks")
		if err := os.Chmod(locks, 0o755); err != nil {
			t.Fatal(err)
		}
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorWarn)
		if c["state"].Fix != "chmod 700 "+locks {
			t.Fatalf("fix %q", c["state"].Fix)
		}
	})
	t.Run("doctor never tightens", func(t *testing.T) {
		t.Parallel()
		if runtime.GOOS == "windows" {
			t.Skip("file modes are checked on macOS and Linux")
		}
		h := newHarness(t)
		h.init()
		h.asApp()
		if err := os.Chmod(h.eng.WS.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		h.doctor(doctorOpts())
		if fi, err := os.Stat(h.eng.WS.Dir); err != nil || fi.Mode().Perm() != 0o755 {
			t.Fatalf("doctor changed the workspace's mode: %v %v", fi.Mode(), err)
		}
	})
	t.Run("private state says so", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.init()
		h.asApp()
		_, c := h.doctor(doctorOpts())
		wantStatus(t, c["state"], engine.DoctorOK)
		if !strings.HasSuffix(c["state"].Message, "; its state directories are private to you") {
			t.Fatalf("message %q", c["state"].Message)
		}
	})
}

func TestDoctorCICheck(t *testing.T) {
	t.Parallel()
	secretFlow := "on: [pull_request]\njobs:\n  r:\n    runs-on: x\n    steps:\n      - run: echo ${{ secrets.NPM_TOKEN }}\n"
	pushFlow := "on:\n  push:\n    branches: [main]\njobs:\n  r:\n    runs-on: x\n    steps:\n      - run: echo hi\n"
	cases := []struct {
		name   string
		setup  func(h *harness)
		status string
	}{
		{"a missing secret", func(h *harness) {
			testutil.Commit(h.t, h.pubGit, "main", "secret", map[string]string{".github/workflows/rel.yml": secretFlow})
			h.forge.Secrets = map[string][]string{shadowName: {}}
		}, engine.DoctorWarn},
		{"secret names the token can't list", func(h *harness) {
			testutil.Commit(h.t, h.pubGit, "main", "secret", map[string]string{".github/workflows/rel.yml": secretFlow})
			h.forge.SecretsForbidden = map[string]bool{shadowName: true}
		}, engine.DoctorOK},
		{"a push-triggered workflow", func(h *harness) {
			testutil.Commit(h.t, h.pubGit, "main", "push", map[string]string{".github/workflows/push.yml": pushFlow})
		}, engine.DoctorWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			tc.setup(h) // before init, so init mirrors the workflows
			h.init()
			h.asApp()
			_, c := h.doctor(doctorOpts())
			wantStatus(t, c["ci"], tc.status)
			if tc.status == engine.DoctorWarn && c["ci"].Fix != "ghostbranch audit --repo acme/widget" {
				t.Fatalf("fix %q", c["ci"].Fix)
			}
		})
	}
}
