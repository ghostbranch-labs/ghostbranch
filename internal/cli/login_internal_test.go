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
