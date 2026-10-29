package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
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
	local := &engine.LocalCheckout{Remote: remoteShadow, Shadow: p.Shadow, Login: a.login}
	res.Local = local
	if remote == "" {
		if have, taken := remotes[remoteShadow]; taken {
			local.Status, local.Reason, local.HaveURL = engine.LocalSkipped, engine.ReasonRemoteDiffers, redactURLs(have)
			return
		}
		if _, err := localGit(ctx, co.Top, "remote", "add", remoteShadow, shadowURL(p.Shadow)); err != nil {
			local.Status, local.Reason, local.GitError, local.AddFailed = engine.LocalSkipped, engine.ReasonFetchFailed, err.Error(), true
			return
		}
		remote, local.Added = remoteShadow, true
		res.SetCheckout(checkoutArgv(name, p.Shadow, remote, free, true))
		a.shadowRemote = remote
	}
	local.Remote = remote
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
		local.UntrackedOnly = err == nil && untrackedOnly(st)
		return
	}
	// A detached HEAD may carry commits no branch, tag or remote reaches
	// (a hand-off PR's fix, say): switching away would strand them.
	if _, err := localGit(ctx, co.Top, "symbolic-ref", "--quiet", "HEAD"); err != nil {
		n, err := localGit(ctx, co.Top, "rev-list", "--count", "HEAD", "--not", "--branches", "--tags", "--remotes")
		if err != nil || n != "0" {
			local.Reason = engine.ReasonDetachedCommits
			local.DetachedHead, _ = localGit(ctx, co.Top, "rev-parse", "HEAD")
			local.DetachedCommits, _ = strconv.Atoi(n)
			return
		}
	}
	if _, err := localGit(ctx, co.Top, "switch", "--quiet", "--create", name, "--track", remote+"/"+name); err != nil {
		local.Reason, local.GitError = engine.ReasonSwitchFailed, err.Error()
		return
	}
	local.Status, local.Reason = engine.LocalSwitched, ""
}

// untrackedOnly says git status --porcelain lists only untracked files.
func untrackedOnly(porcelain string) bool {
	for _, l := range strings.Split(porcelain, "\n") {
		if !strings.HasPrefix(l, "?? ") {
			return false
		}
	}
	return true
}

// userinfo is the user[:password]@ part of a URL with a scheme, where a
// token may sit.
var userinfo = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.-]*://)[^/@\s]*@`)

// redactURLs drops the userinfo from every URL in s, so a token in a
// remote's URL never reaches the output.
func redactURLs(s string) string { return userinfo.ReplaceAllString(s, "${1}") }

// remoteShadow is the remote name feature start adds for the shadow repo.
const remoteShadow = "shadow"

func shadowURL(shadow string) string { return "https://github.com/" + shadow + ".git" }

// setURL is the command that has git sign in to GitHub as login for
// remote only, leaving every other remote as it was. With no login known
// it says where the person's own login goes.
func setURL(remote, shadow, login string) string {
	if login == "" || !safeRemoteName.MatchString(login) {
		login = "<your-github-login>"
	}
	return "git remote set-url " + remote + " https://" + login + "@github.com/" + shadow + ".git"
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
		return strings.TrimSpace(out.String()), errors.New(redactURLs(msg))
	}
	return strings.TrimSpace(out.String()), nil
}

