package auth_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

// flow is a device flow against srv on a fake clock; it records each sleep.
func flow(srv *authtest.Server, clock *testutil.Clock, slept *[]time.Duration) auth.DeviceFlow {
	return auth.DeviceFlow{ClientID: srv.ClientID, BaseURL: srv.URL, HTTP: srv.Client(), Now: clock.Now,
		Sleep: func(d time.Duration) { *slept = append(*slept, d); clock.Sleep(d) }}
}

func TestDeviceFlowApproves(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{Pending: 2})
	clock := testutil.NewClock(t0)
	var slept []time.Duration
	f := flow(srv, clock, &slept)
	dc, err := f.Start(t.Context())
	if err != nil || dc.UserCode != authtest.UserCode || dc.DeviceCode != authtest.DeviceCode ||
		dc.VerificationURI != srv.URL+"/login/device" || dc.Interval != 5 || dc.ExpiresIn != 900 {
		t.Fatalf("start: %+v %v", dc, err)
	}
	l, err := f.Poll(t.Context(), dc)
	if err != nil {
		t.Fatal(err)
	}
	if l.Token != srv.AccessToken(1) || l.RefreshToken != srv.RefreshToken(1) || l.Kind != "app" || l.Account != "" {
		t.Fatalf("login: %v", l)
	}
	if !l.Expires.Equal(clock.Now().Add(8*time.Hour)) || !l.RefreshExpires.Equal(clock.Now().Add(15897600*time.Second)) {
		t.Fatalf("expiries: %v %v", l.Expires, l.RefreshExpires)
	}
	if !slices.Equal(slept, []time.Duration{5 * time.Second, 5 * time.Second, 5 * time.Second}) || srv.Polls() != 3 {
		t.Fatalf("slept %v over %d polls", slept, srv.Polls())
	}
	for _, f := range srv.Forms()[1:] {
		if f.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || f.Get("device_code") != authtest.DeviceCode || f.Has("client_secret") {
			t.Fatalf("poll form: %v", f)
		}
	}
}

func TestDeviceFlowSlowDownAddsFiveSeconds(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{SlowDown: true})
	clock := testutil.NewClock(t0)
	var slept []time.Duration
	f := flow(srv, clock, &slept)
	dc, _ := f.Start(t.Context())
	if _, err := f.Poll(t.Context(), dc); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(slept, []time.Duration{5 * time.Second, 10 * time.Second}) {
		t.Fatalf("slept %v", slept)
	}
}

func TestDeviceFlowRefusals(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		o      authtest.Options
		reason string
	}{
		{"denied", authtest.Options{Deny: true}, "denied"},
		{"expired at GitHub", authtest.Options{ExpiredCode: true}, "expired_code"},
		// The code lives 12 s and polls come every 5 s: the third poll is
		// past its life, so Poll stops without asking.
		{"expired here", authtest.Options{Pending: 100, CodeExpiresIn: 12}, "expired_code"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			srv := authtest.New(t, c.o)
			var slept []time.Duration
			f := flow(srv, testutil.NewClock(t0), &slept)
			dc, _ := f.Start(t.Context())
			_, err := f.Poll(t.Context(), dc)
			if code(err) != contract.LoginUnavailable || reason(err) != c.reason {
				t.Fatalf("%v", err)
			}
			if srv.Polls() > 3 {
				t.Fatalf("kept polling: %d", srv.Polls())
			}
		})
	}
}

func TestDeviceFlowStopsOnCancel(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{Pending: 100})
	ctx, cancel := context.WithCancel(t.Context())
	clock := testutil.NewClock(t0)
	polls := 0
	f := auth.DeviceFlow{ClientID: srv.ClientID, BaseURL: srv.URL, HTTP: srv.Client(), Now: clock.Now,
		Sleep: func(d time.Duration) {
			if polls++; polls == 3 {
				cancel() // Ctrl-C during the third wait
			}
			clock.Sleep(d)
		}}
	dc, _ := f.Start(ctx)
	if _, err := f.Poll(ctx, dc); !errors.Is(err, context.Canceled) {
		t.Fatalf("want the context's error, got %v", err)
	}
	if srv.Polls() != 2 {
		t.Fatalf("polled %d times; the cancel came before the third", srv.Polls())
	}
}

func TestDeviceFlowWithoutAClientID(t *testing.T) {
	t.Parallel()
	_, err := auth.DeviceFlow{}.Start(t.Context())
	if code(err) != contract.LoginUnavailable || reason(err) != "no_client_id" {
		t.Fatalf("%v", err)
	}
}

func TestRefreshSendsNoClientSecret(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	var slept []time.Duration
	f := flow(srv, testutil.NewClock(t0), &slept)
	l, err := f.Refresh(t.Context(), "ghr_test_seed")
	if err != nil || l.Token != srv.AccessToken(1) || l.RefreshToken != srv.RefreshToken(1) {
		t.Fatalf("%v %v", l, err)
	}
	form := srv.Forms()[0]
	if form.Get("grant_type") != "refresh_token" || form.Get("refresh_token") != "ghr_test_seed" ||
		form.Get("client_id") != srv.ClientID || form.Has("client_secret") {
		t.Fatalf("form: %v", form)
	}
	// The pair rotated: the old refresh token is dead.
	if _, err := f.Refresh(t.Context(), "ghr_test_seed"); code(err) != contract.LoginExpired {
		t.Fatalf("reused refresh token: %v", err)
	}
}
