package engine_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

const testInstallURL = "https://github.com/apps/ghostbranch-cli/installations/new"

// setupHarness is a harness signed in as user, with an app login when app
// is true, and the repos given created in the fake.
func setupHarness(t *testing.T, user string, app bool, repos ...string) *harness {
	t.Helper()
	h := newHarness(t)
	h.forge.User = forge.User{Login: user, ID: 7}
	h.eng.Token, h.eng.TokenSource, h.eng.AppLogin = "tok", "login", app
	for _, r := range repos {
		if _, err := h.forge.AddRepo(r, false); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func (h *harness) setup(opt engine.SetupOptions) *engine.Setup {
	h.t.Helper()
	if opt.InstallURL == "" {
		opt.InstallURL = testInstallURL
	}
	s, err := h.eng.SetupCheck(h.ctx, opt)
	if err != nil {
		h.t.Fatalf("SetupCheck: %v", err)
	}
	ids := make([]string, len(s.Steps))
	for i, st := range s.Steps {
		ids[i] = st.ID
		if st.Details == nil {
			h.t.Errorf("step %s has nil details", st.ID)
		}
	}
	if !reflect.DeepEqual(ids, engine.SetupStepIDs) {
		h.t.Fatalf("step ids = %v", ids)
	}
	return s
}

func step(t *testing.T, s *engine.Setup, id string) engine.SetupStep {
	t.Helper()
	for _, st := range s.Steps {
		if st.ID == id {
			return st
		}
	}
	t.Fatalf("no step %s", id)
	return engine.SetupStep{}
}

func statuses(s *engine.Setup) map[string]string {
	m := map[string]string{}
	for _, st := range s.Steps {
		m[st.ID] = st.Status
	}
	return m
}

func TestSetupAllDoneAppLogin(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", true)
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "acme", AccountType: "User", Selection: "all"}}
	h.init()
	s := h.setup(engine.SetupOptions{Public: publicName})
	want := map[string]string{"login": "done", "account": "done", "app_installed": "done", "shadow": "done",
		"app_access": "done", "pair": "done", "guard": "skipped", "doctor": "todo"}
	if got := statuses(s); !reflect.DeepEqual(got, want) {
		t.Fatalf("statuses = %v", got)
	}
	if st := s.FirstStop(); st != nil {
		t.Fatalf("FirstStop = %+v", st)
	}
	if st := step(t, s, "app_access"); st.URL != "" {
		t.Fatalf("app_access url = %q", st.URL)
	}
}

func TestSetupAppNotInstalled(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushgux", true, "ayushgux/trial")
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial"})
	st := step(t, s, "app_installed")
	if st.Status != engine.SetupNeedsHuman || st.URL != testInstallURL {
		t.Fatalf("app_installed = %+v", st)
	}
	wantAcc := []map[string]any{{"account": "ayushgux", "status": "missing", "install_url": testInstallURL}}
	if !reflect.DeepEqual(st.Details["accounts"], wantAcc) {
		t.Fatalf("accounts = %#v", st.Details["accounts"])
	}
	if !strings.Contains(st.Message, "ayushgux") || !strings.Contains(st.Message, "ayushgux/trial") {
		t.Fatalf("message = %q", st.Message)
	}
	if fs := s.FirstStop(); fs == nil || fs.ID != "app_installed" {
		t.Fatalf("FirstStop = %+v", fs)
	}
	if got := step(t, s, "app_access").Status; got != engine.SetupTodo {
		t.Fatalf("app_access = %s, want todo until the app is installed", got)
	}
	ce := s.StopError(s.FirstStop())
	if ce.Code != contract.AppNotInstalled {
		t.Fatalf("code = %s", ce.Code)
	}
	if want := []map[string]any{{"account": "ayushgux", "install_url": testInstallURL}}; !reflect.DeepEqual(ce.Details["accounts"], want) {
		t.Fatalf("error accounts = %#v", ce.Details["accounts"])
	}
	if !reflect.DeepEqual(ce.Details["steps"], s.Steps) {
		t.Fatalf("error steps = %#v", ce.Details["steps"])
	}
}

