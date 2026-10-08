package cli

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// DEFERRED (sp2): editor settings with spaces, quotes and Windows paths.
func TestSplitCommand(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	spaced := filepath.Join(dir, "My Editor", "edit.exe")
	if err := os.MkdirAll(filepath.Dir(spaced), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(spaced, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	exists := func(p string) bool { fi, err := os.Stat(p); return err == nil && !fi.IsDir() }
	for in, want := range map[string][]string{
		"vi":                                    {"vi"},
		"code --wait":                           {"code", "--wait"},
		`"C:\Program Files\N\n.exe" -multiInst`: {`C:\Program Files\N\n.exe`, "-multiInst"},
		`'vim' -u NONE`:                         {"vim", "-u", "NONE"},
		spaced:                                  {spaced},
		"  ":                                    nil,
	} {
		if got := splitCommand(in, exists); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}
