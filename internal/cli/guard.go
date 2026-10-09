package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) guardCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "guard",
		Short: "A git pre-push hook that blocks pushing feature work to the public repo",
		Long: `guard installs a pre-push hook in this checkout. A push to the public repo is refused when it
would publish a Ghostbranch feature (by branch name, or because it contains a feature's commits),
the ghostbranch overlay branch, or a gate branch. Pushes to any other remote are untouched, and an
existing pre-push hook (Git LFS, for example) keeps running. Publish features with ghostbranch promote.`,
	}
	c.AddCommand(a.guardInstallCmd(), a.guardUninstallCmd(), a.guardStatusCmd(), a.guardCheckCmd())
	return c
}

func (a *app) here(ctx context.Context) (*guard.Checkout, *workspace.Workspace, error) {
	a.ran = true
	dir, err := a.env.Getwd()
	if err != nil {
		return nil, nil, contract.Wrap(contract.Internal, err, "find the current directory")
	}
	ws, err := a.openWorkspace()
	if err != nil {
		return nil, nil, err
	}
	co, err := guard.Open(ctx, dir)
	return co, ws, err
}

// publicRemotes finds the remotes whose configured URL is a registered
// public repo (never a shadow repo).
func publicRemotes(ctx context.Context, co *guard.Checkout, ws *workspace.Workspace) (guard.Hook, error) {
	h := guard.Hook{Workspace: ws.Name}
	remotes, err := co.Remotes(ctx)
	if err != nil {
		return h, err
	}
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, n := range names {
		slug, ok := guard.Slug(remotes[n])
		if !ok {
			continue
		}
		if p, ok := ws.Find(slug); ok && strings.EqualFold(p.Public, slug) {
			h.Remotes = append(h.Remotes, n)
			if !seen[slug] {
				seen[slug] = true
				h.Publics = append(h.Publics, slug)
			}
		}
	}
	if len(h.Remotes) == 0 {
		return h, contract.New(contract.NotAPairCheckout,
			"no remote of %s points at a public repo registered in workspace %q; run ghostbranch init <owner/repo> first", co.Top, ws.Name)
	}
	return h, nil
}

func (a *app) guardInstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "install",
		Short: "Install the pre-push guard in this checkout",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			co, ws, err := a.here(cmd.Context())
			if err != nil {
				return err
			}
			h, err := publicRemotes(cmd.Context(), co, ws)
			if err != nil {
				return err
			}
			bin, err := a.env.Executable()
			if err != nil {
				return contract.Wrap(contract.Internal, err, "find this binary's path")
			}
			h.Binary = filepath.ToSlash(bin)
			res, err := guard.Install(cmd.Context(), co, h, a.dryRun)
			if err != nil {
				return err
			}
			return done(a, &res, nil, nil)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "show what would change, and change nothing")
	return c
}

func (a *app) guardUninstallCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Remove the pre-push guard, restoring any hook it chained",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			co, _, err := a.here(cmd.Context())
			if err != nil {
				return err
			}
			res, err := guard.Uninstall(co, a.dryRun)
			if err != nil {
				return err
			}
			return done(a, &res, nil, nil)
		},
	}
	c.Flags().BoolVar(&a.dryRun, "plan", false, "show what would change, and change nothing")
	return c
}

func (a *app) guardStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show whether the guard is installed here, and what it covers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			co, _, err := a.here(cmd.Context())
			if err != nil {
				return err
			}
			st, err := guard.Inspect(co)
			if err != nil {
				return err
			}
			return done(a, &st, nil, nil)
		},
	}
}

// mirrorFacts reads the features recorded in the engine's mirror. Only a
// mirror directory that does not exist yet means "no features", so the
// reserved names still apply; anything else the guard cannot read is an
// error, and the push is refused.
func mirrorFacts(ctx context.Context, dir string) (guard.Facts, error) {
	facts := guard.Facts{Names: map[string]bool{}, Heads: map[string]string{}}
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return facts, nil
	} else if err != nil {
		return facts, err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return facts, fmt.Errorf("%s is not a readable git repository: %w", dir, err)
	}
	return guard.LoadFacts(ctx, gitx.New(dir))
}

func (a *app) guardCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check <remote> <url>",
		Short: "Check a push to the public repo; run by the pre-push hook with git's input on stdin",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			co, ws, err := a.here(ctx)
			if err != nil {
				return contract.Wrap(contract.PushBlocked, err, "the guard could not inspect this checkout, so the push is refused")
			}
			lines, err := guard.ParseInput(a.env.Stdin)
			if err != nil {
				return contract.Wrap(contract.PushBlocked, err, "the guard could not read git's pre-push input, so the push is refused")
			}
			// git hands the hook the rewritten URL (insteadOf), so fall back to
			// the remote's configured URL.
			slug, ok := guard.Slug(args[1])
			if !ok {
				slug, ok = guard.Slug(args[0])
			}
			if !ok {
				if rs, err := co.Remotes(ctx); err == nil {
					slug, ok = guard.Slug(rs[args[0]])
				}
			}
			p, found := ws.Find(slug)
			if !ok || !found || !strings.EqualFold(p.Public, slug) {
				return contract.New(contract.PushBlocked, "the guard cannot tell which public repo %q is, so the push is refused; reinstall it with ghostbranch guard install, or remove the guard with ghostbranch guard uninstall", args[0])
			}
			facts, err := mirrorFacts(ctx, ws.MirrorDir(p))
			if err != nil {
				return contract.Wrap(contract.PushBlocked, err, "the guard could not read the features of %s, so the push is refused", p.Public)
			}
			// The checkout reads its objects with plain git, so the user's
			// config (safe.directory and the like) applies.
			blocks, err := guard.Check(ctx, co, lines, facts)
			if err != nil {
				return contract.Wrap(contract.PushBlocked, err, "check the push, so it is refused")
			}
			if len(blocks) > 0 {
				var b strings.Builder
				fmt.Fprintf(&b, "pushing Ghostbranch feature work to the public repo %s is blocked:", p.Public)
				for _, bl := range blocks {
					fmt.Fprintf(&b, "\n  %s: %s", bl.Ref, bl.Reason)
				}
				b.WriteString("\npublish features only with: ghostbranch promote <feature>")
				return contract.New(contract.PushBlocked, "%s", b.String()).WithDetails(map[string]any{"public": p.Public, "blocked": blocks})
			}
			return done(a, &guard.CheckResult{Checked: len(lines)}, nil, nil)
		},
	}
}
