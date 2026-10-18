package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
)

// Source is everything Resolve reads.
type Source struct {
	Getenv  func(string) string
	GH      func() (string, error)
	Store   keychain.Store // nil: no stored login is consulted
	Refresh func(ctx context.Context, l Login) (Login, error)
	Lock    func() (unlock func(), err error) // <GHOSTBRANCH_HOME>/locks/auth.lock in the CLI
	Now     func() time.Time
	// OnSaveError hears that a refreshed login couldn't be stored. The
	// error may carry a secret, so it is for tests and logic, never output.
	OnSaveError func(error)
}

func (s Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Resolve returns the token and where it came from, in this order:
// GHOSTBRANCH_TOKEN, the stored login ("login"), GH_TOKEN, GITHUB_TOKEN,
// `gh auth token` (off when GHOSTBRANCH_NO_GH is set; see NoGH). With GHOSTBRANCH_TOKEN set the keychain is never touched.
// A stored login that can't be used (corrupt, locked keychain, expired,
// newer) is an error, never a quiet fall back to a later source; only a
// machine with no keychain at all goes on.
func Resolve(ctx context.Context, src Source) (string, string, error) {
	get := src.Getenv
	if get == nil {
		get = func(string) string { return "" }
	}
	if v := strings.TrimSpace(get("GHOSTBRANCH_TOKEN")); v != "" {
		return v, "GHOSTBRANCH_TOKEN", nil
	}
	if src.Store != nil {
		l, err := LoadLogin(src.Store)
		switch {
		case errors.Is(err, keychain.ErrUnavailable):
			// No OS keychain: no login can be stored here, so there is none.
		case err != nil:
			return "", "", err
		case l != nil:
			tok, err := current(ctx, src, l)
			if err != nil {
				return "", "", err
			}
			return tok, "login", nil
		}
	}
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(get(k)); v != "" {
			return v, k, nil
		}
	}
	noGH := NoGH(get)
	if src.GH != nil && !noGH {
		if t, err := src.GH(); err == nil && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t), "gh auth token", nil
		}
	}
	if noGH {
		return "", "", contract.New(contract.AuthMissing, "no GitHub token: run `ghostbranch login`, or set GHOSTBRANCH_TOKEN or GH_TOKEN (the gh fallback is off: GHOSTBRANCH_NO_GH is set)")
	}
	return "", "", contract.New(contract.AuthMissing, "no GitHub token: run `ghostbranch login`, or set GHOSTBRANCH_TOKEN or GH_TOKEN, or run `gh auth login`")
}

// NoGH reports whether GHOSTBRANCH_NO_GH turns off the `gh auth token`
// fallback: 1, true or yes, in any case, after trimming spaces.
func NoGH(getenv func(string) string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv("GHOSTBRANCH_NO_GH"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func stale(l *Login, now time.Time) bool {
	return !l.Expires.IsZero() && !now.Before(l.Expires.Add(-RefreshWindow))
}

func refreshDead(l *Login, now time.Time) bool {
	return l.RefreshToken == "" || (!l.RefreshExpires.IsZero() && !now.Before(l.RefreshExpires))
}

// current returns l's token, refreshing it first when it is within
// RefreshWindow of expiry. A source without Refresh or Lock never refreshes
// and returns a stale token as it is. The refresh happens under src.Lock, and the login
// is read again once the lock is held: a command that waited behind another
// one's refresh uses that refresh's token. A second refresh would spend the
// already-rotated refresh token and log the person out.
func current(ctx context.Context, src Source, l *Login) (string, error) {
	if !stale(l, src.now()) {
		return l.Token, nil
	}
	if refreshDead(l, src.now()) {
		return "", expired()
	}
	if src.Refresh == nil || src.Lock == nil {
		// A read-only source (doctor) never refreshes: it uses the stored
		// token as it is and lets GitHub say whether it still works.
		return l.Token, nil
	}
	unlock, err := src.Lock()
	if err != nil {
		return "", err
	}
	defer unlock()
	cur, err := LoadLogin(src.Store)
	if err != nil {
		return "", err
	}
	if cur == nil {
		return "", contract.New(contract.AuthMissing, "the stored login was removed (ghostbranch logout) while this command waited; run `ghostbranch login`")
	}
	if !stale(cur, src.now()) {
		return cur.Token, nil
	}
	if refreshDead(cur, src.now()) {
		return "", expired()
	}
	nl, err := src.Refresh(ctx, *cur)
	if err != nil {
		// A command under another GHOSTBRANCH_HOME holds another lock. If it
		// refreshed first, its new pair is stored: use it.
		if contract.From(err).Code == contract.LoginExpired {
			if won, lerr := LoadLogin(src.Store); lerr == nil && won != nil && won.RefreshToken != cur.RefreshToken && !stale(won, src.now()) {
				return won.Token, nil
			}
		}
		return "", err
	}
	nl.Account, nl.Kind = cur.Account, cur.Kind
	// If storing fails, the old pair is already dead: this command still
	// works with the new token, and the next one fails LOGIN_EXPIRED and
	// asks for a login. OnSaveError says so now.
	if err := SaveLogin(src.Store, nl); err != nil && src.OnSaveError != nil {
		src.OnSaveError(err)
	}
	return nl.Token, nil
}
