//go:build unix

package cli

import (
	"os"
	"os/signal"
	"syscall"
)

// stopSignals cancel a run like Ctrl-C does: the running step is marked and
// the cursor shown before the process goes, whether a person pressed Ctrl-C,
// a supervisor sent SIGTERM, or the terminal closed (SIGHUP).
var stopSignals = stopSignalList()

// stopSignalList leaves SIGHUP out when the process ignores it (nohup):
// Notify on an ignored SIGHUP installs a handler, which would undo nohup.
func stopSignalList() []os.Signal {
	list := []os.Signal{os.Interrupt, syscall.SIGTERM}
	if !signal.Ignored(syscall.SIGHUP) {
		list = append(list, syscall.SIGHUP)
	}
	return list
}
