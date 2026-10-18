package cli

import (
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
)

// After a login in this run, "Using GitHub account" repeats nothing only
// when the token in use is that login; any other token keeps the line.
func TestSayAccountSkipsOnlyForTheLoginJustMade(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		source string
		just   bool
		want   bool
	}{
		{"login", true, false},
		{"login", false, true},
		{"GHOSTBRANCH_TOKEN", true, true},
		{"GH_TOKEN", true, true},
	} {
		s := prompt.NewScript()
		sayAccount(s, &engine.Engine{TokenSource: tc.source}, "acme", tc.just)
		said := len(s.Said) == 1 && strings.HasPrefix(s.Said[0], "Using GitHub account acme (from ")
		if said != tc.want {
			t.Errorf("source %s, justLoggedIn %v: said %q, want line %v", tc.source, tc.just, s.Said, tc.want)
		}
	}
}
