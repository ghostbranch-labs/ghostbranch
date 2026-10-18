package engine

import (
	"context"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// AgentTokenCheck is what read-only probes learned about a token offered
// for an agent (spec § 4.5). It never holds the token.
type AgentTokenCheck struct {
	Account    string   `json:"account"`
	Shadow     string   `json:"shadow"`
	CanPublish string   `json:"can_publish"` // forge.PushAccess.Can on the public repo
	Missing    []string `json:"missing"`     // of forge.AgentTokenPermissions on the shadow
	// reachesShadow is false when the token can't see the shadow repo at
	// all (the wrong repo was picked).
	reachesShadow bool
	pushStatus    int // the public push probe's HTTP status
}

// AgentsTokenResult is agents token's data: the token link and the repo to
// pick there, the check of the token a person entered (nil when none was
// read, as with --json), and the line that sets it, per shell.
type AgentsTokenResult struct {
	Link    string            `json:"link"`
	Select  string            `json:"select"`
	Check   *AgentTokenCheck  `json:"check"`
	SetLine map[string]string `json:"set_line"`
}

// AgentTokenLink is GitHub's fine-grained token page for an agent's token
// on p: the agent permissions only, on the shadow repo's owner, for 30
// days. GitHub can't pre-fill the repo, so the person picks p.Shadow.
func AgentTokenLink(p workspace.Pair) string {
	owner, _, _ := forge.SplitName(p.Shadow)
	_, name, _ := forge.SplitName(p.Public)
	return forge.TokenLink("ghostbranch-agent-"+name, "Ghostbranch agent: "+p.Shadow, owner, 30, forge.AgentTokenPermissions)
}

// AgentTokenSetLines are the lines that set the agent's token, per shell,
// with <paste> where the person pastes it. Ghostbranch never writes the
// token anywhere itself.
func AgentTokenSetLines() map[string]string {
	return map[string]string{
		"sh":         "export GHOSTBRANCH_TOKEN=<paste>",
		"powershell": "$env:GHOSTBRANCH_TOKEN = '<paste>'",
	}
}

// CheckAgentToken probes with f (a forge on the offered token); read-only.
// It asks whose token it is, what it may do on the shadow repo, and whether
// it may push to the public repo (the token-split check of agents status).
func CheckAgentToken(ctx context.Context, f forge.Forge, p workspace.Pair) (*AgentTokenCheck, error) {
	u, err := f.CurrentUser(ctx)
	if err != nil {
		return nil, forgeErr(err, "read the offered token's GitHub account")
	}
	c := &AgentTokenCheck{Account: u.Login, Shadow: p.Shadow, CanPublish: "unknown", Missing: []string{}}
	var have map[string]string
	if pp, ok := f.(forge.PermissionProber); ok {
		probe, err := pp.ProbePermissions(ctx, p.Shadow)
		if err != nil {
			return nil, forgeErr(err, "check the offered token on %s", p.Shadow)
		}
		have = probe.Have
	}
	c.reachesShadow = have[forge.PermMetadataRead] != forge.AccessNo
	if c.reachesShadow {
		for _, perm := range forge.AgentTokenPermissions {
			if accessOf(have, perm) == forge.AccessNo {
				c.Missing = append(c.Missing, perm)
			}
		}
	}
	if pr, ok := f.(forge.PushProber); ok {
		a := pr.PushAccess(ctx, p.Public)
		c.CanPublish, c.pushStatus = a.Can, a.HTTPStatus
	}
	return c, nil
}

// AgentTokenRefusal is nil when c is a token an agent may use on p: it
// reaches the shadow repo with what agents need and is proved unable to push
// to the public repo. Otherwise it is AGENT_TOKEN_REFUSED with the first
// thing to fix (wrong_repo, then can_publish, then missing_permissions) and
// the link for a new token. A push probe that couldn't tell is a retryable
// FORGE_ERROR: the token isn't proved safe, but it may be.
func AgentTokenRefusal(c *AgentTokenCheck, p workspace.Pair) error {
	link := AgentTokenLink(p)
	owner, _, _ := forge.SplitName(p.Shadow)
	steps := []string{"make a new token on GitHub, for " + owner + ": " + link,
		"on that page, choose Only select repositories, then " + p.Shadow + ", and keep the permissions the link set"}
	refuse := func(reason, format string, args ...any) *contract.Error {
		return contract.New(contract.AgentTokenRefused, format, args...).
			WithDetails(map[string]any{"reason": reason, "link": link, "shadow": p.Shadow, "public": p.Public, "steps": steps})
	}
	switch {
	case !c.reachesShadow:
		return refuse("wrong_repo", "this token can't reach the shadow repo %s; make a new token with only %s selected", p.Shadow, p.Shadow)
	case c.CanPublish == "true":
		return refuse("can_publish", "this token can push to the public repo %s, so an agent could publish with it; make a new token with only %s selected", p.Public, p.Shadow)
	case len(c.Missing) > 0:
		e := refuse("missing_permissions", "this token lacks %s on %s, which agents need; make a new token from the link, which sets them", strings.Join(c.Missing, ", "), p.Shadow)
		e.Details["missing"] = c.Missing
		return e
	case c.CanPublish != "false":
		e := contract.New(contract.ForgeError, "couldn't tell whether this token can push to the public repo %s, so it isn't accepted; try again", p.Public).
			WithDetails(map[string]any{"public": p.Public, "http_status": c.pushStatus})
		e.Retryable = true
		return e
	}
	return nil
}
