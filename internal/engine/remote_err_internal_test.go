package engine

import (
	"context"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// A git transport failure is retryable only when it may pass on its own:
// a bad or missing credential, or a repo the token can't see, fails the
// same way every time.
func TestRemoteErrRetriesOnlyWhatMayPass(t *testing.T) {
	t.Parallel()
	gitErr := func(stderr string) error { return &gitx.Error{Args: []string{"fetch"}, ExitCode: 128, Stderr: stderr} }
	for name, c := range map[string]struct {
		err   error
		retry bool
	}{
		"network":        {gitErr("fatal: unable to access 'https://github.com/acme/widget/': Could not resolve host: github.com"), true},
		"server error":   {gitErr("error: RPC failed; HTTP 502 curl 22 The requested URL returned error: 502"), true},
		"bad token":      {gitErr("remote: Invalid username or token. Password authentication is not supported for Git operations.\nfatal: Authentication failed for 'https://github.com/acme/widget/'"), false},
		"no credentials": {gitErr("fatal: could not read Username for 'https://github.com': terminal prompts disabled"), false},
		"403":            {gitErr("fatal: unable to access 'https://github.com/acme/widget/': The requested URL returned error: 403"), false},
		"not visible":    {gitErr("remote: Repository not found.\nfatal: repository 'https://github.com/acme/widget-shadow/' not found"), false},
		"push denied":    {&gitx.PermissionError{Detail: "returned error: 403"}, false},
		"cancelled":      {context.Canceled, false},
	} {
		ce := contract.From(remoteErr(c.err, "fetch the shadow repo"))
		if ce.Code != contract.ForgeError || ce.Retryable != c.retry {
			t.Errorf("%s: code %s retryable %v, want FORGE_ERROR retryable %v", name, ce.Code, ce.Retryable, c.retry)
		}
	}
}
