package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

const revokeURL = "https://github.com/settings/apps/authorizations"

type LoginResult struct {
	Account    string     `json:"account"`     // GitHub login; "" when GitHub didn't say
	Kind       string     `json:"kind"`        // "app" (device flow) | "pat" (pasted)
	TokenKind  string     `json:"token_kind"`  // auth.Kind of the token
	Expires    *time.Time `json:"expires"`     // app tokens; null for a pasted token
	Replaced   bool       `json:"replaced"`    // a stored login was replaced
	InstallURL string     `json:"install_url"` // first app login: where to pick the repos; "" otherwise
	EnvToken   string     `json:"env_token"`   // "GHOSTBRANCH_TOKEN" when it is set and wins over this login
}

type LogoutResult struct {
	Removed   bool   `json:"removed"`
	Account   string `json:"account"`
	Kind      string `json:"kind"`       // "app" | "pat" | "" (nothing readable was stored)
	RevokeURL string `json:"revoke_url"` // app logins: where the person revokes the app's access at GitHub
	EnvToken  string `json:"env_token"`  // an env token commands still use: GHOSTBRANCH_TOKEN, GH_TOKEN or GITHUB_TOKEN; ""
}

// AuthStatusResult is auth status's data. An agent using a person's stored
// login is the AGENT_ON_PERSON_LOGIN warning, not a field.
type AuthStatusResult struct {
	Source             string       `json:"source"` // GHOSTBRANCH_TOKEN | login | GH_TOKEN | GITHUB_TOKEN | gh auth token | ""
	Account            string       `json:"account"`
	TokenKind          string       `json:"token_kind"`           // auth.Kind; "" with no token
	Valid              string       `json:"valid"`                // "true" | "false" | "unknown" (offline or no token)
	CanPublish         string       `json:"can_publish"`          // push access to Pair: "true" | "false" | "unknown"
	Pair               string       `json:"pair"`                 // the public repo probed; "" when no pair
	Problem            string       `json:"problem"`              // the error code that stopped resolution; ""
	StoredLoginChecked bool         `json:"stored_login_checked"` // false when GHOSTBRANCH_TOKEN is set: the keychain is not touched
	StoredLogin        *StoredLogin `json:"stored_login"`
}

type StoredLogin struct {
	Account        string     `json:"account"`
	Kind           string     `json:"kind"` // "app" | "pat"
	Expires        *time.Time `json:"expires"`
	RefreshExpires *time.Time `json:"refresh_expires"`
}

func loginUnavailable(reason, format string, args ...any) *contract.Error {
	return contract.New(contract.LoginUnavailable, format, args...).WithDetails(map[string]any{"reason": reason})
}

func noKeychain() *contract.Error {
	return loginUnavailable("no_keychain", "this machine has no OS keychain to keep a GitHub login in; set GHOSTBRANCH_TOKEN instead")
}

