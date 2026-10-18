package cli

import (
	"encoding/json"
	"strings"
	"time"

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

Every command takes --json for machine-readable output on stdout: one JSON object, except wait --json,
which streams NDJSON (one event per line, the result envelope last). Exit codes:
0 ok, 1 failure, 2 usage, 3 gate failed, 4 conflict, 5 needs human approval, 6 state changed since plan.

Coding agents: run "ghostbranch guide" first.`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&a.jsonOut, "json", a.jsonOut, "print JSON on stdout: one object (wait streams one event per line, the result last)")
	pf.BoolVar(&a.noColor, "no-color", a.noColor, "never colour human output (NO_COLOR does the same)")
	pf.StringVar(&a.repo, "repo", "", "public or shadow repo (owner/name) to act on")
	pf.StringVar(&a.workspace, "workspace", "", `workspace name (default $GHOSTBRANCH_WORKSPACE or "default")`)
	pf.BoolVar(&a.nonInteractive, "non-interactive", false, "never prompt; missing input is an error")
	pf.BoolVar(&a.dryRun, "dry-run", false, "preview exactly what would change, and change nothing")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return contract.New(contract.BadFlag, "%s", err.Error())
	})
	feature := &cobra.Command{Use: "feature", Short: "Work with features (private branches in the shadow repo)"}
	feature.AddCommand(a.featureStartCmd(), a.featureListCmd(), a.featureShowCmd(), a.featureAbandonCmd(), a.featureCloseCmd(), a.featureFoldCmd(), a.featureRestackCmd(), a.featurePullCmd(), a.featureArchiveCmd())
	root.AddCommand(a.initCmd(), a.syncCmd(), a.statusCmd(), a.auditCmd(), a.doctorCmd(), a.upgradeCmd(), feature, a.gateCmd(), a.waitCmd(), a.promoteCmd(), a.applyCmd(), a.agentsCmd(), a.waiversCmd(), a.explainCmd(), a.guideCmd(), a.guardCmd(), a.removeCmd(), a.logCmd(), a.loginCmd(), a.logoutCmd(), a.authCmd(), a.versionCmd(), a.referenceCmd())
	var showVersion bool
	root.Flags().BoolVar(&showVersion, "version", false, "print this binary's version (as ghostbranch version does)")
	// Spinner only for read-only commands and dry runs (R16), and the update
	// look started in the background (R12 says where it isn't). Flags are
	// parsed by now, so a.dryRun and showVersion are set. No subcommand
	// defines its own PersistentPreRun*, so this runs for every command.
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if c, ok := a.reporter().(*checklist); ok {
			c.setQuiet(quietProgress(commandName(cmd), a.dryRun))
		}
		if !skipsUpdateCheck(cmd, showVersion) {
			a.startUpdateCheck(cmd.Context())
		}
		return nil
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error {
		if !showVersion {
			return cmd.Help()
		}
		a.versionFlag = true
		return a.printVersion()
	}
	return root
}

func (a *app) initCmd() *cobra.Command {
	var f initFlags
	var guardOn, guardOff, syncOn, syncOff bool
	c := &cobra.Command{
		Use:   "init [<owner/repo>]",
		Short: "Create or adopt the private shadow repo for a public repo",
		Long: `init creates <owner/repo>-shadow as a private repo (or adopts an existing one with --adopt),
mirrors the public repo's branches and tags into it, and creates the "ghostbranch" overlay branch,
which holds .ghostbranch.yml and is the shadow repo's default branch. It never writes to the public
repo. Running it again resumes a partial setup.
Run by a person at a terminal with an answer missing, it asks: the public repo (from origin), a
login if there is none, the shadow repo, the push guard (in a clone of the public repo) and
scheduled sync. It shows the --plan preview, then runs init, guard install and doctor, and ends
with how to start a first feature. Flags answer the questions; --json, --non-interactive and --yes
never ask.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			var err error
			if f.guard, err = onOff("guard", guardOn, guardOff); err != nil {
				a.ran = true
				return err
			}
			if f.sync, err = onOff("sync", syncOn, syncOff); err != nil {
				a.ran = true
				return err
			}
			if a.wantsGuide(args, f) {
				return a.guidedInit(cmd.Context(), args, f)
			}
			if len(args) == 0 {
				a.ran = true
				return contract.New(contract.BadFlag, "init needs the public repo: ghostbranch init <owner/repo>").WithField("public")
			}
			return a.plainInit(cmd.Context(), args[0], f)
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.shadow, "shadow", "", "shadow repo name (default <owner/repo>-shadow)")
	fl.BoolVar(&f.adopt, "adopt", false, "take over an existing, empty or mirror-only, private repo")
	fl.BoolVar(&f.yes, "yes", false, "never ask: use the flags given and the defaults")
	fl.BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	fl.BoolVar(&guardOn, "guard", false, "install the push guard in this checkout, a clone of the public repo, after init")
	fl.BoolVar(&guardOff, "no-guard", false, "don't install the push guard, and don't ask")
	fl.BoolVar(&syncOn, "sync", false, `turn on scheduled sync in the new overlay (sync.schedule "23 */6 * * *", about 120 Actions minutes a month)`)
	fl.BoolVar(&syncOff, "no-sync", false, "leave scheduled sync off, and don't ask")
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
	var yes, noCheckout bool
	var from string
	c := &cobra.Command{
		Use:   "start <name>",
		Short: "Start a feature branch in the shadow repo",
		Long: `feature start creates <name> from shadow main in the shadow repo and records it as a feature. With --from <feature> it starts at that feature's tip instead and records it as the parent, stacking the new feature on it.
Run in a checkout of the public repo, it then adds a remote named shadow for the shadow repo (if no remote reaches it), fetches it, and switches to <name>, tracking shadow/<name>. It never rewrites a remote: a shadow remote that points elsewhere is left alone. With uncommitted changes, or a local branch called <name>, it fetches but doesn't switch. Elsewhere, or with --no-checkout, it prints the commands instead.`,
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
			a.startName = args[0]
			a.startFrom = from
			res, w, err := e.FeatureStart(cmd.Context(), p, args[0], engine.FeatureStartOptions{From: from})
			switch {
			case res != nil && err == nil && res.Preview == nil:
				a.checkOutFeature(cmd.Context(), p, res, noCheckout)
			case res != nil && a.env.ShadowRemote != nil:
				remote, free, in := a.env.ShadowRemote(p.Shadow)
				res.SetCheckout(checkoutArgv(res.Name, p.Shadow, remote, free, in))
				a.shadowRemote = pushRemote(remote, free, in)
			}
			return done(a, res, w, err)
		},
	}
	c.Flags().StringVar(&from, "from", "", "stack the feature on another feature: start it at that feature's tip in the shadow repo (default: shadow main)")
	c.Flags().BoolVar(&noCheckout, "no-checkout", false, "leave this checkout alone: don't add the shadow remote, fetch or switch; print the commands instead")
	c.Flags().BoolVar(&yes, "yes", false, "accepted for consistency; feature start never asks")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	return c
}

