# Releasing Ghostbranch

Releases are built by [GoReleaser](https://goreleaser.com) in GitHub Actions
(`.github/workflows/release.yml`) when a maintainer pushes a version tag. The
workflow runs CI, builds five platforms, writes checksums and SBOMs, signs
the checksums with cosign, and creates a **draft** release. Nothing is public
until a maintainer publishes the draft. Publishing it runs
`.github/workflows/publish.yml`, which checks the files again and then
updates Homebrew, Scoop and npm (below).

## Versions

- Tags are `vMAJOR.MINOR.PATCH`, for example `v0.1.0`.
- Ghostbranch is in beta, so versions are `0.x`. Below 1.0, a breaking change
  to commands or the JSON contract bumps the minor version (`v0.1.3` → `v0.2.0`).
- Test builds are `vX.Y.Z-rc.N`. They are published as pre-releases, so they
  are never offered as the latest version.

## Platforms

macOS (Apple Silicon and Intel), Linux (amd64 and arm64) and Windows (amd64).
Windows on ARM runs the amd64 build.

## Cut a test build

1. On an up-to-date `main`: `git tag v0.2.0-rc.1 && git push origin v0.2.0-rc.1`.
2. Wait for the `release` workflow. It leaves a draft pre-release.
3. Download it: `gh release download v0.2.0-rc.1 --repo ghostbranch-labs/ghostbranch --dir rc`.
4. Check it (below) and try the binary on each machine you have.
5. Delete the draft and the tag when you are done. `--cleanup-tag` deletes
   the tag on GitHub; also delete your local copy, so a later
   `git push --tags` can't push it again:
   `gh release delete v0.2.0-rc.1 --repo ghostbranch-labs/ghostbranch --cleanup-tag`,
   then `git tag -d v0.2.0-rc.1`.

If you publish a test build instead of deleting it, the `publish` workflow
runs. Before the first stable release it updates every route (the quiet
window); after that it goes to npm under the `next` tag only, never to
Homebrew or Scoop. A published rc on npm can't be taken back, so delete
test builds unless you mean it.

## Cut a release

1. On an up-to-date `main`, with CI green: `git tag v0.2.0 && git push origin v0.2.0`.
2. Wait for the `release` workflow. It leaves a draft release with notes
   grouped from the commit messages.
3. Edit the notes (checklist below), then press **Publish release**. That
   starts the `publish` workflow; check it went green (below).

## Release notes checklist

- One or two sentences on what this release is about.
- The highlights, from the grouped changelog. Cut the noise.
- **Upgrading:** anything a user of the previous version must do, such as
  re-running a command or upgrading every machine that shares a state
  folder. Say "nothing" if nothing.
- How to verify the download (below).

## Verify a download

```sh
# The files are what was released:
sha256sum --ignore-missing -c checksums.txt
shasum -a 256 -c --ignore-missing checksums.txt   # macOS

# checksums.txt was signed by this repo's release workflow:
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/ghostbranch-labs/ghostbranch/.github/workflows/release.yml@refs/tags/v0.2.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# GitHub's build provenance for an archive (releases made after the repo
# went public; GitHub attests public repos only):
gh attestation verify ghostbranch_0.2.0_linux_amd64.tar.gz --repo ghostbranch-labs/ghostbranch
```

## Where a published release goes

`publish.yml` runs when a release is published. A draft that is deleted
never distributes anything. Publish runs go one at a time, in the order the
releases were published. GitHub keeps only one queued publish run: a third
release published while one runs and one waits cancels the waiting one.
The cancelled run is for an older release than the one published after it,
which has already updated the tap, the bucket and npm, so usually leave it
alone: re-running it would move the tap, the bucket and npm `latest` back
to the older version. Re-run it (from the Actions tab, or set that release
back to draft and publish it again) only when the newer release didn't
replace it: for example, a stable release cancelled by a test build, which
goes only to npm `next`.

1. It downloads the release, checks every file against `checksums.txt`, and
   checks that `checksums.txt` was signed by `release.yml` at that tag. A
   failure stops it before anything is pushed or published.
2. It fills `packaging/homebrew/ghostbranch.rb.tmpl` and
   `packaging/scoop/ghostbranch.json.tmpl` with the version and checksums
   (`go run ./packaging/render`), and shows both on the run's summary page.
3. It pushes them, as one commit each named `ghostbranch <version>`, to
   `Formula/ghostbranch.rb` in `ghostbranch-labs/homebrew-tap` and
   `bucket/ghostbranch.json` in `ghostbranch-labs/scoop-bucket`.
4. It builds the six npm packages (`node npm/build.mjs`) and publishes them,
   the five platform packages first.

| Release | Homebrew and Scoop | npm tag |
|---|---|---|
| `vX.Y.Z` | yes | `latest` |
| `vX.Y.Z-rc.N` before any stable release | yes | `next` |
| `vX.Y.Z-rc.N` after a stable release | no | `next` |

A release counts as a test build if its tag has `-rc.N` **or** it is marked
pre-release. A plain tag marked pre-release is treated the same way as an
rc. After a stable release, a test build never reaches the tap or the
bucket, only npm's `next` tag. A tag that is neither `vX.Y.Z` nor
`vX.Y.Z-rc.N` stops the run.

While the repo is private, steps 3 and 4 are skipped. The run still checks
that the App reaches the tap and the bucket and nothing else.

The install scripts are attached to every release and listed in
`checksums.txt`, so they are checked and signed with the rest.
`ghostbranch.site/install.sh` and `/install.ps1` redirect to the latest
release's copies, so the scripts change only with a release. A copy
installed in another folder is upgraded by running the script again with
`GHOSTBRANCH_INSTALL_DIR` set to that folder.

Later work: a winget route is planned as roadmap item 8.1, and running the
gate as a GitHub Actions workflow as 8.2.

### When a publish run fails

- **The download step failed:** re-run the failed job.
- **The check failed:** don't re-run it. Find out why the files or the
  signature don't match, delete the release, and cut a new tag.
- **The tap or bucket push failed:** fix the cause (usually the App, below),
  then re-run the failed job. A push that already landed is skipped.
- **npm failed partway:** re-run the failed job. Packages already at that
  version are skipped. npm never lets a version be published twice, and
  unpublishing is limited, so don't try to "replace" a broken version: fix
  it and release the next patch.

## Release infrastructure

Everything below belongs to the `ghostbranch-labs` organization, not to a
person, so any maintainer with the right role can run, rotate or rebuild it.

| Piece | What it is for | How to rotate or recreate it | If it's missing |
|---|---|---|---|
| GitHub App `ghostbranch-release` (org-owned) | Lets `publish.yml` push to the tap and the bucket. Repository permission **Contents: read and write**, nothing else; no webhook. Install it with **Only select repositories** set to `homebrew-tap` and `scoop-bucket`. The workflow asks for a push token scoped to those two repositories, and uses a second, read-only token to list the installation's repositories; it fails if the list is anything other than exactly those two. Not the `ghostbranch-cli` App, which is the one people log in with. | Org settings → Developer settings → GitHub Apps (the Apps the org owns) → `ghostbranch-release` → Edit → Private keys → **Generate a private key**; put it in the `release` environment's `RELEASE_APP_PRIVATE_KEY`, then re-run the latest stable release's publish run (or publish a test build, which goes to npm `next`) and check the access step passes, then delete the old key. To recreate: a new App with the same permissions and installs, then update both values below. | The token step fails; nothing is pushed. |
| Environment `release` | The `publish` job's App credentials; `release.yml` uses the environment only for its tag restriction. **Deployment branches and tags:** tags `v*` only. Holds variable `RELEASE_APP_CLIENT_ID` (the App's Client ID) and secret `RELEASE_APP_PRIVATE_KEY`. | Repo → Settings → Environments → `release`. | GitHub recreates it empty on the next run, with no tag policy and no secrets. The token step then fails. Restore the `v*` tag policy, the variable and the secret before the next tag. |
| Environment `e2e` | The sandbox E2E token. Required reviewer: a maintainer. Secret `GHOSTBRANCH_E2E_TOKEN`. | Same page; a new fine-grained token limited to the sandbox repos. | The E2E workflow can't run. |
| Ruleset `release-tags` | Only maintainers may create, move or delete `v*` tags. | Repo → Settings → Rules → Rulesets. | Anyone with write access could start a release. |
| Allowed Actions | Only GitHub's own Actions and the listed ones run: `golangci/golangci-lint-action`, `goreleaser/goreleaser-action`, `sigstore/cosign-installer`, `anchore/sbom-action/download-syft`. (`actions/create-github-app-token` and `actions/setup-node` are GitHub's.) Every `uses:` is pinned to a commit. | Repo → Settings → Actions → General. | A workflow using another Action fails. |
| `ghostbranch-labs/homebrew-tap` | `brew install ghostbranch-labs/tap/ghostbranch`. `publish.yml` writes only `Formula/ghostbranch.rb`. | Recreate the repo with the same name. Add it back to the `ghostbranch-release` installation (Org settings → Third-party Access → GitHub Apps (the installed Apps) → `ghostbranch-release` → Configure → Repository access, **Only select repositories**); a deleted repo drops out of the list and a new one with the same name is not added back, and `publish.yml` fails its access check until it is. Then run `publish.yml` again for the latest **stable** release: Actions → publish → that release's run → Re-run. If that run is over 30 days old there is no Re-run button (`publish.yml` has no manual trigger); edit the release back to a draft and publish it again, which starts `publish.yml` afresh. That needs GitHub's immutable releases setting to be off: with it on, a published release can't go back to a draft. Packages already on npm are skipped. | `brew install` and `brew upgrade` fail. |
| `ghostbranch-labs/scoop-bucket` | `scoop bucket add ghostbranch https://github.com/ghostbranch-labs/scoop-bucket` then `scoop install ghostbranch`. `publish.yml` writes only `bucket/ghostbranch.json`. | Same as the tap. | `scoop install` and `scoop update` fail. |
| npm org `ghostbranch` | Owns `ghostbranch` and the five `@ghostbranch/cli-*` packages. The packages require two-factor authentication and refuse tokens (`mfa=publish`). | npmjs.com → the org → Members. | Nobody can publish. |
| npm trusted publishing, on each of the six packages | Lets `publish.yml` publish without a token, with provenance (the workflow pins npm to 11.21.0). On npmjs.com → the package → Settings → **Trusted publishing**: GitHub Actions, organization `ghostbranch-labs`, repository `ghostbranch`, workflow `publish.yml`, environment `release`. | Delete and add the entry again on that page (entries can't be edited). A new platform package needs a first publish by hand (`npm publish --access public`, with 2FA), then this entry. | That package's publish fails with an authentication error. |
| Site redirects (`ghostbranch-labs/ghostbranch-site`, `public/_redirects`) | `ghostbranch.site/install.sh` and `/install.ps1` → `https://github.com/ghostbranch-labs/ghostbranch/releases/latest/download/<script>` (302). | Edit the file in the site repo. | The one-line install commands fail; the scripts still work from a release page. |

### Adding a maintainer

1. Org role: member, with **Maintain** (or Admin) on `ghostbranch`, `homebrew-tap` and `scoop-bucket`.
2. The `release-tags` ruleset's bypass list, so they can push tags.
3. The `e2e` environment's required reviewers.
4. The npm org `ghostbranch`, as a member of a team with write access to the six packages. They need two-factor authentication on npm.
5. Org settings → Developer settings → GitHub Apps → `ghostbranch-release` → Edit: owners of the org can manage it; nobody else needs access.
