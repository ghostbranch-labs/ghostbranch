package cli_test

import "testing"

// M8: a negative --timeout is a bad flag, not "no timeout".
func TestWaitRefusesANegativeTimeout(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	code, env := te.runJSON("wait", "gate/feat-x/0123456789ab", "--timeout=-1s")
	e, _ := env["error"].(map[string]any)
	if code != 2 || e["code"] != "BAD_FLAG" || e["field"] != "timeout" {
		t.Fatalf("%d %v", code, env)
	}
}