func (a *app) gateCmd() *cobra.Command {
	var detach bool
	c := &cobra.Command{
		Use:   "gate <feature>",
		Short: "Rebase on public main, scan for leaks, and run CI; report only",
		Long: `gate rebases the feature onto public main, scans exactly what would be published for leaks,
pushes a CI candidate (the feature plus the overlay) to a draft PR in the shadow repo, and waits for
every check. It records the result and never touches the public repo. Exit 3 means the gate failed.
With --detach it returns a job id once CI has started; ghostbranch wait <job> finishes it, from this machine or another. Without --detach, Ctrl-C leaves the job pending for wait.`,
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
			gate := e.Gate
			if detach {
				gate = e.GateDetached
			}
			res, w, err := gate(cmd.Context(), p, args[0])
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "preview exactly what would change, and change nothing (same as --dry-run)")
	c.Flags().BoolVar(&detach, "detach", false, "push the candidate, record the job and return without waiting for CI; finish it with ghostbranch wait <job>")
	return c
}

func (a *app) waitCmd() *cobra.Command {
	var timeout time.Duration
	c := &cobra.Command{
		Use:   "wait <job>",
		Short: "Wait for a gate job's CI and record its result",
		Long:  `wait finishes a gate started with --detach (or interrupted): it waits for the candidate's CI, records the result, and prints what gate would have. A finished job answers at once. With --json it streams one JSON event per line on stdout, and the last line is the result envelope.`,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if a.dryRun {
				// It would wait on CI and then be refused the write.
				a.ran = true
				return contract.New(contract.BadFlag, "wait has no dry run: it only waits on CI and records the result; drop --dry-run").WithField("dry-run")
			}
			if timeout < 0 {
				a.ran = true
				return contract.New(contract.BadFlag, "--timeout must be 0 (wait until CI settles) or more, not %s", timeout).WithField("timeout")
			}
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			p, err := a.pair(e)
			if err != nil {
				return err
			}
			var events func(engine.JobEvent)
			if a.jsonOut {
				enc := json.NewEncoder(a.env.Stdout)
				enc.SetEscapeHTML(false)
				events = func(ev engine.JobEvent) { _ = enc.Encode(ev) }
			}
			res, w, err := e.Wait(cmd.Context(), p, args[0], engine.WaitOptions{Timeout: timeout, Events: events})
			return done(a, res, w, err)
		},
	}
	c.Flags().DurationVar(&timeout, "timeout", 0, "stop waiting after this long and report pending; the job keeps running (0, the default, waits until CI settles)")
	return c
}

