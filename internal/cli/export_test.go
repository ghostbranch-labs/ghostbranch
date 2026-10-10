package cli

import "github.com/ghostbranch-labs/ghostbranch/internal/contract"

// NextFor is the next a command called command gives for err, when nothing
// else is known (no result, no pair, the default workspace).
func NextFor(command string, err error) []contract.Next {
	a := &app{command: command, env: Env{Getenv: func(string) string { return "" }}}
	return a.next(err)
}
