package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/ghostbranch-labs/ghostbranch/internal/auth"
	"github.com/ghostbranch-labs/ghostbranch/internal/contract"
	"github.com/ghostbranch-labs/ghostbranch/internal/forge"
	"github.com/ghostbranch-labs/ghostbranch/internal/grant"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/plan"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// sourceLogin is auth.Resolve's source for the stored login.
const sourceLogin = "login"

// AgentOnLoginMessage is the warning when a likely agent uses the person's
// stored login: doctor's token check says it, and the CLI gives it as the
// AGENT_ON_PERSON_LOGIN warning.
const AgentOnLoginMessage = "this looks like an agent's session (GHOSTBRANCH_ACTOR names an agent, a coding agent's marker variable is set, or no person is at a console), and it is using the person's stored login, which can publish; give the agent its own token, reaching only the shadow repo, in GHOSTBRANCH_TOKEN (see \"Tokens\" in ghostbranch guide)"

// agentOnLoginMessage is AgentOnLoginMessage, for the engine's own use
// (doctor, and agents status).
const agentOnLoginMessage = AgentOnLoginMessage

// likelyAgentOnLogin: the token is the person's stored login, and the run
// looks like an agent's (e.LooksLikeAgent, from auth.LooksLikeAgent, the one
// definition).
func likelyAgentOnLogin(e *Engine) bool {
	return e.TokenSource == sourceLogin && e.LooksLikeAgent
}

// canPublish probes whether the token may push to the public repo, pushing
// nothing. Can is "unknown" when the forge can't tell.
func (e *Engine) canPublish(ctx context.Context, public string) forge.PushAccess {
	if pr, ok := e.Forge.(forge.PushProber); ok {
		return pr.PushAccess(ctx, public)
	}
	return forge.PushAccess{Can: "unknown"}
}

func (d *doctorRun) findPair() {
	e := d.e
	if e.WS == nil {
		d.noPair = "the workspace couldn't be opened (see state)"
		return
	}
	repo := d.opt.Repo
	if repo == "" {
		repo = "." // this checkout's pair only
	}
	p, err := e.WS.Resolve(repo, d.opt.Remotes)
	if err == nil {
		d.pair = &p
		return
	}
	pub, _ := contract.From(err).Details["public"].(string)
	switch {
	case pub != "" && d.opt.Repo != "":
		d.unregistered = pub
	case pub != "":
		d.noPair = fmt.Sprintf("this checkout's repo %s isn't set up in workspace %q (ghostbranch init %s sets it up)", pub, e.WS.Name, pub)
	default:
		d.noPair = "not in a checkout of a registered public repo; pass --repo <owner/repo> to check a pair"
	}
}

// GroupAccess is what a token lacks for one command group, each entry
// "<permission> on <owner/repo>": Missing it doesn't have, Unknown GitHub
// won't say, and Optional ones the group can do without (sync's cancels,
// audit's secret names) that it doesn't have.
type GroupAccess struct {
	Missing  []string `json:"missing"`
	Unknown  []string `json:"unknown"`
	Optional []string `json:"optional_missing"`
}

type permNeed struct {
	public   bool // on the public repo; else the shadow repo
	perm     string
	optional bool
}

// permGroups are the permissions each command group needs (the guide's
// token table).
var permGroups = []struct {
	name  string
	needs []permNeed
}{
	{"init", []permNeed{{true, forge.PermContentsRead, false}, {false, forge.PermContentsWrite, false},
		{false, forge.PermWorkflowsWrite, false}, {false, forge.PermAdminWrite, false}}},
	{"sync", []permNeed{{true, forge.PermContentsRead, false}, {true, forge.PermPullsRead, false},
		{false, forge.PermContentsWrite, false}, {false, forge.PermWorkflowsWrite, false}, {false, forge.PermActionsWrite, true}}},
	{"gate", []permNeed{{false, forge.PermContentsWrite, false}, {false, forge.PermPullsWrite, false},
		{false, forge.PermWorkflowsWrite, false}, {false, forge.PermActionsRead, false}, {false, forge.PermStatusesRead, false}}},
	{"promote", []permNeed{{true, forge.PermContentsWrite, false}, {true, forge.PermPullsWrite, false}, {false, forge.PermContentsWrite, false}}},
	{"audit", []permNeed{{false, forge.PermContentsRead, false}, {false, forge.PermSecretsRead, true}}},
}

