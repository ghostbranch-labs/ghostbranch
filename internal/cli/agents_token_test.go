package cli_test

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// agentToken is the token the person types at the hidden prompt.
const agentToken = "github_pat_11AGENTSECRET0123456789"

var tokenTestPair = workspace.Pair{Public: "acme/widget", Shadow: "acme/widget-shadow"}

// secretPrompt is a person at a console who types token at the hidden
// prompt. It records what the console showed, the pages opened, how often
// the token was read, and the tokens forges were built on.
type secretPrompt struct {
	con    *consoleLog
	mu     sync.Mutex
	reads  int
	opened []string
	forged []string
}

func atSecretPrompt(te *testEnv, token string) *secretPrompt {
	sp := &secretPrompt{con: &consoleLog{}}
	te.env.ConsoleUsable = true
	te.env.OpenConsole = func() (io.ReadWriteCloser, error) { return sp.con, nil }
	te.env.ReadSecret = func(_ context.Context, dev io.ReadWriteCloser) ([]byte, error) {
		sp.mu.Lock()
		defer sp.mu.Unlock()
		if dev != sp.con {
			return nil, errors.New("read from something other than the console device")
		}
		sp.reads++
		return []byte(token + "\n"), nil
	}
	te.env.OpenBrowser = func(u string) error { sp.opened = append(sp.opened, u); return nil }
	inner := te.env.NewForge
	te.env.NewForge = func(tok string) (forge.Forge, error) {
		sp.mu.Lock()
		sp.forged = append(sp.forged, tok)
		sp.mu.Unlock()
		return inner(tok)
	}
	return sp
}

// noToken fails if anything reads stdin.
type noToken struct{ t *testing.T }

func (r noToken) Read([]byte) (int, error) {
	r.t.Error("agents token read stdin")
	return 0, io.EOF
}

func TestAgentsTokenJSONReadsNothing(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	sp := atSecretPrompt(te, agentToken)
	te.env.Stdin = noToken{t}
	code, m := te.runJSON("agents", "token")
	if code != 0 {
		t.Fatalf("exit %d: %v", code, m)
	}
	if sp.reads != 0 || len(sp.opened) != 0 || sp.con.String() != "" {
		t.Errorf("--json read %d token(s), opened %v, showed %q", sp.reads, sp.opened, sp.con.String())
	}
	data, _ := m["data"].(map[string]any)
	if data["check"] != nil {
		t.Errorf("--json has a check: %v", data["check"])
	}
	if data["link"] != engine.AgentTokenLink(tokenTestPair) || data["select"] != "acme/widget-shadow" {
		t.Errorf("data = %v", data)
	}
	lines, _ := data["set_line"].(map[string]any)
	if lines["sh"] != "export GHOSTBRANCH_TOKEN=<paste>" || lines["powershell"] != "$env:GHOSTBRANCH_TOKEN = '<paste>'" {
		t.Errorf("set_line = %v", lines)
	}
	next, _ := m["next"].([]any)
	if len(next) != 2 {
		t.Fatalf("next = %v", next)
	}
	first, _ := next[0].(map[string]any)
	second, _ := next[1].(map[string]any)
	if cmdOf(first) != "ghostbranch agents token --repo acme/widget" || first["needs_human"] != true {
		t.Errorf("next[0] = %v", first)
	}
	if cmdOf(second) != "ghostbranch agents status --json --repo acme/widget" || second["needs_human"] != false ||
		!strings.Contains(second["why"].(string), "after the person restarts the agent with GHOSTBRANCH_TOKEN set") {
		t.Errorf("next[1] = %v", second)
	}
}

func cmdOf(n map[string]any) string {
	var parts []string
	for _, p := range n["command"].([]any) {
		parts = append(parts, p.(string))
	}
	return strings.Join(parts, " ")
}

