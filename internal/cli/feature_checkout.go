package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/ghostbranch-labs/ghostbranch/internal/cli/style"
	"github.com/ghostbranch-labs/ghostbranch/internal/engine"
	"github.com/ghostbranch-labs/ghostbranch/internal/feature"
	"github.com/ghostbranch-labs/ghostbranch/internal/guard"
	"github.com/ghostbranch-labs/ghostbranch/internal/workspace"
)

// checkOutFeature gets a feature feature start just created into the
// checkout it runs in, when that is a checkout of the pair's public repo:
// it adds a shadow remote if none reaches the shadow repo, fetches it, and
// switches to the new branch, tracking the remote's. It never rewrites a
// remote, and never switches over uncommitted work or an existing local
// branch. It is local git only, with the person's own git config, so git
// signs in to GitHub as it does for them. It fills res.Local, res.Checkout
// (the commands still to run, as before) and a.shadowRemote.
func (a *app) checkOutFeature(ctx context.Context, p workspace.Pair, res *engine.FeatureStartResult, skip bool) {
	hintAsToday := func(local *engine.LocalCheckout) {
		res.Local = local
		if a.env.ShadowRemote != nil {
			remote, free, in := a.env.ShadowRemote(p.Shadow)
			res.SetCheckout(checkoutArgv(res.Name, p.Shadow, remote, free, in))
			a.shadowRemote = pushRemote(remote, free, in)
		}
	}
	if skip {
		hintAsToday(&engine.LocalCheckout{Status: engine.LocalSkipped, Reason: engine.ReasonNoCheckoutFlag})
		return
	}
	co, remotes := a.publicCheckout(ctx, p)
	if co == nil {
		hintAsToday(&engine.LocalCheckout{Status: engine.LocalSkipped, Reason: engine.ReasonNotACheckout})
		return
	}
	name := res.Name
	remote, free := shadowRemoteIn(remotes, p.Shadow)
	res.SetCheckout(checkoutArgv(name, p.Shadow, remote, free, true))
	a.shadowRemote = pushRemote(remote, free, true)
	if remote == "" {
		if have, taken := remotes[remoteShadow]; taken {
			res.Local = &engine.LocalCheckout{Status: engine.LocalSkipped, Reason: engine.ReasonRemoteDiffers, Remote: remoteShadow, Shadow: p.Shadow, HaveURL: have}
			return
		}
		if _, err := localGit(ctx, co.Top, "remote", "add", remoteShadow, shadowURL(p.Shadow)); err != nil {
			res.Local = &engine.LocalCheckout{Status: engine.LocalSkipped, Reason: engine.ReasonFetchFailed, Remote: remoteShadow, Shadow: p.Shadow, GitError: err.Error()}
			return
		}
		remote = remoteShadow
		res.SetCheckout(checkoutArgv(name, p.Shadow, remote, free, true))
		a.shadowRemote = remote
	}
	local := &engine.LocalCheckout{Remote: remote, Shadow: p.Shadow}
	res.Local = local
	if _, err := localGit(ctx, co.Top, "fetch", remote); err != nil {
		local.Status, local.Reason, local.GitError = engine.LocalSkipped, engine.ReasonFetchFailed, err.Error()
		local.NotFound = strings.Contains(strings.ToLower(err.Error()), "not found")
		return
	}
	local.Status = engine.LocalFetchedOnly
	if _, err := localGit(ctx, co.Top, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); err == nil {
		local.Reason = engine.ReasonBranchExistsLocally
		return
	}
	// Untracked files count too: they are work nobody committed yet.
	if st, err := localGit(ctx, co.Top, "status", "--porcelain"); err != nil || st != "" {
		local.Reason = engine.ReasonDirtyTree
		return
	}
	if _, err := localGit(ctx, co.Top, "switch", "--quiet", "--create", name, "--track", remote+"/"+name); err != nil {
		local.Reason, local.GitError = engine.ReasonSwitchFailed, err.Error()
		return
	}
	local.Status = engine.LocalSwitched
}

