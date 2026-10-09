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

// publicRemotes finds the remotes with any url or pushurl at a registered
// public repo (never a shadow repo).
func publicRemotes(ctx context.Context, co *guard.Checkout, ws *workspace.Workspace) (guard.Hook, error) {
	h := guard.Hook{Workspace: ws.Name}
	remotes, err := co.RemoteURLs(ctx)
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
		public := false
		for _, u := range remotes[n] {
			slug, ok := guard.Slug(u)
			if !ok {
				continue
			}
			if p, ok := ws.Find(slug); ok && strings.EqualFold(p.Public, slug) {
				public = true
				if !seen[slug] {
					seen[slug] = true
					h.Publics = append(h.Publics, slug)
				}
			}
		}
		if public {
			h.Remotes = append(h.Remotes, n)
		}
	}
	if len(h.Remotes) == 0 {
		return h, contract.New(contract.NotAPairCheckout,
			"no remote of %s points at a public repo registered in workspace %q; run ghostbranch init <owner/repo> first", co.Top, ws.Name)
	}
	return h, nil
}

// pairOrGuard is the pair check status and uninstall share with install:
// a remote of the checkout must point at a registered public repo, unless
// the guard is installed here. An installed guard stays visible and
// removable, since guard check tells a person to uninstall a guard it can't
// place (after remove, or a changed remote).
func pairOrGuard(ctx context.Context, co *guard.Checkout, ws *workspace.Workspace) error {
	_, err := publicRemotes(ctx, co, ws)
	if err == nil || contract.From(err).Code != contract.NotAPairCheckout {
		return err
	}
	if st, ierr := guard.Inspect(co); ierr == nil && st.Installed {
		return nil
	}
	return err
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
			co, ws, err := a.here(cmd.Context())
			if err != nil {
				return err
			}
			if err := pairOrGuard(cmd.Context(), co, ws); err != nil {
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
			co, ws, err := a.here(cmd.Context())
			if err != nil {
				return err
			}
			if err := pairOrGuard(cmd.Context(), co, ws); err != nil {
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
			// The push goes to the URL git hands the hook (after insteadOf),
			// or the remote argument when it is a URL: when either names a
			// registered public repo, that pair alone is checked. Otherwise
			// the guard can't tell which of the remote's urls and pushurls
			// the push goes to, so it checks the push against every
			// registered public repo among them and refuses it if any would.
			// No match at all fails closed.
			pairs := registeredPairs(ws, args[1])
			if len(pairs) == 0 {
				pairs = registeredPairs(ws, args[0])
			}
			if len(pairs) == 0 {
				rs, err := co.RemoteURLs(ctx)
				if err != nil {
					return contract.Wrap(contract.PushBlocked, err, "the guard could not read the remotes of this checkout, so the push is refused")
				}
				pairs = registeredPairs(ws, rs[args[0]]...)
			}
			if len(pairs) == 0 {
				return contract.New(contract.PushBlocked, "the guard cannot tell which public repo %q is, so the push is refused; reinstall it with ghostbranch guard install, or remove the guard with ghostbranch guard uninstall", args[0])
			}
			var blocks []guard.Block
			var blockedBy []string
			for _, p := range pairs {
				facts, err := mirrorFacts(ctx, ws.MirrorDir(p))
				if err != nil {
					return contract.Wrap(contract.PushBlocked, err, "the guard could not read the features of %s, so the push is refused", p.Public)
				}
				// The checkout reads its objects with plain git, so the
				// user's config (safe.directory and the like) applies.
				bs, err := guard.Check(ctx, co, lines, facts)
				if err != nil {
					return contract.Wrap(contract.PushBlocked, err, "check the push to %s, so it is refused", p.Public)
				}
				if len(bs) > 0 {
					blocks = append(blocks, bs...)
					blockedBy = append(blockedBy, p.Public)
				}
			}
			if len(blocks) > 0 {
				var b strings.Builder
				if len(blockedBy) == 1 {
					fmt.Fprintf(&b, "pushing Ghostbranch feature work to the public repo %s is blocked:", blockedBy[0])
				} else {
					fmt.Fprintf(&b, "pushing Ghostbranch feature work to the public repos %s is blocked:", strings.Join(blockedBy, ", "))
				}
				for _, bl := range blocks {
					fmt.Fprintf(&b, "\n  %s: %s", bl.Ref, bl.Reason)
				}
				b.WriteString("\npublish features only with: ghostbranch promote <feature>")
				return contract.New(contract.PushBlocked, "%s", b.String()).WithDetails(map[string]any{"public": strings.Join(blockedBy, ", "), "blocked": blocks})
			}
			return done(a, &guard.CheckResult{Checked: len(lines)}, nil, nil)
		},
	}
}

// registeredPairs is the distinct registered pairs whose public repo one of
// urls names, in the order the urls give them.
func registeredPairs(ws *workspace.Workspace, urls ...string) []workspace.Pair {
	var out []workspace.Pair
	seen := map[string]bool{}
	for _, u := range urls {
		slug, ok := guard.Slug(u)
		if !ok {
			continue
		}
		p, ok := ws.Find(slug)
		if !ok || !strings.EqualFold(p.Public, slug) || seen[strings.ToLower(p.Public)] {
			continue
		}
		seen[strings.ToLower(p.Public)] = true
		out = append(out, p)
	}
	return out
}
