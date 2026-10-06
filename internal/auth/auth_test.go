package auth_test

import (
	"errors"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

func TestTokenOrder(t *testing.T) {
	gh := func() (string, error) { return "from-gh\n", nil }
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{"GHOSTBRANCH_TOKEN": "a", "GH_TOKEN": "b", "GITHUB_TOKEN": "c"}, "a"},
		{map[string]string{"GH_TOKEN": "b", "GITHUB_TOKEN": "c"}, "b"},
		{map[string]string{"GITHUB_TOKEN": "c"}, "c"},
		{nil, "from-gh"},
	}
	for _, c := range cases {
		if got, err := auth.Token(get(c.env), gh); err != nil || got != c.want {
			t.Errorf("%v: got %q %v", c.env, got, err)
		}
	}
	_, err := auth.Token(get(nil), func() (string, error) { return "", errors.New("not logged in") })
	if contract.From(err).Code != contract.AuthMissing {
		t.Fatalf("no token: %v", err)
	}
}

func TestResolveNamesTheSource(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	gh := func() (string, error) { return "t\n", nil }
	for env, want := range map[string]string{"GHOSTBRANCH_TOKEN": "GHOSTBRANCH_TOKEN", "GH_TOKEN": "GH_TOKEN", "GITHUB_TOKEN": "GITHUB_TOKEN"} {
		if v, src, err := auth.Resolve(get(map[string]string{env: " x "}), gh); err != nil || v != "x" || src != want {
			t.Errorf("%s: %q %q %v", env, v, src, err)
		}
	}
	if v, src, err := auth.Resolve(get(nil), gh); err != nil || v != "t" || src != "gh auth token" {
		t.Errorf("gh: %q %q %v", v, src, err)
	}
}
