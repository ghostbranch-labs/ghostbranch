package gitx

import "testing"

func TestPermissionDeniedClassifier(t *testing.T) {
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
