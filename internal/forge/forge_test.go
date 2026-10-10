package forge_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
)

// HostMessage gives the host's message on one printable line, capped, and
// "" when the chain carries none.
func TestHostMessage(t *testing.T) {
	t.Parallel()
	wrap := func(msg string) error {
		return fmt.Errorf("create x: %w", &forge.HostError{Message: msg, Err: forge.ErrForbidden})
	}
	if got := forge.HostMessage(wrap("Resource not\n accessible\x1b[31m by integration")); got != "Resource not accessible[31m by integration" {
		t.Fatalf("got %q", got)
	}
	if got := forge.HostMessage(wrap(strings.Repeat("a", 300))); len([]rune(got)) != 201 {
		t.Fatalf("not capped: %d runes", len([]rune(got)))
	}
	if got := forge.HostMessage(errors.New("plain")); got != "" {
		t.Fatalf("got %q", got)
	}
	if !errors.Is(wrap("m"), forge.ErrForbidden) {
		t.Fatal("HostError must unwrap")
	}
}
