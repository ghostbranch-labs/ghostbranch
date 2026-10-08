package engine

import (
	"context"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

// The runtime guard: no operation but promote and apply may push to the
// public remote, whatever a future bug does.
func TestPushGuardRefusesPublicOutsidePromoteAndApply(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	for _, op := range []string{"init", "sync", "feature start", "gate", ""} {
		pushed := false
		e.OnPush = func(string, string, []string) { pushed = true }
		err := e.push(context.Background(), &pairCtx{op: op}, remotePublic, gitx.PushOptions{}, "x:refs/heads/x")
		if contract.From(err) == nil || contract.From(err).Code != contract.Internal || pushed {
			t.Errorf("op %q: guard did not refuse (err=%v pushed=%v)", op, err, pushed)
		}
	}
}