func TestSetupTwoAccountsNeeded(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "me", true, "acme/x")
	h.forge.Orgs = map[string]bool{"acme": true}
	h.forge.Installations = []forge.Installation{{ID: 3, Account: "me", AccountType: "User", Selection: "all"}}
	s := h.setup(engine.SetupOptions{Public: "acme/x", Shadow: "me/x-shadow"})
	st := step(t, s, "app_installed")
	if st.Status != engine.SetupNeedsHuman {
		t.Fatalf("app_installed = %+v", st)
	}
	want := []map[string]any{
		{"account": "acme", "status": "missing", "install_url": testInstallURL},
		{"account": "me", "status": "installed", "install_url": ""},
	}
	if !reflect.DeepEqual(st.Details["accounts"], want) {
		t.Fatalf("accounts = %#v", st.Details["accounts"])
	}
	if got := step(t, s, "account").Status; got != engine.SetupDone {
		t.Fatalf("account = %s", got)
	}
	ce := s.StopError(s.FirstStop())
	if want := []map[string]any{{"account": "acme", "install_url": testInstallURL}}; ce.Code != contract.AppNotInstalled || !reflect.DeepEqual(ce.Details["accounts"], want) {
		t.Fatalf("error = %s %#v", ce.Code, ce.Details["accounts"])
	}
}

func TestSetupOrgNotInstalledWording(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "me", true, "acme/x")
	h.forge.Orgs = map[string]bool{"acme": true}
	s := h.setup(engine.SetupOptions{Public: "acme/x"})
	st := step(t, s, "app_installed")
	if st.Status != engine.SetupNeedsHuman || !strings.Contains(st.Message, "if you aren't an owner of acme") ||
		!strings.Contains(st.Message, "GitHub asks an owner to approve the request, and setup continues once they do") {
		t.Fatalf("app_installed = %+v", st)
	}
}

func TestSetupSelectedMissingPublic(t *testing.T) {
	t.Parallel()
	// The public repo is the user's own: a selection that doesn't list it
	// lacks it, so GetRepo (which reads any public repo) isn't asked.
	h := setupHarness(t, "ayushgux", true, "ayushgux/trial")
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "selected"}}
	h.forge.InstallRepos = map[int64][]string{7: {}}
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial"})
	st := step(t, s, "app_access")
	if st.Status != engine.SetupNeedsHuman || st.URL != "https://github.com/settings/installations/7" ||
		!strings.Contains(st.Message, "ayushgux/trial") {
		t.Fatalf("app_access = %+v", st)
	}
	if fs := s.FirstStop(); fs == nil || fs.ID != "app_access" {
		t.Fatalf("FirstStop = %+v", fs)
	}
	ce := s.StopError(s.FirstStop())
	if ce.Code != contract.AppCantSeeRepo || ce.Details["configure_url"] != "https://github.com/settings/installations/7" ||
		!reflect.DeepEqual(ce.Details["repos"], []string{"ayushgux/trial"}) {
		t.Fatalf("error = %s %#v", ce.Code, ce.Details)
	}
}

// A user-owned public repo the app's user token can't even read (private,
// say) is missing too.
func TestSetupSelectedMissingUnreadablePublic(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushgux", true)
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "selected"}}
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial"})
	if st := step(t, s, "app_access"); st.Status != engine.SetupNeedsHuman || !reflect.DeepEqual(st.Details["repos"], []string{"ayushgux/trial"}) {
		t.Fatalf("app_access = %+v", st)
	}
}

func TestSetupShadowUnlistedButReachableIsDone(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushgux", true, "ayushgux/trial", "ayushgux/trial-shadow")
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "selected"}}
	h.forge.InstallRepos = map[int64][]string{7: {"ayushgux/trial"}}
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial", Adopt: true})
	if st := step(t, s, "app_access"); st.Status != engine.SetupDone {
		t.Fatalf("app_access = %+v", st)
	}
	if st := step(t, s, "shadow"); st.Status != engine.SetupDone {
		t.Fatalf("shadow = %+v", st)
	}
	if fs := s.FirstStop(); fs != nil {
		t.Fatalf("FirstStop = %+v", fs)
	}
}

// Review Focus 4: the repositories call lists only repos the user can also
// reach, so for an org a repo it doesn't list may still be in the
// selection. GetRepo working makes it done, not a Configure link.
func TestSetupOrgRepoUnlistedButReachableIsDone(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "me", true, "acme/x")
	h.forge.Orgs = map[string]bool{"acme": true}
	h.forge.Installations = []forge.Installation{{ID: 9, Account: "acme", AccountType: "Organization", Selection: "selected"}}
	h.forge.InstallRepos = map[int64][]string{9: {}}
	s := h.setup(engine.SetupOptions{Public: "acme/x"})
	st := step(t, s, "app_access")
	if st.Status != engine.SetupTodo || st.URL != "" {
		t.Fatalf("app_access = %+v, want todo (public reachable; the shadow is still to create)", st)
	}
	if fs := s.FirstStop(); fs != nil {
		t.Fatalf("FirstStop = %+v", fs)
	}
	// Once the shadow exists, it is done.
	if _, err := h.forge.AddRepo("acme/x-shadow", true); err != nil {
		t.Fatal(err)
	}
	s = h.setup(engine.SetupOptions{Public: "acme/x", Adopt: true})
	if st := step(t, s, "app_access"); st.Status != engine.SetupDone {
		t.Fatalf("app_access = %+v", st)
	}
}

