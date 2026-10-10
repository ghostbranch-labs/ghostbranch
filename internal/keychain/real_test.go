package keychain_test

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

// TestRealKeychainRoundTrip writes, reads and deletes one throwaway entry in
// this machine's OS keychain. It runs only with GHOSTBRANCH_TEST_KEYCHAIN=1,
// and skips when the machine has no usable store (Linux CI).
func TestRealKeychainRoundTrip(t *testing.T) {
	t.Parallel()
	if os.Getenv("GHOSTBRANCH_TEST_KEYCHAIN") != "1" {
		t.Skip("set GHOSTBRANCH_TEST_KEYCHAIN=1 to use the real OS keychain")
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	service, account := "ghostbranch-test", "roundtrip-"+hex.EncodeToString(b)
	s := keychain.OS()
	if err := s.Set(service, account, `{"token":"ghu_test"}`); errors.Is(err, keychain.ErrUnavailable) {
		t.Skipf("no usable OS keychain here: %v", err)
	} else if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Delete(service, account) })
	if v, err := s.Get(service, account); err != nil || v != `{"token":"ghu_test"}` {
		t.Fatalf("Get = %q %v", v, err)
	}
	if err := s.Delete(service, account); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(service, account); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if err := s.Delete(service, account); err != nil {
		t.Fatalf("deleting an absent entry: %v", err)
	}
}
