# Ghostbranch

Private feature branches for public GitHub repos.

GitHub has no branch-level read permissions, and forks of public repos are always public. Ghostbranch gives a public repo a private **shadow repo** where unreleased features are developed and tested with the same CI as the public repo. A feature reaches the public repo only through a gated **promote**: rebase on the latest public `main`, full CI, and a leak scan of exactly what will be published.

It is local-first: a CLI that people and coding agents both drive, with stable JSON output and exit codes, and later a desktop app, all on one engine. Ghostbranch runs no servers and collects no telemetry. Everything happens on your machine or in your own GitHub account.

---

## Status

Pre-alpha. There is nothing to run yet.

## License

[Apache-2.0](LICENSE). Contributions will require signing a contributor license agreement.