// An org repo GetRepo can't reach either: ask an owner of the org.
func TestSetupOrgRepoUnreachableAsksAnOwner(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "me", true)
	h.forge.Orgs = map[string]bool{"acme": true}
	h.forge.Installations = []forge.Installation{{ID: 9, Account: "acme", AccountType: "Organization", Selection: "selected"}}
	s := h.setup(engine.SetupOptions{Public: "acme/x"})
	st := step(t, s, "app_access")
	if st.Status != engine.SetupNeedsHuman || !strings.Contains(st.Message, "ask an owner of acme to add acme/x") ||
		st.URL != "https://github.com/organizations/acme/settings/installations/9" {
		t.Fatalf("app_access = %+v", st)
	}
}

// Review Focus 1: the token's account isn't the owner's, though the app is
// installed on the owner. Setup stops at account, before the install check.
func TestSetupAccountMismatchStopsBeforeInstallCheck(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushguptax", true, "ayushgux/trial")
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "all"}}
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial"})
	fs := s.FirstStop()
	if fs == nil || fs.ID != "account" || fs.Status != engine.SetupNeedsHuman {
		t.Fatalf("FirstStop = %+v", fs)
	}
	if !strings.Contains(fs.Message, "--shadow ayushguptax/trial-shadow") || !strings.Contains(fs.Message, "ghostbranch login") {
		t.Fatalf("message = %q", fs.Message)
	}
	ce := s.StopError(fs)
	if ce.Code != contract.TokenWrongAccount || ce.Details["token_account"] != "ayushguptax" || ce.Details["owner"] != "ayushgux" {
		t.Fatalf("error = %s %#v", ce.Code, ce.Details)
	}
	// The --shadow way on clears it.
	s = h.setup(engine.SetupOptions{Public: "ayushgux/trial", Shadow: "ayushguptax/trial-shadow"})
	if st := step(t, s, "account"); st.Status != engine.SetupDone {
		t.Fatalf("account with --shadow = %+v", st)
	}
}

func TestSetupGhostbranchTokenMismatchWording(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushguptax", false, "ayushgux/trial")
	h.eng.TokenSource = "GHOSTBRANCH_TOKEN"
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial"})
	st := step(t, s, "account")
	if st.Status != engine.SetupNeedsHuman || !strings.Contains(st.Message, "GHOSTBRANCH_TOKEN") ||
		!strings.Contains(st.Message, "--shadow ayushguptax/trial-shadow") {
		t.Fatalf("account = %+v", st)
	}
}

func TestSetupNonAppTokenSkipsAppSteps(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", false)
	s := h.setup(engine.SetupOptions{Public: publicName})
	for _, id := range []string{"app_installed", "app_access"} {
		if st := step(t, s, id); st.Status != engine.SetupSkipped {
			t.Fatalf("%s = %+v", id, st)
		}
	}
	if h.forge.InstallCalls != 0 {
		t.Fatalf("UserInstallations called %d times", h.forge.InstallCalls)
	}
	if got := statuses(s); got["shadow"] != "todo" || got["pair"] != "todo" || got["login"] != "done" || got["account"] != "done" {
		t.Fatalf("statuses = %v", got)
	}
}

func TestSetupNoToken(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", true)
	h.eng.Token = ""
	s := h.setup(engine.SetupOptions{Public: publicName})
	fs := s.FirstStop()
	if fs == nil || fs.ID != "login" || fs.Message != "log in with ghostbranch login" {
		t.Fatalf("FirstStop = %+v", fs)
	}
	if ce := s.StopError(fs); ce.Code != contract.AuthMissing {
		t.Fatalf("code = %s", ce.Code)
	}
	if got := statuses(s); got["account"] != "todo" || got["shadow"] != "todo" || got["app_installed"] != "todo" {
		t.Fatalf("statuses = %v", got)
	}
	// No login at all: the login to come is ghostbranch login's app login,
	// so the app steps are still to come, not skipped.
	h2 := setupHarness(t, "acme", false)
	h2.eng.Token, h2.eng.Forge, h2.eng.AppLogin = "", nil, false
	if got := statuses(h2.setup(engine.SetupOptions{Public: publicName})); got["app_installed"] != "todo" || got["app_access"] != "todo" {
		t.Fatalf("no login: statuses = %v", got)
	}
}

