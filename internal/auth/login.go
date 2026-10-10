package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

// Login is the stored GitHub login. Expiries are zero for a pasted token.
type Login struct {
	Token, RefreshToken     string
	Expires, RefreshExpires time.Time
	Account                 string // GitHub login
	Kind                    string // "app" (device flow) | "pat" (pasted)
	SchemaVersion           string
}

// String keeps both tokens out of any %v, %+v, %#v or %s.
func (l Login) String() string {
	return fmt.Sprintf("auth.Login{Account:%q Kind:%q Expires:%s Token:<redacted> RefreshToken:<redacted>}",
		l.Account, l.Kind, l.Expires.UTC().Format(time.RFC3339))
}

func (l Login) GoString() string { return l.String() }

// MarshalJSON refuses: a Login reaches storage only through SaveLogin, and
// never output.
func (Login) MarshalJSON() ([]byte, error) {
	return nil, errors.New("auth: a Login is never marshalled")
}

// stored is the keychain entry's JSON.
type stored struct {
	SchemaVersion  string     `json:"schema_version"`
	Token          string     `json:"token"`
	RefreshToken   string     `json:"refresh_token,omitempty"`
	Expires        *time.Time `json:"expires,omitempty"`
	RefreshExpires *time.Time `json:"refresh_expires,omitempty"`
	Account        string     `json:"account"`
	Kind           string     `json:"kind"`
}

func unavailable(reason, format string, args ...any) *contract.Error {
	return contract.New(contract.LoginUnavailable, format, args...).WithDetails(map[string]any{"reason": reason})
}

func expired() *contract.Error {
	return contract.New(contract.LoginExpired, "the stored GitHub login has expired; run `ghostbranch login`")
}

// storeErr explains a keychain failure. It wraps the keychain error (for
// errors.Is) but never quotes it: the message is fixed text.
func storeErr(err error) error {
	var e *contract.Error
	if errors.Is(err, keychain.ErrUnavailable) {
		e = unavailable("no_keychain", "this machine has no OS keychain to keep a GitHub login in; set GHOSTBRANCH_TOKEN instead")
	} else {
		e = unavailable("keychain_error", "the OS keychain can't be used (is it locked?); unlock it and try again, or set GHOSTBRANCH_TOKEN")
	}
	e.Err = err
	return e
}

func corrupt() *contract.Error {
	return unavailable("corrupt", "the GitHub login stored in the OS keychain can't be read; run `ghostbranch logout`, then `ghostbranch login` (or set GHOSTBRANCH_TOKEN)")
}

// peekVersion reads only an entry's schema_version, so an entry from a newer
// Ghostbranch is recognised even when the rest of it doesn't parse here.
func peekVersion(raw string) (string, bool) {
	var v struct {
		SchemaVersion string `json:"schema_version"`
	}
	if json.Unmarshal([]byte(raw), &v) != nil {
		return "", false
	}
	return v.SchemaVersion, true
}

// LoadLogin reads the stored login: (nil, nil) when none is stored.
//   - A keychain failure is LOGIN_UNAVAILABLE (no_keychain or keychain_error)
//     wrapping the keychain error, so errors.Is(err, keychain.ErrUnavailable)
//     works.
//   - An entry from a newer Ghostbranch is UPGRADE_NEEDED.
//   - An unreadable entry is LOGIN_UNAVAILABLE (corrupt).
//
// No message quotes the entry or a decoder error, which could carry its text.
func LoadLogin(s keychain.Store) (*Login, error) {
	raw, err := s.Get(Service, Account)
	if errors.Is(err, keychain.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, storeErr(err)
	}
	if v, ok := peekVersion(raw); ok && contract.Newer(v) {
		return nil, contract.NewerRecord("login", v)
	}
	var st stored
	if json.Unmarshal([]byte(raw), &st) != nil || st.Token == "" ||
		strings.ContainsAny(st.Token, " \t\r\n") || (st.Kind != "app" && st.Kind != "pat") {
		return nil, corrupt()
	}
	l := &Login{Token: st.Token, RefreshToken: st.RefreshToken, Account: st.Account, Kind: st.Kind, SchemaVersion: st.SchemaVersion}
	if st.Expires != nil {
		l.Expires = *st.Expires
	}
	if st.RefreshExpires != nil {
		l.RefreshExpires = *st.RefreshExpires
	}
	return l, nil
}

// refuseNewer refuses to change an entry a newer Ghostbranch wrote.
func refuseNewer(s keychain.Store) error {
	raw, err := s.Get(Service, Account)
	switch {
	case errors.Is(err, keychain.ErrNotFound):
		return nil
	case err != nil:
		return storeErr(err)
	}
	if v, ok := peekVersion(raw); ok && contract.Newer(v) {
		return contract.NewerRecord("login", v)
	}
	return nil
}

// SaveLogin stores l, replacing the stored login unless a newer Ghostbranch
// wrote it.
func SaveLogin(s keychain.Store, l Login) error {
	if l.Token == "" || (l.Kind != "app" && l.Kind != "pat") {
		return contract.New(contract.Internal, "auth: refusing to store a login without a token or kind")
	}
	if err := refuseNewer(s); err != nil {
		return err
	}
	st := stored{SchemaVersion: contract.SchemaVersion, Token: l.Token, RefreshToken: l.RefreshToken, Account: l.Account, Kind: l.Kind}
	if !l.Expires.IsZero() {
		t := l.Expires.UTC()
		st.Expires = &t
	}
	if !l.RefreshExpires.IsZero() {
		t := l.RefreshExpires.UTC()
		st.RefreshExpires = &t
	}
	b, _ := json.Marshal(st) // plain strings and times: can't fail
	if err := s.Set(Service, Account, string(b)); err != nil {
		return storeErr(err)
	}
	return nil
}

// DeleteLogin removes the stored login, a corrupt one included; nothing
// stored is not an error. A newer Ghostbranch's login is kept.
func DeleteLogin(s keychain.Store) error {
	if err := refuseNewer(s); err != nil {
		return err
	}
	if err := s.Delete(Service, Account); err != nil {
		return storeErr(err)
	}
	return nil
}
