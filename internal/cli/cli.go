// Package cli is the ghostbranch command line: it parses flags, builds an
// engine, and renders results as text or as one JSON envelope. It holds no
// product logic.
package cli

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/keychain"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/progress"
	"github.com/ghostbranch-labs/ghostbranch/internal/prompt"
	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Env is everything the CLI takes from the outside world, so tests can
// replace any of it.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Getenv func(string) string
	// UserHome is the home directory user-level skills go under, and the
	// state directory on macOS and Linux (when GHOSTBRANCH_HOME is unset).
	UserHome func() (string, error)
	// ConsoleUsable is console.Usable(os.Stdin, os.Stderr): stdin and stderr
	// are real terminals, so a person could be asked at the console.
	ConsoleUsable bool
	// OpenConsole opens the console device for the approval code.
	OpenConsole func() (io.ReadWriteCloser, error)
	// ReadSecret reads one line from the opened console device with echo
	// off (agents token's token); DefaultEnv's is console.ReadSecret.
	ReadSecret func(ctx context.Context, dev io.ReadWriteCloser) ([]byte, error)
	// Asker opens the console for guided init's questions: the console
	// device, never stdin. DefaultEnv's is prompt.OnConsole over
	// ConsoleUsable and OpenConsole.
	Asker    func() (prompt.Asker, error)
	Now      func() time.Time
	Sleep    func(context.Context, time.Duration) error
	Rand     io.Reader
	NewForge func(token string) (forge.Forge, error)
	GHToken  func() (string, error)
	// Keychain holds the stored login. nil means no login is consulted or
	// stored; tests use keychain.NewFake().
	Keychain keychain.Store
	// ClientID is the GitHub App's client ID (auth.ClientID). OAuthBaseURL
	// is GitHub's web origin for the device flow and refresh, and
	// HTTPClient the client those calls use.
	ClientID     string
	OAuthBaseURL string
	HTTPClient   *http.Client
	// AppSlug is the GitHub App's URL name (auth.AppSlug), for its install
	// page. OpenBrowser opens a URL in the person's browser, best effort.
	AppSlug      string
	OpenBrowser  func(url string) error
	CwdRemotes   func() []workspace.Remote
	NewScanner   func(leak.Rules) (leak.Scanner, error)
	PollInterval time.Duration
	SettleWindow time.Duration
	OnPush       func(op, remote string, refspecs []string)
	// CIAppearWindow is how long sync looks for the workflow runs its
	// pushes start in the shadow repo, and CIAppearPoll how often.
	CIAppearWindow time.Duration
	CIAppearPoll   time.Duration
	// Executable is this binary's path; the pre-push hook embeds it.
	Executable func() (string, error)
	// Getwd is the directory guard commands act on.
	Getwd func() (string, error)
	// GuardState reports whether the current directory is a checkout of the
	// public repo and whether the guard is installed there (for next).
	GuardState func(public string) (inCheckout, installed bool)
	// ShadowRemote names the remote of the current checkout that points at
	// the shadow repo ("" if none, or if its name is not safe to print into
	// a command), a remote name not yet in use for adding one, and whether
	// the current directory is a git checkout at all.
	ShadowRemote func(shadow string) (remote, free string, inCheckout bool)
	// GitVersion runs `git version` (doctor's tools check); nil means
	// gitx.Version. Build is this binary's version and commit; nil means
	// "(unknown)" for both.
	GitVersion func(context.Context) (string, error)
	Build      func() (version, commit string)
	// StdoutColor and StderrColor say each stream is a terminal that takes
	// ANSI colour and the environment allows it (NO_COLOR, TERM=dumb);
	// DefaultEnv sets them, tests leave them false. The app also drops
	// colour for --json and --no-color. TrueColor picks 24-bit colours.
	StdoutColor, StderrColor, TrueColor bool
	// StderrTerminal says stderr is a terminal (the update reminder).
	// StderrAnimate says it also takes ANSI control and TERM isn't dumb, so
	// progress may animate there (NO_COLOR doesn't matter). StderrWidth is
	// its width in columns, 0 when unknown. DefaultEnv sets them; tests
	// leave them false and nil.
	StderrTerminal, StderrAnimate bool
	StderrWidth                   func() int
	// UpdateWait is the most a command waits at its end for an update look
	// in flight; 0 means 500 ms.
	UpdateWait time.Duration
}