// An app login GitHub refuses to list installations for (an expired
// login answers 401) is blocked, not "not installed".
func TestSetupInstallListRefusedIsBlocked(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", true)
	h.forge.InstallErr = forge.ErrForbidden
	s := h.setup(engine.SetupOptions{Public: publicName})
	fs := s.FirstStop()
	if fs == nil || fs.ID != "app_installed" || fs.Status != engine.SetupBlocked || !strings.Contains(fs.Message, "ghostbranch login") {
		t.Fatalf("FirstStop = %+v", fs)
	}
	if got := step(t, s, "app_access").Status; got != engine.SetupTodo {
		t.Fatalf("app_access = %s", got)
	}
}

func TestSetupShadowFlagsNeedAChoice(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", false, shadowName)
	if st := step(t, h.setup(engine.SetupOptions{Public: publicName}), "shadow"); st.Status != engine.SetupNeedsHuman || !strings.Contains(st.Message, "--adopt") {
		t.Fatalf("shadow exists, no --adopt: %+v", st)
	}
	if st := step(t, h.setup(engine.SetupOptions{Public: publicName, Shadow: "acme/other", Adopt: true}), "shadow"); st.Status != engine.SetupNeedsHuman {
		t.Fatalf("--adopt of a missing shadow: %+v", st)
	}
}

func TestSetupBadPublicIsBadFlag(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "acme", false)
	_, err := h.eng.SetupCheck(h.ctx, engine.SetupOptions{Public: "nope"})
	_ = wantCode(t, err, contract.BadFlag)
}

func TestSetupCheckIsReadOnly(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "ayushgux", true, "ayushgux/trial")
	h.forge.Installations = []forge.Installation{{ID: 7, Account: "ayushgux", AccountType: "User", Selection: "selected"}}
	h.forge.InstallRepos = map[int64][]string{7: {"ayushgux/trial"}}
	before := dirFiles(t, h.eng.WS.Dir)
	s := h.setup(engine.SetupOptions{Public: "ayushgux/trial", Checkout: t.TempDir()})
	if s.FirstStop() != nil {
		t.Fatalf("FirstStop = %+v", s.FirstStop())
	}
	if len(h.forge.Created) != 0 || len(h.forge.DefaultBranchSets) != 0 || len(h.pushes) != 0 {
		t.Fatalf("writes: created %v, default branch %v, pushes %v", h.forge.Created, h.forge.DefaultBranchSets, h.pushes)
	}
	if after := dirFiles(t, h.eng.WS.Dir); !reflect.DeepEqual(before, after) {
		t.Fatalf("workspace changed:\nbefore %v\nafter  %v", before, after)
	}
	if _, ok := h.eng.WS.Find("ayushgux/trial"); ok {
		t.Fatal("pair registered")
	}
}

// dirFiles is every file under dir with its contents ("" for a missing dir).
func dirFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[p] = string(b)
		}
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return out
}

// A registered pair or --adopt creates no repo, so a token of another
// account than the personal owner's (a collaborator's) passes the account
// step, as in v0.1.x.
func TestSetupAccountDoneForRegisteredPair(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "octo", false)
	h.init() // registers acme/widget with acme/widget-shadow, which now exists
	s := h.setup(engine.SetupOptions{Public: publicName})
	if st := step(t, s, "account"); st.Status != engine.SetupDone {
		t.Fatalf("account = %+v", st)
	}
	if fs := s.FirstStop(); fs != nil {
		t.Fatalf("FirstStop = %+v", fs)
	}
}

func TestSetupAccountDoneForAdopt(t *testing.T) {
	t.Parallel()
	h := setupHarness(t, "octo", false, shadowName)
	s := h.setup(engine.SetupOptions{Public: publicName, Adopt: true})
	if st := step(t, s, "account"); st.Status != engine.SetupDone {
		t.Fatalf("account = %+v", st)
	}
	if fs := s.FirstStop(); fs != nil {
		t.Fatalf("FirstStop = %+v", fs)
	}
	// Without --adopt, the same token still stops at account.
	s = h.setup(engine.SetupOptions{Public: publicName})
	if st := step(t, s, "account"); st.Status != engine.SetupNeedsHuman {
		t.Fatalf("account without --adopt = %+v", st)
	}
}
