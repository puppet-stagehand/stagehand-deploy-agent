# Stagehand deploy agent: Maintaining

How a release is cut and verified, and what to keep an eye on. This repository is its own project: there is no
upstream to rebase onto. The agent began as a port of the Stagehand fork of voxpupuli/webhook-go; the files that
still carry that code say so in their headers, and `NOTICE` keeps the attribution. Nothing is merged from upstream
any more, and there is no branch model beyond `main` and release tags.

## What is in the repository

| Path | What it is |
|------|------------|
| `cmd/stagehand-deploy-agent` | The command: `serve`, `--check-config`, `import-history`, `version`. |
| `internal/config` | The strict YAML loader and the legacy `/etc/voxpupuli/webhook.yml` reader. |
| `internal/jobs` | Job store, runner, lock, deploy marker judgement, removal, history import. |
| `internal/server` | The mTLS server, CRL, handlers and the `/api/v1/info` document. |
| `internal/ops`, `internal/metrics` | The plain-HTTP ops listener and the Prometheus exposition. |
| `contract` | The golden replay of the fork's `/api/v1/deploys` answers and the list of recorded differences. |
| `packaging` | The systemd unit, package scripts, example config and `ssh_config`. |
| `hack` | The local checks. `hack/release-gate.sh` runs all of them. |
| `.goreleaser.yml`, `.github/workflows` | The release build and CI. |

## The image is not a deploy runtime

`ghcr.io/puppet-stagehand/stagehand-deploy-agent:<version>` is built `FROM scratch` and holds the agent binary only.
It is for `--check-config`, tests and the contract harness. It has no r10k, git or ssh, so it is not a deploy
runtime and the documentation must never suggest running deploys from it. A deployable image is a separate piece of
work. Keep the sentence "not a deploy runtime" in the Dockerfile label, the release notes and the user guide.

## Before you cut a release

1. Update `CHANGELOG.md` with a section for the new version and commit it on `main`.
2. Run the local gate on the exact commit you will tag, from a clean tree:

   ```sh
   sh hack/release-gate.sh ../stagehand-pack-code-manager
   ```

   It runs, in order, `go vet`, `go test -race`, govulncheck, the golden replay, the pack contract check, the
   smoke test, the unit verification, the package smoke test, the promtool check, `goreleaser check`, actionlint,
   shellcheck and a clean-tree check, prints `GATE <name>: ok` after each and ends with `RELEASE GATE OK`. It needs
   docker, goreleaser, actionlint, shellcheck and network access, and a checkout of the Stagehand Code Manager pack
   at the commit the script names (it never modifies it).
   govulncheck reports the standard library of the Go toolchain on your machine. If your local Go is older than
   the newest patch, it can list standard-library findings; the gate records them and passes only when every
   finding is in the standard library, and the release build in CI uses the newest patch of the 1.26 line. Set
   `GATE_STRICT_VULN=1` to make any finding fail.
3. Check CI is green for that commit: `gh run list --repo puppet-stagehand/stagehand-deploy-agent --branch main --limit 1`.
4. Check the names that become a contract: the metric names (`internal/metrics/metrics.go`) and the seven
   `/api/v1/info` fields (`internal/server/info.go`) are frozen at v1.0.0. Renaming one is a 2.0.0 change.

## Cut a release

Only from `main`; the workflow refuses a tag that is not on `main`.

```sh
git tag -a vX.Y.Z -m "stagehand-deploy-agent vX.Y.Z"
git push origin vX.Y.Z
gh run watch --repo puppet-stagehand/stagehand-deploy-agent
```

The `release` workflow runs these steps in this order, and any failure stops the run:

1. refuse a tag that is not an ancestor of `origin/main`; `go vet` and `go test -race`;
2. GoReleaser builds `stagehand-deploy-agent_<version>_linux_{amd64,arm64}.{deb,rpm,tar.gz}`, writes
   `checksums.txt`, signs it keyless into `checksums.txt.sigstore.json`, writes an SBOM per artifact and creates the
   GitHub release as a **draft**;
3. the workflow verifies the signature with the exact workflow identity, checks every sha256 and attests build
   provenance;
4. it builds the image from the verified archives, pushes it, signs the digest and verifies that signature;
5. only then it publishes the draft.