var allPermissions = []string{forge.PermMetadataRead, forge.PermContentsRead, forge.PermContentsWrite,
	forge.PermPullsRead, forge.PermPullsWrite, forge.PermActionsRead, forge.PermActionsWrite,
	forge.PermStatusesRead, forge.PermWorkflowsWrite, forge.PermAdminWrite, forge.PermSecretsRead}

// appPermissions are the Ghostbranch GitHub App's repository permissions
// (spec § 2.4). Secrets isn't one of them.
var appPermissions = map[string]string{
	forge.PermMetadataRead: forge.AccessYes, forge.PermContentsWrite: forge.AccessYes, forge.PermPullsWrite: forge.AccessYes,
	forge.PermWorkflowsWrite: forge.AccessYes, forge.PermAdminWrite: forge.AccessYes, forge.PermActionsWrite: forge.AccessYes,
	forge.PermStatusesRead: forge.AccessYes, forge.PermSecretsRead: forge.AccessNo,
}

// repoAccess is what the token may do on one repo: what its kind (or a
// classic token's scopes) implies, overridden by every probe answer.
func repoAccess(kind string, scopes []string, scoped, public bool, probe forge.RepoPermissions) map[string]string {
	out := map[string]string{}
	if probe.Have[forge.PermMetadataRead] == forge.AccessNo {
		for _, p := range allPermissions {
			out[p] = forge.AccessNo // the token can't see the repo
		}
		return out
	}
	switch {
	case kind == "app":
		maps.Copy(out, appPermissions)
	case scoped:
		full := slices.Contains(scopes, "repo")
		pub := public && slices.Contains(scopes, "public_repo")
		for _, p := range allPermissions {
			if full || pub || (public && strings.HasSuffix(p, ": read")) {
				out[p] = forge.AccessYes
			} else {
				out[p] = forge.AccessNo
			}
		}
		if !slices.Contains(scopes, "workflow") {
			out[forge.PermWorkflowsWrite] = forge.AccessNo
		}
	}
	maps.Copy(out, probe.Have)
	return out
}

// accessOf is one permission's answer; a read is there when its write is.
func accessOf(m map[string]string, perm string) string {
	if v := m[perm]; v != "" {
		return v
	}
	if base, ok := strings.CutSuffix(perm, ": read"); ok && m[base+": write"] == forge.AccessYes {
		return forge.AccessYes
	}
	return forge.AccessUnknown
}

// tokenGroups probes both repos and says, per command group, what the
// token lacks.
func (e *Engine) tokenGroups(ctx context.Context, kind string, p workspace.Pair) (map[string]GroupAccess, error) {
	pp, _ := e.Forge.(forge.PermissionProber)
	var scopes []string
	scoped := false
	if pp != nil && (kind == "classic" || kind == "oauth") {
		s, ok, err := pp.OAuthScopes(ctx)
		if err != nil {
			return nil, err
		}
		scopes, scoped = s, ok
	}
	access := map[bool]map[string]string{}
	for _, public := range []bool{true, false} {
		full := p.Shadow
		if public {
			full = p.Public
		}
		var probe forge.RepoPermissions
		if pp != nil {
			var err error
			if probe, err = pp.ProbePermissions(ctx, full); err != nil {
				return nil, err
			}
		}
		access[public] = repoAccess(kind, scopes, scoped, public, probe)
	}
	out := map[string]GroupAccess{}
	for _, g := range permGroups {
		ga := GroupAccess{Missing: []string{}, Unknown: []string{}, Optional: []string{}}
		for _, n := range g.needs {
			full := p.Shadow
			if n.public {
				full = p.Public
			}
			name := n.perm + " on " + full
			switch accessOf(access[n.public], n.perm) {
			case forge.AccessNo:
				if n.optional {
					ga.Optional = append(ga.Optional, name)
				} else {
					ga.Missing = append(ga.Missing, name)
				}
			case forge.AccessUnknown:
				if !n.optional {
					ga.Unknown = append(ga.Unknown, name)
				}
			}
		}
		out[g.name] = ga
	}
	return out, nil
}