// colorAllowed is the environment's say on colour: NO_COLOR set to any
// non-empty value, or TERM=dumb, turns it off.
func colorAllowed(getenv func(string) string) bool {
	return getenv("NO_COLOR") == "" && getenv("TERM") != "dumb"
}

// trueColorTerm is true when COLORTERM promises 24-bit colour.
func trueColorTerm(getenv func(string) string) bool {
	v := getenv("COLORTERM")
	return v == "truecolor" || v == "24bit"
}

// DefaultEnv is the real process environment.
func DefaultEnv() Env {
	env := Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv, UserHome: os.UserHomeDir,
		ConsoleUsable: console.Usable(os.Stdin, os.Stderr), OpenConsole: console.Open, ReadSecret: console.ReadSecret,
		Now: time.Now, Sleep: sleepCtx, Rand: rand.Reader,
		NewForge: func(token string) (forge.Forge, error) {
			c, err := ghforge.New(token)
			if err != nil {
				return nil, err // never a non-nil Forge holding a nil *Client
			}
			return c, nil
		},
		GHToken:  auth.GHCLI,
		Keychain: keychain.OS(), ClientID: auth.ClientID, OAuthBaseURL: "https://github.com",
		AppSlug: auth.AppSlug, OpenBrowser: openBrowser,
		HTTPClient:   &http.Client{Timeout: 30 * time.Second},
		CwdRemotes:   cwdRemotes,
		NewScanner:   func(r leak.Rules) (leak.Scanner, error) { return leak.NewGitleaks(r) },
		PollInterval: engine.DefaultPollInterval,
		SettleWindow: engine.DefaultSettleWindow, CIAppearWindow: engine.DefaultCIAppearWindow, CIAppearPoll: engine.DefaultCIAppearPoll,
		Executable:   os.Executable,
		Getwd:        os.Getwd,
		GuardState:   guardState,
		ShadowRemote: shadowRemote,
		GitVersion:   gitx.Version,
		Build:        buildInfo,
	}
	if colorAllowed(env.Getenv) {
		env.StdoutColor = console.ColorCapable(os.Stdout)
		env.StderrColor = console.ColorCapable(os.Stderr)
		env.TrueColor = trueColorTerm(env.Getenv)
	}
	env.StderrTerminal = console.IsTerminal(os.Stderr)
	env.StderrAnimate = animateAllowed(env.Getenv, console.ColorCapable(os.Stderr))
	env.StderrWidth = func() int { return console.Width(os.Stderr) }
	env.Asker = func() (prompt.Asker, error) { return prompt.OnConsole(env.ConsoleUsable, env.OpenConsole) }
	return env
}

type app struct {
	env            Env
	jsonOut        bool
	noColor        bool
	ndjson         bool // wait --json: events stream first, so the envelope is one line too
	nonInteractive bool
	dryRun         bool
	repo           string
	workspace      string
	command        string
	ran            bool
	result         any
	warnings       []contract.Warning
	eng            *engine.Engine
	args           []string
	pairFull       workspace.Pair
	shadowRemote   string
	startName      string     // feature start's name argument, for next
	startFrom      string     // feature start's --from, for next
	versionFlag    bool       // ghostbranch --version ran: the command is version
	prog           uiProgress // this run's progress (newProgress); Finish runs before any output
	updates        *updateRun // this run's update check (startUpdateCheck); nil when none was started
}

