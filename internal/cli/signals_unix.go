//go:build unix

package cli

import (
	"os"
	"syscall"
)

// stopSignals cancel a run like Ctrl-C does: the running step is marked and
// the cursor shown before the process goes, whether a person pressed Ctrl-C,
// a supervisor sent SIGTERM, or the terminal closed (SIGHUP).
var stopSignals = []os.Signal{os.Interrupt, syscall.SIGTERM, syscall.SIGHUP}
