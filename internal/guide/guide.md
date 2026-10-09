# Ghostbranch: guide for coding agents

Ghostbranch gives a public GitHub repo private feature branches. Unreleased work lives in a private
**shadow repo**, and reaches the public repo only through a gated **promote**: a rebase on public
main, green CI, and a leak scan of exactly what will be published.

`ghostbranch guide` prints this guide. It always matches the installed binary.

## The loop

0. **Always run `ghostbranch sync --repo <public> --json` first**, before `feature start` or resuming
   a feature: features start from shadow main. Sync never writes to public. New repo? *Set up a repo*.
1. `ghostbranch feature start <name> --repo <public> --json` creates the feature branch in the shadow repo.
2. Run the commands in `data.checkout`, in order (argvs; `data.checkout_hint` is the same as one shell
   line). They fetch the branch from the shadow repo (in a checkout of the public repo, usually a remote
   named `shadow`) and switch to it. Commit there, and push **to the
   shadow repo only** (`git push shadow <name>`), never to `origin`.
3. `ghostbranch gate <name> --repo <public> --json` rebases on public main, scans for leaks and runs CI.
4. `ghostbranch promote <name> --repo <public> --plan --json` publishes in the repo's `promote.default_mode`
   (`preserve` unless set); pass `--mode` to choose. With more than one commit, add
   `--title "<PR title>"`, written by you. It saves a plan of exactly what goes public: every commit,
   file, author and the PR text.
5. Follow `next`:
   - `needs_human: true`: stop, and give the person that exact command.
   - `needs_human: false`: a person has allowed agents to publish here; you may run it.

## Choosing the history

- **`preserve` (default unless `promote.default_mode` says otherwise):** each feature commit goes public
  as it is, minus overlay paths and internal sections; commits left empty are dropped. Tidy history on
  your feature branch first if it needs it.
- **`squash`:** one commit: `--mode squash -m "<message>"`.
- **`curated`:** write a JSON file from the gate result's `"commits"` (each has `source`, `subject`,
  `author`, `files`) to group, reorder and reword:
  `{"commits": [{"from": ["<source sha>", "<source sha>"], "message": "feat(x): what this does"},
  {"from": ["<source sha>"], "message": "fix(y): ..."}]}`
  then `ghostbranch promote <name> --mode curated --curate groups.json --title "<PR title>" --plan --json`.
  Every commit must appear exactly once. Reordering commits that touch the same lines fails with
  `CURATE_CONFLICT` or `CURATE_TREE_MISMATCH`: keep those in order, or reorder on your branch.
- **Authors:** `promote.author_email` (`noreply` by default) decides who each public commit says wrote
  it. Never rewrite authors on your branch to hide them.
- **History leaks:** a preserve publish (and its gate) checks every commit, so a secret added and
  later deleted still blocks; squash and curated check what they publish. A finding's `commits` are
  the feature commits that introduce or change it (a later commit that only carries it unchanged is
  not listed). To fix an older commit, rewrite it on your branch and push with
  `git push --force-with-lease <shadow remote> <name>`, or publish with `--mode squash` or
  `--mode curated` so it never ships. A leaking commit message: reword it, or use curated mode with
  new messages. Never waive a finding yourself; a person may waive a false positive at their console.

## Long CI: jobs

- `gate` waits for CI by default; that is the normal path. For CI that runs long, detach:
  `ghostbranch gate <name> --detach --repo <public> --json` returns `data.job` once CI has started
  (`data.checks` is `pending`). Do other work, then `ghostbranch wait <job> --repo <public> --json`.
- `wait --json` prints one JSON object per line. **The last line is the result**, the same as
  `gate --json`; the lines before it are progress events (`waiting`, `check`, `settled`, whose
  `outcome` is `passed`, `failed`, `no_ci` or `timeout`). Read the last line only.
- A finished job answers at once. `wait --timeout 10m` returns `checks: pending` (exit 0) if CI is still
  running; run `wait` again later.
- After Ctrl-C you get `INTERRUPTED`. With `error.details.job`, CI is still running: run
  `ghostbranch wait <job> --repo <public> --json`, not `gate`. Without `job` or `plan_id` nothing is pending: run the
  command again. If the session dropped, `ghostbranch status --repo <public> --json` names a pending job
  (`gate.job`): wait on it.
- `JOB_STALE`: a newer gate replaced the job; `error.details.current_job` names the current one; if
  `current_job` is empty, gate again.
- `promote --detach` returns the job in `data.gate.job` and no plan; once `wait` passes, run the same
  promote without `--detach` (it reuses the green gate).