// Run executes one command and returns the process exit code.
func Run(args []string, env Env) int {
	a := &app{env: env, args: args}
	for _, arg := range args {
		if arg == "--json" {
			a.jsonOut = true // honoured even when flag parsing fails
		} else if v, ok := strings.CutPrefix(arg, "--json="); ok {
			if b, err := strconv.ParseBool(v); err == nil {
				a.jsonOut = b
			}
		}
		if arg == "--no-color" {
			a.noColor = true // likewise
		} else if v, ok := strings.CutPrefix(arg, "--no-color="); ok {
			if b, err := strconv.ParseBool(v); err == nil && b {
				a.noColor = true
			}
		}
	}
	a.prog = a.newProgress()
	// A panic or an early return never leaves a spinner or a hidden cursor;
	// render normally finishes first, and Finish is idempotent.
	defer a.prog.Finish(true)
	root := a.root()
	root.SetArgs(args)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	// Ctrl-C cancels the context: a gate waiting on CI leaves its job
	// pending for wait, and every command reports INTERRUPTED
	// (asInterrupted). SIGTERM and SIGHUP do the same (stopSignals), so the
	// cursor is shown again whichever way the run is stopped.
	ctx, stop := signal.NotifyContext(context.Background(), stopSignals...)
	defer stop()
	// After the first Ctrl-C, stop restores the default handling, so a
	// second Ctrl-C kills the process if the command is slow to wind down.
	context.AfterFunc(ctx, stop)
	a.finishOnCancel(ctx)
	cmd, err := root.ExecuteContextC(ctx)
	a.command = commandName(cmd)
	if a.versionFlag {
		a.command = "version"
	}
	if a.command == "wait" {
		a.ndjson = a.jsonOut // a usage error before RunE is one line too
	}
	if err != nil {
		var ce *contract.Error
		if !errors.As(err, &ce) {
			if a.ran {
				err = contract.From(err) // plumbing failure after arg validation
			} else {
				err = contract.New(contract.BadFlag, "%s", err.Error())
			}
		}
		if a.ran {
			err = asInterrupted(ctx, err)
		}
	} else if !a.ran {
		return contract.ExitOK // help output only
	}
	return a.render(err)
}

// styler is the colour for one stream, given whether Env says the stream
// takes it. Human output only: --json, --no-color, NO_COLOR and TERM=dumb
// each turn it off, so no escape byte reaches JSON.
func (a *app) styler(streamColor bool) style.Styler {
	on := streamColor && !a.jsonOut && !a.noColor && colorAllowed(a.env.Getenv)
	return style.New(on, a.env.TrueColor)
}

// asInterrupted makes an error that came back after Ctrl-C an INTERRUPTED,
// so every command reports a cancel the same way. It keeps the error's
// details and adds details.error_code, the code the cancel turned into.
// gate, wait and promote return INTERRUPTED themselves (naming the job or
// the plan), and a settled gate outcome is the answer whatever happened, so
// both pass through. So do JOB_STALE and JOB_NOT_FOUND: they are settled
// answers too, and an INTERRUPTED would have next offer wait on a dead job.
// Running any other command again is safe: apply resumes, sync and feature
// close are leased, and the rest only read or write leased records.
func asInterrupted(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil {
		return err
	}
	ce := contract.From(err)
	switch {
	case ce.Code == contract.Interrupted, ce.Code == contract.JobStale, ce.Code == contract.JobNotFound,
		ce.Code.Exit() == contract.ExitGateFailed:
		return err
	}
	details := maps.Clone(ce.Details)
	if details == nil {
		details = map[string]any{}
	}
	details["error_code"] = string(ce.Code)
	out := contract.New(contract.Interrupted, "interrupted (Ctrl-C): %s", ce.Message).WithDetails(details)
	out.Err = ce
	return out
}

