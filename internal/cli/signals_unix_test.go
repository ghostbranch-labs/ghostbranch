//go:build unix

package cli

import (
	"os"
	"os/signal"
	"slices"
	"syscall"
	"testing"
)

// The ignored case (nohup) is not tested: signal.Ignore is process-wide and
// would change signal handling for the parallel tests around it.
func TestStopSignalsIncludeHUPUnlessIgnored(t *testing.T) {
	t.Parallel()
	got := slices.Contains(stopSignals, os.Signal(syscall.SIGHUP))
	if want := !signal.Ignored(syscall.SIGHUP); got != want {
		t.Fatalf("HUP in stopSignals = %v, want %v (%v)", got, want, stopSignals)
	}
}
