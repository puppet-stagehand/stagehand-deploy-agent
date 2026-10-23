# Stagehand deploy agent

The Stagehand deploy agent is a small service that runs on a Puppet Core primary. The Stagehand console and the
Code Manager pack call it over mutual TLS to deploy Puppet code: it runs r10k (or g10k) for a branch, reads the
deploy marker the tool leaves behind, and reports honestly whether the environment now holds the commit that was
asked for. It can also remove an environment whose git branch is gone, and it exposes a health check, a readiness
check and Prometheus metrics on a separate local port.

It runs as its own unprivileged user, never as root.

## It replaces webhook-go

The agent replaces `puppet-stagehand/webhook-go`, the Stagehand fork of voxpupuli/webhook-go, on Stagehand Core
estates. The `/api/v1/deploys` API is the fork's, byte for byte apart from two recorded differences (see the
changelog), so the published Code Manager pack works unchanged. Chat integration, basic authentication and the
git-host webhook routes are gone: only the console and the pack call the agent.

The fork will be archived, with its README pointing here, after the first live deploy through the pack. Until then
it stays available as the rollback source. For one release the agent reads the fork's `/etc/voxpupuli/webhook.yml`
when it has no config of its own; that fallback is removed in 1.2.0.

## Documentation

| Read this | If you want to |
|-----------|----------------|
| [docs/USER-GUIDE.md](docs/USER-GUIDE.md) | understand what the agent does, install it, configure it and call it. Written for someone new to Puppet. |
| [docs/TESTER-GUIDE.md](docs/TESTER-GUIDE.md) | check a release by hand on a lab primary: the tests that need a real Puppet Core primary. |
| [docs/MAINTAINING.md](docs/MAINTAINING.md) | cut a release, verify one, and keep the project healthy. |
| [CHANGELOG.md](CHANGELOG.md) | see what changed. |

The easy way to install it is the `stagehand::webhook` class in the `stagehand` Puppet module, which also moves a
primary off the old fork in one Puppet run.

## Build and test

```sh
go vet ./...
go test -race -count=1 ./...
sh hack/release-gate.sh ../stagehand-pack-code-manager   # every local check, for a release candidate
```

The container image holds the agent binary only. It is for `--check-config`, tests and the contract harness; it is
not a deploy runtime.

## Licence

Apache License 2.0; see [LICENSE](LICENSE). Parts of the code are derived from voxpupuli/webhook-go, also Apache-2.0;
see [NOTICE](NOTICE). Files derived from it carry a header that says so.