func commandName(cmd *cobra.Command) string {
	if cmd == nil || !cmd.HasParent() {
		return "ghostbranch"
	}
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

func (a *app) render(err error) int {
	// The spinner line goes before anything else is written: warnings, the
	// result, the error.
	a.reporter().Finish(err != nil)
	err = a.permissionMissing(err)
	if a.eng != nil {
		a.warnings = append(a.warnings, a.eng.LedgerWarnings()...)
	}
	a.awaitLook() // whoever reads the result, so a look that lands in time is saved
	if a.jsonOut {
		if n := a.updateNotice(updatecheck.Agent); n != nil {
			a.warnings = append(a.warnings, n.Warning())
		}
	}
	next := a.next(err)
	env := contract.NewEnvelope(a.command, a.result, a.warnings, next, err)
	if a.jsonOut {
		enc := json.NewEncoder(a.env.Stdout)
		enc.SetEscapeHTML(false)
		if !a.ndjson {
			enc.SetIndent("", "  ")
		}
		_ = enc.Encode(env)
		return env.ExitCode
	}
	outSt := a.styler(a.env.StdoutColor)
	errSt := a.styler(a.env.StderrColor)
	for _, w := range a.warnings {
		_, _ = fmt.Fprintf(a.env.Stderr, "%s %s\n", errSt.Warn("warning:"), w.Message)
	}
	if a.result != nil {
		_, _ = fmt.Fprint(a.env.Stdout, withoutNothing(human(a.result, outSt), env.Error != nil))
	}
	if env.Error != nil {
		msg := env.Error.Message
		if env.Error.Code == contract.AgentTokenRefused {
			msg = errSt.Fail(msg)
		}
		_, _ = fmt.Fprintf(a.env.Stderr, "%s %s [%s]\n", errSt.Fail("error:"), msg, env.Error.Code)
		printSteps(a.env.Stderr, env.Error)
		printNext(a.env.Stderr, next, errSt)
	} else {
		printNext(a.env.Stdout, next, outSt)
	}
	if a.env.StderrTerminal {
		if n := a.updateNotice(updatecheck.Person); n != nil {
			printReminder(a.env.Stderr, *n, errSt)
		}
	}
	return env.ExitCode
}

// permissionMissing turns a FORGE_ERROR or INTERNAL whose cause is a
// *forge.PermissionError into TOKEN_PERMISSION_MISSING, naming the
// permission, the repo and this command. It runs in render, which every
// command's error passes through exactly once, and render is the only place
// that knows the command's name. Codes the engine chose on purpose from a
// refusal (SHADOW_CREATE_DENIED, TOKEN_CANNOT_PUBLISH, ...) are left alone.
// It converts only errors the engine left generic: one the engine shaped on
// purpose (feature close's "close it by hand") drops the PermissionError from
// its chain and keeps its own code.
func (a *app) permissionMissing(err error) error {
	ce := contract.From(err)
	if ce == nil || (ce.Code != contract.ForgeError && ce.Code != contract.Internal) {
		return err
	}
	var pe *forge.PermissionError
	if !errors.As(err, &pe) {
		return err
	}
	where := ""
	if pe.Repo != "" {
		where = " on " + pe.Repo
	}
	out := contract.New(contract.TokenPermissionMissing,
		"the token lacks %s%s, which ghostbranch %s needs; a person grants it, and ghostbranch doctor lists every missing permission",
		pe.Permission, where, a.command).
		WithDetails(map[string]any{"permission": pe.Permission, "repo": pe.Repo, "command": a.command})
	out.Err = err
	return out
}

// printSteps lists an error's details.steps, numbered, for people. The JSON
// output carries them in error.details.steps.
func printSteps(w io.Writer, e *contract.ErrorBody) {
	steps, _ := e.Details["steps"].([]string)
	for i, s := range steps {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", i+1, s)
	}
}

func printNext(w io.Writer, next []contract.Next, st style.Styler) {
	if len(next) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, st.Bold("Next:"))
	for _, n := range next {
		line := shellJoin(n.Command)
		for _, in := range n.NeedsInput {
			if strings.Contains(in, "<") { // a positional argument or a flag with its placeholder, already named
				line += " " + in
				continue
			}
			line += " " + in + " <" + strings.TrimLeft(in, "-") + ">"
		}
		who := ""
		if n.NeedsHuman {
			who = " (a person must run this)"
		}
		_, _ = fmt.Fprintf(w, "  %s\n    %s\n", st.Bold(line+who), st.Dim(n.Why))
	}
}

