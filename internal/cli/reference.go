package cli

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// ReferenceResult is the hidden `ghostbranch reference` output: the visible
// command tree, for the product site's generated reference pages.
type ReferenceResult struct {
	Commands    []RefCommand `json:"commands"`
	GlobalFlags []RefFlag    `json:"global_flags"`
}

// RefCommand is one runnable command.
type RefCommand struct {
	Path     string    `json:"path"`
	Use      string    `json:"use"`
	Short    string    `json:"short"`
	Long     string    `json:"long"`
	ReadOnly bool      `json:"read_only"`
	Flags    []RefFlag `json:"flags"`
}

// RefFlag is one flag.
type RefFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
}

func (a *app) referenceCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "reference",
		Short:  "Print every command and flag as data, for the product site's reference pages",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			return done(a, buildReference(cmd.Root()), nil, nil)
		},
	}
}

func buildReference(root *cobra.Command) *ReferenceResult {
	r := &ReferenceResult{Commands: []RefCommand{}, GlobalFlags: flagsOf(root.PersistentFlags())}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range c.Commands() {
			if s.Hidden || s.Name() == "help" || s.Name() == "completion" || strings.HasPrefix(s.Name(), "__complete") {
				continue
			}
			if s.Runnable() {
				path := commandName(s)
				r.Commands = append(r.Commands, RefCommand{
					Path: path, Use: s.Use, Short: s.Short, Long: s.Long,
					ReadOnly: readOnlyCommands[path], Flags: flagsOf(s.LocalNonPersistentFlags()),
				})
			}
			walk(s)
		}
	}
	walk(root)
	sort.Slice(r.Commands, func(i, j int) bool { return r.Commands[i].Path < r.Commands[j].Path })
	return r
}

func flagsOf(fs *pflag.FlagSet) []RefFlag {
	out := []RefFlag{}
	fs.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		out = append(out, RefFlag{Name: f.Name, Shorthand: f.Shorthand, Type: f.Value.Type(), Default: f.DefValue, Usage: f.Usage})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
