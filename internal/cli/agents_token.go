package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

func (a *app) agentsTokenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "token",
		Short: "Make a coding agent's own token that reaches only the shadow repo, and check it can't publish",
		Long: `agents token shows GitHub's fine-grained token page, pre-filled with what agents need on the
shadow repo, and says which repo to pick there. At a console it then reads the new token with hidden
input, checks it reaches the shadow repo and can't push to the public repo, and prints the line that
sets GHOSTBRANCH_TOKEN in the agent's shell. The token is never shown, logged or stored: the person
restarts the agent in a shell with it set. With --json (for an agent) it reads nothing and returns the
link, the repo to pick and those lines for a person.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			ws, err := a.openWorkspace()
			if err != nil {
				return err
			}
			p, err := ws.Resolve(a.repo, a.env.CwdRemotes())
			if err != nil {
				return err
			}
			a.pairFull = p
			res, err := a.agentsToken(cmd.Context(), p)
			return done(a, res, nil, err)
		},
	}
}

// agentsToken is spec § 4.5. Only a person at a console is asked for the
// token; --json and a run without a console read nothing.
func (a *app) agentsToken(ctx context.Context, p workspace.Pair) (*engine.AgentsTokenResult, error) {
	res := &engine.AgentsTokenResult{Link: engine.AgentTokenLink(p), Select: p.Shadow, SetLine: engine.AgentTokenSetLines()}
	if a.jsonOut || a.nonInteractive || !a.env.ConsoleUsable || a.env.OpenConsole == nil || a.env.ReadSecret == nil {
		return res, nil
	}
	dev, err := a.env.OpenConsole()
	if err != nil || dev == nil {
		return res, nil // no console after all: show the steps instead
	}
	// The device is never closed, as prompt's isn't: after a Ctrl-C the
	// hidden read is still pending, and on Windows that blocks Close. It
	// ends with the process (one command).
	a.reporter().Pause()
	defer a.reporter().Resume()
	owner, _, _ := forge.SplitName(p.Shadow)
	say := func(s string) { _, _ = fmt.Fprintln(dev, s) }
	say(fmt.Sprintf("Make the agent's token on GitHub, for %s: %s", owner, res.Link))
	// Only GitHub's own page is ever opened.
	if a.env.OpenBrowser != nil && strings.HasPrefix(res.Link, "https://github.com/") && a.env.OpenBrowser(res.Link) == nil {
		say("(It is open in your browser.)")
	}
	say("On that page, choose Only select repositories, then " + p.Shadow + ". Keep the permissions the link set.")
	_, _ = fmt.Fprint(dev, "Paste the new token here (it isn't shown, and Ghostbranch never stores it): ")
	c, err := a.checkSecret(ctx, dev, p)
	say("")
	if err != nil {
		return nil, err
	}
	if err := engine.AgentTokenRefusal(c, p); err != nil {
		return nil, err
	}
	res.Check = c
	return res, nil
}

// checkSecret reads the token from dev with hidden input and checks it. The
// token lives only here and in the forge made on it, which is dropped when
// the check ends; the bytes read are cleared. It is never printed or kept.
func (a *app) checkSecret(ctx context.Context, dev io.ReadWriteCloser, p workspace.Pair) (*engine.AgentTokenCheck, error) {
	raw, err := a.env.ReadSecret(ctx, dev)
	defer clear(raw)
	if err != nil {
		if ctx.Err() != nil {
			return nil, contract.New(contract.Interrupted, "interrupted (Ctrl-C) while waiting for the agent's token; nothing was read or kept")
		}
		return nil, contract.New(contract.BadFlag, "the token couldn't be read from the console with hidden input; run ghostbranch agents token in a terminal").WithField("token")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, contract.New(contract.BadFlag, "no token was entered; paste the one token GitHub showed, then press Enter").WithField("token")
	}
	f, err := a.env.NewForge(token)
	if err != nil {
		return nil, contract.New(contract.Internal, "can't create the GitHub client for the agent's token")
	}
	return engine.CheckAgentToken(ctx, f, p)
}
