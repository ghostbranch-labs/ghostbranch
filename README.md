<h1>
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset=".github/logo-dark.svg">
    <img src=".github/logo-light.svg" alt="" width="40" align="center">
  </picture>
  Ghostbranch
</h1>

[![Sponsor](https://img.shields.io/github/sponsors/ayushguptax)](https://github.com/sponsors/ayushguptax)

**Private branches for public repos.**

Build unreleased features in a private shadow of your open-source repo, running the same CI. Publish with one gated command, after a leak scan and with a person's approval.

<img width="1100" height="600" alt="Demo: ghostbranch init creates a private shadow repo, the guard refuses a push of feature work to the public repo, the gate's checks pass, and one clean public pull request opens without the internal note." src="https://github.com/user-attachments/assets/e881a266-e807-4d36-a4be-2b76d82b0c28" />

Website and docs: <https://ghostbranch.site>.

**Jump to:** [Try it](#try-it) · [Install](#install) · [What it needs and costs](#what-it-needs-and-what-it-costs) · [First feature](#a-first-feature-in-six-commands) · [How it works](#how-it-works) · [Support](#support) · [License](#license)

## The problem

On GitHub, a public repo can't have a private branch. Teams building unannounced features on an open-source project, and maintainers fixing a vulnerability under embargo, end up running a private mirror by hand. That works until someone makes a mistake.

- **Every branch is public.** Anyone can see a branch pushed to a public repo, and its CI runs, too.
- **Forks stay public.** A fork of a public repository is always public, so a fork can't keep anything private.
- **Security fixes get no CI.** GitHub's temporary private forks for security advisories can't run Actions, so a fix under embargo is tested by hand, or not at all, before it goes public.
- **Hand-run mirrors leak.** A push to the wrong remote, a committed secret, or CI that has drifted from the real repo: the feature goes out early, or arrives broken.

## How it works

Sync brings public changes in. Only promote goes out.

1. **Work privately.** Each feature lives in a private shadow repo that mirrors your public one. Its CI runs there just as it would on the public repo.
2. **Gate it.** Rebase on public `main`, run the full CI, and scan every commit for secrets, internal files and private notes.
3. **Promote it.** Publish exactly what was scanned as a normal public pull request. A person approves it with a one-time code typed at their own terminal.

Ghostbranch is a local-first CLI that people and coding agents both drive, with stable JSON output and exit codes. It runs no servers and collects no telemetry. Everything happens on your machine or in your own GitHub account.

## Status

Ghostbranch is in beta. Commands, their output and the JSON contract may still change between releases.

Agent runs have been tested with Claude Code. Codex has a skill (`ghostbranch guide --install codex`) but hasn't been tested yet.

## Try it

To see it work before pointing it at a repo you care about, use the template [ghostbranch-labs/try-ghostbranch](https://github.com/ghostbranch-labs/try-ghostbranch). It has one file and a CI check that runs in seconds. Click **Use this template**, keep your copy public, and follow its README: about five minutes from install to a passing gate, with the cleanup at the end. The site has the same steps: [Try it](https://ghostbranch.site/docs/getting-started/try-it).

## Install

One Go binary for macOS, Windows and Linux. It needs git 2.38 or later on your `PATH`.

Pick one route for your system. Each installs the same binary.

### Any system

npm (pnpm, yarn and bun work too):

```sh
npm install -g ghostbranch
```

Go:

```sh
go install github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch@latest
```

### macOS

Homebrew:

```sh
brew install ghostbranch-labs/tap/ghostbranch
```

Or the install script:

```sh
curl -fsSL https://ghostbranch.site/install.sh | sh
```

### Linux

The install script:

```sh
curl -fsSL https://ghostbranch.site/install.sh | sh
```

Or Homebrew:

```sh
brew install ghostbranch-labs/tap/ghostbranch
```

### Windows

The install script, in PowerShell:

```powershell
irm https://ghostbranch.site/install.ps1 | iex
```

Or Scoop:

```powershell
scoop bucket add ghostbranch https://github.com/ghostbranch-labs/scoop-bucket
scoop install ghostbranch
```

winget is coming after the first release.

Then run `ghostbranch init` in a clone of your public repo. It asks only what it can't work out on its own: the public repo, a GitHub login if there's no token yet, where the private shadow repo lives, whether to install the push guard, and whether to turn on scheduled sync. After a preview and your confirmation, it creates the shadow repo, installs the guard if you said yes, runs `ghostbranch doctor` and walks you through your first feature. Flags or `--json` skip the prompts.

### What it needs and what it costs

- **CI that runs on pull requests.** The gate runs your public repo's GitHub Actions workflows on a draft pull request in the shadow repo, so at least one workflow needs `on: pull_request`. Without one, the gate stops with `NO_CI`.
- **The Ghostbranch GitHub app, on the repos you pick.** It asks for write access to contents, pull requests, workflows, Actions and administration (administration only so `init` can create the shadow repo). [What each permission is for](https://ghostbranch.site/docs/security/tokens-and-keychain#the-github-app-login). Ghostbranch never deletes a repo, and `init` never writes to the public repo.
- **Actions minutes.** The shadow repo is private, so its CI counts against your account's minutes: GitHub Free includes 2,000 a month, Pro and Team 3,000. Each gate costs one run of your pull-request CI. Scheduled sync is off unless you turn it on.

## A first feature in six commands

Six Ghostbranch commands, with your usual git work in between. The example feature is `add-search`.

```sh
ghostbranch sync                          # bring the shadow repo up to date with public main
ghostbranch feature start add-search      # create the feature branch in the shadow repo and switch to it

# edit, test and commit as usual (outside a checkout, run the "Check it out with" line it prints)
git push shadow add-search                # push to the shadow repo, never to origin

ghostbranch gate add-search               # rebase on public main, leak scan, full CI
ghostbranch promote add-search --plan --title "Add search"   # save a plan of exactly what goes public
ghostbranch apply <plan-id>               # a person types the code it shows; this opens the public PR

# review and merge the PR as usual (a merge commit or a rebase, not "Squash and merge"), then:
ghostbranch sync                          # bring the merge in and close the feature
```

Every result ends with **Next**: the command to run after it, and why. `ghostbranch doctor` checks your setup and names the fix for anything that's off. The full walkthrough is in the docs: [Your first feature](https://ghostbranch.site/docs/getting-started/first-feature).

Coding agents run the same loop with `--json`, and `ghostbranch guide` prints their instructions. Only a person can approve a publish, unless that person has allowed agents to publish with `ghostbranch agents allow`.

To give your agent the Ghostbranch skill, run `ghostbranch guide --install claude` (or `codex`). Claude Code can also install it as a plugin:

```sh
claude plugin marketplace add ghostbranch-labs/ghostbranch
claude plugin install ghostbranch@ghostbranch
```

The skill needs the `ghostbranch` binary installed.

## Support

Ghostbranch is free and open source. Sponsoring it pays for the time spent building and fixing it, the CI that tests every change, the website's domain and, later, code signing, so your computer can tell that a download really came from us. If it saves you work, please consider [sponsoring it on GitHub](https://github.com/sponsors/ayushguptax).

Sponsorships go through the personal GitHub Sponsors page of Ayush ([@ayushguptax](https://github.com/ayushguptax)), who builds and maintains Ghostbranch, until Ghostbranch's organisation finishes its tax setup. Every penny goes into Ghostbranch.

### Sponsors

Thank you to the people and companies who sponsor Ghostbranch. Sponsors are listed here and on the website.

No sponsors yet. [Be the first](https://github.com/sponsors/ayushguptax).

## License

[Apache-2.0](LICENSE). Contributions will require signing a contributor license agreement.