// permList is "<permission> on <repo> (<groups>)", sorted, comma-joined.
func permList(m map[string][]string) string {
	keys := slices.Sorted(maps.Keys(m))
	parts := make([]string, len(keys))
	for i, k := range keys {
		gs := slices.Clone(m[k])
		slices.Sort(gs)
		parts[i] = k + " (" + strings.Join(gs, ", ") + ")"
	}
	return strings.Join(parts, ", ")
}

func (d *doctorRun) token(ctx context.Context) DoctorCheck {
	e := d.e
	det := map[string]any{"source": e.TokenSource, "kind": "", "account": "", "can_publish": "unknown"}
	if e.Token == "" || e.Forge == nil {
		msg, fix := "no GitHub token: set GHOSTBRANCH_TOKEN, or log in", []string{"ghostbranch", "login"}
		if ce := contract.From(d.opt.TokenErr); ce != nil {
			msg = ce.Message
			if ce.Code == contract.LoginUnavailable {
				fix = []string{"ghostbranch", "logout"}
			}
		}
		return DoctorCheck{Status: DoctorFail, Message: msg, Details: det}.withFix(fix...)
	}
	kind := auth.Kind(e.Token)
	det["kind"] = kind
	var v verdict
	now := e.now()
	l := d.opt.Login
	if e.TokenSource != sourceLogin {
		l = nil
	}
	if l != nil {
		det["account"] = l.Account
		if !l.Expires.IsZero() {
			det["expires"] = l.Expires.UTC().Format(time.RFC3339)
		}
		if !l.RefreshExpires.IsZero() {
			det["refresh_expires"] = l.RefreshExpires.UTC().Format(time.RFC3339)
			if !now.Before(l.RefreshExpires) {
				return DoctorCheck{Status: DoctorFail, Details: det,
					Message: "the stored login expired on " + day(l.RefreshExpires) + "; log in again"}.withFix("ghostbranch", "login")
			}
			if l.RefreshExpires.Sub(now) < 14*24*time.Hour {
				v.add(DoctorWarn, "the stored login ends on "+day(l.RefreshExpires)+"; log in again before then", "ghostbranch", "login")
			}
		}
	}
	if likelyAgentOnLogin(e) {
		det["agent_on_person_login"] = true
		v.add(DoctorWarn, agentOnLoginMessage)
	}
	head := fmt.Sprintf("the %s token from %s", kind, e.TokenSource)
	if l != nil && !l.Expires.IsZero() && !now.Before(l.Expires) {
		v.add(DoctorWarn, "its access token expired at "+l.Expires.UTC().Format(time.RFC3339)+
			"; the next command that uses GitHub refreshes it (doctor doesn't, as it writes nothing), so it wasn't checked", "")
		return v.result(head, "", det)
	}
	u, err := e.Forge.CurrentUser(ctx)
	if err != nil {
		if errors.Is(err, forge.ErrTransient) || ctx.Err() != nil {
			v.add(DoctorFail, "couldn't reach GitHub to check it: "+err.Error())
		} else {
			var fix []string
			if e.TokenSource == sourceLogin {
				fix = []string{"ghostbranch", "login"}
			}
			v.add(DoctorFail, "GitHub refused the token from "+e.TokenSource+": "+err.Error(), fix...)
		}
		return v.result(head, "", det)
	}
	det["account"] = u.Login
	head += " works, as " + u.Login
	if d.pair == nil {
		return v.result(head, "; permissions not checked: no pair (run doctor in a checkout of the public repo, or pass --repo)", det)
	}
	pa := e.canPublish(ctx, d.pair.Public)
	det["can_publish"] = pa.Can
	groups, err := e.tokenGroups(ctx, kind, *d.pair)
	if err != nil {
		v.add(DoctorWarn, "its permissions couldn't be probed: "+err.Error())
		return v.result(head, "", det)
	}
	det["groups"] = groups
	missing, unknown := map[string][]string{}, map[string][]string{}
	for _, g := range permGroups {
		for _, p := range groups[g.name].Missing {
			if g.name != "promote" {
				missing[p] = append(missing[p], g.name)
			}
		}
		for _, p := range groups[g.name].Unknown {
			unknown[p] = append(unknown[p], g.name)
		}
	}
	if len(missing) > 0 {
		fix := []string{"ghostbranch", "guide"} // its token table names each permission
		if kind == "app" {
			fix = []string{"ghostbranch", "login"} // shows the App's install page
		}
		v.add(DoctorFail, "it lacks permissions these commands need (or can't see the repo): "+permList(missing), fix...)
	}
	if pa.Can == "false" || len(groups["promote"].Missing) > 0 {
		v.add(DoctorWarn, "it can't publish to "+d.pair.Public+": a person publishes, from a terminal whose token can (an agent's token should be like this)")
	}
	if len(unknown) > 0 {
		v.add(DoctorWarn, "GitHub doesn't say whether it has: "+permList(unknown))
	}
	return v.result(head, fmt.Sprintf("; can publish to %s: %s; it has every permission the commands need", d.pair.Public, pa.Can), det)
}