// shellJoin quotes arguments with spaces or quotes, for display only.
func shellJoin(argv []string) string {
	out := make([]string, len(argv))
	for i, s := range argv {
		if s == "" || strings.ContainsAny(s, " \t\"'") {
			s = `"` + strings.ReplaceAll(s, `"`, `\"`) + `"`
		}
		out[i] = s
	}
	return strings.Join(out, " ")
}

// done records a command's result. r may be nil.
func done[T any](a *app, r *T, w []contract.Warning, err error) error {
	if r != nil {
		a.result = r
	}
	a.warnings = append(a.warnings, w...)
	if err != nil {
		return contract.From(err)
	}
	return nil
}

var actorPattern = regexp.MustCompile(`^(human:\S+|app|agent:\S+)$`)

// actor validates GHOSTBRANCH_ACTOR before anything else runs; "" means
// unset. The actor is a ledger label only, never part of approval.
func (a *app) actor() (string, error) {
	actor := strings.TrimSpace(a.env.Getenv("GHOSTBRANCH_ACTOR"))
	if actor != "" && !actorPattern.MatchString(actor) {
		return "", contract.New(contract.BadFlag, "GHOSTBRANCH_ACTOR must be human:<name>, app, or agent:<name>").WithField("GHOSTBRANCH_ACTOR")
	}
	return actor, nil
}

// userHome is Env.UserHome, else os.UserHomeDir: tests set the seam so
// that none reads the real state directory.
func (a *app) userHome() func() (string, error) {
	if a.env.UserHome != nil {
		return a.env.UserHome
	}
	return os.UserHomeDir
}

// openWorkspace opens this run's workspace. It needs no token or network.
func (a *app) openWorkspace() (*workspace.Workspace, error) {
	home, err := workspace.HomeDir(a.env.Getenv, runtime.GOOS, a.userHome())
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "find the state directory")
	}
	ws, err := workspace.Open(home, a.workspaceName())
	if err != nil {
		return nil, err
	}
	ws.OwnHome = a.env.Getenv("GHOSTBRANCH_HOME") == ""
	return ws, nil
}

// workspaceName is the workspace this run uses: --workspace, else
// $GHOSTBRANCH_WORKSPACE, else "default".
func (a *app) workspaceName() string {
	name := a.workspace
	if name == "" {
		name = a.env.Getenv("GHOSTBRANCH_WORKSPACE")
	}
	if name == "" {
		name = "default"
	}
	return name
}

// oauthBase is GitHub's web origin for login and refresh.
func (a *app) oauthBase() string {
	if a.env.OAuthBaseURL == "" {
		return "https://github.com"
	}
	return strings.TrimRight(a.env.OAuthBaseURL, "/")
}

// deviceFlow is the GitHub App device flow; its waits end early on Ctrl-C.
func (a *app) deviceFlow(ctx context.Context) auth.DeviceFlow {
	return auth.DeviceFlow{ClientID: a.env.ClientID, BaseURL: a.oauthBase(), HTTP: a.env.HTTPClient, Now: a.env.Now,
		Sleep: func(d time.Duration) { _ = a.env.Sleep(ctx, d) }}
}

