package cli_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
)

// Every command's output is part of the pinned contract: it has a data
// schema, and at least one golden shows it. guard install, uninstall and
// check need a real checkout with a hook, so guard_test.go validates
// their output against guard.schema.json instead of a golden.
func TestEveryCommandHasASchemaAndAGolden(t *testing.T) {
	t.Parallel()
	noGolden := map[string]bool{"guard install": true, "guard uninstall": true, "guard check": true}
	files, err := filepath.Glob(filepath.Join("testdata", "golden", "*.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("goldens: %v", err)
	}
	shown := map[string]bool{}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var env struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(raw, &env) != nil { // wait's NDJSON: the envelope is the last line
			lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(raw), "\r\n", "\n")), "\n")
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &env); err != nil {
				t.Fatalf("%s: %v", f, err)
			}
		}
		shown[env.Command] = true
	}
	for _, cmd := range cli.CommandPaths() {
		schema, ok := dataSchemas[cmd]
		if !ok {
			t.Errorf("%s has no entry in dataSchemas", cmd)
		} else if _, err := os.Stat(filepath.Join("..", "..", "schemas", "v0", schema)); err != nil {
			t.Errorf("%s: schema %s: %v", cmd, schema, err)
		}
		if !shown[cmd] && !noGolden[cmd] {
			t.Errorf("%s has no golden in testdata/golden", cmd)
		}
	}
}
