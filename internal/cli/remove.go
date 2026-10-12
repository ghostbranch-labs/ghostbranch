package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) removeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "remove",
		Short: "Unregister the pair from this machine (needs a person at a console)",
		Long: `remove takes the pair off this machine: it revokes the pair's agent grants and waivers, deletes
its plan files and its local mirror, and drops it from the workspace. Nothing on GitHub changes: the
shadow repo, its branches and features stay, and the ledger keeps its history. Because it drops grants
and waivers, a person confirms by typing a code at the console; --plan shows what would go. It needs
no GitHub token.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := a.localEngine()
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

// localEngine builds the engine for remove, which touches nothing on GitHub:
// no token, no GitHub client, and so no GitHub login for the ledger's actor.
// Without GHOSTBRANCH_ACTOR the actor is human:unknown, with ACTOR_UNKNOWN
// unless nothing is recorded (--plan).
func (a *app) localEngine() (*engine.Engine, error) {
	a.ran = true
	actor, err := a.actor()
	if err != nil {
		return nil, err
	}
	ws, err := a.openWorkspace()
	if err != nil {
		return nil, err
	}
	if actor == "" {
		actor = "human:unknown"
		if !a.dryRun {
			a.warnings = append(a.warnings, contract.Warning{Code: contract.ActorUnknown,
				Message: "GHOSTBRANCH_ACTOR is unset, and remove asks GitHub nothing, so the ledger records human:unknown; set GHOSTBRANCH_ACTOR, for example human:<your login>"})
		}
	}
	e := &engine.Engine{WS: ws, Actor: actor, Now: a.env.Now, Sleep: a.env.Sleep, Rand: a.env.Rand, Progress: a.reporter(),
		DryRun: a.dryRun, OnPush: a.env.OnPush,
		Grants:  grant.NewStore(ws.GrantsPath(), ws.GrantsLockPath()),
		Waivers: waiver.NewStore(ws.WaiversPath(), ws.WaiversLockPath())}
	if a.env.ConsoleUsable && !a.nonInteractive && a.env.OpenConsole != nil {
		e.Approver = pausingApprover{inner: &console.Challenger{Open: a.env.OpenConsole, Rand: a.env.Rand}, p: a.reporter()}
	}
	a.eng = e
	return e, nil
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
from now (24h) or an RFC 3339 time. --repo shows one pair's entries, even after remove; --repo . is the
pair this checkout's remotes name.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			if o.Limit < 1 {
				return contract.New(contract.BadFlag, "--limit must be 1 or more (the default is %d)", engine.DefaultLogLimit).WithField("limit")
			}
			var err error
			if o.Since, err = engine.ParseSince(since, a.env.Now()); err != nil {
				return err
			}
			ws, err := a.openWorkspace()
			if err != nil {
				return err
			}
			if o.Pair, err = a.logPair(ws); err != nil {
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
	f.IntVar(&o.Limit, "limit", engine.DefaultLogLimit, "at most this many entries (1 or more)")
	return c
}

// logPair is the public repo whose entries log shows, from --repo, or ""
// for every pair. It reads only workspace.json and the checkout's remotes:
// no engine, no token, no network. A registered public or shadow repo names
// its pair; any other owner/name is taken as a public repo, so the history
// of a removed pair can still be read. --repo . is the registered pair this
// checkout's remotes name.
func (a *app) logPair(ws *workspace.Workspace) (string, error) {
	switch a.repo {
	case "":
		return "", nil
	case ".":
		p, err := ws.Resolve(".", a.env.CwdRemotes())
		if err != nil {
			return "", err
		}
		return p.Public, nil
	}
	if p, ok := ws.Find(a.repo); ok {
		return p.Public, nil
	}
	if _, _, err := forge.SplitName(a.repo); err != nil {
		return "", contract.New(contract.BadFlag, "--repo %q is not owner/name or .", a.repo).WithField("repo")
	}
	return a.repo, nil
}