// featureStartText is feature start's human text: the new branch, then
// what happened in this checkout and what is left for the person.
func featureStartText(b *strings.Builder, v *engine.FeatureStartResult, st style.Styler) {
	from := "shadow " + baseName(v.BaseBranch) + " " + short(v.Base)
	if v.Parent != "" && v.Parent != feature.MainParent {
		fmt.Fprintf(b, "Started feature %s on %s, at its tip %s.\n", v.Name, v.Parent, short(v.Base))
		from = v.Parent + " " + short(v.Base)
	} else {
		fmt.Fprintf(b, "Started feature %s from %s.\n", v.Name, from)
	}
	l := v.Local
	line := func(s string) { b.WriteString(s + "\n") }
	if l == nil || l.Reason == engine.ReasonNotACheckout || l.Reason == engine.ReasonNoCheckoutFlag {
		line("Check it out with: " + v.CheckoutHint)
		return
	}
	if l.Added {
		line("Added remote " + l.Remote + " (" + shadowURL(l.Shadow) + ").")
	}
	push := "git push " + l.Remote + " " + v.Name
	switch {
	case l.Status == engine.LocalSwitched:
		line(fmt.Sprintf("Switched to %s (from %s). ", v.Name, from) + attention(st, "Commit here, then: "+push))
	case l.Reason == engine.ReasonDirtyTree:
		line("Fetched " + l.Remote + ", but this checkout has uncommitted changes, so it stays on its branch.")
		line(attention(st, "To work on "+v.Name+": "+stashFirst(l, v.Name)+". Commit there, then: "+push))
	case l.Reason == engine.ReasonDetachedCommits:
		line(fmt.Sprintf("Fetched %s, but HEAD is detached at %s with %d commit(s) no branch has, so it stays there.", l.Remote, short(l.DetachedHead), l.DetachedCommits))
		line(attention(st, "To work on "+v.Name+": "+keepDetached(v.Name)+". Commit there, then: "+push))
	case l.Reason == engine.ReasonBranchExistsLocally:
		line("Fetched " + l.Remote + ", but a local branch " + v.Name + " already exists; Ghostbranch left it alone.")
		line(attention(st, "Compare it with "+l.Remote+"/"+v.Name+" and switch to the one you mean to work on. Commit there, then: "+push))
	case l.Reason == engine.ReasonSwitchFailed:
		line("Fetched " + l.Remote + ", but git switch failed: " + gitLine(l.GitError))
		line(attention(st, "Fix that, then `git switch "+v.Name+"`. Commit there, then: "+push))
	case l.Reason == engine.ReasonRemoteDiffers:
		line("This checkout's remote " + l.Remote + " points at " + oneLine(l.HaveURL) + ", not " + shadowURL(l.Shadow) + "; Ghostbranch left it alone.")
		line(attention(st, "Check it out with: "+v.CheckoutHint))
	case l.AddFailed:
		line("git remote add " + l.Remote + " failed: " + gitLine(l.GitError))
		line(attention(st, "Check it out with: "+v.CheckoutHint))
		line("The branch exists in the shadow repo either way.")
	case l.NotFound:
		line("git fetch " + l.Remote + " says the repository was not found. " + accountTrap(l) + "; Ghostbranch's own login doesn't cover git.")
		line(attention(st, "Use the right account for this remote only: "+setURL(l.Remote, l.Shadow, l.Login)))
		line(attention(st, "Then: git fetch "+l.Remote+" && git switch "+v.Name))
		line("The branch exists in the shadow repo either way.")
	default: // fetch_failed for another reason
		line("git fetch " + l.Remote + " failed: " + gitLine(l.GitError))
		line(attention(st, "Once git can reach the shadow repo: git fetch "+l.Remote+" && git switch "+v.Name))
		line("The branch exists in the shadow repo either way.")
	}
}

// accountTrap is why git may not find a private shadow repo that exists.
func accountTrap(l *engine.LocalCheckout) string {
	return "git may be signed in to GitHub as a different GitHub account than the one that can see " + l.Shadow
}

// stashFirst is how to set uncommitted work aside before switching to name.
func stashFirst(l *engine.LocalCheckout, name string) string {
	if l.UntrackedOnly {
		return "commit the untracked files, or stash them with `git stash --include-untracked`, then `git switch " + name + "`"
	}
	return "commit or stash first, then `git switch " + name + "`"
}

// keepDetached is how to keep a detached HEAD's commits before switching
// to name.
func keepDetached(name string) string {
	return "keep them on a branch first (`git switch -c <branch-name>`), then `git switch " + name + "`"
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
		return "this checkout has uncommitted changes, so feature start fetched " + l.Remote + " but didn't switch: " + stashFirst(l, name) + "; commit on " + name + ", "
	case l.Reason == engine.ReasonDetachedCommits:
		return fmt.Sprintf("HEAD is detached at %s with %d commit(s) no branch has, so feature start fetched %s but didn't switch: %s; commit on %s, ",
			short(l.DetachedHead), l.DetachedCommits, l.Remote, keepDetached(name), name)
	case l.Reason == engine.ReasonBranchExistsLocally:
		return "a local branch " + name + " already exists, so feature start fetched " + l.Remote + " but left it alone: compare it with " + l.Remote + "/" + name + " and switch to the one to work on; commit on " + name + ", "
	case l.Reason == engine.ReasonSwitchFailed:
		return "feature start fetched " + l.Remote + " but git switch failed (" + gitLine(l.GitError) + "): fix that, then `git switch " + name + "`; commit on " + name + ", "
	case l.Reason == engine.ReasonRemoteDiffers:
		return "this checkout's remote " + l.Remote + " points at " + oneLine(l.HaveURL) + ", not " + shadowURL(l.Shadow) + ", so feature start left it alone: check out with `" + r.CheckoutHint + "`, commit on " + name + ", "
	case l.AddFailed:
		return "git remote add " + l.Remote + " failed (" + gitLine(l.GitError) + "): check out with `" + r.CheckoutHint + "`, commit on " + name + ", "
	case l.NotFound:
		return "git fetch " + l.Remote + " said the repository was not found: " + accountTrap(l) + ". A person runs `" + setURL(l.Remote, l.Shadow, l.Login) + "` (that remote only), then `git fetch " + l.Remote + " && git switch " + name + "`; commit on " + name + ", "
	default:
		return "git fetch " + l.Remote + " failed (" + gitLine(l.GitError) + "); once git can reach the shadow repo, `git fetch " + l.Remote + " && git switch " + name + "`; commit on " + name + ", "
	}
	return "check out with `" + r.CheckoutHint + "`, commit on " + name + ", "
}

// gitLine is git's message on one line.
func gitLine(msg string) string { return oneLine(strings.Join(strings.Fields(msg), " ")) }
