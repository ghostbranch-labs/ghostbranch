package keychain

import (
	"errors"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	keyring "github.com/zalando/go-keyring"
)

func stubStore(goos string, err error) *osStore {
	return &osStore{
		get:     func(string, string) (string, error) { return "", err },
		set:     func(string, string, string) error { return err },
		del:     func(string, string) error { return err },
		goos:    goos,
		timeout: time.Minute,
	}
}

func TestOSStoreMapsErrors(t *testing.T) {
	t.Parallel()
	noBus := errors.New("The name org.freedesktop.secrets was not provided by any .service files")
	other := errors.New("The user name or passphrase you entered is not correct.")
	for name, c := range map[string]struct {
		goos              string
		err               error
		notFound, unavail bool
	}{
		"linux not found":       {"linux", keyring.ErrNotFound, true, false},
		"linux no service":      {"linux", noBus, false, true},
		"freebsd no bus":        {"freebsd", errors.New("dbus: DBUS_SESSION_BUS_ADDRESS not set"), false, true},
		"unsupported":           {"plan9", keyring.ErrUnsupportedPlatform, false, true},
		"darwin no security":    {"darwin", &exec.Error{Name: "/usr/bin/security", Err: exec.ErrNotFound}, false, true},
		"darwin other":          {"darwin", other, false, false},
		"windows logon session": {"windows", syscall.Errno(1312), false, true},
		"windows other":         {"windows", syscall.Errno(5), false, false},
		"too big":               {"windows", keyring.ErrSetDataTooBig, false, false},
	} {
		s := stubStore(c.goos, c.err)
		_, err := s.Get("ghostbranch", "github.com")
		if errors.Is(err, ErrNotFound) != c.notFound || errors.Is(err, ErrUnavailable) != c.unavail || err == nil {
			t.Errorf("%s: Get = %v (not found %v, unavailable %v)", name, err, errors.Is(err, ErrNotFound), errors.Is(err, ErrUnavailable))
		}
		if err := s.Set("ghostbranch", "github.com", "v"); c.err != keyring.ErrNotFound && errors.Is(err, ErrUnavailable) != c.unavail {
			t.Errorf("%s: Set = %v", name, err)
		}
	}
	if err := stubStore("linux", keyring.ErrNotFound).Delete("ghostbranch", "github.com"); err != nil {
		t.Errorf("deleting an absent entry: %v", err)
	}
	if err := stubStore("linux", noBus).Delete("ghostbranch", "github.com"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Delete without a store: %v", err)
	}
}

// A store that never answers (a Secret Service waiting on an unlock prompt
// no one can see) is ErrUnavailable, not a hang.
func TestOSStoreTimesOut(t *testing.T) {
	t.Parallel()
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	s := &osStore{get: func(string, string) (string, error) { <-block; return "", nil }, goos: "linux", timeout: 20 * time.Millisecond}
	if _, err := s.Get("ghostbranch", "github.com"); !errors.Is(err, ErrUnavailable) || !strings.Contains(err.Error(), "didn't answer") {
		t.Fatalf("Get = %v", err)
	}
}

// No error text ever carries the secret, even if the OS store echoes it.
func TestOSStoreNeverEchoesTheSecret(t *testing.T) {
	t.Parallel()
	for _, goos := range []string{"linux", "darwin", "windows"} {
		s := &osStore{set: func(_, _, secret string) error { return errors.New("refused " + secret) }, goos: goos, timeout: time.Minute}
		err := s.Set("ghostbranch", "github.com", `{"token":"ghu_test"}`)
		if err == nil || strings.Contains(err.Error(), "ghu_test") {
			t.Errorf("%s: Set = %v", goos, err)
		}
	}
}
