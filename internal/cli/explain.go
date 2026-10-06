package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
)

// ExplainResult is `ghostbranch explain` output.
type ExplainResult struct {
	Entries []contract.Entry `json:"entries"`
}

func (a *app) explainCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "explain <CODE | exit number>",
		Short: "Explain an error, warning or exit code, and what to do about it",
		Long:  `explain prints what a code means, its usual causes, what to do, and whether a person must act. It needs no token and no network. --all lists every code.`,
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			a.ran = true
			switch {
			case all && len(args) == 0:
				return done(a, &ExplainResult{Entries: contract.Registry()}, nil, nil)
			case !all && len(args) == 1:
				es, err := contract.Explain(args[0])
				if err != nil {
					return err
				}
				return done(a, &ExplainResult{Entries: es}, nil, nil)
			}
			return contract.New(contract.BadFlag, "pass one code or exit number, or --all").WithField("code")
		},
	}
	c.Flags().BoolVar(&all, "all", false, "list every exit, error and warning code")
	return c
}