// Without a console it shows the same steps and reads nothing.
func TestAgentsTokenWithoutAConsoleReadsNothing(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	sp := atSecretPrompt(te, agentToken)
	te.env.ConsoleUsable = false
	te.env.Stdin = noToken{t}
	code, out, errOut := te.run("agents", "token")
	if code != 0 || sp.reads != 0 {
		t.Fatalf("exit %d, %d read(s)\n%s%s", code, sp.reads, out, errOut)
	}
	for _, want := range []string{engine.AgentTokenLink(tokenTestPair), "Only select repositories", "acme/widget-shadow", "export GHOSTBRANCH_TOKEN=<paste>"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
}

func (sp *secretPrompt) noLeak(t *testing.T, token string, outs ...string) {
	t.Helper()
	for i, s := range append(outs, sp.con.String()) {
		if strings.Contains(s, token) || strings.Contains(s, "AGENTSECRET") {
			t.Errorf("output %d shows the token:\n%s", i, s)
		}
	}
}

func TestAgentsTokenConsoleRefusesPublishingToken(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	sp := atSecretPrompt(te, agentToken)
	te.forge.PushDenied = nil // the token can push to acme/widget
	code, out, errOut := te.run("agents", "token")
	if code != 2 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	if !strings.Contains(errOut, "[AGENT_TOKEN_REFUSED]") || !strings.Contains(errOut, "can push to the public repo acme/widget") ||
		!strings.Contains(errOut, engine.AgentTokenLink(tokenTestPair)) {
		t.Errorf("stderr:\n%s", errOut)
	}
	if strings.Contains(out, "export GHOSTBRANCH_TOKEN") {
		t.Errorf("a refused token got the line that sets it:\n%s", out)
	}
	if sp.reads != 1 || len(sp.forged) == 0 || sp.forged[len(sp.forged)-1] != agentToken {
		t.Errorf("reads %d, forges on %d token(s)", sp.reads, len(sp.forged))
	}
	sp.noLeak(t, agentToken, out, errOut)
}

func TestAgentsTokenConsoleAccepted(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	sp := atSecretPrompt(te, agentToken)
	te.forge.PushDenied = map[string]bool{"acme/widget": true}
	code, out, errOut := te.run("agents", "token")
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
	for _, want := range []string{"reaches acme/widget-shadow, and it can't push to acme/widget.", "export GHOSTBRANCH_TOKEN=", "$env:GHOSTBRANCH_TOKEN = ", "restart your agent", "ghostbranch agents status"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout lacks %q:\n%s", want, out)
		}
	}
	link := engine.AgentTokenLink(tokenTestPair)
	shown := sp.con.String()
	for _, want := range []string{link, "Only select repositories", "acme/widget-shadow"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the console lacks %q:\n%s", want, shown)
		}
	}
	if len(sp.opened) != 1 || sp.opened[0] != link {
		t.Errorf("opened %v", sp.opened)
	}
	sp.noLeak(t, agentToken, out, errOut)
}

func TestAgentsTokenEmptyIsABadAnswer(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	atSecretPrompt(te, "  ")
	if code, out, errOut := te.run("agents", "token"); code != 2 || !strings.Contains(errOut, "[BAD_FLAG]") {
		t.Fatalf("exit %d\n%s%s", code, out, errOut)
	}
}

func TestAgentsTokenColourOnlyOnATerminal(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, terminal bool, noColor string, push bool) (shown, out, errOut string) {
		te := newTestEnv(t)
		initOnly(te)
		sp := atSecretPrompt(te, agentToken)
		if push {
			te.forge.PushDenied = nil
		} else {
			te.forge.PushDenied = map[string]bool{"acme/widget": true}
		}
		te.env.StderrColor, te.env.StdoutColor = terminal, terminal
		if noColor != "" {
			te.vars["NO_COLOR"] = noColor
		}
		_, out, errOut = te.run("agents", "token")
		return sp.con.String(), out, errOut
	}
	const yellow = "\x1b[1m\x1b[93m"
	shown, out, _ := run(t, true, "", false)
	for _, want := range []string{yellow + "Make the agent's token", yellow + "On that page, choose", yellow + "Paste the new token here"} {
		if !strings.Contains(shown, want) {
			t.Errorf("console lacks %q: %q", want, shown)
		}
	}
	if !strings.Contains(out, "\x1b[92mThis token (account") {
		t.Errorf("accepted line not green: %q", out)
	}
	_, _, errOut := run(t, true, "", true)
	if !regexp.MustCompile(`\x1b\[91m[^\x1b]*can push to the public repo`).MatchString(errOut) {
		t.Errorf("no red in the refusal: %q", errOut)
	}
	for name, got := range map[string][3]string{"NO_COLOR": func() [3]string { a, b, c := run(t, true, "1", false); return [3]string{a, b, c} }(),
		"pipe": func() [3]string { a, b, c := run(t, false, "", false); return [3]string{a, b, c} }()} {
		for _, s := range got {
			if strings.Contains(s, "\x1b") {
				t.Errorf("%s: escape code in %q", name, s)
			}
		}
	}
}
