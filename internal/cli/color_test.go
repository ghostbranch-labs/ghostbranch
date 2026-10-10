package cli_test

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// runColor runs args with both streams marked as colour terminals.
func (te *testEnv) runColor(truecolor bool, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := te.env
	env.Stdout, env.Stderr = &out, &errb
	env.StdoutColor, env.StderrColor, env.TrueColor = true, true, truecolor
	code := cli.Run(args, env)
	return code, out.String(), errb.String()
}

func TestColorDoctorMarks(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	_, plain, _ := te.run("doctor", "--repo", "acme/widget")
	_, out, _ := te.runColor(false, "doctor", "--repo", "acme/widget")
	for _, want := range []string{"\x1b[92m✓\x1b[0m", "\x1b[93m!\x1b[0m", "\x1b[2m–\x1b[0m"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output lacks %q:\n%q", want, out)
		}
	}
	if got := ansi.ReplaceAllString(out, ""); got != plain {
		t.Errorf("colour changed the text:\n%q\nvs\n%q", got, plain)
	}
	_, tc, _ := te.runColor(true, "doctor", "--repo", "acme/widget")
	if !strings.Contains(tc, "\x1b[38;2;132;204;22m✓\x1b[0m") {
		t.Errorf("truecolor mark missing: %q", tc)
	}
}

func TestColorErrorNextAndFailMark(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	_, plainOut, plainErr := te.run("doctor", "--repo", "acme/widget")
	code, out, errOut := te.runColor(false, "doctor", "--repo", "acme/widget")
	if code == 0 {
		t.Fatal("doctor should fail without a token")
	}
	if !strings.Contains(out, "\x1b[91m✗\x1b[0m") {
		t.Errorf("no red fail mark:\n%q", out)
	}
	if !strings.Contains(errOut, "\x1b[91merror:\x1b[0m ") {
		t.Errorf("no red error prefix:\n%q", errOut)
	}
	if !strings.Contains(errOut, "\x1b[1mNext:\x1b[0m\n  \x1b[1m") {
		t.Errorf("Next heading or command not bold:\n%q", errOut)
	}
	if !regexp.MustCompile(`\n    \x1b\[2m[^\x1b]+\x1b\[0m\n`).MatchString(errOut) {
		t.Errorf("why line not dim:\n%q", errOut)
	}
	if ansi.ReplaceAllString(out, "") != plainOut || ansi.ReplaceAllString(errOut, "") != plainErr {
		t.Error("colour changed the text")
	}
}

func TestColorWarningPrefix(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	ledger := filepath.Join(te.vars["GHOSTBRANCH_HOME"], "workspaces", "default", "ledger.jsonl")
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, errOut := te.runColor(false, "feature", "start", "feat-y")
	if !strings.Contains(errOut, "\x1b[93mwarning:\x1b[0m the ledger entry") {
		t.Errorf("warning prefix not yellow:\n%q", errOut)
	}
}

func TestNoEscapeWhenColorIsOff(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	for name, args := range map[string][]string{
		"json":     {"doctor", "--repo", "acme/widget", "--json"},
		"no-color": {"doctor", "--repo", "acme/widget", "--no-color"},
	} {
		_, out, errOut := te.runColor(true, args...)
		if strings.Contains(out+errOut, "\x1b") {
			t.Errorf("%s: ESC in output:\n%q\n%q", name, out, errOut)
		}
	}
	// --no-color even when flag parsing fails before it.
	_, out, errOut := te.runColor(true, "doctor", "--bogus", "--no-color")
	if strings.Contains(out+errOut, "") {
		t.Errorf("no-color usage error: ESC in output: %q %q", out, errOut)
	}
	// --json even when flag parsing fails.
	_, out, errOut = te.runColor(true, "doctor", "--json", "--bogus")
	if strings.Contains(out+errOut, "\x1b") {
		t.Errorf("json usage error: ESC in output: %q %q", out, errOut)
	}
}

func TestNoColorEnvAndDumbTerm(t *testing.T) {
	t.Parallel()
	for _, v := range []map[string]string{{"NO_COLOR": "1"}, {"TERM": "dumb"}} {
		te := newTestEnv(t)
		initOnly(te)
		delete(te.vars, "GHOSTBRANCH_TOKEN")
		for k, x := range v {
			te.vars[k] = x
		}
		_, out, errOut := te.runColor(true, "doctor", "--repo", "acme/widget")
		if strings.Contains(out+errOut, "\x1b") {
			t.Errorf("%v: ESC in output: %q %q", v, out, errOut)
		}
	}
}

func TestColorAllowed(t *testing.T) {
	t.Parallel()
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"plain", nil, true},
		{"xterm", map[string]string{"TERM": "xterm-256color"}, true},
		{"NO_COLOR set", map[string]string{"NO_COLOR": "1"}, false},
		{"NO_COLOR any non-empty", map[string]string{"NO_COLOR": "0"}, false},
		{"NO_COLOR empty", map[string]string{"NO_COLOR": ""}, true},
		{"dumb", map[string]string{"TERM": "dumb"}, false},
	} {
		if got := cli.ColorAllowed(get(c.env)); got != c.want {
			t.Errorf("%s: ColorAllowed = %v, want %v", c.name, got, c.want)
		}
	}
	for v, want := range map[string]bool{"truecolor": true, "24bit": true, "": false, "256": false} {
		if got := cli.TrueColorTerm(get(map[string]string{"COLORTERM": v})); got != want {
			t.Errorf("COLORTERM=%q: %v, want %v", v, got, want)
		}
	}
}

func TestColorIsDecidedPerStream(t *testing.T) {
	t.Parallel()
	te := newTestEnv(t)
	initOnly(te)
	delete(te.vars, "GHOSTBRANCH_TOKEN")
	var out, errb bytes.Buffer
	env := te.env
	env.Stdout, env.Stderr = &out, &errb
	env.StdoutColor, env.StderrColor = true, false
	cli.Run([]string{"doctor", "--repo", "acme/widget"}, env)
	if !strings.Contains(out.String(), "\x1b[91m✗\x1b[0m") {
		t.Errorf("stdout not styled: %q", out.String())
	}
	if strings.Contains(errb.String(), "\x1b") || !strings.Contains(errb.String(), "error: ") {
		t.Errorf("stderr should be plain: %q", errb.String())
	}
}
