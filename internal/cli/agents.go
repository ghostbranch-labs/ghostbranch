package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) agentsCmd() *cobra.Command {
	c := &cobra.Command{Use: "agents", Short: "Let coding agents publish to a repo from this machine, or take that back"}
	c.AddCommand(a.agentsAllowCmd(), a.agentsRevokeCmd(), a.agentsStatusCmd(), a.agentsTokenCmd())
	return c
}

func (a *app) agentsAllowCmd() *cobra.Command {
	var o engine.AgentsAllowOptions
	c := &cobra.Command{
		Use:   "allow",
		Short: "Let agents publish to this repo with --yes, for a while (needs a person at a console)",
		Long: `agents allow lets coding agents on this machine publish to one repo pair with --yes, without the
console code, for 1h, 6h (the default), 1d, 7d, 15d, 30d or 90d, or --until-revoked. Making the grant itself needs a
person to type a code at a console, so an agent cannot grant itself. Agents still need a green gate
and a clean leak scan for every publish. Grants live on this machine only.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := grant.ParseFor(o.For, o.UntilRevoked); err != nil {
				a.ran = true
				return err
			}
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			res, w, err := e.AgentsAllow(cmd.Context(), p, o)
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&o.For, "for", "", "how long: 1h, 6h (default), 1d, 7d, 15d, 30d or 90d")
	c.Flags().BoolVar(&o.UntilRevoked, "until-revoked", false, "never expire; take it back with ghostbranch agents revoke")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview the grant without making it")
	return c
}

func (a *app) agentsRevokeCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "revoke",
		Short: "Take back the agent grant for this repo (or --all); needs no console",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			// --all with --repo revokes every grant for that repo only.
			var pp *workspace.Pair
			if !all || a.repo != "" {
				p, err := a.pair(e)
				if err != nil {
					return err
				}
				pp = &p
			}
			res, w, err := e.AgentsRevoke(cmd.Context(), pp, all && pp == nil)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "revoke every grant in this workspace (also clears a grants.json that isn't valid JSON; a file from a newer Ghostbranch is kept); with --repo, every grant for that repo")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview what would be revoked")
	return c
}

func (a *app) agentsStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "List agent grants, and whether the current token can publish to the public repo",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			var pp *workspace.Pair
			p, err := a.pair(e)
			switch ce := contract.From(err); {
			case err == nil:
				pp = &p
			case (ce.Code != contract.PairAmbiguous && ce.Code != contract.PairNotFound) || a.repo != "":
				return err
			}
			res, w, err := e.AgentsStatus(cmd.Context(), pp)
			return done(a, res, w, err)
		},
	}
}
