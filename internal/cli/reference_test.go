package cli

import (
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// The reference lists exactly the visible runnable commands, sorted, with
// their own flags; hidden commands (reference itself) are left out.
func TestReferenceListsEveryVisibleCommand(t *testing.T) {
	t.Parallel()
	a := &app{}
	root := a.root()
	ref := buildReference(root)

	var want []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if s.Hidden || s.Name() == "help" || s.Name() == "completion" || strings.HasPrefix(s.Name(), "__complete") {
				continue
			}
			if s.Runnable() {
				want = append(want, commandName(s))
			}
			walk(s)
		}
	}
	walk(root)
	sort.Strings(want)

	var got []string
	for _, c := range ref.Commands {
		got = append(got, c.Path)
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("paths:\n got %v\nwant %v", got, want)
	}
	for _, c := range ref.Commands {
		if c.Path == "reference" {
			t.Fatal("the hidden reference command is listed")
		}
	}
}

func TestReferenceFlagsAndReadOnly(t *testing.T) {
	t.Parallel()
	ref := buildReference((&app{}).root())
	byPath := map[string]RefCommand{}
	for _, c := range ref.Commands {
		byPath[c.Path] = c
	}
	gate, ok := byPath["gate"]
	if !ok {
		t.Fatal("gate missing")
	}
	if gate.ReadOnly {
		t.Error("gate is read-only")
	}
	names := map[string]bool{}
	for _, f := range gate.Flags {
		names[f.Name] = true
		if f.Name == "help" || f.Name == "json" {
			t.Errorf("gate lists %q, a help or global flag", f.Name)
		}
	}
	if !names["detach"] {
		t.Errorf("gate flags %v lack detach", gate.Flags)
	}
	if !byPath["status"].ReadOnly {
		t.Error("status is not read-only")
	}
	globals := map[string]bool{}
	for _, f := range ref.GlobalFlags {
		globals[f.Name] = true
	}
	for _, g := range []string{"json", "repo", "dry-run", "non-interactive"} {
		if !globals[g] {
			t.Errorf("global flag %s missing", g)
		}
	}
}
