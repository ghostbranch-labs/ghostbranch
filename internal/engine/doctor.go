package engine

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/config"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// Doctor check statuses.
const (
	DoctorOK      = "ok"
	DoctorWarn    = "warn"
	DoctorFail    = "fail"
	DoctorSkipped = "skipped"
)

// DoctorCheckIDs are doctor's checks, in the order it runs and reports them.
var DoctorCheckIDs = []string{"tools", "token", "pair", "guard", "state", "ci"}

// The oldest git Ghostbranch supports: drift and the gate use git merge-tree
// --write-tree, new in 2.38. (The guard alone needs 2.31.)
const (
	MinGitMajor = 2
	MinGitMinor = 38
)

// DoctorOptions is what doctor needs from outside the engine; the CLI fills
// it. Doctor also reads e.Token and e.TokenSource ("" when none resolved),
// e.Forge (nil then), e.LooksLikeAgent, and e.WS (nil when the workspace
// couldn't be opened).
type DoctorOptions struct {
	// Repo is --repo. Empty means the current checkout's pair; never the
	// workspace's only pair, so outside a checkout the pair checks skip.
	Repo    string
	Remotes []workspace.Remote // the current checkout's remotes
	Dir     string             // the current directory; "" when unknown
	// GitVersion is `git version`'s output, or GitErr why it didn't run.
	GitVersion string
	GitErr     error
	// Version and Commit are this binary's build.
	Version, Commit string
	// TokenErr is why no token resolved; the stored login is never
	// refreshed for doctor, since it writes nothing.
	TokenErr error
	// Login is the stored login when the token came from it, else nil.
	Login *auth.Login
	// WorkspaceErr is why the workspace couldn't be opened.
	WorkspaceErr error
	Getenv       func(string) string
}

// DoctorCheck is one check's finding. Fix is a command that fixes it, or
// "" when there is none to run; FixArgv is that command as argv (nil with
// no fix), for next. Fix is FixArgv joined with spaces, for reading only.
type DoctorCheck struct {
	ID      string         `json:"id"`
	Status  string         `json:"status"`
	Message string         `json:"message"`
	Fix     string         `json:"fix"`
	FixArgv []string       `json:"-"`
	Details map[string]any `json:"details"`
}

// withFix sets c's fix to argv; no argv leaves it without one.
func (c DoctorCheck) withFix(argv ...string) DoctorCheck {
	if len(argv) > 0 {
		c.FixArgv, c.Fix = argv, strings.Join(argv, " ")
	}
	return c
}

// DoctorResult is doctor's data: every check, in DoctorCheckIDs order.
type DoctorResult struct {
	Checks []DoctorCheck `json:"checks"`
	// UpdateCheck is the update checker's state from a fresh look (6.4a).
	// The CLI sets it; the engine never looks.
	UpdateCheck *updatecheck.Status `json:"update_check,omitempty"`
}

// DoctorSummary counts a result's checks by status; guided init reports it.
type DoctorSummary struct {
	OK      int      `json:"ok"`
	Warn    int      `json:"warn"`
	Fail    int      `json:"fail"`
	Skipped int      `json:"skipped"`
	Failed  []string `json:"failed"`
}

// Failed is the IDs of the failing checks, never nil.
func (r *DoctorResult) Failed() []string {
	out := []string{}
	for _, c := range r.Checks {
		if c.Status == DoctorFail {
			out = append(out, c.ID)
		}
	}
	return out
}

// Summary counts r's checks by status.
func (r *DoctorResult) Summary() DoctorSummary {
	s := DoctorSummary{Failed: r.Failed()}
	for _, c := range r.Checks {
		switch c.Status {
		case DoctorOK:
			s.OK++
		case DoctorWarn:
			s.Warn++
		case DoctorFail:
			s.Fail++
		case DoctorSkipped:
			s.Skipped++
		}
	}
	return s
}

// doctorRun is one run's shared findings: later checks read what the
// earlier ones found.
type doctorRun struct {
	e            *Engine
	opt          DoctorOptions
	pair         *workspace.Pair
	unregistered string // --repo names this public repo, which isn't set up here
	noPair       string // why there is no pair, as a skip reason
	pc           *pairCtx
	cfg          config.Config
	O            string
}