func (a *app) loginCmd() *cobra.Command {
	var withToken bool
	c := &cobra.Command{
		Use:   "login",
		Short: "Log in to GitHub with the Ghostbranch GitHub App, or store a token from stdin",
		Long: `login runs GitHub's device flow for the Ghostbranch GitHub App: it shows a code and a URL on the
console, opens the browser when it can, and waits while you approve. The token reaches only the repos
you install the app on, refreshes itself, and is kept in the OS keychain (never in a file).
With --with-token it reads one token from stdin instead (never from an argument, so it stays out of
shell history), checks it with GitHub, and stores it. GHOSTBRANCH_TOKEN, when set, still wins over
the stored login.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			if withToken {
				res, err := a.loginWithToken(cmd.Context())
				return done(a, res, nil, err)
			}
			res, err := a.loginDevice(cmd.Context())
			return done(a, res, nil, err)
		},
	}
	c.Flags().BoolVar(&withToken, "with-token", false, "read a token from stdin and store it")
	return c
}

// priorLogin is the login a new one replaces. A corrupt entry is replaced
// without complaint (logging in again is its fix); a newer Ghostbranch's
// entry and keychain failures stop login before anything is asked.
func (a *app) priorLogin() (*auth.Login, error) {
	if a.env.Keychain == nil {
		return nil, noKeychain()
	}
	prev, err := auth.LoadLogin(a.env.Keychain)
	if ce := contract.From(err); ce != nil {
		if r, _ := ce.Details["reason"].(string); ce.Code == contract.LoginUnavailable && r == "corrupt" {
			return nil, nil
		}
		return nil, err
	}
	return prev, nil
}

func (a *app) loginDevice(ctx context.Context) (*LoginResult, error) {
	if !a.env.ConsoleUsable || a.nonInteractive || a.env.OpenConsole == nil {
		return nil, loginUnavailable("no_console", "login needs a person at a terminal to approve it on GitHub; pipe a token to `ghostbranch login --with-token`, or set GHOSTBRANCH_TOKEN")
	}
	prev, err := a.priorLogin()
	if err != nil {
		return nil, err
	}
	con, err := prompt.OnConsole(a.env.ConsoleUsable, a.env.OpenConsole)
	if err != nil {
		return nil, loginUnavailable("no_console", "the console can't be opened; pipe a token to `ghostbranch login --with-token`, or set GHOSTBRANCH_TOKEN")
	}
	l, err := a.runLogin(ctx, con)
	if err != nil {
		return nil, err
	}
	res := a.loginResult(*l, prev)
	if prev == nil && a.env.AppSlug != "" {
		res.InstallURL = "https://github.com/apps/" + a.env.AppSlug + "/installations/new"
	}
	return res, nil
}

// runLogin runs the device flow on ask (the console device, or guided
// init's asker): it shows the code and URL there, opens GitHub's page in the
// browser when it can, waits for the approval, fills the account and stores
// the login. It asks nothing. The device code itself is never shown.
func (a *app) runLogin(ctx context.Context, ask prompt.Asker) (*auth.Login, error) {
	// Probe the keychain before GitHub is asked: a fresh pair that can't be
	// stored is lost. priorLogin handles a nil, unusable or locked keychain
	// and lets a corrupt entry be replaced.
	if _, err := a.priorLogin(); err != nil {
		return nil, err
	}
	flow := a.deviceFlow(ctx)
	dc, err := flow.Start(ctx)
	if err != nil {
		return nil, err
	}
	ask.Say(fmt.Sprintf("To log in, open %s and enter this code: %s", dc.VerificationURI, dc.UserCode))
	// Only GitHub's own page is ever opened.
	if a.env.OpenBrowser != nil && strings.HasPrefix(dc.VerificationURI, a.oauthBase()+"/") &&
		a.env.OpenBrowser(dc.VerificationURI) == nil {
		ask.Say("(It is open in your browser.)")
	}
	ask.Say("Waiting for you to approve it on GitHub. Ctrl-C cancels.")
	l, err := flow.Poll(ctx, dc)
	if err != nil {
		return nil, err
	}
	l.Account = a.accountOf(ctx, l.Token)
	if err := auth.SaveLogin(a.env.Keychain, l); err != nil {
		return nil, err
	}
	return &l, nil
}

// accountOf is the token's GitHub login, or "" when GitHub doesn't say. A
// fresh device-flow token is stored either way: its pair can't be had again.
func (a *app) accountOf(ctx context.Context, token string) string {
	f, err := a.env.NewForge(token)
	if err != nil {
		return ""
	}
	u, err := f.CurrentUser(ctx)
	if err != nil {
		return ""
	}
	return u.Login
}

func (a *app) loginWithToken(ctx context.Context) (*LoginResult, error) {
	raw, err := io.ReadAll(io.LimitReader(a.env.Stdin, 4096))
	if err != nil {
		return nil, contract.New(contract.Internal, "can't read the token from stdin")
	}
	token := strings.TrimSpace(string(raw))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, contract.New(contract.BadFlag, "login --with-token reads one token from stdin, for example: gh auth token | ghostbranch login --with-token").WithField("stdin")
	}
	prev, err := a.priorLogin()
	if err != nil {
		return nil, err
	}
	f, err := a.env.NewForge(token)
	if err != nil {
		return nil, contract.New(contract.Internal, "can't create the GitHub client")
	}
	u, err := f.CurrentUser(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// Never quote err: a client error may repeat what it was given.
		if errors.Is(err, forge.ErrTransient) {
			e := contract.New(contract.ForgeError, "GitHub can't be reached to check the token; nothing was stored")
			e.Retryable = true
			return nil, e
		}
		return nil, contract.New(contract.AuthMissing, "GitHub didn't accept the token from stdin; nothing was stored")
	}
	l := auth.Login{Token: token, Account: u.Login, Kind: "pat", SchemaVersion: contract.SchemaVersion}
	if err := auth.SaveLogin(a.env.Keychain, l); err != nil {
		return nil, err
	}
	return a.loginResult(l, prev), nil
}

func (a *app) loginResult(l auth.Login, prev *auth.Login) *LoginResult {
	r := &LoginResult{Account: l.Account, Kind: l.Kind, TokenKind: auth.Kind(l.Token), Replaced: prev != nil}
	if !l.Expires.IsZero() {
		t := l.Expires.UTC()
		r.Expires = &t
	}
	if strings.TrimSpace(a.env.Getenv("GHOSTBRANCH_TOKEN")) != "" {
		r.EnvToken = "GHOSTBRANCH_TOKEN"
	}
	return r
}

func (a *app) logoutCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "logout",
		Short: "Remove the stored GitHub login from the OS keychain",
		Long: `logout removes the login that ghostbranch login stored in the OS keychain, a corrupt one included.
It can't revoke the token at GitHub (that needs the app's client secret, which Ghostbranch doesn't
ship); for a GitHub App login it gives the page where you revoke the app's access. Environment tokens
(GHOSTBRANCH_TOKEN, GH_TOKEN, GITHUB_TOKEN) are untouched.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			a.ran = true
			res, err := a.logout()
			return done(a, res, nil, err)
		},
	}
}

