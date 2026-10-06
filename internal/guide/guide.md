# Ghostbranch: guide for coding agents

Ghostbranch gives a public GitHub repo private feature branches. Unreleased work lives in a private
**shadow repo**, and reaches the public repo only through a gated **promote**: a rebase on public
main, green CI, and a leak scan of exactly what will be published.

`ghostbranch guide` prints this guide. It always matches the installed binary.

## The loop

1. `ghostbranch feature start <name> --json` creates the feature branch in the shadow repo.
2. Work on that branch, and push it **to the shadow repo only**.
3. `ghostbranch gate <name> --json` rebases on public main, scans for leaks and runs CI.
4. `ghostbranch promote <name> --mode squash -m "<message>" --plan --json` saves a plan of exactly
   what would be published.
5. Follow `next`:
   - `needs_human: true`: stop, and give the person that exact command.
   - `needs_human: false`: a person has allowed agents to publish here; you may run it.

## Rules

- Always pass `--json`. Act on `exit_code`, `error.code` and `next`; don't parse the text.
- Run `next` commands exactly as given. Add the flags listed in `needs_input` yourself.
- **When `needs_human` is true, stop.** Tell the person the exact command and why, and don't look
  for another way.
- Exit 6: the repos moved, so plan again. Exit 3: fix the code or remove the leaked content, then
  gate again. Exit 4: resolve the conflict the error names.
- Set `GHOSTBRANCH_ACTOR=agent:<your-name>` (for example `agent:claude-code`) on every
  `ghostbranch` command, so the ledger records that an agent acted. It is a label only; it never
  grants or blocks anything.
- Don't know a code? `ghostbranch explain <CODE> --json`.
- Want to see what a command would do? Add `--plan` (or `--dry-run`): it returns the exact
  operations and changes nothing.

## Never

- Never push a feature branch, `ghostbranch`, or a `ghostbranch-gate/*` branch to the public repo
  with git. Publishing goes through `ghostbranch promote` only.
- Never skip git hooks with `--no-verify`. The pre-push guard is there to stop leaks.
- Never fake a terminal, read or type a console code, or run `ghostbranch agents allow`. Those are
  the person's.
- Never edit `grants.json`, plan files or `.ghostbranch.yml` to get past a check.
- Never try to waive or hide a leak-scan finding.

You may *suggest* these to the person, never run them yourself:

- `ghostbranch guard install` in their checkout, which blocks pushing feature work to the public
  remote.
- `ghostbranch agents allow --for 6h`, if they want you to publish without them for a while.

## Exit codes

{{EXIT_CODES}}

## Error codes

{{ERROR_CODES}}

## The pre-push guard (for people)

`ghostbranch guard install` adds a pre-push hook to your checkout that refuses to push Ghostbranch
feature work to the public repo. Its limits:

- It needs git 2.31 or later, and it refuses checkouts whose hooks are managed by Husky.
- An existing pre-push hook keeps running, under the name `pre-push.ghostbranch-chained`; a hook
  that decides what to do from its own file name may not run as before.
- It recognizes GitHub remotes by URL; an SSH host alias (for example `git@github-work:…`) is not
  recognized.
- It knows features from the local mirror. Feature work pushed under another branch name is caught
  only if the mirror has seen it, so run `ghostbranch sync` now and then. A branch named after a
  feature is always caught.
- It covers only the checkout it is installed in, and `git push --no-verify` skips it.
- It reads Ghostbranch's state from the default location unless `GHOSTBRANCH_HOME` is set where git
  runs the hook.

## Agent grants in Claude Code (for people)

An agent grant (`ghostbranch agents allow`) lets agents publish with `--yes`, but Claude Code may
still block an agent from running `ghostbranch apply … --yes` as self-approval. If you want the
agent to publish under the grant, also allow the command in Claude Code, for example in the
project's `.claude/settings.local.json`:

```json
{ "permissions": { "allow": ["Bash(ghostbranch apply:*)"] } }
```

Keep that file out of commits.

## Strict isolation (for people)

The console code, grants and the guard stop mistakes. They do not stop a determined agent: an agent
running as your OS user could fake a terminal, edit local files, or push with git directly. The real
boundary is the token.

1. Create a fine-grained token scoped to **the shadow repo only**: Contents, Pull requests and
   Workflows read and write; Actions and Commit statuses read.
2. Set it as `GHOSTBRANCH_TOKEN` **only in the agent's environment**.
3. Make sure the agent's sandbox cannot read your `gh` login or keychain. Otherwise
   `gh auth token` hands it your full token.
4. Check it: in the agent's environment, `ghostbranch agents status --json` shows
   `"token_can_publish": "false"`.

Then only you can publish: run `ghostbranch apply <plan>` in your own terminal, where Ghostbranch
uses your `gh` login.