// repoProblem says why looking up a repo failed.
func repoProblem(full string, err error) string {
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return full + " doesn't exist, or the token can't see it"
	case errors.Is(err, forge.ErrForbidden):
		return "the token may not read " + full
	case errors.Is(err, forge.ErrTransient):
		return "couldn't reach GitHub to look up " + full + ": " + err.Error()
	}
	return "looking up " + full + " failed: " + err.Error()
}

func (d *doctorRun) pairCheck(ctx context.Context) DoctorCheck {
	e := d.e
	switch {
	case d.unregistered != "":
		return DoctorCheck{Status: DoctorFail, Details: map[string]any{"public": d.unregistered},
			Message: fmt.Sprintf("%s isn't set up in workspace %q", d.unregistered, e.WS.Name)}.withFix("ghostbranch", "init", d.unregistered)
	case d.pair == nil:
		return skipped(d.noPair)
	case e.Forge == nil:
		return skipped("no GitHub token (see token)")
	}
	p := *d.pair
	det := map[string]any{"public": p.Public, "shadow": p.Shadow}
	fail := func(msg string, fix ...string) DoctorCheck {
		return DoctorCheck{Status: DoctorFail, Message: msg, Details: det}.withFix(fix...)
	}
	pub, err := e.Forge.GetRepo(ctx, p.Public)
	if err != nil {
		return fail(repoProblem(p.Public, err))
	}
	sh, err := e.Forge.GetRepo(ctx, p.Shadow)
	if errors.Is(err, forge.ErrNotFound) {
		return fail(repoProblem(p.Shadow, err), "ghostbranch", "init", p.Public)
	}
	if err != nil {
		return fail(repoProblem(p.Shadow, err))
	}
	det["public_private"], det["shadow_private"] = pub.Private, sh.Private && !sh.Fork
	if !sh.Private || sh.Fork {
		return fail(fmt.Sprintf("the shadow repo %s must be private and not a fork: make it private in its GitHub settings (a fork never can be: create a private repo and adopt it)", p.Shadow))
	}
	pc, err := e.open(ctx, "doctor", p, false)
	if err != nil {
		return fail("couldn't fetch the pair into the local mirror: " + contract.From(err).Message)
	}
	d.pc = pc
	if _, ok, err := pc.git.Resolve(ctx, shadowRef(OverlayBranch)); err != nil || !ok {
		return fail(fmt.Sprintf("the shadow repo %s has no %q overlay branch", p.Shadow, OverlayBranch), "ghostbranch", "init", p.Public)
	}
	cfg, O, _, err := e.readConfig(ctx, pc)
	if err != nil {
		return fail("the overlay's .ghostbranch.yml: " + contract.From(err).Message)
	}
	d.cfg, d.O = cfg, O
	var v verdict
	if pub.Private {
		v.add(DoctorWarn, p.Public+" is private; Ghostbranch is meant for public repos")
	}
	m, err := compareMains(ctx, pc.git)
	switch {
	case err != nil:
		v.add(DoctorFail, "couldn't compare shadow main with public main: "+err.Error())
	case m.Shadow == "":
		v.add(DoctorWarn, "the shadow repo has no main branch", "ghostbranch", "sync", "--repo", p.Public)
	case m.Behind > 0:
		det["behind"] = m.Behind
		v.add(DoctorWarn, fmt.Sprintf("shadow main is %d commit(s) behind public main", m.Behind), "ghostbranch", "sync", "--repo", p.Public)
	}
	files, err := e.managedFiles(ctx, pc.git, O, cfg)
	if err != nil {
		v.add(DoctorFail, "couldn't compare the managed overlay files: "+contract.From(err).Message)
	} else {
		det["overlay_status"] = overlayStatus(files)
		if overlayStatus(files) == overlayOutdated {
			v.add(DoctorWarn, "Ghostbranch's managed overlay files are out of date", "ghostbranch", "upgrade", "--repo", p.Public)
		}
	}
	return v.result(fmt.Sprintf("%s (public) and %s (private) are reachable, and the overlay and its config are valid", p.Public, p.Shadow),
		"; shadow main and the managed files are current", det)
}

