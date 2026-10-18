package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth/authtest"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/testutil"
)

func noGH(t *testing.T) func() (string, error) {
	return func() (string, error) { t.Error("fell back to gh"); return "", errors.New("no gh") }
}

// stored saves an app login that expires in left.
func stored(t *testing.T, s keychain.Store, refresh string, left time.Duration) {
	t.Helper()
	if err := auth.SaveLogin(s, auth.Login{Token: "ghu_test_old", RefreshToken: refresh, Expires: t0.Add(left),
		RefreshExpires: t0.Add(4000 * time.Hour), Account: "octo", Kind: "app"}); err != nil {
		t.Fatal(err)
	}
}

// source refreshes through srv under an in-process mutex.
func source(srv *authtest.Server, s keychain.Store, env map[string]string, gh func() (string, error)) auth.Source {
	var mu sync.Mutex
	f := auth.DeviceFlow{ClientID: srv.ClientID, BaseURL: srv.URL, HTTP: srv.Client(), Now: func() time.Time { return t0 }}
	return auth.Source{Getenv: vars(env), GH: gh, Store: s, Now: func() time.Time { return t0 },
		Refresh: func(ctx context.Context, l auth.Login) (auth.Login, error) { return f.Refresh(ctx, l.RefreshToken) },
		Lock:    func() (func(), error) { mu.Lock(); return mu.Unlock, nil }}
}

func TestResolveOrder(t *testing.T) {
	t.Parallel()
	withLogin := func() keychain.Store {
		s := keychain.NewFake()
		_ = auth.SaveLogin(s, auth.Login{Token: "ghu_test_login", Kind: "app", Account: "octo",
			Expires: t0.Add(time.Hour), RefreshToken: "ghr_test_x", RefreshExpires: t0.Add(24 * time.Hour)})
		return s
	}
	gh := func() (string, error) { return "gho_test_gh\n", nil }
	cases := []struct {
		env          map[string]string
		store        keychain.Store
		want, source string
	}{
		{map[string]string{"GHOSTBRANCH_TOKEN": " ghp_test_a ", "GH_TOKEN": "b"}, withLogin(), "ghp_test_a", "GHOSTBRANCH_TOKEN"},
		{map[string]string{"GH_TOKEN": "b", "GITHUB_TOKEN": "c"}, withLogin(), "ghu_test_login", "login"},
		{map[string]string{"GH_TOKEN": " b ", "GITHUB_TOKEN": "c"}, keychain.NewFake(), "b", "GH_TOKEN"},
		{map[string]string{"GITHUB_TOKEN": "c"}, nil, "c", "GITHUB_TOKEN"},
		{nil, keychain.NewFake(), "gho_test_gh", "gh auth token"},
	}
	for _, c := range cases {
		tok, src, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(c.env), GH: gh, Store: c.store, Now: func() time.Time { return t0 }})
		if err != nil || tok != c.want || src != c.source {
			t.Errorf("%v: %q %q %v", c.env, tok, src, err)
		}
	}
	_, _, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(nil), Store: keychain.NewFake(),
		GH: func() (string, error) { return "", errors.New("not logged in") }})
	if code(err) != contract.AuthMissing || !strings.Contains(contract.From(err).Message, "ghostbranch login") {
		t.Fatalf("no token: %v", err)
	}
}

// Review Focus 4: an agent with its own token never touches the keychain.
func TestEnvTokenNeverReadsTheKeychain(t *testing.T) {
	t.Parallel()
	src := auth.Source{Getenv: vars(map[string]string{"GHOSTBRANCH_TOKEN": " ghp_test_agent "}), GH: noGH(t), Store: tripwire{t},
		Refresh: func(context.Context, auth.Login) (auth.Login, error) { t.Error("refreshed"); return auth.Login{}, nil },
		Lock:    func() (func(), error) { t.Error("locked"); return func() {}, nil }}
	tok, from, err := auth.Resolve(t.Context(), src)
	if err != nil || tok != "ghp_test_agent" || from != "GHOSTBRANCH_TOKEN" {
		t.Fatalf("%q %q %v", tok, from, err)
	}
}

