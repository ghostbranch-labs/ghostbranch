package cli

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

var ansiCodes = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// Colour never changes the text: one failing doctor result, with its
// error and next, rendered once plain and once in colour, reads the same
// once the colour codes are stripped. It renders one result twice rather
// than comparing two doctor runs, whose output can differ (the 6.4b CI
// flake).
func TestColourNeverChangesTheText(t *testing.T) {
	t.Parallel()
	res := &engine.DoctorResult{Checks: []engine.DoctorCheck{
		{ID: "tools", Status: engine.DoctorOK, Message: "git 2.51.0, ghostbranch (devel)"},
		{ID: "token", Status: engine.DoctorFail, Message: "no GitHub token", Fix: "ghostbranch login", FixArgv: []string{"ghostbranch", "login"}},
		{ID: "guard", Status: engine.DoctorWarn, Message: "the guard is not installed in this checkout", Fix: "ghostbranch guard install"},
		{ID: "ci", Status: engine.DoctorSkipped, Message: "needs a token"},
	}}
	render := func(colour bool) (string, string) {
		var out, errOut strings.Builder
		a := &app{command: "doctor", result: res, env: Env{Stdout: &out, Stderr: &errOut,
			StdoutColor: colour, StderrColor: colour, Getenv: func(string) string { return "" }}}
		a.render(contract.New(contract.DoctorFailed, "1 check failed: token").WithDetails(map[string]any{"failed": []string{"token"}}))
		return out.String(), errOut.String()
	}
	plainOut, plainErr := render(false)
	out, errOut := render(true)
	if !strings.Contains(out, "\x1b[91m✗\x1b[0m") || !strings.Contains(errOut, "\x1b[1mNext:\x1b[0m") {
		t.Fatalf("the colour run isn't coloured:\n%q\n%q", out, errOut)
	}
	if strings.Contains(plainOut+plainErr, "\x1b") {
		t.Fatalf("the plain run has colour:\n%q\n%q", plainOut, plainErr)
	}
	if got := ansiCodes.ReplaceAllString(out, ""); got != plainOut {
		t.Errorf("colour changed stdout:\n%q\nvs\n%q", got, plainOut)
	}
	if got := ansiCodes.ReplaceAllString(errOut, ""); got != plainErr {
		t.Errorf("colour changed stderr:\n%q\nvs\n%q", got, plainErr)
	}
}