// remoteShadow is the remote name feature start adds for the shadow repo.
const remoteShadow = "shadow"

func shadowURL(shadow string) string { return "https://github.com/" + shadow + ".git" }

// setURL is the command that has git sign in to GitHub as the shadow
// repo's owner for remote only, leaving every other remote as it was.
func setURL(remote, shadow string) string {
	owner, _, _ := strings.Cut(shadow, "/")
	return "git remote set-url " + remote + " https://" + owner + "@github.com/" + shadow + ".git"
}

// publicCheckout is the checkout feature start runs in, with its remotes,
// when one of them is the pair's public repo; nil otherwise.
func (a *app) publicCheckout(ctx context.Context, p workspace.Pair) (*guard.Checkout, map[string]string) {
	if a.env.Getwd == nil {
		return nil, nil
	}
	dir, err := a.env.Getwd()
	if err != nil {
		return nil, nil
	}
	co, err := guard.Open(ctx, dir)
	if err != nil {
		return nil, nil
	}
	remotes, err := co.Remotes(ctx)
	if err != nil {
		return nil, nil
	}
	for _, u := range remotes {
		if s, ok := guard.Slug(u); ok && strings.EqualFold(s, p.Public) {
			return co, remotes
		}
	}
	return nil, nil
}

// shadowRemoteIn is the first remote, by name, whose URL is the shadow
// repo in any of GitHub's URL forms and whose name is safe to print into a
// command ("" if none), and a name no remote uses yet.
func shadowRemoteIn(remotes map[string]string, shadow string) (remote, free string) {
	names := make([]string, 0, len(remotes))
	for n := range remotes {
		names = append(names, n)
	}
	sort.Strings(names)
	free = freeRemoteName(remotes)
	for _, n := range names {
		if s, ok := guard.Slug(remotes[n]); ok && strings.EqualFold(s, shadow) && safeRemoteName.MatchString(n) {
			return n, free
		}
	}
	return "", free
}

// localGit runs git in the person's checkout as they would: their own
// config and credentials, unlike gitx, which isolates Ghostbranch's own
// repos from them. It never prompts: a fetch git can't sign in for fails
// and says why. The error carries git's stderr.
func localGit(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}

// featureStartText is feature start's human text: the new branch, then
// what happened in this checkout and what is left for the person.
func featureStartText(b *strings.Builder, v *engine.FeatureStartResult, st style.Styler) {
	from := "shadow main " + short(v.Base)
	if v.Parent != "" && v.Parent != feature.MainParent {
		fmt.Fprintf(b, "Started feature %s on %s, at its tip %s.\n", v.Name, v.Parent, short(v.Base))
		from = v.Parent + " " + short(v.Base)
	} else {
		fmt.Fprintf(b, "Started feature %s from shadow main %s.\n", v.Name, short(v.Base))
	}
	l := v.Local
	push := ""
	if l != nil {
		push = "git push " + l.Remote + " " + v.Name
	}
	line := func(s string) { b.WriteString(s + "\n") }
	switch {
	case l == nil || l.Reason == engine.ReasonNotACheckout || l.Reason == engine.ReasonNoCheckoutFlag:
		fmt.Fprintf(b, "Check it out with: %s\n", v.CheckoutHint)
	case l.Status == engine.LocalSwitched:
		line(fmt.Sprintf("Switched to %s (from %s). ", v.Name, from) + attention(st, "Commit here, then: "+push))
	case l.Reason == engine.ReasonDirtyTree:
		line("Fetched " + l.Remote + ", but this checkout has uncommitted changes, so it stays on its branch.")
		line(attention(st, "To work on "+v.Name+": commit or stash first, then `git switch "+v.Name+"`. Commit there, then: "+push))
	case l.Reason == engine.ReasonBranchExistsLocally:
		line("Fetched " + l.Remote + ", but a local branch " + v.Name + " already exists; Ghostbranch left it alone.")
		line(attention(st, "Compare it with "+l.Remote+"/"+v.Name+" and switch to the one you mean to work on. Commit there, then: "+push))
	case l.Reason == engine.ReasonSwitchFailed:
		line("Fetched " + l.Remote + ", but git switch failed: " + gitLine(l.GitError))
		line(attention(st, "Fix that, then `git switch "+v.Name+"`. Commit there, then: "+push))
	case l.Reason == engine.ReasonRemoteDiffers:
		line("This checkout's remote " + l.Remote + " points at " + oneLine(l.HaveURL) + ", not " + shadowURL(l.Shadow) + "; Ghostbranch left it alone.")
		line(attention(st, "Check it out with: "+v.CheckoutHint))
	case l.NotFound:
		owner, _, _ := strings.Cut(l.Shadow, "/")
		line("git fetch " + l.Remote + " says the repository was not found. git may be signed in to GitHub as a different GitHub account than " + owner + ", which owns the shadow repo; Ghostbranch's own login doesn't cover git.")
		line(attention(st, "Use the right account for this remote only: "+setURL(l.Remote, l.Shadow)))
		line(attention(st, "Then: git fetch "+l.Remote+" && git switch "+v.Name))
		line("The branch exists in the shadow repo either way.")
	default: // fetch_failed for another reason
		line("git fetch " + l.Remote + " failed: " + gitLine(l.GitError))
		line(attention(st, "Once git can reach the shadow repo: git fetch "+l.Remote+" && git switch "+v.Name))
		line("The branch exists in the shadow repo either way.")
	}
}

