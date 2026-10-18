package cli_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

var cliNow = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC) // newTestEnv's clock

// keychainTripwire fails the test on any keychain call.
type keychainTripwire struct{ t *testing.T }

func (s keychainTripwire) Get(string, string) (string, error) {
	s.t.Error("the keychain was read")
	return "", keychain.ErrNotFound
}
func (s keychainTripwire) Set(string, string, string) error {
	s.t.Error("the keychain was written")
	return nil
}
func (s keychainTripwire) Delete(string, string) error {
	s.t.Error("the keychain was written")
	return nil
}

// storeLogin saves an app login for octo that expires in left.
func storeLogin(te *testEnv, token, refresh string, left time.Duration) {
	te.t.Helper()
	if err := auth.SaveLogin(te.env.Keychain, auth.Login{Token: token, RefreshToken: refresh, Expires: cliNow.Add(left),
		RefreshExpires: cliNow.Add(4000 * time.Hour), Account: "octo", Kind: "app"}); err != nil {
		te.t.Fatal(err)
	}
}

func TestEnvTokenNeverReadsTheKeychainThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	te.env.Keychain = keychainTripwire{t}
	if code, env := te.runJSON("init", "acme/widget"); code != 0 {
		t.Fatalf("%d %v", code, env)
	}
}

func TestTheStoredLoginIsUsedWithoutAnEnvToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.vars["GH_TOKEN"] = "ghp_test_gh"
	storeLogin(te, "ghu_test_stored", "ghr_test_stored", 8*time.Hour)
	var used string
	te.env.NewForge = func(tok string) (forge.Forge, error) { used = tok; return te.forge, nil }
	code, env := te.runJSON("agents", "status", "--repo", "acme/widget")
	data, _ := env["data"].(map[string]any)
	token, _ := data["token"].(map[string]any)
	if code != 0 || token["source"] != "login" || used != "ghu_test_stored" {
		t.Fatalf("%d %v (forge got %q)", code, env, used)
	}
}

func TestACorruptLoginStopsTheCommand(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	te.vars["GH_TOKEN"] = "ghp_test_gh"
	_ = te.env.Keychain.Set(auth.Service, auth.Account, "{not json")
	te.env.GHToken = func() (string, error) { t.Error("fell back to gh"); return "gho_test_gh", nil }
	code, env := te.runJSON("sync")
	e, _ := env["error"].(map[string]any)
	if code != 1 || e["code"] != "LOGIN_UNAVAILABLE" {
		t.Fatalf("%d %v", code, env)
	}
}

func TestAnExpiringLoginRefreshesThroughTheCLI(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	te.env.ClientID, te.env.OAuthBaseURL, te.env.HTTPClient = srv.ClientID, srv.URL, srv.Client()
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	storeLogin(te, "ghu_test_old", "ghr_test_seed", 2*time.Minute)
	// An app login: init checks the app is installed before it acts.
	te.forge.Installations = []forge.Installation{{ID: 7, Account: "acme", AccountType: forge.OwnerOrganization, Selection: "all"}}
	if code, env := te.runJSON("init", "acme/widget"); code != 0 {
		t.Fatalf("%d %v", code, env)
	}
	l, _ := auth.LoadLogin(te.env.Keychain)
	if srv.Refreshes() != 1 || l.Token != srv.AccessToken(1) {
		t.Fatalf("%d refreshes, stored %v", srv.Refreshes(), l)
	}
	if _, err := os.Stat(filepath.Join(te.vars["GHOSTBRANCH_HOME"], "locks", "auth.lock")); err != nil {
		t.Fatalf("the refresh didn't take the auth lock: %v", err)
	}
}
