// Package auth finds the GitHub token. Tokens are never stored by
// Ghostbranch and never leave this machine.
package auth

import (
	"os/exec"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// Token returns GHOSTBRANCH_TOKEN, GH_TOKEN, GITHUB_TOKEN, or `gh auth token`,
// in that order.
func Token(getenv func(string) string, gh func() (string, error)) (string, error) {
	for _, k := range []string{"GHOSTBRANCH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(getenv(k)); v != "" {
			return v, nil
		}
	}
	if gh != nil {
		if t, err := gh(); err == nil && strings.TrimSpace(t) != "" {
			return strings.TrimSpace(t), nil
		}
	}
	return "", contract.New(contract.AuthMissing, "no GitHub token: set GHOSTBRANCH_TOKEN or GH_TOKEN, or run `gh auth login`")
}

// GHCLI asks the GitHub CLI for its token.
func GHCLI() (string, error) {
	out, err := exec.Command("gh", "auth", "token").Output()
	return string(out), err
}
