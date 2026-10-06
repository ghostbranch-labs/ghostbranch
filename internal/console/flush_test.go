package console

import (
	"bytes"
	"io"
	"testing"
)

type fakeDevice struct{ io.Reader }

func (fakeDevice) Write(p []byte) (int, error) { return len(p), nil }
func (fakeDevice) Close() error                { return nil }

// Only the real console device is flushed: anything else, such as a test's
// fake with the answer already typed, keeps its input.
func TestFlushLeavesOtherDevicesAlone(t *testing.T) {
	dev := fakeDevice{bytes.NewBufferString("ABC234\n")}
	flush(dev)
	got, err := io.ReadAll(dev)
	if err != nil || string(got) != "ABC234\n" {
		t.Fatalf("after flush: %q %v", got, err)
	}
}
