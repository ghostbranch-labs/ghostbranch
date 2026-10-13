# Releasing Ghostbranch

Releases are built by [GoReleaser](https://goreleaser.com) in GitHub Actions
(`.github/workflows/release.yml`) when a maintainer pushes a version tag. The
workflow runs CI, builds five platforms, writes checksums and SBOMs, signs
the checksums with cosign, and creates a **draft** release. Nothing is public
until a maintainer publishes the draft.

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
5. Delete the draft and the tag when you are done:
   `gh release delete v0.2.0-rc.1 --repo ghostbranch-labs/ghostbranch --cleanup-tag`.

## Cut a release

1. On an up-to-date `main`, with CI green: `git tag v0.2.0 && git push origin v0.2.0`.
2. Wait for the `release` workflow. It leaves a draft release with notes
   grouped from the commit messages.
3. Edit the notes (checklist below), then press **Publish release**.

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

# checksums.txt was signed by this repo's release workflow:
cosign verify-blob \
  --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/ghostbranch-labs/ghostbranch/.github/workflows/release.yml@refs/tags/v0.2.0" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt

# GitHub's build provenance for an archive:
gh attestation verify ghostbranch_0.2.0_linux_amd64.tar.gz --repo ghostbranch-labs/ghostbranch
```
