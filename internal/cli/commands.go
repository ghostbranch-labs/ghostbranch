package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) root() *cobra.Command {
	root := &cobra.Command{
		Use:   "ghostbranch",
		Short: "Private feature branches for public GitHub repos",
		Long: `Ghostbranch keeps unreleased work in a private shadow repo, runs the public repo's CI on it,
and publishes a feature only through a gated promote: rebase on public main, green CI, and a leak
scan of exactly what will go public.

Every command takes --json for one machine-readable object on stdout. Exit codes:
0 ok, 1 failure, 2 usage, 3 gate failed, 4 conflict, 5 needs human approval, 6 state changed since plan.

Coding agents: run "ghostbranch guide" first.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", a.jsonOut, "print exactly one JSON object on stdout")
	pf.StringVar(&a.repo, "repo", "", "public or shadow repo (owner/name) to act on")
	pf.StringVar(&a.workspace, "workspace", "", `workspace name (default $GHOSTBRANCH_WORKSPACE or "default")`)
	pf.BoolVar(&a.nonInteractive, "non-interactive", false, "never prompt; missing input is an error")
	pf.BoolVar(&a.dryRun, "dry-run", false, "preview exactly what would change, and change nothing")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return contract.New(contract.BadFlag, "%s", err.Error())
	})
	feature := &cobra.Command{Use: "feature", Short: "Work with features (private branches in the shadow repo)"}
	feature.AddCommand(a.featureStartCmd(), a.featureListCmd(), a.featureShowCmd(), a.featureAbandonCmd(), a.featureCloseCmd())
	root.AddCommand(a.initCmd(), a.syncCmd(), a.statusCmd(), a.auditCmd(), a.upgradeCmd(), feature, a.gateCmd(), a.promoteCmd(), a.applyCmd(), a.agentsCmd(), a.waiversCmd(), a.explainCmd(), a.guideCmd(), a.guardCmd(), a.removeCmd(), a.logCmd())
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
			if res != nil {
				a.pairFull = workspace.Pair{Public: res.Public, Shadow: res.Shadow}
			}
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&o.Shadow, "shadow", "", "shadow repo name (default <owner/repo>-shadow)")
	c.Flags().BoolVar(&o.Adopt, "adopt", false, "take over an existing, empty or mirror-only, private repo")
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; init never asks")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) syncCmd() *cobra.Command {
	var yes bool
	var o engine.SyncOptions
	c := &cobra.Command{
		Use:   "sync",
		Short: "Fast-forward shadow main to public main",
		Long: `sync fast-forwards the shadow repo's main to public main. It never force-pushes: if shadow main has diverged it stops with exit 4.
Its pushes start the shadow repo's push-triggered workflows, which re-test what public CI already tested,
so it then cancels those runs (the token needs Actions write on the shadow repo); --no-cancel leaves them running.
Last, it closes each feature whose public PR merged and whose branch hasn't moved since it was published,
as feature close does; --no-close leaves them. It never closes a feature that needs a person.`,
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
			res, w, err := e.Sync(cmd.Context(), p, o)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&o.NoCancel, "no-cancel", false, "don't cancel the workflow runs the sync's pushes start in the shadow repo")
	c.Flags().BoolVar(&o.NoClose, "no-close", false, "don't close features whose public PR merged")
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; sync never asks")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) auditCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "audit",
		Short: "Report what keeps the shadow repo's CI from matching public CI",
		Long: `audit reads the workflows the shadow repo's CI runs and reports repo guards (jobs that skip in
the shadow repo), referenced secrets and which are missing (when the token may list them), workflows a
sync starts in the shadow repo and the guard that stops them, billed Actions minutes, and whether
Ghostbranch's managed overlay files are current. It changes nothing.`,
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
			res, w, err := e.Audit(cmd.Context(), p)
			return done(a, res, w, err)
		},
	}
}

