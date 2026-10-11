package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func (a *app) featurePullCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "pull <feature>",
		Short: "Bring commits someone made on a feature's open public PR into its shadow branch",
		Long: `feature pull reads the open public PR of <feature> and the commits on its branch that Ghostbranch didn't
publish (someone pushed them on GitHub: a maintainer's fix, an applied suggestion). It brings them into the
feature's branch in the shadow repo and records them as adopted, in one push leased on what it read; the
next gate scans them like any other change, and the next promote keeps them. Your later commits are replayed
on top of them. A merge commit or a conflict stops it with the git steps to merge the PR's branch yourself;
Ghostbranch never resolves conflicts. It only touches the shadow repo; your checkout needs a fetch and reset
afterwards. --plan shows what would change.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			a.noteShadowRemote(p)
			res, w, err := e.FeaturePull(cmd.Context(), p, engine.PullOptions{Feature: args[0]})
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}
