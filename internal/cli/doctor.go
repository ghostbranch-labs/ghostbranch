package cli

import (
	"context"
	"errors"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/gitx"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/updatecheck"
	"github.com/ghostbranch-labs/ghostbranch/internal/waiver"
)

func (a *app) doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check this machine's setup and the pair, and name each fix; changes nothing",
		Long: `doctor runs six checks, in order: tools (git and this binary), token (its source, account,
expiry, whether it can publish, and the permissions each command group needs), pair (both repos,
the overlay and its config, shadow main, the managed files), guard (the pre-push hook in this
checkout), state (this machine's records) and ci (the audit's summary). Each is ok, warn, fail or
skipped, and names the command that fixes it. It changes nothing in your repos, records or
keychain and never prompts; it fetches into its local copy of the pair (making it if needed) and
refreshes the update check's cache. It isn't ledgered.
In a checkout of a pair it checks that pair; --repo names another; with neither it skips the
checks that need a pair. It exits 0 unless a check fails (DOCTOR_FAILED, details.failed).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// The fresh look runs here, not in doctorRun: guided init calls
			// doctorRun too and must not pay up to the look's budget (F5).
			looked := make(chan updatecheck.Status, 1)
			go func() { looked <- a.doctorUpdateCheck(cmd.Context()) }()
			res, err := a.doctorRun(cmd.Context(), a.repo)
			st := a.waitForDoctorLook(looked) // always received: orders the goroutine's write of a.updates before render reads it
			if res != nil {
				res.UpdateCheck = &st
				a.result = res
				a.warnings = append(a.warnings, doctorWarnings(res)...)
			}
			if err != nil {
				return err
			}
			if failed := res.Failed(); len(failed) > 0 {
				return contract.New(contract.DoctorFailed, "%d check(s) failed: %s", len(failed), strings.Join(failed, ", ")).
					WithDetails(map[string]any{"failed": failed})
			}
			return nil
		},
	}
}

// waitForDoctorLook waits for doctor's own look, showing a step while it is still
// running (up to the look's budget) so doctor never looks hung.
func (a *app) waitForDoctorLook(looked <-chan updatecheck.Status) updatecheck.Status {
	select {
	case st := <-looked:
		return st
	default:
	}
	step := a.reporter().Step("Looking for a newer Ghostbranch")
	st := <-looked
	step.Done("")
	return st
}

// doctorWarnings is AGENT_ON_PERSON_LOGIN when the token check found a
// likely agent on the person's stored login.
func doctorWarnings(r *engine.DoctorResult) []contract.Warning {
	for _, c := range r.Checks {
		if c.ID == "token" && c.Details["agent_on_person_login"] == true {
			return []contract.Warning{{Code: contract.AgentOnPersonLogin, Message: engine.AgentOnLoginMessage}}
		}
	}
	return nil
}

// doctorRun gathers what doctor needs without failing on a missing token or
// an unreadable workspace, and runs it. It never refreshes the stored
// login and reads the keychain only when the token came from it.
func (a *app) doctorRun(ctx context.Context, repo string) (*engine.DoctorResult, error) {
	a.ran = true
	actor, err := a.actor()
	if err != nil {
		return nil, err
	}
	opt := engine.DoctorOptions{Repo: repo, Getenv: a.env.Getenv, ForPerson: !a.jsonOut}
	if a.env.CwdRemotes != nil {
		opt.Remotes = a.env.CwdRemotes()
	}
	if a.env.Getwd != nil {
		if dir, err := a.env.Getwd(); err == nil {
			opt.Dir = dir
		}
	}
	gitVersion := a.env.GitVersion
	if gitVersion == nil {
		gitVersion = gitx.Version
	}
	opt.GitVersion, opt.GitErr = gitVersion(ctx)
	opt.Version, opt.Commit = a.build()
	src := a.authSource()
	src.Refresh, src.Lock = nil, nil // doctor writes nothing, the keychain included
	token, source, terr := auth.Resolve(ctx, src)
	opt.TokenErr = terr
	if source == "login" { // never when GHOSTBRANCH_TOKEN is set: it comes first
		opt.Login, _ = auth.LoadLogin(a.env.Keychain)
	}
	// DryRun: every engine write path refuses a dry run. Doctor makes none;
	// this keeps it so.
	e := &engine.Engine{Actor: actor, Token: token, TokenSource: source,
		LooksLikeAgent: auth.LooksLikeAgent(a.env.Getenv, a.env.ConsoleUsable), Now: a.env.Now, Sleep: a.env.Sleep, Rand: a.env.Rand, DryRun: true,
		Progress: a.reporter()}
	if token != "" {
		f, err := a.env.NewForge(token)
		if err != nil {
			e.Token, opt.TokenErr = "", contract.Wrap(contract.Internal, err, "create the GitHub client")
		} else {
			e.Forge = f
		}
	}
	if ws, err := a.openWorkspace(); err != nil {
		opt.WorkspaceErr = err
	} else {
		e.WS = ws
		e.Grants = grant.NewStore(ws.GrantsPath(), ws.GrantsLockPath())
		e.Waivers = waiver.NewStore(ws.WaiversPath(), ws.WaiversLockPath())
	}
	return e.Doctor(ctx, opt)
}

// buildInfo is this binary's module version and VCS commit, from the Go
// build info; stampedVersion, when set, replaces the version.
func buildInfo() (version, commit string) {
	version, commit = "(unknown)", "(unknown)"
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			version = bi.Main.Version
		}
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				commit = s.Value
				if len(commit) > 12 {
					commit = commit[:12]
				}
			}
		}
	}
	if stampedVersion != "" {
		version = stampedVersion // R10: a release build's link-time stamp
	}
	return version, commit
}

// doctorNext is the failing checks' fixes, in check order, each once, from
// each check's FixArgv. Logging in or out and installing the guard are a
// person's to run.
func (a *app) doctorNext() []contract.Next {
	r, ok := a.result.(*engine.DoctorResult)
	if !ok {
		return nil
	}
	var out []contract.Next
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c.Status == engine.DoctorFail && c.Unregistered != "" {
			if key := "init\x00" + c.Unregistered; !seen[key] {
				seen[key] = true
				if !a.jsonOut && len(c.PersonFixArgv) > 1 { // a person runs plain init
					out = append(out, contract.Next{Command: a.gb(c.PersonFixArgv[1:]...), NeedsHuman: true, Why: c.ID + ": " + c.PersonMessage})
				} else {
					out = append(out, a.initNexts(c.Unregistered)...)
				}
			}
			continue
		}
		argv := c.FixArgv
		key := strings.Join(argv, "\x00")
		if c.Status != engine.DoctorFail || len(argv) == 0 || seen[key] {
			continue
		}
		seen[key] = true
		human := false
		if argv[0] == "ghostbranch" {
			sub := argv[1:]
			human = len(sub) > 0 && (sub[0] == "login" || sub[0] == "logout" || (sub[0] == "guard" && len(sub) > 1 && sub[1] == "install"))
			argv = a.gb(sub...)
		} else {
			argv = append([]string(nil), argv...)
		}
		out = append(out, contract.Next{Command: argv, NeedsHuman: human, Why: c.ID + ": " + c.Message})
	}
	return out
}

// doctorUpdateCheck runs a fresh look within the budget, whatever the
// schedule says, saves it without touching the reminder times, and keeps it
// for the end-of-command reminder (R11).
func (a *app) doctorUpdateCheck(ctx context.Context) updatecheck.Status {
	u := a.newUpdateRun()
	a.updates = u
	_, u.override, _ = updatecheck.BaseURL(a.env.Getenv) // shown even when the check is off
	if u.off == "" {
		if c, ok := a.updateClient(u); ok {
			r := updatecheck.Look(ctx, c, u.current, a.env.Now, updatecheck.Budget)
			// A Ctrl-C'd look is not a failed one: record nothing, so the
			// schedule and the last result stay as they were.
			if ctx.Err() == nil && !errors.Is(r.Err, context.Canceled) {
				u.mu.Lock()
				u.cache.Merge(r, u.current)           // r.Err is stored cleaned, never printed here
				_ = updatecheck.Save(u.path, u.cache) // a cache: a failed write is silent
				u.mu.Unlock()
			}
		}
	}
	page := "https://github.com/" + updatecheck.Repo + "/releases/latest"
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cache.Latest != nil {
		if v, ok := updatecheck.ParseRelease(u.cache.Latest.Version); ok { // nothing from the cache file unchecked
			page = updatecheck.ReleaseURL("v" + v.String())
		}
	}
	return updatecheck.StatusOf(u.off, u.current, u.cache, u.override, a.upgradeCommand(page), a.env.Now())
}
