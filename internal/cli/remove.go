package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func (a *app) removeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "remove",
		Short: "Unregister the pair from this machine (needs a person at a console)",
		Long: `remove takes the pair off this machine: it revokes the pair's agent grants and waivers, deletes
its plan files and its local mirror, and drops it from the workspace. Nothing on GitHub changes: the
shadow repo, its branches and features stay, and the ledger keeps its history. Because it drops grants
and waivers, a person confirms by typing a code at the console; --plan shows what would go.`,
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
			res, w, err := e.Remove(cmd.Context(), p)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) logCmd() *cobra.Command {
	var o engine.LogOptions
	var since string
	c := &cobra.Command{
		Use:   "log",
		Short: "Show the ledger of what ghostbranch did on this machine, newest first",
		Long: `log prints this workspace's ledger: every mutating command, who ran it (the actor), and how it
ended, newest first. It reads only the local ledger: no token, no network, no pair. --feature and --op
match exactly, --actor matches a prefix (agent: shows every agent), and --since takes a duration back
from now (24h) or an RFC 3339 time.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			if o.Limit < 0 {
				return contract.New(contract.BadFlag, "--limit must be 0 or more").WithField("limit")
			}
			var err error
			if o.Since, err = engine.ParseSince(since, a.env.Now()); err != nil {
				return err
			}
			ws, err := a.openWorkspace()
			if err != nil {
				return err
			}
			res, err := engine.Log(ws, o)
			return done(a, res, nil, err)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Feature, "feature", "", "only entries about this feature")
	f.StringVar(&o.Op, "op", "", "only entries for this operation, such as promote or \"feature start\"")
	f.StringVar(&o.ActorPrefix, "actor", "", "only entries whose actor starts with this, such as agent: or human:octo")
	f.StringVar(&since, "since", "", "only entries from this long ago (24h) or since this RFC 3339 time")
	f.IntVar(&o.Limit, "limit", engine.DefaultLogLimit, "at most this many entries")
	return c
}