// No OS keychain (headless Linux): no login can be stored, so resolution
// goes on to GH_TOKEN.
func TestNoKeychainFallsThrough(t *testing.T) {
	t.Parallel()
	s := keychain.NewFake()
	s.Unavailable = true
	tok, from, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(map[string]string{"GH_TOKEN": "ghp_test_gh"}), Store: s})
	if err != nil || tok != "ghp_test_gh" || from != "GH_TOKEN" {
		t.Fatalf("%q %q %v", tok, from, err)
	}
}

// Review Focus 4: a corrupt entry is a clear LOGIN_UNAVAILABLE naming the
// fix, never a crash and never a quiet fall back to GH_TOKEN or gh.
func TestCorruptStoredLogin(t *testing.T) {
	t.Parallel()
	for _, raw := range []string{"{not json", `{"schema_version":"0.10","kind":"app"}`, `{"token":"ghu_test_x","kind":"robot"}`, `"just a string"`} {
		s := keychain.NewFake()
		_ = s.Set(auth.Service, auth.Account, raw)
		_, _, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(map[string]string{"GH_TOKEN": "ghp_test_gh"}), GH: noGH(t), Store: s})
		ce := contract.From(err)
		if ce == nil || ce.Code != contract.LoginUnavailable || reason(err) != "corrupt" ||
			!strings.Contains(ce.Message, "ghostbranch logout") || !strings.Contains(ce.Message, "ghostbranch login") {
			t.Fatalf("%q: %v", raw, err)
		}
		if err := auth.DeleteLogin(s); err != nil {
			t.Fatalf("%q: logout can't clear it: %v", raw, err)
		}
	}
}

func TestRefreshWithinFiveMinutesOfExpiry(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	s := keychain.NewFake()
	stored(t, s, "ghr_test_seed", 6*time.Minute)
	tok, from, err := auth.Resolve(t.Context(), source(srv, s, nil, noGH(t)))
	if err != nil || tok != "ghu_test_old" || from != "login" || srv.Refreshes() != 0 {
		t.Fatalf("6 min left: %q %q %v (%d refreshes)", tok, from, err, srv.Refreshes())
	}
	stored(t, s, "ghr_test_seed", 4*time.Minute)
	tok, _, err = auth.Resolve(t.Context(), source(srv, s, nil, noGH(t)))
	if err != nil || tok != srv.AccessToken(1) || srv.Refreshes() != 1 {
		t.Fatalf("4 min left: %q %v (%d refreshes)", tok, err, srv.Refreshes())
	}
	l, _ := auth.LoadLogin(s)
	if l.Token != srv.AccessToken(1) || l.RefreshToken != srv.RefreshToken(1) || l.Account != "octo" || l.Kind != "app" {
		t.Fatalf("stored after refresh: %v", l)
	}
}

func TestExpiredRefreshTokenIsLoginExpired(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{})
	s := keychain.NewFake()
	_ = auth.SaveLogin(s, auth.Login{Token: "ghu_test_old", RefreshToken: "ghr_test_old", Expires: t0.Add(-time.Hour),
		RefreshExpires: t0.Add(-time.Minute), Account: "octo", Kind: "app"})
	_, _, err := auth.Resolve(t.Context(), source(srv, s, map[string]string{"GH_TOKEN": "ghp_test_gh"}, noGH(t)))
	if code(err) != contract.LoginExpired || srv.Refreshes() != 0 || !strings.Contains(contract.From(err).Message, "ghostbranch login") {
		t.Fatalf("%v (%d refreshes)", err, srv.Refreshes())
	}
	// GitHub refusing the refresh token is LOGIN_EXPIRED too.
	bad := authtest.New(t, authtest.Options{RefreshBad: true})
	stored(t, s, "ghr_test_seed", time.Minute)
	if _, _, err := auth.Resolve(t.Context(), source(bad, s, nil, noGH(t))); code(err) != contract.LoginExpired {
		t.Fatalf("refused refresh: %v", err)
	}
}

