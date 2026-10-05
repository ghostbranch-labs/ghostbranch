package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
)

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "ghostbranch",
		Short: "Private feature branches for public GitHub repos",
		Long: `Ghostbranch keeps unreleased work in a private shadow repo, runs the public repo's CI on it,
and publishes a feature only through a gated promote: rebase on public main, green CI, and a leak
scan of exactly what will go public.

Every command takes --json for one machine-readable object on stdout. Exit codes:
0 ok, 1 failure, 2 usage, 3 gate failed, 4 conflict, 5 needs human approval, 6 state changed since plan.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", a.jsonOut, "print exactly one JSON object on stdout")
	pf.StringVar(&a.repo, "repo", "", "public or shadow repo (owner/name) to act on")
	pf.StringVar(&a.workspace, "workspace", "", `workspace name (default $GHOSTBRANCH_WORKSPACE or "default")`)
	pf.BoolVar(&a.nonInteractive, "non-interactive", false, "never prompt; missing input is an error")
	pf.BoolVar(&a.dryRun, "dry-run", false, "show what would happen without writing to any repo")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return contract.New(contract.BadFlag, "%s", err.Error())
	})
	feature := &cobra.Command{Use: "feature", Short: "Work with features (private branches in the shadow repo)"}
	feature.AddCommand(a.featureStartCmd())
	root.AddCommand(a.initCmd(), a.syncCmd(), feature, a.gateCmd(), a.promoteCmd(), a.applyCmd())
	return root
}

func (a *app) initCmd() *cobra.Command {
	var o engine.InitOptions
	var yes bool
	c := &cobra.Command{
		Use:   "init <owner/repo>",
		Short: "Create or adopt the private shadow repo for a public repo",
		Long: `init creates <owner/repo>-shadow as a private repo (or adopts an existing one with --adopt),
mirrors the public repo's branches and tags into it, and creates the "ghostbranch" overlay branch,
which holds .ghostbranch.yml and is the shadow repo's default branch. It never writes to the public
repo. Running it again resumes a partial setup.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			o.Public = args[0]
			res, w, err := e.Init(cmd.Context(), o)
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&o.Shadow, "shadow", "", "shadow repo name (default <owner/repo>-shadow)")
	c.Flags().BoolVar(&o.Adopt, "adopt", false, "take over an existing, empty or mirror-only, private repo")
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; init never asks")
	return c
}

func (a *app) syncCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "sync",
		Short: "Fast-forward shadow main to public main",
		Long:  `sync fast-forwards the shadow repo's main to public main. It never force-pushes: if shadow main has diverged it stops with exit 4.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			res, w, err := e.Sync(cmd.Context(), p)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; sync never asks")
	return c
}

func (a *app) featureStartCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a feature branch in the shadow repo",
		Long:  `feature start creates <name> from shadow main in the shadow repo and records it as a feature. It does not touch your checkout; fetch and switch to the branch afterwards.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			res, w, err := e.FeatureStart(cmd.Context(), p, args[0])
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; feature start never asks")
	return c
}

func (a *app) gateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "gate <feature>",
		Short: "Rebase on public main, scan for leaks, and run CI; report only",
		Long: `gate rebases the feature onto public main, scans exactly what would be published for leaks,
pushes a CI candidate (the feature plus the overlay) to a draft PR in the shadow repo, and waits for
every check. It records the result and never touches the public repo. Exit 3 means the gate failed.`,
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
			res, w, err := e.Gate(cmd.Context(), p, args[0])
			return done(a, res, w, err)
		},
	}
}

func (a *app) promoteCmd() *cobra.Command {
	var o engine.PromoteOptions
	c := &cobra.Command{
		Use:   "promote <feature>",
		Short: "Publish a gated feature to the public repo as a branch and PR",
		Long: `promote reuses a fresh green gate (or runs one), builds one squash commit on public main with
exactly the gated tree, scans it and its message, branch name and PR text for leaks, and saves a plan.
With --plan it stops there. Otherwise it asks for confirmation (or takes --yes) and publishes.
Agents get exit 5 when the plan needs a human; a person then runs ghostbranch apply <plan> --yes.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// A flag value is a usage matter; reject it before touching the network.
			// (`--mode --json` parses "--json" as the mode.)
			if o.Mode != "squash" {
				return contract.New(contract.BadFlag, "--mode %s is not supported in this version; use squash", o.Mode).WithField("mode")
			}
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			o.Feature = args[0]
			res, w, err := e.Promote(cmd.Context(), p, o)
			return done(a, res, w, err)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Mode, "mode", "squash", "history mode; this version supports squash")
	f.StringVarP(&o.Message, "message", "m", "", "commit message for the public squash commit")
	f.StringVar(&o.Title, "title", "", "PR title (default: the message's first line)")
	f.StringVar(&o.Body, "body", "", "PR body (default: the rest of the message)")
	f.StringVar(&o.Branch, "branch", "", "public branch name (default: the feature name)")
	f.BoolVar(&o.PlanOnly, "plan", false, "save and print the plan without publishing")
	f.BoolVar(&o.Yes, "yes", false, "publish without asking (agents still need a human when require_human is set)")
	return c
}

func (a *app) applyCmd() *cobra.Command {
	var o engine.ApplyOptions
	c := &cobra.Command{
		Use:   "apply <plan-id|plan.json>",
		Short: "Publish a saved plan if the repos still match it",
		Long:  `apply publishes a plan saved by promote. It refuses with exit 6 if public main, the feature, the overlay or the gate candidate moved since the plan was made. Re-running it finishes an interrupted publish.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			o.Plan = args[0]
			res, w, err := e.Apply(cmd.Context(), o)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&o.Yes, "yes", false, "publish without asking")
	return c
}
