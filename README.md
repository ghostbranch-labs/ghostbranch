# Ghostbranch

[![Sponsor](https://img.shields.io/github/sponsors/ayushguptax)](https://github.com/sponsors/ayushguptax)

**Private branches for public repos.**

Build unreleased features in a private shadow of your open-source repo, running the same CI. Publish with one gated command, after a leak scan and with a person's approval.

Website and docs: <https://ghostbranch.site>.

## The problem

On GitHub, a public repo can't have a private branch. Teams building unannounced features on an open-source project end up running a private mirror by hand. That works until someone makes a mistake.

- **Every branch is public.** Anyone can see a branch pushed to a public repo, and its CI runs, too.
- **Forks stay public.** A fork of a public repository is always public, so a fork can't keep anything private.
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

## Install

One Go binary for macOS, Windows and Linux. It needs git 2.38 or later on your `PATH`.

| Route | Command | Note |
|---|---|---|
| Homebrew | `brew install ghostbranch-labs/tap/ghostbranch` | |
| Scoop | `scoop bucket add ghostbranch https://github.com/ghostbranch-labs/scoop-bucket`<br>`scoop install ghostbranch` | |
| winget | Coming soon | Coming after the first release. |
| npm | `npm install -g ghostbranch` (also pnpm, yarn and bun) | |
| Install script | `curl -fsSL https://ghostbranch.site/install.sh \| sh` | macOS and Linux |
| Install script | `irm https://ghostbranch.site/install.ps1 \| iex` | Windows (PowerShell) |
| Go | `go install github.com/ghostbranch-labs/ghostbranch/cmd/ghostbranch@latest` | |

Then run `ghostbranch init` in a clone of your public repo. It asks only what it can't work out on its own: the public repo, a GitHub login if there's no token yet, where the private shadow repo lives, whether to install the push guard, and whether to turn on scheduled sync. After a preview and your confirmation, it creates the shadow repo, installs the guard if you said yes, runs `ghostbranch doctor` and walks you through your first feature. Flags or `--json` skip the prompts.

## A first feature in six commands

Six Ghostbranch commands, with your usual git work in between. The example feature is `add-search`.

```sh
ghostbranch sync                          # bring the shadow repo up to date with public main
ghostbranch feature start add-search      # create the feature branch in the shadow repo

# run the "Check it out with" line it prints, then edit, test and commit as usual
git push shadow add-search                # push to the shadow repo, never to origin

ghostbranch gate add-search               # rebase on public main, leak scan, full CI
ghostbranch promote add-search --plan --title "Add search"   # save a plan of exactly what goes public
ghostbranch apply <plan-id>               # a person types the code it shows; this opens the public PR

# review and merge the PR as usual (a merge commit or a rebase, not "Squash and merge"), then:
ghostbranch sync                          # bring the merge in and close the feature
```

Every result ends with **Next**: the command to run after it, and why. `ghostbranch doctor` checks your setup and names the fix for anything that's off. The full walkthrough is in the docs: [Your first feature](https://ghostbranch.site/docs/getting-started/first-feature).

Coding agents run the same loop with `--json`, and `ghostbranch guide` prints their instructions. Only a person can approve a publish, unless that person has allowed agents to publish with `ghostbranch agents allow`.

## Support

Ghostbranch is free and open source. Sponsoring it pays for the time spent building and fixing it, the CI that tests every change, the website's domain and, later, code signing, so your computer can tell that a download really came from us. If it saves you work, please consider [sponsoring it on GitHub](https://github.com/sponsors/ayushguptax).

Sponsorships go through the personal GitHub Sponsors page of Ayush ([@ayushguptax](https://github.com/ayushguptax)), who builds and maintains Ghostbranch, until Ghostbranch's organisation finishes its tax setup. Every penny goes into Ghostbranch.

### Sponsors

Thank you to the people and companies who sponsor Ghostbranch. Sponsors are listed here and on the website.

No sponsors yet. [Be the first](https://github.com/sponsors/ayushguptax).

## License

[Apache-2.0](LICENSE). Contributions will require signing a contributor license agreement.