- `INTERRUPTED` with `error.details.plan_id` after `promote`: the plan is saved and the publish may have
  started. Resume with `ghostbranch apply <plan_id> --repo <public>` (as `next` says; with `needs_human`
  the person runs it), never `promote` again (it makes new commits). `BRANCH_EXISTS` with
  `error.details.plan_id` means the same: apply that plan.

## Set up a repo

1. Shadow repo already exists (another machine, or a fresh `GHOSTBRANCH_HOME`)? Run
   `ghostbranch init <public> --adopt --plan --json`, then without `--plan`: it only registers the pair
   here and checks the shadow repo. Otherwise the same without `--adopt`. If the person named a shadow
   repo, add `--shadow <owner/name>` to both.
2. Exit 1 with `SHADOW_CREATE_DENIED` means the token may not create repos. **Stop**: show the person
   `error.details.steps` and **ask them to do steps 1 and 2 of `error.details.steps`**: create the empty
   private repo, and add it to the token. Never ask the person for a token with wider access.
   Once the person confirms both are done, run `ghostbranch init <public> --adopt --json` (the command
   in `next`; its `needs_human` stands for their steps 1 and 2).
3. `ghostbranch audit --json` and report to the person: repo guards the gate can't rewrite
   (`REPO_GUARD_UNREWRITABLE`), secrets the tests need (`SECRETS_MISSING`, or the to-check list when
   `SECRETS_UNLISTED`), that the shadow repo's Actions minutes are billed, and, if they opt in to
   scheduled sync, whether they need a `GHOSTBRANCH_SYNC_TOKEN` secret (see *Keeping up with public*).
   Guarded jobs run in the shadow repo on unreleased code: never deploy or publish credentials there.

## Lifecycle

- `ghostbranch status --repo <public> --json` shows the pair's health and every feature's state:
  `draft`, `in_review`, `gate_passed`, `published`, `merged`, `merged_moved`, `pr_closed`,
  `branch_gone`, `abandoned`, and `unreadable` (a record that can't be read; only a person at the
  console closes it, and the other features still show). `feature list --repo <public>` and `feature show <name> --repo <public>`
  give the same by feature.
- `ghostbranch sync --repo <public>` closes features whose public PR merged and whose branch hasn't
  moved: it deletes their shadow branches and says so in `data.closed`. `--no-close` skips that.