// Review Focus 3: two commands find the login expiring at once. Exactly one
// refresh is made, and both get the new token. The server holds the first
// refresh open until the second command is waiting on the lock, so the two
// really overlap.
func TestConcurrentRefreshRefreshesOnce(t *testing.T) {
	t.Parallel()
	both := make(chan struct{})
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed", RefreshGate: func() {
		select {
		case <-both:
		case <-time.After(10 * time.Second):
			t.Error("the second command never reached the lock")
		}
	}})
	s := keychain.NewFake()
	stored(t, s, "ghr_test_seed", 2*time.Minute)
	src := source(srv, s, nil, noGH(t))
	inner := src.Lock
	var waiting atomic.Int32
	src.Lock = func() (func(), error) {
		if waiting.Add(1) == 2 {
			close(both)
		}
		return inner()
	}
	var wg sync.WaitGroup
	got, errs := make([]string, 2), make([]error, 2)
	for i := range 2 {
		wg.Go(func() { got[i], _, errs[i] = auth.Resolve(t.Context(), src) })
	}
	wg.Wait()
	want := srv.AccessToken(1)
	for i := range 2 {
		if errs[i] != nil || got[i] != want {
			t.Fatalf("command %d: %q %v (want %q)", i, got[i], errs[i], want)
		}
	}
	if n := srv.Refreshes(); n != 1 {
		t.Fatalf("%d refreshes; a second one would have logged the person out", n)
	}
	if l, _ := auth.LoadLogin(s); l.Token != want || l.Account != "octo" {
		t.Fatalf("stored: %v", l)
	}
}

// A command under another GHOSTBRANCH_HOME holds another lock. If it wins
// the race, this command's refresh fails, and the winner's stored pair is used.
func TestALostRefreshRaceUsesTheStoredWinner(t *testing.T) {
	t.Parallel()
	s := keychain.NewFake()
	stored(t, s, "ghr_test_seed", time.Minute)
	src := auth.Source{Getenv: vars(nil), GH: noGH(t), Store: s, Now: func() time.Time { return t0 },
		Lock: func() (func(), error) { return func() {}, nil },
		Refresh: func(context.Context, auth.Login) (auth.Login, error) {
			_ = auth.SaveLogin(s, auth.Login{Token: "ghu_test_winner", RefreshToken: "ghr_test_winner",
				Expires: t0.Add(8 * time.Hour), RefreshExpires: t0.Add(4000 * time.Hour), Account: "octo", Kind: "app"})
			return auth.Login{}, contract.New(contract.LoginExpired, "the stored GitHub login has expired")
		}}
	tok, _, err := auth.Resolve(t.Context(), src)
	if err != nil || tok != "ghu_test_winner" {
		t.Fatalf("%q %v", tok, err)
	}
}

// Review Focus 1: no error from auth shows a token: not a corrupt entry's
// text, not a refresh GitHub refused (even when its reply repeats the
// token), not an HTTP 500 whose body repeats it, not a declined device flow.
func TestTokenNeverInErrors(t *testing.T) {
	t.Parallel()
	check := func(name string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: want an error", name)
		}
		ce := contract.From(err)
		details, _ := json.Marshal(ce.Details)
		for _, s := range []string{err.Error(), fmt.Sprintf("%v %+v", err, err), ce.Message, string(details)} {
			if strings.Contains(s, "test_secret") {
				t.Errorf("%s: a token leaked: %q", name, s)
			}
		}
	}
	corrupt := keychain.NewFake()
	_ = corrupt.Set(auth.Service, auth.Account, `{"token":"ghu_test_secret_corrupt","kind":7`)
	_, _, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(nil), Store: corrupt})
	check("corrupt entry", err)

	wrongType := keychain.NewFake()
	_ = wrongType.Set(auth.Service, auth.Account, `{"token":"ghu_test_secret_type","kind":"app","expires":"ghu_test_secret_type"}`)
	_, _, err = auth.Resolve(t.Context(), auth.Source{Getenv: vars(nil), Store: wrongType})
	check("wrong field type", err)

	for _, o := range []authtest.Options{{RefreshBad: true, EchoSecrets: true}, {Fail500: true}} {
		srv := authtest.New(t, o)
		s := keychain.NewFake()
		_ = auth.SaveLogin(s, auth.Login{Token: "ghu_test_secret_access", RefreshToken: "ghr_test_secret_refresh",
			Expires: t0.Add(time.Minute), RefreshExpires: t0.Add(time.Hour), Account: "octo", Kind: "app"})
		_, _, err := auth.Resolve(t.Context(), source(srv, s, nil, noGH(t)))
		check(fmt.Sprintf("refresh %+v", o), err)
	}

	srv := authtest.New(t, authtest.Options{Deny: true, EchoSecrets: true})
	var slept []time.Duration
	f := flow(srv, testutil.NewClock(t0), &slept)
	dc, _ := f.Start(t.Context())
	dc.DeviceCode = "dc_test_secret_device" // a reply that repeats it must not reach the error
	_, err = f.Poll(t.Context(), dc)
	check("declined device flow", err)

	none := keychain.NewFake()
	none.Unavailable = true
	check("no keychain", auth.SaveLogin(none, auth.Login{Token: "ghu_test_secret_save", Kind: "app"}))
}

