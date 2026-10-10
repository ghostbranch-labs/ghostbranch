package keychain

import (
	"fmt"
	"sync"
)

// Fake is an in-memory Store for tests. It is safe for concurrent use.
type Fake struct {
	// Unavailable makes every call fail with ErrUnavailable, as on a
	// headless Linux machine. Set it before the Fake is shared.
	Unavailable bool

	mu                  sync.Mutex
	items               map[string]string
	gets, sets, deletes int
}

func NewFake() *Fake { return &Fake{items: map[string]string{}} }

func fakeKey(service, account string) string { return service + "\x00" + account }

func (f *Fake) Get(service, account string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.Unavailable {
		return "", fmt.Errorf("%w: fake", ErrUnavailable)
	}
	v, ok := f.items[fakeKey(service, account)]
	if !ok {
		return "", ErrNotFound
	}
	return v, nil
}

func (f *Fake) Set(service, account, secret string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.Unavailable {
		return fmt.Errorf("%w: fake", ErrUnavailable)
	}
	f.items[fakeKey(service, account)] = secret
	return nil
}

func (f *Fake) Delete(service, account string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deletes++
	if f.Unavailable {
		return fmt.Errorf("%w: fake", ErrUnavailable)
	}
	delete(f.items, fakeKey(service, account))
	return nil
}

// Calls reports how many Get, Set and Delete calls the Fake has had,
// failed ones included. Tests use it to prove a path never opens the
// keychain (GHOSTBRANCH_TOKEN set).
func (f *Fake) Calls() (gets, sets, deletes int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets, f.sets, f.deletes
}