// Doctor checks this machine's setup and, when there is one, the pair. It
// writes nothing: no record, ledger entry, lock, keychain entry, push or
// forge write. It only fetches into the local mirror, as status does.
// Every check reports, whatever the others found; it stops early only on
// Ctrl-C, with the checks so far and INTERRUPTED.
func (e *Engine) Doctor(ctx context.Context, opt DoctorOptions) (*DoctorResult, error) {
	if opt.Getenv == nil {
		opt.Getenv = func(string) string { return "" }
	}
	d := &doctorRun{e: e, opt: opt}
	defer func() {
		if d.pc != nil {
			d.pc.unlock()
		}
	}()
	d.findPair()
	run := map[string]func(context.Context) DoctorCheck{
		"tools": d.tools, "token": d.token, "pair": d.pairCheck, "guard": d.guard, "state": d.state, "ci": d.ci,
	}
	res := &DoctorResult{Checks: []DoctorCheck{}}
	for _, id := range DoctorCheckIDs {
		if err := ctx.Err(); err != nil {
			return res, contract.Wrap(contract.Interrupted, err, "doctor was interrupted after %d of %d checks", len(res.Checks), len(DoctorCheckIDs))
		}
		res.Checks = append(res.Checks, runCheck(ctx, id, run[id]))
	}
	return res, nil
}

// runCheck runs one check. A panic becomes that check's fail, so one bug
// never hides the other checks.
func runCheck(ctx context.Context, id string, run func(context.Context) DoctorCheck) (c DoctorCheck) {
	defer func() {
		if r := recover(); r != nil {
			c = DoctorCheck{Status: DoctorFail, Message: fmt.Sprintf("the %s check stopped on an internal error (%v); please report it", id, r)}
		}
		c.ID = id
		if c.Details == nil {
			c.Details = map[string]any{}
		}
	}()
	return run(ctx)
}

func skipped(why string) DoctorCheck { return DoctorCheck{Status: DoctorSkipped, Message: why} }

// verdict gathers one check's problems: the worst status wins, and the
// check's fix is the first fix given at that status.
type verdict struct {
	status string // "" while nothing is wrong
	notes  []string
	fix    []string
}

// add notes one problem; fix is the argv that fixes it, if any.
func (v *verdict) add(status, note string, fix ...string) {
	v.notes = append(v.notes, note)
	switch {
	case v.status == "" || (v.status == DoctorWarn && status == DoctorFail):
		v.status, v.fix = status, fix
	case status == v.status && len(v.fix) == 0:
		v.fix = fix
	}
}

// result is the check: lead+okTail when nothing is wrong, else lead and
// each problem.
func (v *verdict) result(lead, okTail string, det map[string]any) DoctorCheck {
	if v.status == "" {
		return DoctorCheck{Status: DoctorOK, Message: lead + okTail, Details: det}
	}
	msg := strings.Join(v.notes, "; ")
	if lead != "" {
		msg = lead + "; " + msg
	}
	return DoctorCheck{Status: v.status, Message: msg, Details: det}.withFix(v.fix...)
}

func day(t time.Time) string { return t.UTC().Format("2006-01-02") }

var gitVersionPattern = regexp.MustCompile(`git version (\d+)\.(\d+)((?:\.\d+)?)`)

func (d *doctorRun) tools(context.Context) DoctorCheck {
	minGit := fmt.Sprintf("%d.%d", MinGitMajor, MinGitMinor)
	det := map[string]any{"git": "", "git_minimum": minGit, "version": d.opt.Version, "commit": d.opt.Commit}
	self := fmt.Sprintf("ghostbranch %s, commit %s", d.opt.Version, d.opt.Commit)
	if d.opt.GitErr != nil {
		return DoctorCheck{Status: DoctorFail, Details: det,
			Message: fmt.Sprintf("git couldn't be run (%v): install git %s or later and put it on PATH; %s", d.opt.GitErr, minGit, self)}
	}
	m := gitVersionPattern.FindStringSubmatch(d.opt.GitVersion)
	if m == nil {
		return DoctorCheck{Status: DoctorFail, Details: det,
			Message: fmt.Sprintf("can't read git's version from %q; Ghostbranch needs git %s or later; %s", d.opt.GitVersion, minGit, self)}
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	v := m[1] + "." + m[2] + m[3]
	det["git"] = v
	if major < MinGitMajor || (major == MinGitMajor && minor < MinGitMinor) {
		return DoctorCheck{Status: DoctorFail, Details: det,
			Message: fmt.Sprintf("git %s is too old: Ghostbranch needs git %s or later; %s", v, minGit, self)}
	}
	return DoctorCheck{Status: DoctorOK, Details: det, Message: fmt.Sprintf("git %s; %s", v, self)}
}
