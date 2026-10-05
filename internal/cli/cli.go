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
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	ghforge "github.com/ghostbranch-labs/ghostbranch/internal/forge/github"
	"github.com/ghostbranch-labs/ghostbranch/internal/leak"
	"github.com/ghostbranch-labs/ghostbranch/internal/overlay"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Env is everything the CLI takes from the outside world, so tests can
// replace any of it.
type Env struct {
	Stdout       io.Writer
	Stderr       io.Writer
	Stdin        io.Reader
	Getenv       func(string) string
	StdinIsTTY   bool
	Now          func() time.Time
	Sleep        func(time.Duration)
	Rand         io.Reader
	NewForge     func(token string) (forge.Forge, error)
	GHToken      func() (string, error)
	CwdRemotes   func() []string
	NewScanner   func(overlay.Set) (leak.Scanner, error)
	PollInterval time.Duration
	SettleWindow time.Duration
	OnPush       func(op, remote string, refspecs []string)
}

// DefaultEnv is the real process environment.
func DefaultEnv() Env {
	return Env{
		Stdout: os.Stdout, Stderr: os.Stderr, Stdin: os.Stdin, Getenv: os.Getenv,
		StdinIsTTY: isTerminal(os.Stdin),
		Now:        time.Now, Sleep: time.Sleep, Rand: rand.Reader,
		NewForge:     func(token string) (forge.Forge, error) { return ghforge.New(token), nil },
		GHToken:      auth.GHCLI,
		CwdRemotes:   cwdRemotes,
		NewScanner:   func(s overlay.Set) (leak.Scanner, error) { return leak.NewGitleaks(s) },
		PollInterval: engine.DefaultPollInterval,
		SettleWindow: engine.DefaultSettleWindow,
	}
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
}

// Run executes one command and returns the process exit code.
func Run(args []string, env Env) int {
	a := &app{env: env}
	for _, arg := range args {
		if arg == "--json" {
			a.jsonOut = true // honoured even when flag parsing fails
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
	env := contract.NewEnvelope(a.command, a.result, a.warnings, err)
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
	}
	return env.ExitCode
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

func (a *app) engine(ctx context.Context) (*engine.Engine, error) {
	a.ran = true
	token, err := auth.Token(a.env.Getenv, a.env.GHToken)
	if err != nil {
		return nil, err
	}
	f, err := a.env.NewForge(token)
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "create the GitHub client")
	}
	home, err := workspace.HomeDir(a.env.Getenv, runtime.GOOS, os.UserHomeDir)
	if err != nil {
		return nil, contract.Wrap(contract.Internal, err, "find the state directory")
	}
	name := a.workspace
	if name == "" {
		name = a.env.Getenv("GHOSTBRANCH_WORKSPACE")
	}
	if name == "" {
		name = "default"
	}
	ws, err := workspace.Open(home, name)
	if err != nil {
		return nil, err
	}
	actor := strings.TrimSpace(a.env.Getenv("GHOSTBRANCH_ACTOR"))
	if actor != "" && !actorPattern.MatchString(actor) {
		return nil, contract.New(contract.BadFlag, "GHOSTBRANCH_ACTOR must be human:<name>, app, or agent:<name>").WithField("GHOSTBRANCH_ACTOR")
	}
	if actor == "" {
		actor = "human:unknown"
		if u, err := f.CurrentUser(ctx); err == nil {
			actor = "human:" + u.Login
		}
	}
	e := &engine.Engine{Forge: f, WS: ws, Actor: actor, Token: token,
		Now: a.env.Now, Sleep: a.env.Sleep, Rand: a.env.Rand, Progress: a.env.Stderr,
		NewScanner: a.env.NewScanner, PollInterval: a.env.PollInterval, SettleWindow: a.env.SettleWindow,
		DryRun: a.dryRun, OnPush: a.env.OnPush}
	if a.env.StdinIsTTY && !a.nonInteractive {
		e.Prompt, e.Editor = a.prompt, a.editor
	}
	return e, nil
}

func (a *app) pair(e *engine.Engine) (workspace.Pair, error) {
	return e.WS.Resolve(a.repo, a.env.CwdRemotes())
}
