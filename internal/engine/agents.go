package engine

import (
	"context"
	"fmt"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/approval"
	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/ledger"
	"github.com/ghostbranch-labs/ghostbranch/internal/preview"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

type AgentsAllowOptions struct {
	For          string // 1h, 6h (default), 1d, 7d, 15d, 30d or 90d
	UntilRevoked bool
}

type AgentsAllowResult struct {
	Grant    grant.Grant      `json:"grant"`
	Replaced *grant.Grant     `json:"replaced"`
	Preview  *preview.Preview `json:"preview"`
}

type AgentsRevokeResult struct {
	Revoked []grant.Grant    `json:"revoked"`
	Preview *preview.Preview `json:"preview"`
}

type GrantStatus struct {
	grant.Grant
	Expired          bool   `json:"expired"`
	RemainingSeconds *int64 `json:"remaining_seconds"`
}

type TokenCheck struct {
	Source     string `json:"source"`
	HTTPStatus int    `json:"http_status"`
}

type AgentsStatusResult struct {
	Pair            string        `json:"pair"`
	Grants          []GrantStatus `json:"grants"`
	TokenCanPublish string        `json:"token_can_publish"`
	TokenKind       string        `json:"token_kind"`
	Token           TokenCheck    `json:"token"`
}

func grantRef(g grant.Grant) *ledger.GrantRef {
	return &ledger.GrantRef{ID: g.ID, ExpiresAt: g.ExpiresAt}
}

func (e *Engine) grants() (*grant.Store, error) {
	if e.Grants == nil {
		return nil, contract.New(contract.Internal, "agent grants are not available")
	}
	return e.Grants, nil
}

func grantErr(err error, what string) error {
	if ce := contract.From(err); ce.Code != contract.Internal {
		return ce // LOCKED, or UPGRADE_NEEDED for a file from a newer Ghostbranch
	}
	return internalErr(err, what)
}

// AgentsAllow lets agents publish to p from this machine without the
// console code. Making the grant itself needs a person at a console, so an
// agent cannot grant itself (spec §7.2).
func (e *Engine) AgentsAllow(ctx context.Context, p workspace.Pair, opt AgentsAllowOptions) (res *AgentsAllowResult, warns []contract.Warning, err error) {
	defer func() {
		op, summary := "agents allow", ""
		var ref *ledger.GrantRef
		if err == nil && res != nil {
			op, ref = "grant_created", grantRef(res.Grant)
			if res.Replaced != nil {
				op = "grant_replaced"
			}
			summary = "agents may publish to " + p.Public
		}
		e.recordWith(op, &p, "", "", summary, err, func(ent *ledger.Entry) { ent.Grant = ref })
	}()
	dur, err := grant.ParseFor(opt.For, opt.UntilRevoked)
	if err != nil {
		return nil, nil, err
	}
	s, err := e.grants()
	if err != nil {
		return nil, nil, err
	}
	now := e.now()
	g := grant.Grant{Public: p.Public, Shadow: p.Shadow, CreatedAt: now, CreatedBy: e.Actor}
	label := "until revoked"
	if dur != nil {
		exp := now.Add(*dur)
		g.ExpiresAt = &exp
		label = opt.For
		if label == "" {
			label = grant.DefaultFor
		}
		label = "for " + label + ", until " + exp.UTC().Format("2006-01-02 15:04 UTC")
	}
	if g.ID, err = grant.NewID(e.Rand); err != nil {
		return nil, nil, internalErr(err, "make a grant id")
	}
	res = &AgentsAllowResult{Grant: g}
	if e.DryRun {
		res.Preview = preview.New(nil)
		res.Preview.Add(preview.Grant(p.Public, p.Shadow, g.ExpiresAt))
		return res, nil, nil
	}
	if e.Approver == nil {
		return nil, nil, needsApproval("", approval.NoConsole)
	}
	summary := fmt.Sprintf("Allow coding agents on this machine to publish to %s (shadow %s) with --yes, without this code, %s.\n"+
		"Agents still need a green gate and a clean leak scan for every publish. Revoke with: ghostbranch agents revoke --repo %s",
		p.Public, p.Shadow, label, p.Public)
	// A grants file that cannot be read could not be saved to either; say so
	// before the person types a code.
	if _, err := s.Load(); err != nil {
		return nil, nil, grantErr(err, "read agent grants")
	}
	if err := e.confirm(ctx, "", summary); err != nil {
		return nil, nil, err
	}
	if res.Replaced, err = s.Put(g, now); err != nil {
		return nil, nil, grantErr(err, "save the agent grant")
	}
	return res, nil, nil
}

// AgentsRevoke removes the grant for p, or every grant with all. Taking
// power away needs no console.
func (e *Engine) AgentsRevoke(ctx context.Context, p *workspace.Pair, all bool) (res *AgentsRevokeResult, warns []contract.Warning, err error) {
	defer func() {
		if err != nil {
			e.record("agents revoke", p, "", "", "", err)
			return
		}
		for _, g := range res.Revoked {
			pair := workspace.Pair{Public: g.Public, Shadow: g.Shadow}
			ref := grantRef(g)
			e.recordWith("grant_revoked", &pair, "", "", "agents may no longer publish to "+g.Public, nil, func(ent *ledger.Entry) { ent.Grant = ref })
		}
	}()
	if !all && p == nil {
		return nil, nil, contract.New(contract.BadFlag, "name a repo with --repo, or pass --all").WithField("repo")
	}
	s, err := e.grants()
	if err != nil {
		return nil, nil, err
	}
	public := ""
	if p != nil {
		public = p.Public
	}
	res = &AgentsRevokeResult{Revoked: []grant.Grant{}}
	if e.DryRun {
		gs, err := s.Load()
		res.Preview = preview.New(nil)
		switch {
		case err != nil && all && grant.Corrupt(err):
			// revoke --all replaces a grants.json that doesn't decode.
			res.Preview.Add(preview.WriteFile(s.Path()))
			return res, nil, nil
		case err != nil:
			return nil, nil, grantErr(err, "read agent grants")
		}
		for _, g := range gs {
			if (all || strings.EqualFold(g.Public, public)) && g.ValidAt(e.now()) {
				res.Revoked = append(res.Revoked, g)
				res.Preview.Add(preview.Revoke(g.Public, g.Shadow))
			}
		}
		return res, nil, nil
	}
	if res.Revoked, err = s.Revoke(public, all, e.now()); err != nil {
		return nil, nil, grantErr(err, "revoke agent grants")
	}
	return res, nil, nil
}

// AgentsStatus lists grants and, for a pair, whether the current token can
// push to its public repo (the token-split check, spec §8).
func (e *Engine) AgentsStatus(ctx context.Context, p *workspace.Pair) (*AgentsStatusResult, []contract.Warning, error) {
	s, err := e.grants()
	if err != nil {
		return nil, nil, err
	}
	gs, err := s.Load()
	if err != nil {
		return nil, nil, grantErr(err, "read agent grants")
	}
	now := e.now()
	res := &AgentsStatusResult{Grants: []GrantStatus{}, TokenCanPublish: "unknown", TokenKind: auth.Kind(e.Token), Token: TokenCheck{Source: e.TokenSource}}
	for _, g := range gs {
		if p != nil && !g.For(p.Public, p.Shadow) {
			continue
		}
		st := GrantStatus{Grant: g, Expired: !g.ValidAt(now)}
		if g.ExpiresAt != nil && !st.Expired {
			left := int64(g.ExpiresAt.Sub(now).Seconds())
			st.RemainingSeconds = &left
		}
		res.Grants = append(res.Grants, st)
	}
	if p != nil {
		res.Pair = p.Public
		a := e.canPublish(ctx, p.Public)
		res.TokenCanPublish, res.Token.HTTPStatus = a.Can, a.HTTPStatus
	}
	var warns []contract.Warning
	if likelyAgentOnLogin(e) {
		warns = append(warns, contract.Warning{Code: contract.AgentOnPersonLogin, Message: agentOnLoginMessage})
	}
	return res, warns, nil
}
