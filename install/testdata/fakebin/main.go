// Command fakebin stands in for ghostbranch in the install script tests: it
// answers "version" the way the real binary does. The tests stamp version.
package main

import (
	"fmt"
	"os"
)

var version = "v0.0.0"

func main() {
	if len(os.Args) == 2 && os.Args[1] == "version" {
		fmt.Printf("ghostbranch %s (commit fake), JSON contract 0.13\n", version)
		return
	}
	os.Exit(2)
}
