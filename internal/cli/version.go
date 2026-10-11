package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// VersionResult is version's data.
type VersionResult struct {
	Version       string `json:"version"`
	Commit        string `json:"commit"`
	SchemaVersion string `json:"schema_version"`
}

func (a *app) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print this binary's version",
		Long:  `version prints this binary's module version and VCS commit, and the JSON contract version it speaks. ghostbranch --version does the same.`,
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return a.printVersion() },
	}
}

// printVersion is version and --version.
func (a *app) printVersion() error {
	a.ran = true
	build := a.env.Build
	if build == nil {
		build = func() (string, string) { return "(unknown)", "(unknown)" }
	}
	v, c := build()
	return done(a, &VersionResult{Version: v, Commit: c, SchemaVersion: contract.SchemaVersion}, nil, nil)
}
