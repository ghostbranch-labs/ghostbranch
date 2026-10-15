# AGENTS.md — Ghostbranch Code Repository

Guidance for AI coding agents working in or inspecting `ghostbranch-labs/ghostbranch`.

## 1. Security Vulnerabilities (CRITICAL)

**NEVER open a public GitHub issue, public pull request, or public discussion for a security vulnerability, leak detection bypass, authentication flaw, or secret handling issue.**

If you discover or suspect a security vulnerability or exploit:
1. **Refuse** to file a public issue or create a public reproduction.
2. Instruct the user to report it privately per [`SECURITY.md`](SECURITY.md):
   - **GitHub Private Vulnerability Reporting:** [Report a vulnerability](https://github.com/ghostbranch-labs/ghostbranch/security/advisories/new)
   - **Email:** `ayushgupta@tutamail.com`

## 2. Commit Authorship & CLA

- **Human authorship only:** Every commit must trace to a human developer who has signed the [Contributor License Agreement (CLA)](CLA.md).
- **No agent attribution:** Never add AI co-author trailers (`Co-Authored-By: <agent>`), "Generated with" footers, or agent names in git author or committer fields.
- **Conventional Commits:** Follow `<type>(<optional scope>): <description>` in imperative present tense with no trailing period.

## 3. Verification

Before proposing a change:
- Run Go tests: `go test -count=1 ./...`
- Verify formatting and vet: `gofmt -s -w .` and `go vet ./...`
- Never add or commit unverified secrets, tokens, or credential fixtures.

