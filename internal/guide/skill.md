---
name: ghostbranch
description: Use when the repo has a Ghostbranch shadow repo, or when asked to start, gate, promote or publish a private feature with Ghostbranch. Run `ghostbranch guide` first and follow it.
---
<!-- Installed by `ghostbranch guide --install`. Remove it with `ghostbranch guide --uninstall`. -->

# Ghostbranch

Before using `ghostbranch`, run `ghostbranch guide` and follow it. It matches the installed version.

In short: always pass `--json`, act on `exit_code` and `next`, and when `next` says
`needs_human: true`, stop and give the person that command. Never push feature work to the public
repo with git, never skip git hooks, and never run `ghostbranch agents allow` yourself.
When the gate finds a leak, fix it or give the person the `waivers add` command from `next`;
never run it yourself, and never edit `waivers.json`, marker lines or `.ghostbranch.yml` to get past
it. Set `GHOSTBRANCH_ACTOR=agent:claude-code` (or your agent's name) on every `ghostbranch`
command.
