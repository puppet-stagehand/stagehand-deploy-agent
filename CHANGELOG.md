# Changelog

All notable changes to the Stagehand deploy agent. The project follows semantic versioning: a breaking API change
needs 2.0.0. The metric names and the seven `/api/v1/info` field names are part of the API from 1.0.0.

## [1.0.0] - 2026-10-22

The first release. It replaces the Stagehand fork of webhook-go (`puppet-stagehand/webhook-go`, last release
v2.15.1-stagehand.1) on Stagehand Core estates' Puppet Core primaries.

### Added

- A dedicated unprivileged user. The agent owns the environments directory and r10k's cache only; the packaged
  systemd unit sets `User=stagehand-deploy-agent`, `Group=puppet`, `UMask=0027`, `NoNewPrivileges`, an empty
  capability set, `ProtectSystem=strict` and a system call filter. The key arrives through systemd
  `LoadCredential=`. `serve` refuses to run as root (exit 77) with no override.
- `DELETE /api/v1/environments/{env}` and `GET /api/v1/removals/{id}`: environment removal as a job under the deploy
  lock, renamed aside and then deleted, with `deploys.protected_environments` (default `production`) refused with 403.
- `GET /api/v1/info` on the mTLS port: `version`, `engine`, `engine_version`, `environments_dir`, `cert_not_after`,
  `crl_next_update`, `config_source`.
- A plain-HTTP ops listener (default `127.0.0.1:8089`) with `/healthz`, `/readyz` (checks `engine`,
  `environments_dir`, `certificate`, `crl`, `draining`) and `/metrics`. Prometheus metrics with bounded labels,
  including `stagehand_deploy_agent_server_certificate_seconds_until_expiry` and
  `stagehand_deploy_agent_crl_seconds_until_expiry` (seconds, the Prometheus base unit).
- Structured JSON logs (`log/slog`), one line per request with route, status, duration and certname.
- Strict YAML configuration (`go.yaml.in/yaml/v3`, unknown keys refused) and `--check-config`. Exit codes 0, 1, 2,
  77 and 78.
- `import-history`: copies the fork's finished deploy records into the agent's store once.
- `engine.ssh_config`: git is run with `ssh -F <file>`, so a `Host *` block elsewhere cannot override pinned host
  keys.
- deb and rpm packages that replace the fork's package, a minimal image (binary only, not a deploy runtime) and a
  keyless cosign signature, SBOM and build provenance on every release.

### Changed (from webhook-go)

- The configuration file is `/etc/stagehand-deploy-agent/config.yml` with the keys in `packaging/config.example.yml`.
  `/etc/voxpupuli/webhook.yml` is read only when the new file is absent, with a warning, and the fallback is
  **removed in 1.2.0**. Known keys are mapped; chat integration, basic authentication (`server.protected`) and the
  git-host routes are removed, and an enabled one stops the agent starting and names the key.
- The fork's `deploys.basedir` is `deploys.environments_dir` in the agent's file, matching the `environments_dir` field
  of `GET /api/v1/info`. The agent's own file refuses `deploys.basedir` as an unknown key and names the new key;
  only the legacy fallback still reads the fork's spelling.
- The server uses `net/http` (Go 1.22 method patterns) instead of gin, and the deploy state and ports live under the
  agent's names (`/var/lib/stagehand-deploy-agent`, port 8088 kept).
- Two recorded divergences from the fork's `/api/v1/deploys` answers (the pack is unaffected; see
  `contract/divergences.go`):
  - a request body over 64 KiB answers `413` instead of `400`;
  - data after the JSON object answers `400` instead of being ignored.

  Unknown paths and wrong methods are answered by `net/http`'s own `404`/`405`, and `/api/v1/deploys/` with a trailing
  slash answers `404` instead of a redirect. All other recorded answers are byte-identical.
- The release workflow builds a draft, verifies it and only then publishes (FORK-WR-06).

### Fixed (items from the 57.6 review of the fork)

- FORK-WR-01: shutdown waits for `wait:true` deploys to write their record.
- FORK-WR-02: the `WaitGroup.Add`/`Wait` race in `Runner.Start`.
- FORK-WR-03: the store is closed on shutdown, so no stale instance lock is left.
- FORK-WR-04: the shipped example configuration is valid and checked by `--check-config`.
- FORK-WR-05: no wall-clock assertion in the g10k test.
- FORK-WR-06: nothing is public before the signature, checksums, provenance and image signature verify.
- FORK-IN-01: a numeric `deploys.timeout` is refused (write `15m`) instead of being read as nanoseconds.
- FORK-IN-02: orphaned temp files and corrupt records in `jobs/` are cleaned up or quarantined in `jobs/.corrupt/`.
- FORK-IN-03: a `503` refusal no longer counts toward `max_jobs`.
- FORK-IN-04: a deploy is never reported `deployed` from a marker the engine did not rewrite, even within the same
  second.
- FORK-IN-05: CI runs `-race` and pins every action by commit SHA.
- FORK-IN-06: an oversized body answers `413` and trailing data is rejected.

### Security

- Retires the root-run deploy service of the fork (T-57.6-20 / AR-57.6-08) and restores the ssh host-key pinning
  control (T-57.6-22).
- A release can be verified with `cosign verify-blob` against the exact workflow identity, `sha256sum -c` and
  `gh attestation verify`; see `docs/MAINTAINING.md`.
