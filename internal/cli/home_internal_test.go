package cli

import (
	"path/filepath"
	"testing"
)

// Owner ruling on I2: only Ghostbranch's own default home may be made
// private; a GHOSTBRANCH_HOME the user chose is left alone.
func TestOpenWorkspaceMarksTheDefaultHome(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, c := range []struct {
		name string
		env  map[string]string
		own  bool
	}{
		{"GHOSTBRANCH_HOME unset", map[string]string{"LOCALAPPDATA": dir, "XDG_STATE_HOME": dir}, true},
		{"GHOSTBRANCH_HOME set", map[string]string{"GHOSTBRANCH_HOME": filepath.Join(dir, "gb")}, false},
	} {
		a := &app{env: Env{
			Getenv:   func(k string) string { return c.env[k] },
			UserHome: func() (string, error) { return dir, nil },
		}}
		ws, err := a.openWorkspace()
		if err != nil {
			t.Fatal(err)
		}
		if ws.OwnHome != c.own {
			t.Fatalf("%s: OwnHome = %v", c.name, ws.OwnHome)
		}
	}
}
