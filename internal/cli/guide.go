package cli

import (
	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/guide"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
)

func (a *app) guideCmd() *cobra.Command {
	var install, uninstall string
	c := &cobra.Command{
		Use:   "guide",
		Short: "Print the guide for coding agents, or install a skill that points agents at it",
		Long: `guide prints how coding agents should use Ghostbranch: the loop, the rules, and every exit and
error code. It always matches this binary. --install claude|codex writes a small user-level skill
(never a file in your repo) that tells the agent to read this guide; --uninstall removes it.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			a.ran = true
			if install != "" && uninstall != "" {
				return contract.New(contract.BadFlag, "pass --install or --uninstall, not both").WithField("install")
			}
			if install == "" && uninstall == "" {
				return done(a, &guide.Printed{Markdown: guide.Render(contract.Registry())}, nil, nil)
			}
			target, op := install, guide.Install
			if uninstall != "" {
				target, op = uninstall, guide.Uninstall
			}
			path, err := guide.SkillPath(target, a.env.Getenv, a.env.UserHome)
			if err != nil {
				return err
			}
			action, err := op(path, a.dryRun)
			if err != nil {
				return err
			}
			res := &guide.InstallResult{Target: target, Path: path, Action: action}
			if a.dryRun {
				res.Preview = preview.New(nil)
				switch action {
				case "installed", "updated":
					res.Preview.Add(preview.WriteFile(path))
				case "removed":
					res.Preview.Add(preview.RemoveFile(path))
				}
			}
			return done(a, res, nil, nil)
		},
	}
	c.Flags().StringVar(&install, "install", "", "write a user-level skill for an agent: claude or codex")
	c.Flags().StringVar(&uninstall, "uninstall", "", "remove the skill for an agent: claude or codex")
	c.Flags().BoolVar(&a.dryRun, "plan", false, "show what would be written, and write nothing")
	return c
}
