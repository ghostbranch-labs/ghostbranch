package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// defaultUpdateWait is the most a command waits at its end for a look in
// flight (spec 6.4a § 1, decision 3).
const defaultUpdateWait = 500 * time.Millisecond

// updateRun is this run's update check: whether it is off, the cache, and a
// look in flight. mu guards cache: the look's goroutine and render both
// merge into it and save it.
type updateRun struct {
	mu       sync.Mutex
	off      string
	current  updatecheck.Version
	path     string
	cache    updatecheck.Cache
	override string
	done     chan struct{} // closed when the look finishes; nil when none started
	waited   bool          // awaitLook ran: the end-of-command wait is spent
}

// build is this binary's version and commit; "(unknown)" for both without
// Env.Build. version, --version, doctor and the update check all use it.
func (a *app) build() (string, string) {
	if a.env.Build == nil {
		return "(unknown)", "(unknown)"
	}
	return a.env.Build()
}

// skipsUpdateCheck is true for help, shell completion, the bare root showing
// help, doctor, which looks itself (R11, R12), and guard check: the pre-push
// hook runs inside every git push, where a reminder is noise and a look a
// delay; the person's next command reminds instead.
func skipsUpdateCheck(cmd *cobra.Command, showVersion bool) bool {
	if !cmd.HasParent() {
		return !showVersion
	}
	for c := cmd; c != nil; c = c.Parent() {
		switch name := c.Name(); {
		case name == "help", name == "completion", strings.HasPrefix(name, "__complete"):
			return true
		}
	}
	if cmd.Name() == "check" && cmd.Parent() != nil && cmd.Parent().Name() == "guard" {
		return true
	}
	return cmd.Name() == "doctor" && cmd.Parent() != nil && !cmd.Parent().HasParent()
}

// newUpdateRun loads what this run needs to remind: off and why, or the
// cache. It never fails.
func (a *app) newUpdateRun() *updateRun {
	v, _ := a.build()
	u := &updateRun{}
	if u.off = updatecheck.OffReason(a.env.Getenv, v); u.off != "" {
		return u
	}
	u.current, _ = updatecheck.ParseRelease(v)
	home, err := workspace.HomeDir(a.env.Getenv, runtime.GOOS, a.userHome()) // the same seam the skills use
	if err != nil {
		u.off = "no state directory"
		return u
	}
	u.path = filepath.Join(home, updatecheck.CacheFile)
	u.cache = updatecheck.Load(u.path)
	return u
}

// updateClient is the anonymous client for this run, or false when the
// GHOSTBRANCH_UPDATE_URL override is malformed.
func (a *app) updateClient(u *updateRun) (updatecheck.Client, bool) {
	base, override, err := updatecheck.BaseURL(a.env.Getenv)
	u.override = override
	if err != nil {
		u.cache.LastError = err.Error()
		return updatecheck.Client{}, false
	}
	return updatecheck.Client{BaseURL: base, HTTP: updatecheck.NewHTTPClient()}, true
}

// startUpdateCheck starts a look in the background when one is due (spec
// § 3.3). The look saves its result as soon as it lands.
//
// A look still in flight when the command ends (render waits at most
// UpdateWait) dies with the process and is not saved; the next command
// makes it again (F14, a recorded spec deviation). Only in tests, where the
// process lives on, does it finish and save after Run returns.
func (a *app) startUpdateCheck(ctx context.Context) {
	u := a.newUpdateRun()
	a.updates = u
	if u.off != "" || !updatecheck.Due(u.cache, u.current, a.env.Now()) {
		return
	}
	c, ok := a.updateClient(u)
	if !ok {
		return
	}
	done := make(chan struct{})
	u.done = done
	go func() {
		defer close(done)
		// Not cut short by Ctrl-C: the budget bounds it, and render waits
		// at most UpdateWait anyway.
		r := updatecheck.Look(context.WithoutCancel(ctx), c, u.current, a.env.Now, updatecheck.Budget)
		u.mu.Lock()
		defer u.mu.Unlock()
		u.cache.Merge(r, u.current)           // r.Err is stored cleaned, never printed here
		_ = updatecheck.Save(u.path, u.cache) // a cache: a failed write is silent
	}()
}

// awaitLook is the end-of-command wait (spec § 1, decision 3): up to
// UpdateWait for a look in flight, once per run, whether or not a reminder
// will be shown, so a look that lands in time is saved. render calls it on
// the main goroutine only.
func (a *app) awaitLook() {
	u := a.updates
	if u == nil || u.done == nil || u.waited {
		return
	}
	u.waited = true
	wait := a.env.UpdateWait
	if wait <= 0 {
		wait = defaultUpdateWait
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-u.done:
	case <-t.C:
	}
}

// updateNotice waits for a look in flight (awaitLook), then returns the
// reminder due for aud and records that it was given; nil when none is due.
func (a *app) updateNotice(aud updatecheck.Audience) *updatecheck.Notice {
	a.awaitLook()
	u := a.updates
	if u == nil || u.off != "" || u.path == "" {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	now := a.env.Now()
	n := updatecheck.NoticeFor(u.cache, u.current, aud, now)
	if n == nil {
		return nil
	}
	n.Upgrade = a.upgradeCommand(n.URL)
	u.cache.Reminded(aud, now)
	_ = updatecheck.Save(u.path, u.cache) // a cache: a failed write is silent
	return n
}

// upgradeCommand is the upgrade command for how this binary was installed,
// or page.
func (a *app) upgradeCommand(page string) string {
	exe := ""
	if a.env.Executable != nil {
		if p, err := a.env.Executable(); err == nil {
			exe = p
			if r, err := filepath.EvalSymlinks(p); err == nil {
				exe = r
			}
		}
	}
	home := ""
	if a.env.UserHome != nil {
		home, _ = a.env.UserHome()
	}
	return updatecheck.UpgradeCommand(exe, updatecheck.InstallEnv{Getenv: a.env.Getenv, Home: home, Module: updatecheck.Module()}, page)
}

// printReminder is the person's reminder, after the command's own output
// (spec § 3.5): a yellow "!" for an ordinary update, a red "✗" for a
// security fix.
func printReminder(w io.Writer, n updatecheck.Notice, st style.Styler) {
	mark, first, second := n.Lines()
	color := st.Warn
	if n.Security {
		color = st.Fail
	}
	_, _ = fmt.Fprintf(w, "%s %s\n  %s\n", color(mark), first, second)
}
