package gitx

import "testing"

func TestPermissionDeniedClassifier(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"remote: Permission to acme/widget.git denied to octo.":              true,
		"fatal: unable to access 'x': The requested URL returned error: 403": true,
		"remote: Write access to repository not granted.":                    true,
		"fatal: repository not found":                                        false,
	} {
		if got := permissionDenied.MatchString(s); got != want {
			t.Errorf("%q: got %v want %v", s, got, want)
		}
	}
}

// A [remote rejected] reason that is really a lost race for the ref goes to
// RejectedError (REF_MOVED: re-run); any other reason is the server's own
// refusal, a RefusedError.
func TestPushFailureClassifiesRemoteRejected(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line    string
		refused bool
	}{
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (pre-receive hook declined)", true},
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (refusing to allow a Personal Access Token to create or update workflow `.github/workflows/ci.yml` without `workflow` scope)", true},
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (cannot lock ref 'refs/heads/x': is at 1234 but expected 5678)", false},
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (failed to update ref)", false},
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (incorrect old value provided)", false},
		{"!\trefs/heads/x:refs/heads/x\t[remote rejected] (atomic transaction failed)", false},
		{"!\trefs/heads/x:refs/heads/x\t[rejected] (stale info)", false},
	} {
		err := pushFailure(tc.line+"\nDone\n", "")
		_, isRefused := err.(*RefusedError)
		rej, isRejected := err.(*RejectedError)
		if isRefused != tc.refused || isRejected == tc.refused {
			t.Errorf("%s: got %T %v", tc.line, err, err)
			continue
		}
		if isRejected && (len(rej.Refs) != 1 || rej.Refs[0] != "refs/heads/x:refs/heads/x") {
			t.Errorf("%s: refs = %v", tc.line, rej.Refs)
		}
	}
	// A refusal for one ref still wins over a lost race for another.
	err := pushFailure("!\trefs/heads/a:refs/heads/a\t[remote rejected] (atomic transaction failed)\n"+
		"!\trefs/heads/b:refs/heads/b\t[remote rejected] (pre-receive hook declined)\n", "")
	if r, ok := err.(*RefusedError); !ok || len(r.Refs) != 1 || r.Refs[0] != "refs/heads/b" {
		t.Errorf("mixed: got %T %v", err, err)
	}
}