// With no Refresh (doctor's read-only source) a stale login is used as it is:
// never refreshed, never an error for staleness, and nothing is written.
func TestStaleLoginWithoutRefreshIsUsedAsIs(t *testing.T) {
	t.Parallel()
	for _, left := range []time.Duration{2 * time.Minute, -time.Hour} {
		s := keychain.NewFake()
		stored(t, s, "ghr_test_seed", left)
		_, setsBefore, _ := s.Calls()
		tok, from, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(nil), GH: noGH(t), Store: s, Now: func() time.Time { return t0 }})
		if err != nil || tok != "ghu_test_old" || from != "login" {
			t.Fatalf("%v left: %q %q %v", left, tok, from, err)
		}
		if _, sets, deletes := s.Calls(); sets != setsBefore || deletes != 0 {
			t.Fatalf("%v left: the keychain was written (%d sets, %d deletes)", left, sets-setsBefore, deletes)
		}
	}
}

// A refreshed pair that can't be saved is reported through OnSaveError,
// once, and the command still gets the new token: the old pair is already
// spent, so the next command will ask for a login.
func TestAFailedSaveAfterARefreshIsReported(t *testing.T) {
	t.Parallel()
	srv := authtest.New(t, authtest.Options{RefreshToken: "ghr_test_seed"})
	s := keychain.NewFake()
	stored(t, s, "ghr_test_seed", 2*time.Minute)
	s.SetErr = errors.New("keychain: write refused")
	src := source(srv, s, nil, noGH(t))
	var reported []error
	src.OnSaveError = func(err error) { reported = append(reported, err) }
	tok, _, err := auth.Resolve(t.Context(), src)
	if err != nil || tok != srv.AccessToken(1) {
		t.Fatalf("%q %v", tok, err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], s.SetErr) {
		t.Fatalf("reported %v", reported)
	}
}

// GHOSTBRANCH_NO_GH turns the gh fallback off, and the AUTH_MISSING message
// says so, so a person who expected gh knows why it wasn't used.
func TestResolveSkipsGHWhenNoGH(t *testing.T) {
	t.Parallel()
	gh := func() (string, error) { return "gho_test_gh", nil }
	_, _, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(map[string]string{"GHOSTBRANCH_NO_GH": "1"}), GH: gh, Store: keychain.NewFake()})
	if code(err) != contract.AuthMissing || !strings.Contains(contract.From(err).Message, "(the gh fallback is off: GHOSTBRANCH_NO_GH is set)") {
		t.Fatalf("NO_GH=1: %v", err)
	}
	if !auth.NoGH(vars(map[string]string{"GHOSTBRANCH_NO_GH": " YES "})) || auth.NoGH(vars(map[string]string{"GHOSTBRANCH_NO_GH": "0"})) || auth.NoGH(vars(nil)) {
		t.Fatal("NoGH values: want yes/true/1 (any case) on, anything else off")
	}
}

func TestResolveUsesGHByDefault(t *testing.T) {
	t.Parallel()
	gh := func() (string, error) { return "gho_test_gh", nil }
	tok, from, err := auth.Resolve(t.Context(), auth.Source{Getenv: vars(nil), GH: gh, Store: keychain.NewFake()})
	if err != nil || tok != "gho_test_gh" || from != "gh auth token" {
		t.Fatalf("default: %q %q %v", tok, from, err)
	}
}
