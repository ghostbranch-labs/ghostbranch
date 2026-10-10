package keychain_test

import (
	"errors"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

var _ keychain.Store = (*keychain.Fake)(nil)

func TestFakeRoundTrip(t *testing.T) {
	t.Parallel()
	f := keychain.NewFake()
	if _, err := f.Get("ghostbranch", "github.com"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("empty Get: %v", err)
	}
	if err := f.Set("ghostbranch", "github.com", `{"token":"ghu_test"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Set("ghostbranch", "other", "x"); err != nil {
		t.Fatal(err)
	}
	if v, err := f.Get("ghostbranch", "github.com"); err != nil || v != `{"token":"ghu_test"}` {
		t.Fatalf("Get = %q %v", v, err)
	}
	if err := f.Set("ghostbranch", "github.com", "replaced"); err != nil {
		t.Fatal(err)
	}
	if v, _ := f.Get("ghostbranch", "github.com"); v != "replaced" {
		t.Fatalf("Set didn't replace: %q", v)
	}
	if err := f.Delete("ghostbranch", "github.com"); err != nil {
		t.Fatal(err)
	}
	if err := f.Delete("ghostbranch", "github.com"); err != nil {
		t.Fatalf("deleting an absent entry: %v", err)
	}
	if _, err := f.Get("ghostbranch", "github.com"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if v, err := f.Get("ghostbranch", "other"); err != nil || v != "x" {
		t.Fatalf("another account was touched: %q %v", v, err)
	}
	if g, s, d := f.Calls(); g != 5 || s != 3 || d != 2 {
		t.Fatalf("Calls = %d %d %d, want 5 3 2", g, s, d)
	}
}

func TestFakeUnavailable(t *testing.T) {
	t.Parallel()
	f := keychain.NewFake()
	f.Unavailable = true
	if _, err := f.Get("s", "a"); !errors.Is(err, keychain.ErrUnavailable) {
		t.Errorf("Get: %v", err)
	}
	if err := f.Set("s", "a", "v"); !errors.Is(err, keychain.ErrUnavailable) {
		t.Errorf("Set: %v", err)
	}
	if err := f.Delete("s", "a"); !errors.Is(err, keychain.ErrUnavailable) {
		t.Errorf("Delete: %v", err)
	}
	if g, s, d := f.Calls(); g != 1 || s != 1 || d != 1 {
		t.Fatalf("Calls = %d %d %d", g, s, d)
	}
}
