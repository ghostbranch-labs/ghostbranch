package cli_test

import (
	"strings"
	"testing"
)

// nextCommands is each next step's command, joined with spaces.
func nextCommands(env map[string]any) []string {
	var out []string
	ns, _ := env["next"].([]any)
	for _, n := range ns {
		m, _ := n.(map[string]any)
		parts, _ := m["command"].([]any)
		s := make([]string, len(parts))
		for i, p := range parts {
			s[i], _ = p.(string)
		}
		out = append(out, strings.Join(s, " "))
	}
	return out
}

func hasNext(env map[string]any, want string) bool {
	for _, c := range nextCommands(env) {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// next walks an agent up the stack: gate a, gate b, promote a b --plan; a
// list that isn't a stack points at the stack; a moved parent points at
// restack.
func TestNextWalksUpTheStack(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	te.mustRun("feature", "start", "feat-a")
	te.shadowCommit("feat-a", map[string]string{"a.txt": "a\n"})
	te.mustRun("feature", "start", "feat-b", "--from", "feat-a")
	te.shadowCommit("feat-b", map[string]string{"b.txt": "b\n"})

	if _, env := te.runJSON("gate", "feat-a"); !hasNext(env, "gate feat-b") {
		t.Fatalf("after gate feat-a, next = %v", nextCommands(env))
	}
	if _, env := te.runJSON("gate", "feat-b"); !hasNext(env, "promote feat-a feat-b --mode preserve --plan") {
		t.Fatalf("after gate feat-b, next = %v", nextCommands(env))
	}
	if _, env := te.runJSON("promote", "feat-b", "--plan"); !hasNext(env, "promote feat-a feat-b --plan") {
		t.Fatalf("after NOT_A_STACK, next = %v", nextCommands(env))
	}
	te.shadowCommit("feat-a", map[string]string{"a2.txt": "a2\n"})
	if _, env := te.runJSON("promote", "feat-a", "feat-b", "--plan"); !hasNext(env, "gate feat-a") {
		t.Fatalf("after a stale gate, next = %v", nextCommands(env))
	}
	_, env := te.runJSON("status")
	if !hasNext(env, "feature restack feat-a") {
		t.Fatalf("with feat-b parent_stale, status's next = %v", nextCommands(env))
	}
	// One restack per moved parent, however many branches stand on it.
	te.mustRun("feature", "start", "feat-c", "--from", "feat-a")
	te.shadowCommit("feat-c", map[string]string{"c.txt": "c\n"})
	te.shadowCommit("feat-a", map[string]string{"a3.txt": "a3\n"})
	_, env = te.runJSON("status")
	n := 0
	for _, c := range nextCommands(env) {
		if strings.Contains(c, "feature restack feat-a") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("feat-b and feat-c on a moved feat-a need one restack, next = %v", nextCommands(env))
	}
}