Nothing is public before it has been verified. A failed run leaves the draft, which only people with write access
can see. To retry after a failure, delete the draft and the tag (`gh release delete vX.Y.Z --cleanup-tag`), fix the
cause and tag again; if anything was published (the image), bump the patch number instead.

## Verify a release (operators and maintainers)

```sh
TAG=vX.Y.Z
gh release download "$TAG" --repo puppet-stagehand/stagehand-deploy-agent \
  --pattern 'checksums.txt*' --pattern '*.deb' --pattern '*.rpm'
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity "https://github.com/puppet-stagehand/stagehand-deploy-agent/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  checksums.txt
sha256sum -c --ignore-missing checksums.txt
gh attestation verify stagehand-deploy-agent_*_linux_amd64.deb --repo puppet-stagehand/stagehand-deploy-agent
cosign verify ghcr.io/puppet-stagehand/stagehand-deploy-agent:${TAG#v} \
  --certificate-identity "https://github.com/puppet-stagehand/stagehand-deploy-agent/.github/workflows/release.yml@refs/tags/${TAG}" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

`cosign verify-blob` needs cosign 2.4 or later. Always use the exact `--certificate-identity`, never a pattern over
the whole repository: a pattern such as `^https://github.com/puppet-stagehand/stagehand-deploy-agent/` would also
accept a signature from any other workflow or branch. The release workflow's own verify step uses this exact
identity (built from `GITHUB_REPOSITORY` and `GITHUB_REF`), so keep these commands and the release notes in step
with it.

Only `release.yml` may have `id-token: write`. A second workflow with it could mint a signature for its own
identity. Review any workflow change that adds it.

After a release is verified, update the pinned URL and sha256 in the `stagehand` module's guide
(`docs/stagehand-webhook.md`) and in any Hiera that pins the old release.

## Package names and versions

The packages are named `stagehand-deploy-agent` and declare `Replaces` and `Conflicts` with `webhook-go`, so
installing one removes the other (on rpm, `rpm -U` replaces the fork because the package obsoletes it, while a plain
`rpm -i` refuses and names the conflict). nfpm turns a pre-release tag into a tilde:
`v1.1.0-rc.1` becomes package version `1.1.0~rc.1`, which sorts below `1.1.0`.

## Deprecated GoReleaser `dockers`

`.goreleaser.yml` has no `dockers` section on purpose. GoReleaser would push the image in the same run, before any
check, and `dockers` is deprecated in favour of `dockers_v2`. The workflow builds the image from the verified
archives instead. If you move to `dockers_v2`, keep the image push after the verify steps and before the publish.

## Scheduled change: the legacy config fallback leaves in 1.2.0

The agent reads `/etc/voxpupuli/webhook.yml` only when its own file is absent, with a WARNING. That fallback is
removed in **1.2.0** (`LegacyFallbackRemovedIn` in `internal/config/legacy.go`). When you start 1.2.0:
- delete `internal/config/legacy.go`, its tests and the `--legacy` flag;
- remove the fallback paragraphs from `docs/USER-GUIDE.md`;
- say so in the changelog under Removed.

## Go version and vulnerabilities

`go.mod` says `go 1.25.0` (the oldest language version the code needs); CI and the release workflow install the
newest 1.26 patch (`check-latest: true`) so the binary carries the standard-library security fixes. They moved from
the 1.25 line on 2026-10-22 because its newest patch (1.25.14) still had eight standard-library findings that are fixed
only in Go 1.26.9 and 1.26.10. When Go 1.28 is released and 1.26 leaves support, move `go-version` in `ci.yml` and
`release.yml` to the next supported line. `govulncheck` (pinned in `ci.yml`, and in
`hack/release-gate.sh`, to the same version) runs on every push. A finding in the standard library means: rebuild
with the newest Go patch and re-release if the issue is reachable.

## Repository settings (recommended)

These are applied by an org admin, not by the workflows:
- `main` is the default branch, with branch protection (pull request plus green `ci`, no force-push);
- a tag ruleset that limits who may create `v*` tags;
- the ghcr package is public and linked to the repository;
- Actions are pinned by commit SHA; Dependabot or a monthly manual check keeps the pins current.

The release workflow's ancestry check proves a tag is on `main`, not that the commit was reviewed. The branch
protection and tag ruleset close that gap.

## The fork

The Stagehand fork of webhook-go is archived after the agent's first live deploy through the pack (its README then
points here). Until then it stays as the rollback source. Archiving is an outward-facing action for a person.
