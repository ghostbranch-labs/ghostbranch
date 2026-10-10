package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func (a *app) featureFoldCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "fold <feature>",
		Short: "Fold a stacked feature into its parent: the parent fast-forwards to it, and it closes",
		Long: `feature fold moves the parent feature's branch to <feature>'s head in the shadow repo, a fast-forward
(never a merge or a rebase), and closes <feature>. Its branch, gate branch, metadata and plan refs are deleted
in the same atomic push, leased on what fold read, and the features stacked on <feature> are stacked on the
parent instead. Nothing is lost: every commit of <feature> is on the parent, so no console is asked.
If the parent has moved on since <feature> was started from it, fold refuses with FOLD_NEEDS_REBASE, and next
gives the git commands to rebase <feature> yourself. Ghostbranch never resolves conflicts. --plan shows what
would change.`,
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
			res, w, err := e.FeatureFold(cmd.Context(), p, engine.FoldOptions{Feature: args[0]})
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) featureRestackCmd() *cobra.Command {
	var onto string
	c := &cobra.Command{
		Use:   "restack <feature>",
		Short: "Rebase the features stacked above <feature> onto its tip, in the shadow repo",
		Long: `feature restack replays each feature stacked above <feature> (then the ones above those, in order)
onto its parent's new tip, in a throwaway worktree, and pushes each with a lease, together with its updated
record. A feature that already holds its parent's tip only has its record updated. The first conflict stops
the restack with the git commands to rebase that feature yourself; features already restacked stay
restacked. Ghostbranch never resolves conflicts.
With --onto main or --onto <feature>, <feature> itself moves first: its own commits are replayed onto main
(dropping its parent's commits) or onto that feature, and then the features above it follow.
It only touches the shadow repo; your checkouts of the rewritten branches need a fetch and reset afterwards.`,
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
			res, w, err := e.FeatureRestack(cmd.Context(), p, engine.RestackOptions{Feature: args[0], Onto: onto})
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&onto, "onto", "", `move <feature> itself onto "main" or onto another feature first`)
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}