func (a *app) promoteCmd() *cobra.Command {
	var o engine.PromoteOptions
	var modeFor, titleFor []string
	var keep bool
	c := &cobra.Command{
		Use:   "promote <feature>...",
		Short: "Publish a gated feature, or a stack of them, to the public repo as PRs",
		Long: `promote reuses a fresh green gate (or runs one), builds the public commits (each feature commit
with --mode preserve, one with squash, or the groups of a --curate file) on public main, ending on
exactly the gated tree, scans every commit's files, message and author, the branch name and PR text
for leaks, and saves a plan.
With --plan it stops there. Otherwise a person confirms by typing a code shown on the console, or, if they have allowed agents with "ghostbranch agents allow", --yes publishes. Without either it stops at the plan with exit 5.
Several features, bottom first (promote a b c), publish a stack: each branch's PR is based on the
branch below (a's on main) and shows only that branch's commits, and one approval covers every PR.
Each branch needs a fresh green gate built on the branch below. Where GitHub offers native stacked
PRs, they are registered as one stack. --mode-for and --title-for set one branch's mode and PR title.
After publishing, a person at the console picks Archive (the default: the shadow branch is archived and the PR is fixed by pushing to it) or Keep; with --yes, Archive applies unless --keep.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			o.Features, o.Feature = args, args[0]
			var err error
			if o.Modes, err = keyValues("mode-for", modeFor); err != nil {
				a.ran = true
				return err
			}
			if o.Titles, err = keyValues("title-for", titleFor); err != nil {
				a.ran = true
				return err
			}
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
			o.Archive = !keep
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
	f.BoolVar(&o.Detach, "detach", false, "if a gate is needed, start it and return its job instead of waiting")
	f.StringArrayVar(&modeFor, "mode-for", nil, "one branch's history mode in a stack promote: <feature>=squash or <feature>=preserve (repeatable); default --mode")
	f.StringArrayVar(&titleFor, "title-for", nil, "one branch's PR title in a stack promote: <feature>=<title> (repeatable)")
	f.BoolVar(&keep, "keep", false, "after publishing, keep the feature's shadow branch: fixes go through gate and promote (default: archive it, and fix the PR by pushing to its branch)")
	return c
}

func (a *app) applyCmd() *cobra.Command {
	var o engine.ApplyOptions
	var keep bool
	c := &cobra.Command{
		Use:   "apply <plan-id|plan.json>",
		Short: "Publish a saved plan if the repos still match it",
		Long: `apply publishes a plan saved by promote. It refuses with exit 6 if public main, the feature, the overlay or the gate candidate moved since the plan was made. Re-running it finishes an interrupted publish. A person confirms on the console, or --yes publishes under an agent grant.
After publishing, a person at the console picks Archive (the default: the shadow branch is archived and the PR is fixed by pushing to it) or Keep; with --yes, Archive applies unless --keep.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := a.engine(cmd.Context())
			if err != nil {
				return err
			}
			o.Plan = args[0]
			// A plan file another machine made isn't here: apply then looks
			// for its ref in this pair's shadow repo. Resolve directly, not
			// through a.pair, so apply's other next steps don't change.
			// A --repo that was passed but isn't set up is why nothing could
			// be looked up: apply says that, with its init steps (review M3).
			if p, err := e.WS.Resolve(a.repo, a.env.CwdRemotes()); err == nil {
				o.Lookup = &p
			} else if a.repo != "" {
				o.LookupErr = err
			}
			o.Archive = !keep
			res, w, err := e.Apply(cmd.Context(), o)
			return done(a, res, w, err)
		},
	}
	c.Flags().BoolVar(&o.Yes, "yes", false, "publish under an agent grant (ghostbranch agents allow); on its own it never skips the console")
	c.Flags().BoolVar(&keep, "keep", false, "after publishing, keep the feature's shadow branch: fixes go through gate and promote (default: archive it, and fix the PR by pushing to its branch)")
	return c
}

// keyValues reads repeated --<flag> <feature>=<value> flags. A value without
// "=" or a feature named twice is BAD_FLAG.
func keyValues(flag string, vals []string) (map[string]string, error) {
	out := map[string]string{}
	for _, v := range vals {
		k, val, ok := strings.Cut(v, "=")
		if !ok || k == "" {
			return nil, contract.New(contract.BadFlag, "--%s %q: use <feature>=<value>", flag, v).WithField(flag)
		}
		if _, dup := out[k]; dup {
			return nil, contract.New(contract.BadFlag, "--%s names %s twice", flag, k).WithField(flag)
		}
		out[k] = val
	}
	return out, nil
}
