package auth_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

var t0 = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func vars(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func code(err error) contract.Code {
	if ce := contract.From(err); ce != nil {
		return ce.Code
	}
	return ""
}

func reason(err error) string {
	ce := contract.From(err)
	if ce == nil {
		return ""
	}
	r, _ := ce.Details["reason"].(string)
	return r
}

// tripwire fails the test on any keychain call.
type tripwire struct{ t *testing.T }

func (s tripwire) Get(string, string) (string, error) {
	s.t.Error("the keychain was read")
	return "", keychain.ErrNotFound
}
func (s tripwire) Set(string, string, string) error {
	s.t.Error("the keychain was written")
	return nil
}
func (s tripwire) Delete(string, string) error {
	s.t.Error("the keychain was written")
	return nil
}

func TestKind(t *testing.T) {
	t.Parallel()
	for tok, want := range map[string]string{
		"ghu_test_a": "app", "github_pat_test_b": "fine-grained", "ghp_test_c": "classic",
		"gho_test_d": "oauth", "ghs_test_e": "unknown", "test-token": "unknown", "": "unknown",
	} {
		if got := auth.Kind(tok); got != want {
			t.Errorf("Kind(%q) = %q, want %q", tok, got, want)
		}
	}
}

func TestLoginRoundTrip(t *testing.T) {
	t.Parallel()
	s := keychain.NewFake()
	if l, err := auth.LoadLogin(s); l != nil || err != nil {
		t.Fatalf("empty store: %v %v", l, err)
	}
	app := auth.Login{Token: "ghu_test_a", RefreshToken: "ghr_test_a", Expires: t0.Add(8 * time.Hour),
		RefreshExpires: t0.Add(4000 * time.Hour), Account: "octo", Kind: "app"}
	if err := auth.SaveLogin(s, app); err != nil {
		t.Fatal(err)
	}
	got, err := auth.LoadLogin(s)
	if err != nil || got.Token != app.Token || got.RefreshToken != app.RefreshToken || !got.Expires.Equal(app.Expires) ||
		!got.RefreshExpires.Equal(app.RefreshExpires) || got.Account != "octo" || got.Kind != "app" || got.SchemaVersion != contract.SchemaVersion {
		t.Fatalf("app login: %v %v", got, err)
	}
	if err := auth.SaveLogin(s, auth.Login{Token: "ghp_test_b", Account: "octo", Kind: "pat"}); err != nil {
		t.Fatal(err)
	}
	got, _ = auth.LoadLogin(s)
	if got.Token != "ghp_test_b" || !got.Expires.IsZero() || !got.RefreshExpires.IsZero() || got.RefreshToken != "" {
		t.Fatalf("a pasted token has no expiry: %v", got)
	}
	if err := auth.DeleteLogin(s); err != nil {
		t.Fatal(err)
	}
	if err := auth.DeleteLogin(s); err != nil {
		t.Fatalf("deleting nothing: %v", err)
	}
	if l, _ := auth.LoadLogin(s); l != nil {
		t.Fatalf("still stored: %v", l)
	}
	if err := auth.SaveLogin(s, auth.Login{Kind: "app"}); err == nil {
		t.Fatal("saved a login without a token")
	}
}

// A login written by a newer Ghostbranch is never changed: not refreshed,
// not replaced, not deleted.
func TestNewerStoredLoginIsRefused(t *testing.T) {
	t.Parallel()
	s := keychain.NewFake()
	const newer = `{"schema_version":"99.0","token":"ghu_test_new","kind":"app","whatever":{"x":1}}`
	_ = s.Set(auth.Service, auth.Account, newer)
	if _, err := auth.LoadLogin(s); code(err) != contract.UpgradeNeeded {
		t.Fatalf("load: %v", err)
	}
	if err := auth.SaveLogin(s, auth.Login{Token: "ghp_test_x", Kind: "pat"}); code(err) != contract.UpgradeNeeded {
		t.Fatalf("save: %v", err)
	}
	if err := auth.DeleteLogin(s); code(err) != contract.UpgradeNeeded {
		t.Fatalf("delete: %v", err)
	}
	if raw, _ := s.Get(auth.Service, auth.Account); raw != newer {
		t.Fatalf("the newer login changed: %s", raw)
	}
}

func TestKeychainFailuresAreLoginUnavailable(t *testing.T) {
	t.Parallel()
	s := keychain.NewFake()
	s.Unavailable = true
	_, err := auth.LoadLogin(s)
	if code(err) != contract.LoginUnavailable || reason(err) != "no_keychain" || !errors.Is(err, keychain.ErrUnavailable) {
		t.Fatalf("load: %v", err)
	}
	if err := auth.SaveLogin(s, auth.Login{Token: "ghp_test_x", Kind: "pat"}); reason(err) != "no_keychain" ||
		!strings.Contains(contract.From(err).Message, "GHOSTBRANCH_TOKEN") {
		t.Fatalf("save: %v", err)
	}
}

func TestLoginNeverPrintsItsTokens(t *testing.T) {
	t.Parallel()
	l := auth.Login{Token: "ghu_test_secret_a", RefreshToken: "ghr_test_secret_b", Account: "octo", Kind: "app"}
	for _, s := range []string{fmt.Sprint(l), fmt.Sprintf("%v %+v %#v %s", l, l, l, &l)} {
		if strings.Contains(s, "secret") {
			t.Fatalf("a token printed: %s", s)
		}
	}
	if b, err := json.Marshal(l); err == nil {
		t.Fatalf("a Login marshalled: %s", b)
	}
	if b, err := json.Marshal(map[string]any{"l": &l}); err == nil {
		t.Fatalf("a *Login marshalled: %s", b)
	}
}

func TestLooksLikeAgent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		env     map[string]string
		console bool
		want    bool
	}{
		{map[string]string{"GHOSTBRANCH_ACTOR": "agent:claude-code"}, true, true},
		{map[string]string{"GHOSTBRANCH_ACTOR": "app"}, true, true},
		{map[string]string{"GHOSTBRANCH_ACTOR": "human:octo"}, false, false},
		{map[string]string{"CLAUDECODE": "1"}, true, true},
		{nil, false, true},
		{nil, true, false},
	}
	for _, c := range cases {
		if got := auth.LooksLikeAgent(vars(c.env), c.console); got != c.want {
			t.Errorf("%v console=%v: %v", c.env, c.console, got)
		}
	}
}

func TestFileLockWaitsForItsHolder(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "locks", "auth.lock")
	lock := auth.FileLock(path, time.Now, 10*time.Second)
	unlock, err := lock()
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan error, 1)
	go func() {
		u, err := lock()
		if err == nil {
			u()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("took a held lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatalf("after release: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("never got the lock after its holder let go")
	}
	held, err := lock()
	if err != nil {
		t.Fatal(err)
	}
	defer held()
	if _, err := auth.FileLock(path, time.Now, 100*time.Millisecond)(); code(err) != contract.Locked {
		t.Fatalf("a short wait on a held lock: %v", err)
	}
}
