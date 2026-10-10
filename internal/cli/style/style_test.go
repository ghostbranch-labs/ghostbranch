package style_test

import (
	"testing"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
)

func TestANSI(t *testing.T) {
	t.Parallel()
	s := style.New(true, false)
	for name, c := range map[string]struct{ got, want string }{
		"ok":   {s.OK("x"), "\x1b[92mx\x1b[0m"},
		"warn": {s.Warn("x"), "\x1b[93mx\x1b[0m"},
		"fail": {s.Fail("x"), "\x1b[91mx\x1b[0m"},
		"dim":  {s.Dim("x"), "\x1b[2mx\x1b[0m"},
		"bold": {s.Bold("x"), "\x1b[1mx\x1b[0m"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
}

func TestTrueColor(t *testing.T) {
	t.Parallel()
	s := style.New(true, true)
	for name, c := range map[string]struct{ got, want string }{
		"ok":   {s.OK("x"), "\x1b[38;2;132;204;22mx\x1b[0m"},
		"warn": {s.Warn("x"), "\x1b[38;2;234;179;8mx\x1b[0m"},
		"fail": {s.Fail("x"), "\x1b[38;2;239;68;68mx\x1b[0m"},
		"dim":  {s.Dim("x"), "\x1b[2mx\x1b[0m"},
		"bold": {s.Bold("x"), "\x1b[1mx\x1b[0m"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", name, c.got, c.want)
		}
	}
}

func TestDisabledPassesThrough(t *testing.T) {
	t.Parallel()
	for _, s := range []style.Styler{style.New(false, false), style.New(false, true), {}} {
		for _, got := range []string{s.OK("x"), s.Warn("x"), s.Fail("x"), s.Dim("x"), s.Bold("x")} {
			if got != "x" {
				t.Errorf("disabled styler returned %q", got)
			}
		}
	}
}