// startWhy is how next's gate step after feature start begins: what is
// left to get onto the branch, and where to commit. The push and the gate
// follow it.
func startWhy(r *engine.FeatureStartResult) string {
	l, name := r.Local, r.Name
	switch {
	case l == nil || l.Reason == engine.ReasonNotACheckout || l.Reason == engine.ReasonNoCheckoutFlag:
	case l.Status == engine.LocalSwitched:
		return "feature start switched this checkout to " + name + ", tracking " + l.Remote + "/" + name + "; commit there, "
	case l.Reason == engine.ReasonDirtyTree:
		return "this checkout has uncommitted changes, so feature start fetched " + l.Remote + " but didn't switch: commit or stash first, then `git switch " + name + "`; commit on " + name + ", "
	case l.Reason == engine.ReasonBranchExistsLocally:
		return "a local branch " + name + " already exists, so feature start fetched " + l.Remote + " but left it alone: compare it with " + l.Remote + "/" + name + " and switch to the one to work on; commit on " + name + ", "
	case l.Reason == engine.ReasonSwitchFailed:
		return "feature start fetched " + l.Remote + " but git switch failed (" + gitLine(l.GitError) + "): fix that, then `git switch " + name + "`; commit on " + name + ", "
	case l.Reason == engine.ReasonRemoteDiffers:
		return "this checkout's remote " + l.Remote + " points at " + oneLine(l.HaveURL) + ", not " + shadowURL(l.Shadow) + ", so feature start left it alone: check out with `" + r.CheckoutHint + "`, commit on " + name + ", "
	case l.NotFound:
		return "git fetch " + l.Remote + " said the repository was not found: git may be signed in to GitHub as a different account than the shadow repo's owner. A person runs `" + setURL(l.Remote, l.Shadow) + "` (that remote only), then `git fetch " + l.Remote + " && git switch " + name + "`; commit on " + name + ", "
	default:
		return "git fetch " + l.Remote + " failed (" + gitLine(l.GitError) + "); once git can reach the shadow repo, `git fetch " + l.Remote + " && git switch " + name + "`; commit on " + name + ", "
	}
	return "check out with `" + r.CheckoutHint + "`, commit on " + name + ", "
}

// gitLine is git's message on one line.
func gitLine(msg string) string { return oneLine(strings.Join(strings.Fields(msg), " ")) }
