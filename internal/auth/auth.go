// Package auth finds the GitHub token, runs the GitHub App device flow, and
// keeps the login in the OS keychain. A token is never printed, logged,
// written to a file or put in an error message.
package auth

import (
	"os/exec"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Service and Account name the keychain entry that holds the login.
const (
	Service = "ghostbranch"
	Account = "github.com"
)

const (
	// RefreshWindow: a stored app token this close to expiry is refreshed
	// before it is used.
	RefreshWindow = 5 * time.Minute
	// LockWait is how long a command waits for another one's refresh.
	LockWait = 30 * time.Second
)

// ClientID is the Ghostbranch GitHub App's client ID, which is public by
// design (there is no client secret). AppSlug is the app's URL name, for its
// install page. Both are empty in builds made before the app was registered;
// login then offers only --with-token.
var (
	ClientID = "Iv23liqDTESZ1JStXO0g"
	AppSlug  = "ghostbranch-cli"
)

// Kind names a token's type from its prefix.
func Kind(token string) string {
	switch {
	case strings.HasPrefix(token, "ghu_"):
		return "app"
	case strings.HasPrefix(token, "github_pat_"):
		return "fine-grained"
	case strings.HasPrefix(token, "ghp_"):
		return "classic"
	case strings.HasPrefix(token, "gho_"):
		return "oauth"
	}
	return "unknown"
}

// LooksLikeAgent reports whether this run is likely an agent or a script
// rather than a person at a terminal: GHOSTBRANCH_ACTOR names an agent or
// the app, a coding agent's marker variable is set, or there is no usable
// console. GHOSTBRANCH_ACTOR=human:… says it is a person.
func LooksLikeAgent(getenv func(string) string, consoleUsable bool) bool {
	actor := strings.TrimSpace(getenv("GHOSTBRANCH_ACTOR"))
	switch {
	case strings.HasPrefix(actor, "agent:"), actor == "app":
		return true
	case strings.HasPrefix(actor, "human:"):
		return false
	case getenv("CLAUDECODE") != "":
		return true
	}
	return !consoleUsable
}

// GHCLI asks the GitHub CLI for its token.
func GHCLI() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	return string(out), err
}

// FileLock is a Source.Lock over the OS file lock at path (workspace.Lock,
// which frees itself when its process dies). It waits up to wait for another
// command's hold to end, then gives that command's LOCKED.
func FileLock(path string, now func() time.Time, wait time.Duration) func() (func(), error) {
	return func() (func(), error) {
		deadline := time.Now().Add(wait) // real time: tests run fake clocks
		for {
			unlock, err := workspace.Lock(path, now(), "auth refresh")
			if err == nil {
				return unlock, nil
			}
			if ce := contract.From(err); ce.Code != contract.Locked || time.Now().After(deadline) {
				return nil, err
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}
