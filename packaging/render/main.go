// Command render fills the Homebrew formula and the Scoop manifest for a
// release, and decides where a published release is distributed. The
// publish workflow (.github/workflows/publish.yml) runs it; see RELEASING.md.
//
//	go run ./packaging/render render -version v0.1.0 -checksums checksums.txt -out rendered
//	go run ./packaging/render route -tag v0.1.0 -prerelease=false -stable-exists=true
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "render:", err)
		os.Exit(1)
	}
}
