//go:build !unix

package cli

import (
	"os"
	"syscall"
)

// stopSignals cancel a run like Ctrl-C does. Windows has no SIGHUP; Go
// delivers a closed console window as SIGTERM.
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM}
