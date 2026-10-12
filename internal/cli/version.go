package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// stampedVersion is set at link time for release builds (R10):
//
//	go build -ldflags "-X github.com/ghostbranch-labs/ghostbranch/internal/cli.stampedVersion=v0.13.0" ./cmd/ghostbranch
//
// It wins over the module version Go records. 6.4b's release builds set it
// the same way. Empty, the version is Go's: (devel) or a pseudo-version, and
// such a build never looks for updates.
var stampedVersion string

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
	v, c := a.build()
	return done(a, &VersionResult{Version: v, Commit: c, SchemaVersion: contract.SchemaVersion}, nil, nil)
}
