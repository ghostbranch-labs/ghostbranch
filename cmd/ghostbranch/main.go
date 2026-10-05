// Command ghostbranch gives a public GitHub repo private feature branches.
package main

import (
	"os"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:], cli.DefaultEnv())) }
