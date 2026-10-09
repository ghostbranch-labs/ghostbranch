// Command ghostbranch gives a public GitHub repo private feature branches.
package main

import (
	"os"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
)

func main() {
	code := cli.Run(os.Args[1:], cli.DefaultEnv())
	_ = gitx.Cleanup() // the git isolation directory; best effort, the command is done
	os.Exit(code)
}
