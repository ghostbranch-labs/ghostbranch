package cli

import (
	"sort"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// NextFor is the next a command called command gives for err, when nothing
// else is known (no result, no pair, the default workspace).
func NextFor(command string, err error) []contract.Next {
	a := &app{command: command, env: Env{Getenv: func(string) string { return "" }}}
	return a.next(err)
}

// CommandPaths is every runnable command, as the envelope names it
// ("feature start", "auth status").
func CommandPaths() []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.Runnable() && c.HasParent() {
			out = append(out, commandName(c))
		}
		for _, s := range c.Commands() {
			walk(s)
		}
	}
	walk((&app{}).root())
	sort.Strings(out)
	return out
}

// ColorAllowed and TrueColorTerm expose DefaultEnv's colour decisions.
var (
	ColorAllowed  = colorAllowed
	TrueColorTerm = trueColorTerm
)
