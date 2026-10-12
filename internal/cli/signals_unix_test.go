//go:build unix

package cli

import (
	"os"
	"slices"
	"syscall"
	"testing"
)

func TestStopSignalsIncludeHUP(t *testing.T) {
	t.Parallel()
	if !slices.Contains(stopSignals, os.Signal(syscall.SIGHUP)) {
		t.Fatalf("stopSignals %v", stopSignals)
	}
}
