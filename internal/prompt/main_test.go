package prompt_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
)

// With GB_PROMPT_HELPER set, the test binary is a command that asks one
// question through Console() and prints what happened. TestConsoleNeverReadsStdin
// runs it with "y" piped into stdin.
func TestMain(m *testing.M) {
	if os.Getenv("GB_PROMPT_HELPER") != "" {
		a, err := prompt.Console()
		if err != nil {
			fmt.Println("no console:", err)
			os.Exit(0)
		}
		yes, err := a.YesNo(context.Background(), "Proceed?", false) // never reached in the test: no console
		fmt.Println("answered", yes, err)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
