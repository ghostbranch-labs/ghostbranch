package cli_test

import "testing"

// Usage problems with a stack promote fail before any network work.
func TestPromoteStackFlags(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	for _, c := range []struct {
		args  []string
		field string
	}{
		{[]string{"promote", "feat-a", "feat-b", "--title", "t"}, "title"},
		{[]string{"promote", "feat-a", "feat-b", "--title-for", "feat-c=t"}, "title-for"},
		{[]string{"promote", "feat-a", "feat-b", "--title-for", "feat-a"}, "title-for"},
		{[]string{"promote", "feat-a", "feat-b", "--mode-for", "feat-b=curated"}, "mode-for"},
		{[]string{"promote", "feat-a", "feat-b", "--branch", "x"}, "branch"},
	} {
		code, env := te.runJSON(c.args...)
		e, _ := env["error"].(map[string]any)
		if code != 2 || e["code"] != "BAD_FLAG" || e["field"] != c.field {
			t.Errorf("%v: exit %d, error %v; want BAD_FLAG on %s", c.args, code, e, c.field)
		}
	}
}
