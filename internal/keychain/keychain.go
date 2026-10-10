// Package keychain stores secrets in the operating system's credential
// store: the macOS Keychain, Windows Credential Manager or the Linux
// Secret Service, through go-keyring. Ghostbranch never writes a token to
// a plain file, so with no OS store there is no store at all.
package keychain

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"

	keyring "github.com/zalando/go-keyring"
)

// Store keeps one secret per service and account.
type Store interface {
	Get(service, account string) (string, error) // ErrNotFound when absent
	Set(service, account, secret string) error
	Delete(service, account string) error // nil when absent
}

// ErrNotFound means nothing is stored for that service and account.
var ErrNotFound = errors.New("keychain: not found")

// ErrUnavailable means this process has no OS store it can use: headless
// Linux without a Secret Service, no keychain tool, no logon session, or a
// store that didn't answer in time.
var ErrUnavailable = errors.New("keychain: unavailable")

// DefaultTimeout bounds one OS keychain call. Unlocking a keychain can show
// the person a prompt, so it is generous; a store that never answers is
// then ErrUnavailable rather than a hang.
const DefaultTimeout = 2 * time.Minute

// errNoSuchLogonSession is Windows' ERROR_NO_SUCH_LOGON_SESSION: Credential
// Manager has no logon session to store into (a service account, some SSH
// sessions).
const errNoSuchLogonSession syscall.Errno = 1312

// OS returns the store backed by the operating system, through go-keyring.
func OS() Store {
	return &osStore{get: keyring.Get, set: keyring.Set, del: keyring.Delete, goos: runtime.GOOS, timeout: DefaultTimeout}
}

type osStore struct {
	get     func(service, account string) (string, error)
	set     func(service, account, secret string) error
	del     func(service, account string) error
	goos    string
	timeout time.Duration
}

type result struct {
	v   string
	err error
}

// do runs fn with the timeout. On a timeout fn keeps running in the
// background; the process ends soon after any keychain failure.
func (s *osStore) do(fn func() (string, error)) (string, error) {
	ch := make(chan result, 1)
	go func() {
		v, err := fn()
		ch <- result{v, err}
	}()
	t := time.NewTimer(s.timeout)
	defer t.Stop()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-t.C:
		return "", fmt.Errorf("%w: the OS keychain didn't answer within %s", ErrUnavailable, s.timeout)
	}
}

func (s *osStore) Get(service, account string) (string, error) {
	v, err := s.do(func() (string, error) { return s.get(service, account) })
	if err != nil {
		return "", s.mapErr("read", err, "")
	}
	return v, nil
}

func (s *osStore) Set(service, account, secret string) error {
	_, err := s.do(func() (string, error) { return "", s.set(service, account, secret) })
	if err != nil {
		return s.mapErr("write", err, secret)
	}
	return nil
}

func (s *osStore) Delete(service, account string) error {
	_, err := s.do(func() (string, error) { return "", s.del(service, account) })
	if err == nil {
		return nil
	}
	if err = s.mapErr("delete", err, ""); errors.Is(err, ErrNotFound) {
		return nil
	}
	return err
}

// mapErr maps go-keyring's errors onto this package's, by OS (see the
// package's plan: matching godbus error types would mean importing godbus).
// A message that contains the secret is replaced: no error ever carries it.
func (s *osStore) mapErr(op string, err error, secret string) error {
	out := s.classify(op, err)
	if secret != "" && strings.Contains(out.Error(), secret) {
		if errors.Is(out, ErrUnavailable) {
			return fmt.Errorf("%w: %s: the OS keychain refused the entry", ErrUnavailable, op)
		}
		return fmt.Errorf("keychain: %s: the OS keychain refused the entry", op)
	}
	return out
}

func (s *osStore) classify(op string, err error) error {
	switch {
	case errors.Is(err, ErrUnavailable):
		return err
	case errors.Is(err, keyring.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, keyring.ErrSetDataTooBig):
		return fmt.Errorf("keychain: %s: the entry is too big for the OS keychain: %w", op, err)
	case errors.Is(err, keyring.ErrUnsupportedPlatform), errors.Is(err, exec.ErrNotFound):
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	}
	switch s.goos {
	case "windows":
		var errno syscall.Errno
		if errors.As(err, &errno) && errno == errNoSuchLogonSession {
			return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
		}
		return fmt.Errorf("keychain: %s: %w", op, err)
	case "darwin":
		return fmt.Errorf("keychain: %s: %w", op, err)
	default:
		// Linux and the BSDs reach the Secret Service over D-Bus. Any
		// failure there but "not found" means it can't be used: no session
		// bus (headless, SSH, CI), no Secret Service on the bus, a locked
		// collection whose unlock prompt was dismissed, or a D-Bus error.
		// Retrying fixes none of them.
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, op, err)
	}
}