// clones reports whether any remote URL names public.
func clones(urls map[string][]string, public string) bool {
	for _, us := range urls {
		for _, u := range us {
			if s, ok := guard.Slug(u); ok && strings.EqualFold(s, public) {
				return true
			}
		}
	}
	return false
}

func sameSet(a, b []string) bool {
	norm := func(xs []string) []string {
		out := make([]string, len(xs))
		for i, x := range xs {
			out[i] = strings.ToLower(x)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(norm(a), norm(b))
}

func (d *doctorRun) guard(ctx context.Context) DoctorCheck {
	e := d.e
	switch {
	case d.opt.Dir == "":
		return skipped("the current directory isn't known")
	case d.pair == nil:
		return skipped("no pair, so no public repo to guard (see pair)")
	}
	co, err := guard.Open(ctx, d.opt.Dir)
	if err != nil {
		ce := contract.From(err)
		switch ce.Code {
		case contract.NotAPairCheckout:
			return skipped("not in a git checkout")
		case contract.GuardUnsupported:
			return DoctorCheck{Status: DoctorWarn, Message: "the push guard can't run here: " + ce.Message}
		}
		return DoctorCheck{Status: DoctorFail, Message: "couldn't inspect this checkout: " + ce.Message}
	}
	urls, err := co.RemoteURLs(ctx)
	if err != nil {
		return DoctorCheck{Status: DoctorFail, Message: "couldn't read this checkout's remotes: " + err.Error()}
	}
	if !clones(urls, d.pair.Public) {
		return skipped("this checkout isn't a clone of " + d.pair.Public)
	}
	det := map[string]any{"checkout": co.Top, "hooks_dir": co.HooksDir, "installed": false}
	h, installed, current, err := guard.ReadHook(co)
	if err != nil {
		return DoctorCheck{Status: DoctorFail, Message: contract.From(err).Message, Details: det}
	}
	fix := []string{"ghostbranch", "guard", "install"}
	if !installed {
		return DoctorCheck{Status: DoctorFail, Details: det,
			Message: "the push guard isn't installed in " + co.Top + ", so a plain git push can publish feature work"}.withFix(fix...)
	}
	det["installed"], det["workspace"], det["remotes"], det["publics"] = true, h.Workspace, h.Remotes, h.Publics
	var v verdict
	if !current {
		v.add(DoctorFail, "its hook was written by another version of Ghostbranch", fix...)
	}
	if want, werr := guard.PublicRemotes(ctx, co, e.WS); werr == nil && (!sameSet(h.Remotes, want.Remotes) || !sameSet(h.Publics, want.Publics)) {
		v.add(DoctorFail, fmt.Sprintf("it guards the remotes %s, but these point at a public repo: %s (an older guard misses pushurl)",
			strings.Join(h.Remotes, ", "), strings.Join(want.Remotes, ", ")), fix...)
	}
	if h.Workspace != e.WS.Name {
		v.add(DoctorFail, fmt.Sprintf("it checks workspace %q, but this run uses %q", h.Workspace, e.WS.Name), fix...)
	}
	if fi, err := os.Stat(filepath.FromSlash(h.Binary)); err != nil || fi.IsDir() {
		v.add(DoctorWarn, "its binary "+h.Binary+" is missing: it falls back to ghostbranch on PATH, and refuses pushes to the public repo when there is none", fix...)
	}
	if home := d.opt.Getenv("GHOSTBRANCH_HOME"); home != "" {
		v.add(DoctorWarn, "this run reads its state from GHOSTBRANCH_HOME="+home+", but the hook reads GHOSTBRANCH_HOME where git runs it: a push from a shell without it is checked against the default state directory")
	}
	return v.result("the push guard is installed in "+co.Top, ", current, and guards every remote at the public repo", det)
}

func (d *doctorRun) state(ctx context.Context) DoctorCheck {
	e := d.e
	if e.WS == nil {
		msg := "the workspace couldn't be opened"
		if d.opt.WorkspaceErr != nil {
			msg += ": " + contract.From(d.opt.WorkspaceErr).Message
		}
		return DoctorCheck{Status: DoctorFail, Message: msg + "; fix or move its workspace.json"}
	}
	det := map[string]any{"workspace": e.WS.Name, "dir": e.WS.Dir}
	var v verdict
	record := func(name string, err error, corruptFix ...string) {
		ce := contract.From(err)
		if ce.Code == contract.UpgradeNeeded {
			v.add(DoctorFail, name+" was written by a newer Ghostbranch; upgrade ghostbranch on this machine")
			return
		}
		v.add(DoctorFail, name+" can't be read: "+ce.Message, corruptFix...)
	}
	if e.Grants != nil {
		if _, err := e.Grants.Load(); err != nil {
			var fix []string
			if grant.Corrupt(err) {
				fix = []string{"ghostbranch", "agents", "revoke", "--all"}
			}
			record(e.WS.GrantsPath(), err, fix...)
		}
	}
	if e.Waivers != nil {
		if _, err := e.Waivers.Load(); err != nil {
			record(e.WS.WaiversPath(), err)
		}
	}
	entries, err := os.ReadDir(e.WS.PlansDir())
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		v.add(DoctorFail, "the plans directory can't be read: "+err.Error())
	}
	for _, en := range entries {
		if en.IsDir() || !strings.HasSuffix(en.Name(), ".json") {
			continue
		}
		path := filepath.Join(e.WS.PlansDir(), en.Name())
		if _, _, err := plan.Load(e.WS.PlansDir(), path); err != nil {
			if contract.From(err).Code == contract.UpgradeNeeded {
				v.add(DoctorFail, "plan "+path+" was written by a newer Ghostbranch; upgrade ghostbranch on this machine")
			} else {
				v.add(DoctorWarn, "plan file "+path+" can't be read: "+contract.From(err).Message)
			}
		}
	}
	tail := "; the records are readable, none is newer than this binary, and no gate job has been pending for over an hour"
	if d.pc == nil {
		tail += " (features not checked: no readable pair)"
	} else {
		rs, _, err := e.survey(ctx, d.pc)
		if err != nil {
			v.add(DoctorFail, "the features of "+d.pair.Public+" couldn't be listed: "+contract.From(err).Message)
		} else {
			det["features"] = len(rs)
			var newer, bad, stale []string
			for _, r := range rs {
				switch r.unreadable {
				case unreadableNewer:
					newer = append(newer, r.Name)
				case unreadableCorrupt, unreadableError:
					bad = append(bad, r.Name)
				}
				if r.Gate != nil && r.Gate.Job != nil && e.now().Sub(r.Gate.At) > time.Hour {
					stale = append(stale, *r.Gate.Job)
				}
			}
			if len(newer) > 0 {
				v.add(DoctorFail, "feature record(s) "+strings.Join(newer, ", ")+" were written by a newer Ghostbranch; upgrade ghostbranch on this machine before changing them")
			}
			if len(bad) > 0 {
				v.add(DoctorFail, "feature record(s) "+strings.Join(bad, ", ")+" can't be read", "ghostbranch", "feature", "show", bad[0], "--repo", d.pair.Public)
			}
			if len(stale) > 0 {
				v.add(DoctorWarn, "gate job(s) "+strings.Join(stale, ", ")+" have been pending for over an hour", "ghostbranch", "wait", stale[0], "--repo", d.pair.Public)
			}
		}
	}
	if runtime.GOOS != "windows" {
		paths := []string{e.WS.LedgerPath(), e.WS.GrantsPath(), e.WS.WaiversPath()}
		plans, _ := filepath.Glob(filepath.Join(e.WS.PlansDir(), "*.json"))
		var loose []string
		for _, p := range append(paths, plans...) {
			if fi, err := os.Stat(p); err == nil && fi.Mode().Perm() != 0o600 {
				loose = append(loose, p)
			}
		}
		if len(loose) > 0 {
			v.add(DoctorWarn, "other users on this machine can read "+strings.Join(loose, ", "), append([]string{"chmod", "600"}, loose...)...)
		}
	}
	return v.result(fmt.Sprintf("workspace %q", e.WS.Name), tail, det)
}

func (d *doctorRun) ci(ctx context.Context) DoctorCheck {
	if d.pc == nil || d.O == "" {
		return skipped("no readable pair to audit (see pair)")
	}
	res, warns, err := d.e.audit(ctx, d.pc, d.cfg, d.O)
	if err != nil {
		if errors.Is(err, forge.ErrForbidden) {
			return skipped("the token can't read the shadow repo's CI settings: " + contract.From(err).Message)
		}
		return DoctorCheck{Status: DoctorFail, Message: "the CI audit failed: " + contract.From(err).Message}.withFix("ghostbranch", "audit", "--repo", d.pair.Public)
	}
	det := map[string]any{"guards": len(res.Guards), "secrets_listed": res.Secrets.Listed,
		"missing_secrets": res.Secrets.Missing, "push_triggers": res.PushTriggers}
	var v verdict
	for _, w := range warns {
		switch w.Code {
		case contract.RepoGuardUnrewritable, contract.SecretsMissing, contract.PushTriggersOnSync:
			v.add(DoctorWarn, w.Message, "ghostbranch", "audit", "--repo", d.pair.Public)
		}
	}
	secrets := "secret names checked"
	if !res.Secrets.Listed {
		secrets = "secret names not checked (no workflow uses one, or the token can't list them)"
	}
	return v.result(fmt.Sprintf("%d repo guard(s) the gate rewrites; %s", len(res.Guards), secrets),
		"; no workflow runs on sync's pushes", det)
}
