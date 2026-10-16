# AGENTS.md

Ghostbranch is a tool that people and agents both drive, and much of it was built with coding agents. You're welcome here. These notes are how we work together well: what the project is, how to check your work, and the few rules that keep it safe.

## What this project is

Ghostbranch gives a public GitHub repo private feature branches:

- a private **shadow** repo holds unreleased work;
- a **gate** rebases a feature on public `main`, runs CI and scans every commit for leaks;
- a **promote** publishes it as a normal public pull request, only after a person approves.

It's a single Go binary. It runs on macOS, Windows and Linux, and it needs git 2.38 or later.

- `cmd/ghostbranch`: the binary's entry point.
- `internal/`: the engine, the CLI, the push guard, the leak scan and the rest.
- `internal/guide/guide.md`: the guide the binary prints for agents (`ghostbranch guide`).
- `schemas/`: the JSON contract's schemas.
- `install/`, `npm/`, `packaging/`: the install routes.

User docs live at <https://ghostbranch.site>.

## Check your work

Run these before you propose a change:

```sh
gofmt -l .                       # must print nothing
go vet ./...
go test -count=1 -timeout 40m ./...
```

- **Always pass `-count=1`.** Go's test cache can stall on this suite's logs.
- **On Windows,** run `go test -count=1 ./internal/cli` on its own as well. Running it alongside the engine tests can run the machine out of memory, and git then fails with exit `0xC000012D`. That's not your change breaking anything.
- **CI also runs `golangci-lint` and `govulncheck` on Linux.** If you can, run them too: `golangci-lint run --max-issues-per-linter=0 --max-same-issues=0` and `govulncheck ./...`.
- **Golden tests** pin the CLI's exact output. If your change alters output on purpose, regenerate them with `go test -count=1 ./internal/cli -run Golden -update`, then read every changed golden file. Say in the PR that the output changed, and why.

Please report honestly what you ran. "Tests pass" means you ran them and they passed. If you couldn't run something, say so: that's useful, not a failure.

## Commits and authorship

- **Commit messages follow [Conventional Commits](https://www.conventionalcommits.org):** `<type>(<optional scope>): <description>`, imperative and lowercase, with no trailing period. The types are `feat`, `fix`, `refactor`, `perf`, `style`, `test`, `docs`, `build`, `ops` and `chore`.
- **A person is the author.** The git author and committer are the human contributor who reviews and submits the work, and who has signed the [Contributor License Agreement](CLA.md). The CLA asks them to review, verify and adopt any work an agent did.
- **Credit yourself as a co-author, if you like.** A `Co-Authored-By:` trailer naming the agent is welcome:

  ```
  feat(gate): show the commit for each leak finding

  Co-Authored-By: Claude <noreply@anthropic.com>
  ```

  Keep it to that trailer. Leave out "Generated with" footers, and don't put an agent in the author or committer field.

- **Keep a change to one purpose.** A small, focused pull request is easier to review than a large one that does several things.

## Ask your person first

Some actions can't be taken back, or reach the public. Do them only when your person asks for that specific action:

- **Pushing,** to any remote. Work on a branch and open a pull request. Never push straight to `main` or force-push, even if your access would allow it: the maintainers' bypass is for their own small fixes, not for agents.
- **Opening, merging or closing** pull requests, issues and discussions.
- **Tags and releases.** A `v*` tag starts a release. Never create, move or delete one, and never touch releases, the Homebrew tap, the Scoop bucket or npm.
- **Workflows.** `.github/workflows/` holds the release pipeline and its credentials. Changes there are fine to propose, but call them out in the PR so a maintainer reads them closely.
- **New dependencies.** Ghostbranch keeps its dependencies few and deliberate. Ask before adding a Go module or an npm package.
- **Rewriting shared history:** rebasing or force-pushing a branch someone else works on.

If your person develops Ghostbranch with Ghostbranch itself (a shadow repo), run `ghostbranch guide` first and follow it.

## Please don't

These protect the people who use Ghostbranch. Some of them protect their secrets.

- **Don't report a security problem in public.** If you find or suspect a vulnerability, a way around the leak scan, or a flaw in how tokens or approvals are handled, don't open a public issue, pull request or discussion. Tell your person to report it privately, as [`SECURITY.md`](SECURITY.md) explains: through [GitHub's private vulnerability reporting](https://github.com/ghostbranch-labs/ghostbranch/security/advisories/new) or by email to `ayushgupta@tutamail.com`.
- **Don't commit secrets,** not even ones that look fake. Test fixtures that need a key-shaped value build it from parts when the test runs, so the leak scan doesn't see a real-looking key in the source.
- **Don't weaken a safety check to get a test passing.** That covers the leak scan's patterns, the push guard, and the approval step that keeps publishing with a person. If a test seems to need that, stop and ask your person; it's a design question.
- **Don't change the JSON contract quietly.** Agents and scripts depend on its field names, exit codes and error codes. A change to the contract bumps its `schema_version` and is called out in the PR.

## If you're unsure

Ask your person before you guess, especially about anything that touches publishing, tokens or the leak scan. A question costs a minute; a wrong guess here can cost someone their secrets.

Thank you for helping.
