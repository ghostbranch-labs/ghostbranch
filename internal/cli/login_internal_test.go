package cli

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge/fake"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// runLogin is the device flow guided init reuses: it talks only through the
// asker it is given, asks nothing, and stores the login.
func TestRunLoginOnAScript(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{Pending: 1})
	f := fake.New(t.TempDir())
	f.User = forge.User{Login: "octo", ID: 1}
	clock := testutil.NewClock(time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC))
	var opened []string
	a := &app{env: Env{
		Getenv: func(string) string { return "" }, Now: clock.Now, Sleep: clock.SleepCtx,
		NewForge: func(string) (forge.Forge, error) { return f, nil }, Keychain: keychain.NewFake(),
		ClientID: srv.ClientID, OAuthBaseURL: srv.URL, HTTPClient: srv.Client(),
		OpenBrowser: func(u string) error { opened = append(opened, u); return nil },
	}}
	ask := prompt.NewScript()
	l, err := a.runLogin(context.Background(), ask)
	if err != nil {
		t.Fatal(err)
	}
	ask.Check(t)
	if len(ask.Asked) != 0 {
		t.Fatalf("login asked %q", ask.Asked)
	}
	if !slices.ContainsFunc(ask.Said, func(s string) bool {
		return strings.Contains(s, authtest.UserCode) && strings.Contains(s, srv.URL+"/login/device")
	}) {
		t.Fatalf("the code and URL weren't said: %q", ask.Said)
	}
	if len(opened) != 1 || l == nil || l.Account != "octo" || l.Kind != "app" || l.Token != srv.AccessToken(1) {
		t.Fatalf("login %v, opened %v", l, opened)
	}
	if got, _ := auth.LoadLogin(a.env.Keychain); got == nil || got.Token != srv.AccessToken(1) || got.Account != "octo" {
		t.Fatalf("stored: %v", got)
	}

	// Without a keychain it stops before GitHub is asked for a code.
	a.env.Keychain = nil
	before := len(srv.Forms())
	_, err = a.runLogin(context.Background(), prompt.NewScript())
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != contract.LoginUnavailable || ce.Details["reason"] != "no_keychain" || len(srv.Forms()) != before {
		t.Fatalf("no keychain: %v (%d requests)", err, len(srv.Forms())-before)
	}

	// An unusable keychain stops it before GitHub is asked too.
	kc := keychain.NewFake()
	kc.Unavailable = true
	a.env.Keychain = kc
	_, err = a.runLogin(context.Background(), prompt.NewScript())
	if !errors.As(err, &ce) || ce.Code != contract.LoginUnavailable || ce.Details["reason"] != "no_keychain" || len(srv.Forms()) != before {
		t.Fatalf("unusable keychain: %v (%d requests)", err, len(srv.Forms())-before)
	}
}

// refreshApp is an app whose stored login expires in two minutes, so the
// next Resolve refreshes it through srv.
func refreshApp(t *testing.T, srv *authtest.Server, kc *keychain.Fake, stderr *strings.Builder) *app {
	t.Helper()
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	if err := auth.SaveLogin(kc, auth.Login{Token: "ghu_test_old", RefreshToken: "ghr_test_seed", Expires: now.Add(2 * time.Minute),
		RefreshExpires: now.Add(4000 * time.Hour), Account: "octo", Kind: "app"}); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return &app{env: Env{
		Getenv: func(k string) string {
			if k == "GHOSTBRANCH_HOME" {
				return home
			}
			return ""
		},
		Now: func() time.Time { return now }, Keychain: kc, Stderr: stderr,
		ClientID: srv.ClientID, OAuthBaseURL: srv.URL, HTTPClient: srv.Client(),
	}}
}

// GitHub rotates the refresh token as soon as it answers a refresh. A
// command cancelled mid-refresh still finishes the POST and stores the new
// pair; dropping the reply would leave only the spent pair and log the
// person out.
func TestACancelledRefreshStillStoresTheNewPair(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed", RefreshGate: func() {
		cancel()
		time.Sleep(200 * time.Millisecond) // the client sees the cancel before the reply
	}})
	kc := keychain.NewFake()
	var stderr strings.Builder
	a := refreshApp(t, srv, kc, &stderr)
	_, _, _ = auth.Resolve(ctx, a.authSource())
	l, err := auth.LoadLogin(kc)
	if err != nil || l == nil || l.Token != srv.AccessToken(1) || l.RefreshToken != srv.RefreshToken(1) || srv.Refreshes() != 1 {
		t.Fatalf("stored %v, %v (%d refreshes)", l, err, srv.Refreshes())
	}
}

// A refreshed pair the keychain won't take is said once on stderr, without
// the keychain's error, which could carry a secret.
func TestAFailedSaveAfterARefreshIsSaidOnStderr(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	kc := keychain.NewFake()
	var stderr strings.Builder
	a := refreshApp(t, srv, kc, &stderr)
	kc.SetErr = errors.New("write refused for ghr_test_refresh_1")
	tok, _, err := auth.Resolve(context.Background(), a.authSource())
	if err != nil || tok != srv.AccessToken(1) {
		t.Fatalf("%q %v", tok, err)
	}
	want := "the refreshed GitHub login couldn't be saved to the OS keychain; the next command will ask you to log in again\n"
	if got := stderr.String(); !strings.HasSuffix(got, want) || strings.Count(got, "\n") != 1 || strings.Contains(got, "ghr_test") {
		t.Fatalf("stderr %q", got)
	}
}