func (a *app) logout() (*LogoutResult, error) {
	if a.env.Keychain == nil {
		return nil, loginUnavailable("no_keychain", "this machine has no OS keychain, so no login is stored here")
	}
	res := &LogoutResult{}
	prev, err := auth.LoadLogin(a.env.Keychain)
	if ce := contract.From(err); ce != nil {
		if r, _ := ce.Details["reason"].(string); ce.Code != contract.LoginUnavailable || r != "corrupt" {
			return nil, err
		}
		res.Removed = true // the corrupt entry goes
	}
	if err := auth.DeleteLogin(a.env.Keychain); err != nil {
		return nil, err
	}
	if prev != nil {
		res.Removed, res.Account, res.Kind = true, prev.Account, prev.Kind
		if prev.Kind == "app" {
			res.RevokeURL = revokeURL
		}
	}
	for _, k := range []string{"GHOSTBRANCH_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
		if strings.TrimSpace(a.env.Getenv(k)) != "" {
			res.EnvToken = k
			break
		}
	}
	return res, nil
}

func (a *app) authCmd() *cobra.Command {
	c := &cobra.Command{Use: "auth", Short: "See which GitHub token Ghostbranch uses"}
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the token's source, account, kind and expiry, and whether it can publish",
		Long: `auth status shows where the token comes from (GHOSTBRANCH_TOKEN, the stored login, GH_TOKEN,
GITHUB_TOKEN or gh), its account and kind, whether it can publish to this checkout's pair (or --repo),
and the stored login. Offline, it still shows the stored login. With GHOSTBRANCH_TOKEN set it doesn't
read the keychain at all. A stored login near expiry is refreshed first, as any command would.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.ran = true
			res, warns := a.authStatus(cmd.Context())
			return done(a, res, warns, nil)
		},
	})
	return c
}

func storedStatus(l *auth.Login) *StoredLogin {
	s := &StoredLogin{Account: l.Account, Kind: l.Kind}
	if !l.Expires.IsZero() {
		t := l.Expires.UTC()
		s.Expires = &t
	}
	if !l.RefreshExpires.IsZero() {
		t := l.RefreshExpires.UTC()
		s.RefreshExpires = &t
	}
	return s
}

func (a *app) authStatus(ctx context.Context) (*AuthStatusResult, []contract.Warning) {
	res := &AuthStatusResult{Valid: "unknown", CanPublish: "unknown"}
	var warns []contract.Warning
	var stored *auth.Login
	if a.env.Keychain != nil && strings.TrimSpace(a.env.Getenv("GHOSTBRANCH_TOKEN")) == "" {
		res.StoredLoginChecked = true
		if l, err := auth.LoadLogin(a.env.Keychain); err == nil && l != nil {
			stored, res.StoredLogin = l, storedStatus(l)
		}
	}
	tok, src, err := auth.Resolve(ctx, a.authSource())
	if err != nil {
		if ce := contract.From(err); ce.Code != contract.AuthMissing {
			res.Problem = string(ce.Code)
		}
		return res, warns
	}
	res.Source, res.TokenKind = src, auth.Kind(tok)
	if src == "login" && auth.LooksLikeAgent(a.env.Getenv, a.env.ConsoleUsable) {
		warns = append(warns, contract.Warning{Code: contract.AgentOnPersonLogin,
			Message: "this looks like an agent or a script using a person's stored login, which can publish; give the agent its own shadow-only GHOSTBRANCH_TOKEN (ghostbranch guide, Tokens)"})
	}
	if src == "login" && stored != nil {
		res.Account = stored.Account
	}
	f, err := a.env.NewForge(tok)
	if err != nil {
		return res, warns
	}
	u, err := f.CurrentUser(ctx)
	switch {
	case err == nil:
		res.Valid, res.Account = "true", u.Login
	case errors.Is(err, forge.ErrTransient) || ctx.Err() != nil:
		return res, warns // offline: the stored fields stand
	default:
		res.Valid = "false"
		return res, warns
	}
	if p, ok := a.statusPair(); ok {
		res.Pair = p.Public
		if pr, ok := f.(forge.PushProber); ok {
			res.CanPublish = pr.PushAccess(ctx, p.Public).Can
		}
	}
	return res, warns
}

// statusPair is this checkout's pair (or --repo's), if any; no pair is not
// an error for auth status.
func (a *app) statusPair() (workspace.Pair, bool) {
	ws, err := a.openWorkspace()
	if err != nil {
		return workspace.Pair{}, false
	}
	p, err := ws.Resolve(a.repo, a.env.CwdRemotes())
	if err != nil {
		return workspace.Pair{}, false
	}
	a.pairFull = p
	return p, true
}
