package cli

import (
	"github.com/spf13/cobra"
)

func (a *app) statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Pair health and every feature's state",
		Long: `status reports whether shadow main is behind public main, whether the overlay's managed files
need an upgrade, and whether scheduled sync is on; then every feature, abandoned ones included, with its
state, whether it still merges cleanly onto public main (drift), its last gate and its public PR. It only
reads: nothing is pushed and nothing is written to the ledger.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			res, w, err := e.Status(cmd.Context(), p)
			return done(a, res, w, err)
		},
	}
}

func (a *app) featureListCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "list",
		Short: "List the pair's features with their state, drift, gate and public PR",
		Long: `feature list prints one line per feature: its state, whether it still merges cleanly onto public
main (drift), its last gate and its public PR. Abandoned features are left out unless --all. It only reads.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			res, w, err := e.FeatureList(cmd.Context(), p, all)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include abandoned features")
	return c
}

func (a *app) featureShowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "Show one feature in full: state, drift, gate, PR, commits and ledger",
		Long: `feature show prints everything about one feature: its state, branch and base, drift onto public
main and the files that conflict, its last gate, its public PR and plan, its commits since public main,
and this pair's ledger entries for it. It only reads.`,
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
			res, w, err := e.FeatureShow(cmd.Context(), p, args[0])
			return done(a, res, w, err)
		},
	}
}
