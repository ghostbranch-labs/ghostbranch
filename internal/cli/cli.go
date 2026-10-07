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
	"os"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/console"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
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
	// UserHome is the home directory user-level skills go under.
	UserHome func() (string, error)
	// ConsoleUsable is console.Usable(os.Stdin, os.Stderr): stdin and stderr
	// are real terminals, so a person could be asked at the console.
	ConsoleUsable bool
	// OpenConsole opens the console device for the approval code.
	OpenConsole  func() (io.ReadWriteCloser, error)
	Now          func() time.Time
	Sleep        func(time.Duration)
	Rand         io.Reader
	NewForge     func(token string) (forge.Forge, error)
	GHToken      func() (string, error)
	CwdRemotes   func() []string
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
}

// DefaultEnv is the real process environment.
func DefaultEnv() Env {
	return Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv, UserHome: os.UserHomeDir,
		ConsoleUsable: console.Usable(os.Stdin, os.Stderr), OpenConsole: console.Open,
		Now: time.Now, Sleep: time.Sleep, Rand: rand.Reader,
		NewForge:     func(token string) (forge.Forge, error) { return ghforge.New(token), nil },
		GHToken:      auth.GHCLI,
		CwdRemotes:   cwdRemotes,
		NewScanner:   func(r leak.Rules) (leak.Scanner, error) { return leak.NewGitleaks(r) },
		PollInterval: engine.DefaultPollInterval,
		SettleWindow: engine.DefaultSettleWindow, CIAppearWindow: engine.DefaultCIAppearWindow, CIAppearPoll: engine.DefaultCIAppearPoll,
		Executable:   os.Executable,
		Getwd:        os.Getwd,
		GuardState:   guardState,
		ShadowRemote: shadowRemote,
	}
}

type app struct {
	env            Env
	jsonOut        bool
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
	}
	root := a.root()
	root.SetArgs(args)
	root.SetOut(env.Stdout)
	root.SetErr(env.Stderr)
	cmd, err := root.ExecuteContextC(context.Background())
	a.command = commandName(cmd)
	if err != nil {
		var ce *contract.Error
		if !errors.As(err, &ce) {
			if a.ran {
				err = contract.From(err) // plumbing failure after arg validation
			} else {
				err = contract.New(contract.BadFlag, "%s", err.Error())
			}
		}
	} else if !a.ran {
		return contract.ExitOK // help output only
	}
	return a.render(err)
}

func commandName(cmd *cobra.Command) string {
	if cmd == nil || !cmd.HasParent() {
		return "ghostbranch"
	}
	return strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
}

func (a *app) render(err error) int {
	next := a.next(err)
	env := contract.NewEnvelope(a.command, a.result, a.warnings, next, err)
	if a.jsonOut {
		enc := json.NewEncoder(a.env.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		_ = enc.Encode(env)
		return env.ExitCode
	}
	for _, w := range a.warnings {
		_, _ = fmt.Fprintf(a.env.Stderr, "warning: %s\n", w.Message)
	}
	if a.result != nil {
		_, _ = fmt.Fprint(a.env.Stdout, human(a.result))
	}
	if env.Error != nil {
		_, _ = fmt.Fprintf(a.env.Stderr, "error: %s [%s]\n", env.Error.Message, env.Error.Code)
		printSteps(a.env.Stderr, env.Error)
		printNext(a.env.Stderr, next)
	} else {
		printNext(a.env.Stdout, next)
	}
	return env.ExitCode
}

// printSteps lists an error's details.steps, numbered, for people. The JSON
// output carries them in error.details.steps.
func printSteps(w io.Writer, e *contract.ErrorBody) {
	steps, _ := e.Details["steps"].([]string)
	for i, s := range steps {
		_, _ = fmt.Fprintf(w, "  %d. %s\n", i+1, s)
	}
}

func printNext(w io.Writer, next []contract.Next) {
	if len(next) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "Next:")
	for _, n := range next {
		line := shellJoin(n.Command)
		for _, in := range n.NeedsInput {
			line += " " + in + " <" + strings.TrimLeft(in, "-") + ">"
		}
		who := ""
		if n.NeedsHuman {
			who = " (a person must run this)"
		}
		_, _ = fmt.Fprintf(w, "  %s%s\n    %s\n", line, who, n.Why)
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

// openWorkspace opens this run's workspace. It needs no token or network.
func (a *app) openWorkspace() (*workspace.Workspace, error) {
	home, err := workspace.HomeDir(a.env.Getenv, runtime.GOOS, os.UserHomeDir)
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "find the state directory")
	}
	return workspace.Open(home, a.workspaceName())
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

func (a *app) engine(ctx context.Context) (*engine.Engine, error) {
	a.ran = true
	actor, err := a.actor()
	if err != nil {
		return nil, err
	}
	token, source, err := auth.Resolve(a.env.Getenv, a.env.GHToken)
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
		Now: a.env.Now, Sleep: a.env.Sleep, Rand: a.env.Rand, Progress: a.env.Stderr,
		NewScanner: a.env.NewScanner, PollInterval: a.env.PollInterval, SettleWindow: a.env.SettleWindow, CIAppearWindow: a.env.CIAppearWindow, CIAppearPoll: a.env.CIAppearPoll,
		DryRun: a.dryRun, OnPush: a.env.OnPush, Grants: grant.NewStore(ws.GrantsPath(), ws.GrantsLockPath()),
		Waivers: waiver.NewStore(ws.WaiversPath(), ws.WaiversLockPath())}
	if a.env.ConsoleUsable && !a.nonInteractive {
		e.Editor = a.editor
		if a.env.OpenConsole != nil {
			e.Approver = &console.Challenger{Open: a.env.OpenConsole, Rand: a.env.Rand}
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
