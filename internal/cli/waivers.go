package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) waiversCmd() *cobra.Command {
	c := &cobra.Command{Use: "waivers", Short: "Waive leak findings a person judges false positives, or list and revoke waivers"}
	c.AddCommand(a.waiversAddCmd(), a.waiversListCmd(), a.waiversRevokeCmd())
	return c
}

func (a *app) waiversAddCmd() *cobra.Command {
	var o engine.WaiversAddOptions
	c := &cobra.Command{
		Use:   "add <feature> <id>...",
		Short: "Waive leak findings that are false positives (needs a person at a console)",
		Long: `waivers add lets a publish go ahead despite leak findings a person has checked and judged safe,
such as a fake key in a test fixture. It rebases, strips and scans the feature again on this machine
(no push, no CI), shows each finding at the console, and needs a person to type a code: an agent
cannot waive. A waiver covers exactly that content in that file; changed content is a new finding.
Findings in the commit message, branch or PR text are never waivable: change the text instead.
Waivers live on this machine only.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Feature, o.IDs = args[0], args[1:]
			if _, _, err := engine.ValidateWaiversAdd(o); err != nil {
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
			res, w, err := e.WaiversAdd(cmd.Context(), p, o)
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&o.Reason, "reason", "", "why these findings are safe to publish (recorded in the ledger)")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "show what would be waived without waiving it")
	return c
}

func (a *app) waiversListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List waivers for this repo (or every repo when none can be told apart)",
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
			case ce.Code != contract.PairAmbiguous || a.repo != "":
				return err
			}
			res, w, err := e.WaiversList(cmd.Context(), pp)
			return done(a, res, w, err)
		},
	}
}

func (a *app) waiversRevokeCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "revoke <id>... | --all",
		Short: "Revoke waivers for this repo, or every waiver with --all; needs no console",
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			var pp *workspace.Pair
			if !all {
				p, err := a.pair(e)
				if err != nil {
					return err
				}
				pp = &p
			}
			res, w, err := e.WaiversRevoke(cmd.Context(), pp, args, all)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&all, "all", false, "revoke every waiver in this workspace (also clears an unreadable waivers.json)")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview what would be revoked")
	return c
}