func (a *app) upgradeCmd() *cobra.Command {
	var yes bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Bring the pair's Ghostbranch-managed overlay files up to date",
		Long: `upgrade compares the overlay branch's managed files (today the opt-in sync workflow) with this
version's templates and .ghostbranch.yml, then adds, updates or removes them in one overlay commit.
Turning scheduled sync on or off is: edit sync.schedule, then run upgrade. A file someone edited by hand
is replaced; --plan shows that first. It writes only the shadow repo's overlay branch, then runs the audit.`,
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
			res, w, err := e.Upgrade(cmd.Context(), p)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; upgrade never asks")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
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
			a.startName = args[0]
			res, w, err := e.FeatureStart(cmd.Context(), p, args[0])
			if res != nil && a.env.ShadowRemote != nil {
				remote, free, in := a.env.ShadowRemote(p.Shadow)
				res.SetCheckout(checkoutArgv(res.Name, p.Shadow, remote, free, in))
				a.shadowRemote = pushRemote(remote, free, in)
			}
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; feature start never asks")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) gateCmd() *cobra.Command {
	c := &cobra.Command{
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
			a.noteShadowRemote(p)
			res, w, err := e.Gate(cmd.Context(), p, args[0])
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) promoteCmd() *cobra.Command {
	var o engine.PromoteOptions
	c := &cobra.Command{
		Use:   "promote <feature>",
		Short: "Publish a gated feature to the public repo as a branch and PR",
		Long: `promote reuses a fresh green gate (or runs one), builds the public commits (each feature commit
with --mode preserve, one with squash, or the groups of a --curate file) on public main, ending on
exactly the gated tree, scans every commit's files, message and author, the branch name and PR text
for leaks, and saves a plan.
With --plan it stops there. Otherwise a person confirms by typing a code shown on the console, or, if they have allowed agents with "ghostbranch agents allow", --yes publishes. Without either it stops at the plan with exit 5.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Feature = args[0]
			// Usage problems fail before any network work (`--mode --json`
			// parses "--json" as the mode).
			if err := engine.ValidatePromoteOptions(&o); err != nil {
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
			a.noteShadowRemote(p)
			res, w, err := e.Promote(cmd.Context(), p, o)
			return done(a, res, w, err)
		},
	}
	f := c.Flags()
	f.StringVar(&o.Mode, "mode", "", "history mode: preserve (each commit), squash (one commit) or curated (a --curate file); default promote.default_mode, which is preserve unless set")
	f.StringVarP(&o.Message, "message", "m", "", "the squash commit's message (squash mode only)")
	f.StringVar(&o.Curate, "curate", "", `curated mode: a JSON file {"commits": [{"from": [<sha>...], "message": "..."}]} naming every feature commit once, in the order to publish`)
	f.StringVar(&o.Title, "title", "", "PR title (needed when more than one commit is published; default: the only commit's subject, or the squash message's first line)")
	f.StringVar(&o.Body, "body", "", "PR body (default: the commit's body, or one line per commit)")
	f.StringVar(&o.Branch, "branch", "", "public branch name (default: the feature name)")
	f.BoolVar(&o.PlanOnly, "plan", false, "save and print the plan without publishing")
	f.BoolVar(&o.Yes, "yes", false, "publish under an agent grant (ghostbranch agents allow); on its own it never skips the console")
	return c
}

func (a *app) applyCmd() *cobra.Command {
	var o engine.ApplyOptions
	c := &cobra.Command{
		Use:   "apply <plan-id|plan.json>",
		Short: "Publish a saved plan if the repos still match it",
		Long:  `apply publishes a plan saved by promote. It refuses with exit 6 if public main, the feature, the overlay or the gate candidate moved since the plan was made. Re-running it finishes an interrupted publish. A person confirms on the console, or --yes publishes under an agent grant.`,
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
	c.Flags().BoolVar(&o.Yes, "yes", false, "publish under an agent grant (ghostbranch agents allow); on its own it never skips the console")
	return c
}