- `merged_moved`, `pr_closed` and any `branch_gone` that `sync` leaves (unmerged; or merged, but gated
  again after the publish, with its gate branch moved, or with a record too old to say what was
  published) are for a person to close (or promote a `merged_moved` feature's newer commits): `status`
  gives `ghostbranch feature close <name> --repo <public>` with `needs_human`. A close deletes the
  feature's shadow branches and needs a person at the console unless the feature is merged and
  unmoved; an agent grant does not cover it, so don't ask for `agents allow` to unblock a close.
- `ghostbranch feature abandon <name> --repo <public>` parks a feature and deletes nothing; `--undo`
  brings it back.
- `ghostbranch log --repo <public>` shows the history. `--feature <name>` also lists the entries of an
  earlier feature with the same name: tell them apart by time.
- `LEDGER_NOT_WRITTEN`: keep `details.refs`; after a close it is the only record of the deleted SHAs.
  `PLAN_FILE_UNREADABLE`: fix or delete the file at `details.path`. `remove` needs no GitHub token.
- `UPGRADE_NEEDED` with `error.details.record` (feature, plan, grants, waivers) means another machine
  runs a newer Ghostbranch: upgrade before changing anything. `RECORD_NEWER` on `status`, `feature list`
  or `feature show` means the same; reads still work, and such a feature is never closed unattended. A
  merged feature whose record is newer still counts as active and shows only through `RECORD_NEWER`:
  once this machine is upgraded, `sync` closes it.
- Never close or abandon a feature a person is still working on.

## Keeping up with public

- Sync is manual by default. When a result warns `SHADOW_BEHIND`, run `ghostbranch sync --repo <public>`
  (fast-forward only; it never resolves a conflict). The gate always rebases on public main, so it is
  never affected. `feature start` refuses instead, with the error `SHADOW_BEHIND`: sync, then start again.
- Sync's pushes start the shadow repo's push-triggered workflows, which re-test what public CI already
  tested, so sync cancels those runs (`--no-cancel` skips that). `cancelled_runs` lists only runs
  confirmed cancelled. `CI_NOT_CANCELLED` means runs may still be going: the token couldn't
  cancel them (it needs Actions write), they kept going, or sync couldn't check; it names the runs it
  knows of, or counts them. They only cost minutes. To stop them for good, the person adds
  the guard from the audit's `PUSH_TRIGGERS_ON_SYNC` to each push-triggered job; a job that already
  has an `if:` combines them: `<existing> && (github.event_name != 'push' || github.repository == '<public>')`.
- Scheduled sync is opt-in and costs Actions minutes (every 6 hours is about 120 a month). It is the
  person's choice: they set `sync.schedule` in `.ghostbranch.yml` on the `ghostbranch` branch, then
  `ghostbranch upgrade --plan` and `ghostbranch upgrade` install the workflow. `sync.schedule: off`
  plus `upgrade` removes it. Don't turn it on yourself.
- The workflow uses its own default token. If public commits change workflow files, the person adds a
  `GHOSTBRANCH_SYNC_TOKEN` secret to the shadow repo: a fine-grained token with shadow Contents and
  Workflows write, never public write. A fine-grained token applies its permissions to every repo it
  selects, so for a private public repo it could write that repo too: scheduled sync isn't supported
  there; use `ghostbranch sync --repo <public>`.
- `OVERLAY_OUTDATED` means run `ghostbranch upgrade --plan`, then `ghostbranch upgrade`.
- Repo guards: the gate rewrites `github.repository == 'org/project'` guards in its CI candidate only,
  so those jobs run in the shadow repo, on unreleased code; what publishes is untouched. A guarded job
  that deploys or publishes must never get its secrets in the shadow repo; if guarded jobs deploy, the
  person sets `ci.rewrite_repo_guards: warn`. `REPO_GUARD_UNREWRITABLE` names a guard it can't rewrite:
  tell the person that job skipped and was not tested.

## Rules

- Always pass `--json`. Act on `exit_code`, `error.code` and `next`; don't parse the text.
  JSON is on stdout and progress on stderr: never merge the two streams (no `2>&1`).
- Run `next` commands exactly as given. Add the flags listed in `needs_input` yourself.
- **When `needs_human` is true, stop.** Tell the person the exact command and why, and don't look
  for another way.
- Exit 6: the repos moved, so plan again. Exit 3: fix the code or remove the leaked content, then
  gate again. Exit 4: resolve the conflict the error names, except `SYNC_DIVERGED`, which needs a
  person: stop and tell them.
- Set `GHOSTBRANCH_ACTOR=agent:<your-name>` (for example `agent:claude-code`) on every
  `ghostbranch` command, so the ledger records that an agent acted. It is a label only; it never
  grants or blocks anything.
- Write the PR `--title` (and, for squash, `-m`; for curated, each group's message) yourself from the
  change: they go public. Show them to the person; use theirs if they give them.
- `LOCKED`: another ghostbranch command is running on this machine (`error.details.held_by`: `pid`,
  `op`, `since`). Wait and retry. A lock frees itself when its process ends; never delete lock files.
- An unknown key in `.ghostbranch.yml`, including under `sync:` or `ci:`, is `UPGRADE_NEEDED` with
  `error.details.unknown_key`: fix the typo, or upgrade if a newer Ghostbranch set the pair up.
- Don't know a code? `ghostbranch explain <CODE> --json`.
- Want to see what a command would do? Add `--plan` (or `--dry-run`): it returns the exact operations
  and changes nothing.

## When the gate finds a leak

- **Exit 3, `LEAK_FOUND`:** `error.details.findings` lists what would leak. Remove it from the
  feature branch, push to the shadow repo, and gate again (the first command in `next`).
- A finding in a test fixture that is plainly a placeholder (an obviously fake value such as `fake-`,
  `example-` or `test-`, or a token built for tests): you may rewrite the fixture to build the value at
  runtime (for example by concatenating parts), and tell the person in your handover which finding you
  rewrote. Anything that might be real: **stop and give the person the `waivers add` command from
  `next`**; they check it at their console. Never waive it yourself. Findings in the commit message,
  branch or PR text are never waivable: change the text.
- **Exit 3, `MARKER_INVALID`:** `error.details.problems` names each bad marker line. If it is clear
  which lines are meant to be internal, restore the marker pair (for example, add the missing
  `END-INTERNAL` line). Never delete, rewrite or move marker lines or the internal lines between
  them to make the gate pass; if it is not clear, stop and ask the person.
- Never change `leak_scan` (`markers`, `block_patterns`, `strip`) in `.ghostbranch.yml` to get past a
  finding; that is the person's call.
- The warnings `WORKFLOW_CHANGED`, `LARGE_FILE` and `BINARY_FILE` don't block. Mention them to the
  person when you hand over a publish.

## Never

- Never push a feature branch, `ghostbranch`, or a `ghostbranch-gate/*` branch to the public repo
  with git. Publishing goes through `ghostbranch promote` only.
- Never skip git hooks with `--no-verify`. The pre-push guard is there to stop leaks.
- Never fake a terminal, read or type a console code, or run `ghostbranch agents allow`. Those are
  the person's.
- Never edit `grants.json`, plan files or `.ghostbranch.yml` to get past a check.
- Never run `ghostbranch waivers add`, edit `waivers.json`, or otherwise hide a leak-scan finding.
- Never create or delete repos (other than through `ghostbranch init`), or change repo settings, tokens
  or secrets yourself; those are the person's.

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
- It knows features from the local mirror. It refuses any push that carries an unpublished feature
  commit the mirror knows of, under any branch name, and a pushed ref that isn't a commit. A branch
  named after a feature the mirror knows is refused whatever it carries, rewritten commits included.
  Run `ghostbranch sync --repo <public>` now and then so the mirror has seen your features. Commits
  rewritten out of the shadow repo can't be recognized under another name; publish only with `promote`.
- It covers only the checkout it is installed in, and `git push --no-verify` skips it.
- It reads Ghostbranch's state from the default location unless `GHOSTBRANCH_HOME` is set where git
  runs the hook.

## Agent grants in Claude Code (for people)

An agent grant (`ghostbranch agents allow`) lets agents publish with `--yes`, but Claude Code may
still block an agent from running `ghostbranch apply … --yes` as self-approval. If you want the
agent to publish under the grant, also allow the command in Claude Code, for example in the
project's `.claude/settings.local.json` (keep that file out of commits):

```json
{ "permissions": { "allow": ["Bash(ghostbranch apply:*)"] } }
```

## Internal sections (for people)

Lines between a `BEGIN-INTERNAL` line and an `END-INTERNAL` line are removed from every published
file, and the gate's CI runs on the stripped files, so it tests exactly what goes public. Markers are
matched ignoring case. Put each marker on its own line, in the file's comment syntax:
`// BEGIN-INTERNAL`, `# END-INTERNAL`, `<!-- BEGIN-INTERNAL -->`. A line that looks like a marker but
is not exactly one fails the gate rather than guess: a comment that adds text or punctuation after the
marker (`// BEGIN-INTERNAL billing`, `// BEGIN-INTERNAL.`), a doubled prefix (`/// BEGIN-INTERNAL`,
`## END-INTERNAL`), or a marker inside a multi-line comment (`/* BEGIN-INTERNAL` on one line, `*/` on
another). So do markers in a UTF-16 or UTF-32 file, or in one Git reads as binary. A person can change
the tokens with `leak_scan.markers` in `.ghostbranch.yml`.

## Token permissions (for people)

Any token can read public repos, so for a public repo you only grant permissions on the shadow repo.
A fine-grained token applies the same permissions to every repo it selects, so if your public repo is
private, a token that can read it can also write it: keep it to yourself, never give it to an agent.

Fine-grained tokens, per command:

| Command | Permissions |
|---|---|
| `init` that creates the shadow repo | Administration write on **all repositories** of the owner (a token can't name a repo that doesn't exist yet), plus the `init --adopt` set |
| `init --adopt` (create the empty private repo yourself first) | Shadow: Administration, Contents and Workflows read and write |
| `sync`, `upgrade` | Shadow: Contents and Workflows write; Actions write (optional: lets `sync` cancel the workflow runs its pushes start) |
| `feature start`, `gate` | Shadow: Contents, Pull requests and Workflows write; Actions and Commit statuses read |
| `promote`, `apply` | Public: Contents and Pull requests write (yours, never an agent's) |
| `audit` | Contents read; optionally Secrets: read, to compare secret names |

## Strict isolation (for people)

The console code, grants and the guard stop mistakes. They do not stop a determined agent: an agent
running as your OS user could fake a terminal, edit local files, or push with git directly. The real
boundary is the token. Plan files, grants, waivers and the ledger are mode 0600 on macOS and Linux:
other OS users can't read them, but an agent running as you can.

1. Create a fine-grained token scoped to **the shadow repo only**: Contents, Pull requests and
   Workflows read and write; Actions and Commit statuses read. Actions write is optional: it lets
   `sync` cancel the runs its pushes start.
2. Set it as `GHOSTBRANCH_TOKEN` **only in the agent's environment**.
3. Make sure the agent's sandbox cannot read your `gh` login or keychain, or `gh auth token` hands it
   your full token.
4. Check it: in the agent's environment, `ghostbranch agents status --json` shows
   `"token_can_publish": "false"`.

Then only you can publish: run `ghostbranch apply <plan>` in your own terminal, where Ghostbranch
uses your `gh` login.