// authSource is where this run's token comes from. A refresh takes
// <GHOSTBRANCH_HOME>/locks/auth.lock: the stored login is one per OS user,
// shared by every workspace, so the lock is too.
func (a *app) authSource() auth.Source {
	return auth.Source{
		Getenv: a.env.Getenv, GH: a.env.GHToken, Store: a.env.Keychain, Now: a.env.Now,
		// GitHub rotates the refresh token once it answers, so a refresh is
		// never cut short: a dropped reply would leave only the spent pair.
		// The HTTP client's 30 s timeout still bounds it.
		Refresh: func(ctx context.Context, l auth.Login) (auth.Login, error) {
			ctx = context.WithoutCancel(ctx)
			return a.deviceFlow(ctx).Refresh(ctx, l.RefreshToken)
		},
		// The error is never shown: it could carry a secret.
		OnSaveError: func(error) {
			a.reporter().Note(progress.Warn, "the refreshed GitHub login couldn't be saved to the OS keychain; the next command will ask you to log in again")
		},
		Lock: func() (func(), error) {
			home, err := workspace.HomeDir(a.env.Getenv, runtime.GOOS, a.userHome())
			if err != nil {
				return nil, contract.Wrap(contract.Internal, err, "find the state directory")
			}
			return auth.FileLock(filepath.Join(home, "locks", "auth.lock"), a.env.Now, auth.LockWait)()
		},
	}
}

func (a *app) engine(ctx context.Context) (*engine.Engine, error) {
	a.ran = true
	actor, err := a.actor()
	if err != nil {
		return nil, err
	}
	token, source, err := auth.Resolve(ctx, a.authSource())
	if err != nil {
		return nil, err
	}
	f, err := a.env.NewForge(token)
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "create the GitHub client")
	}
	ws, err := a.openWorkspace()
	if err != nil {
		return nil, err
	}
	if actor == "" {
		actor = "human:unknown"
		if u, err := f.CurrentUser(ctx); err == nil {
			actor = "human:" + u.Login
		} else {
			a.warnings = append(a.warnings, contract.Warning{Code: contract.ActorUnknown,
				Message: "GHOSTBRANCH_ACTOR is unset and the GitHub login could not be read, so the ledger records human:unknown; set GHOSTBRANCH_ACTOR, for example agent:claude-code"})
		}
	}
	e := &engine.Engine{Forge: f, WS: ws, Actor: actor, Token: token, TokenSource: source,
		LooksLikeAgent: auth.LooksLikeAgent(a.env.Getenv, a.env.ConsoleUsable), Now: a.env.Now, Sleep: a.env.Sleep, Rand: a.env.Rand, Progress: a.reporter(),
		NewScanner: a.env.NewScanner, PollInterval: a.env.PollInterval, SettleWindow: a.env.SettleWindow, CIAppearWindow: a.env.CIAppearWindow, CIAppearPoll: a.env.CIAppearPoll,
		DryRun: a.dryRun, OnPush: a.env.OnPush, Grants: grant.NewStore(ws.GrantsPath(), ws.GrantsLockPath()),
		Waivers: waiver.NewStore(ws.WaiversPath(), ws.WaiversLockPath())}
	if source == "login" && a.env.Keychain != nil {
		if l, err := auth.LoadLogin(a.env.Keychain); err == nil && l != nil {
			e.AppLogin = l.Kind == "app"
		}
	}
	if a.env.ConsoleUsable && !a.nonInteractive {
		e.Editor = func(s string) (string, error) {
			a.reporter().Pause()
			defer a.reporter().Resume()
			return a.editor(s)
		}
		if a.env.OpenConsole != nil {
			e.Approver = pausingApprover{inner: &console.Challenger{Open: a.env.OpenConsole, Rand: a.env.Rand}, p: a.reporter()}
		}
	}
	a.eng = e
	return e, nil
}

func (a *app) pair(e *engine.Engine) (workspace.Pair, error) {
	p, err := e.WS.Resolve(a.repo, a.env.CwdRemotes())
	if err == nil {
		a.pairFull = p
	}
	return p, err
}

// sleepCtx sleeps d, or returns early with ctx's error.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
