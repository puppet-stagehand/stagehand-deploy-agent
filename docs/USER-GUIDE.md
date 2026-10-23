# Stagehand deploy agent: User guide

This guide is for anyone who runs Puppet and wants the Stagehand console to deploy Puppet code to a Puppet Core
primary. You do not need to know r10k, mTLS or Go to follow it. Words in **bold** the first time they appear are
explained in the glossary at the end.

## The short version

1. Puppet code lives in a git repository, the **control repository**. Each branch is an **environment**.
2. Something has to copy a branch onto the Puppet primary before Puppet can use it. That something is a
   **deploy engine**: **r10k** or **g10k**.
3. The deploy agent runs on the primary and starts the deploy engine when Stagehand asks it to. It then checks the
   engine's work and says, honestly, whether the environment now holds the commit Stagehand asked for.
4. Only Stagehand can ask, because the agent only talks to clients that hold a specific Puppet certificate.
5. The agent does not run as the machine's administrator (root). It has its own small user account that can touch
   the environments directory and r10k's cache, and nothing else.

## What r10k and g10k do

Think of the control repository as a recipe book and the primary as a kitchen. r10k (Ruby) and g10k (Go) are the
two delivery drivers. You tell a driver "bring me the `production` branch". The driver:

1. fetches that branch;
2. fetches every module its `Puppetfile` lists;
3. puts them all in `/etc/puppetlabs/code/environments/production`;
4. writes a small receipt file in that directory: `.r10k-deploy.json` (r10k) or `.g10k-deploy.json` (g10k). The
   receipt, the **deploy marker**, says which commit was delivered and whether it worked.

## What the agent does

The agent is a small program that waits for a request, runs the delivery driver, and then reads the receipt. It
does not trust the driver's exit code on its own: exit codes can lie (g10k exits 0 even when the branch does not
exist).

- **Who may call it.** Only a client with a certificate from your Puppet CA, whose name is on an allow-list. This
  is **mTLS**: both sides show a certificate. A revoked certificate is refused. You can also pin the exact
  certificate by its fingerprint.
- **What "deployed" means.** An environment is `deployed` only if all four hold:
  1. the engine exited 0;
  2. this run rewrote the deploy marker (an old receipt does not count);
  3. the marker says the deploy succeeded;
  4. the marker's commit equals the `expected_sha` the caller sent.

  Anything else is `failed`, with the reason in plain words.
- **A record of every deploy.** Each request becomes a **job** with an id. Its record is a file on disk that
  survives restarts, and Stagehand reads it back.
- **One at a time.** Jobs take a lock, so two deploys (or a deploy and a removal) never touch the environments
  directory together.
- **Removal.** Stagehand can ask the agent to delete an environment whose git branch is gone. See "Removing an
  environment".

The agent replaces the earlier Stagehand fork of webhook-go. The deploy API is the same, byte for byte, apart from
two recorded differences listed under "The API".

## Why it does not run as root

r10k and g10k write into `/etc/puppetlabs/code/environments` and into r10k's cache. Running that as root means a
bug in the agent could change anything on the primary. So the agent runs as its own account:

- **User and group.** The package creates the user `stagehand-deploy-agent` with primary group `puppet` (the group
  Puppet Server uses). It has no shell and no login. The package refuses to install if the `puppet` group does not
  exist, so install Puppet Server first.
- **Umask 0027.** New environment directories are `0750` and new files `0640`. Puppet Server reads them through the
  `puppet` group, and nobody else can.
- **What it owns.** The environments directory (`/etc/puppetlabs/code/environments`), r10k's cache
  (`/opt/puppetlabs/puppet/cache/r10k`) and its own state directory (`/var/lib/stagehand-deploy-agent`). The
  directory `/etc/puppetlabs/code` itself, `hiera.yaml` and `modules/` stay owned by root.
- **How it reads the host key.** Puppet's private key directory is not opened up. The systemd unit uses
  `LoadCredential=` to hand the agent a private copy of the key when the service starts, at
  `/run/credentials/stagehand-deploy-agent.service/server.key`. A git deploy key arrives the same way.
- **It refuses root.** `stagehand-deploy-agent serve` run as uid 0 stops at once with exit code 77. There is no flag,
  setting or environment variable that overrides this.
- **A locked-down unit.** The packaged systemd unit also turns on `NoNewPrivileges`, an empty capability set,
  `ProtectSystem=strict` (only the two directories above are writable), `ProtectHome`, `PrivateTmp` and a system
  call filter.

## How to install it

The easy way is the Puppet class `stagehand::webhook` from the `stagehand` module. It installs git and r10k,
installs the agent package from a pinned URL and sha256, moves a primary that still runs the old fork over to the
agent, writes the settings, adds a systemd drop-in and starts the service. The class keeps its old name so earlier
documents stay valid. Its guide is `docs/stagehand-webhook.md` in that module; it gives the exact release URLs and
sha256 values.

By hand:

1. Download the `.deb` or `.rpm` for your CPU, plus `checksums.txt` and `checksums.txt.sigstore.json`, from the
   release page, https://github.com/puppet-stagehand/stagehand-deploy-agent/releases.
2. Check the signature once (it needs `cosign` 2.4 or later; 3.x recommended):

   ```sh
   TAG=v1.0.0
   cosign verify-blob --bundle checksums.txt.sigstore.json \
     --certificate-identity "https://github.com/puppet-stagehand/stagehand-deploy-agent/.github/workflows/release.yml@refs/tags/${TAG}" \
     --certificate-oidc-issuer https://token.actions.githubusercontent.com \
     checksums.txt
   sha256sum -c --ignore-missing checksums.txt
   ```

   `Verified OK` and one `OK` line per downloaded file mean the files are exactly what the agent's release
   workflow built for that tag. Anything else: stop, and do not install. Always use the exact certificate
   identity above, never a pattern.
3. Install: `dpkg -i stagehand-deploy-agent_1.0.0_linux_amd64.deb` or
   `rpm -Uvh stagehand-deploy-agent_1.0.0_linux_amd64.rpm`. If the old fork is installed, the new package removes
   it (deb: `Replaces`/`Conflicts`; rpm: an upgrade with `rpm -Uvh` removes the fork because the package obsoletes
   it, while a plain `rpm -i` refuses and names the conflict, so run `rpm -e webhook-go` first). Stop the old
   service when no deploy is running; the module does all of this for you in a safe order.
4. Write `/etc/stagehand-deploy-agent/config.yml`, starting from the example the package installs at
   `/usr/share/doc/stagehand-deploy-agent/config.example.yml`. Replace `<certname>` with the host's Puppet
   certname.
5. Check it without starting anything: `stagehand-deploy-agent --check-config --config /etc/stagehand-deploy-agent/config.yml`.
   It prints one line per problem and exits 78 if any is an ERROR. Run it as root or as the agent user; a run by
   hand cannot see the credential path, so it only warns about the key.
6. Add a systemd drop-in with `LoadCredential=server.key:/etc/puppetlabs/puppet/ssl/private_keys/<certname>.pem`
   (and, when Git uses ssh, the deploy key as a second credential), then
   `systemctl daemon-reload && systemctl enable --now stagehand-deploy-agent`.

The package installs `/usr/bin/stagehand-deploy-agent`, the unit at `/usr/lib/systemd/system`, and the example
config, an example `ssh_config` and the licence under `/usr/share/doc/stagehand-deploy-agent/`. It writes nothing
under `/etc`, never starts or enables the service, and never deletes the user. `stagehand-deploy-agent version`
prints the version.

**Linux only, systemd 247 or newer.** `LoadCredential=` needs systemd 247, so Enterprise Linux 8 and Ubuntu 20.04
are not supported. Releases are built for Linux amd64 and arm64.

## Settings

All settings live in `/etc/stagehand-deploy-agent/config.yml`. The file is strict: an unknown key, a second YAML
document, a duration without a unit (write `15m`, never `900`) or a relative path stops the agent starting, and the
message names the key. Run `--check-config` after every edit; to change a setting, edit the file and restart (the
agent reads it once, at start).

| Setting | Default | What it does |
|---------|---------|--------------|
| `server.address` | `""` (all interfaces) | The interface the mTLS listener binds. |
| `server.port` | `8088` | The mTLS port. 8088 is the Stagehand Code Manager pack's default, so nothing else changes. |
| `server.tls.certificate` | required | This server's own certificate; use the host's Puppet certificate. |
| `server.tls.key` | required | The key for that certificate. The packaged unit uses `/run/credentials/stagehand-deploy-agent.service/server.key`. |
| `server.tls.client_ca` | required | The Puppet CA bundle, usually `/etc/puppetlabs/puppet/ssl/certs/ca.pem`. The agent serves mTLS only; there is no plain mode. |
| `server.tls.crl` | none (no check) | The **certificate revocation list**. See "How revocation works". |
| `server.tls.allowed_certnames` | required | The certificate names allowed to call the API, for example `stagehand-code-manager`. At least one. |
| `server.tls.allowed_fingerprints` | `[]` (no pin) | When set, the client certificate's SHA-256 fingerprint must also be listed, as 32 colon-separated hex pairs, as `puppetserver ca list --all` prints them. |
| `ops.listen` | `127.0.0.1:8089` | Where the monitoring port binds, as `host:port`. See "The ops port". |
| `engine.name` | `r10k` | `r10k` or `g10k`. |
| `engine.command_path` | `/opt/puppetlabs/puppet/bin/r10k` | The engine binary, absolute. With g10k, the g10k binary. |
| `engine.config_path` | `/etc/puppetlabs/r10k/r10k.yaml` | The engine's own configuration file. |
| `engine.verbose` | `true` | Print the engine's progress, so a failed deploy shows why. |
| `engine.deploy_modules` | `true` | Also deploy each environment's modules. |
| `engine.generate_types` | `true` | Run `puppet generate types` after a deploy. |
| `engine.env_incremental` | `false` | Deploy only the modules that changed. |
| `engine.use_legacy_puppetfile_flag` | `false` | Use the old `--puppetfile` flag (very old r10k). |
| `engine.ssh_config` | none | An `ssh_config` for git, passed as `ssh -F <path>`. It is how host keys are pinned. Letters, digits and `. _ / -` only. |
| `deploys.basedir` | `/etc/puppetlabs/code/environments` | Where environments and their deploy markers live. Absolute. |
| `deploys.state_dir` | `/var/lib/stagehand-deploy-agent` | Where job records (`jobs/<id>.json`, mode 0600), the deploy lock (`deploy.lock`) and the per-process lock files live. Mode 0700. |
| `deploys.timeout` | `15m` | How long one environment may take. When it runs out, the engine and everything it started are stopped. At least `1s`. |
| `deploys.output_limit` | `262144` | How many bytes of engine output to keep per environment (the end is kept). At least 1024. |
| `deploys.max_jobs` | `500` | How many job records to keep; the oldest finished ones beyond this are deleted. |
| `deploys.protected_environments` | `[production]` | Environments that can never be removed. Lower-case letters, digits and `_`. |
| `log.level` | `info` | `debug`, `info`, `warn` or `error`. |

The agent refuses to start, with a message naming the setting, if a required key is missing, a path is not
absolute, the CRL does not parse or was not signed by a CA in `client_ca`, a fingerprint is malformed or a value is
out of range. Setting `deploys.protected_environments: []` is allowed but logs a warning, because nothing is then
protected from removal.

Exit codes of the `stagehand-deploy-agent` command:

| Code | Meaning |
|------|---------|
| 0 | Success. |
| 1 | A runtime error (for example `serve` could not start, or `import-history` could not read its source). |
| 2 | Wrong command line (usage is printed). |
| 77 | `serve` was run as root and refused. |
| 78 | The configuration has an ERROR (the same answer from `--check-config` and `serve`). |

## Upgrading by hand from webhook-go, and the legacy fallback

If a primary still runs the old fork, the module's single Puppet run does the whole move in a careful order (it
waits out a running deploy, stops and removes the old service, installs the agent, copies the deploy history,
hands the directories to the agent's user and starts it). Read the "Upgrading from the old service" section of the
module's guide before you do it by hand, because the order matters.

For one release the agent can still read the old configuration file so a move does not stop on a missing file:

- It reads `/etc/voxpupuli/webhook.yml` **only when** `/etc/stagehand-deploy-agent/config.yml` does not exist, and
  logs one WARNING naming the file. An explicit `--config PATH` never falls back.
- **What is mapped:** the port, the TLS settings (certificate, key, client CA, allowed certnames, CRL, fingerprints),
  the r10k and g10k settings (`use_g10k_commands: true` selects g10k) and the `deploys.*` settings. The old default
  port 4000 and state directory `/var/lib/webhook-go` are kept when the old file leaves them out, with a warning
  that the pack expects 8088.
- **What is ignored, with one warning:** `server.queue.*`, `server.deploy_on_success_only`, `mappings`, the branch
  naming settings (`r10k.default_branch`, `prefix`, `allow_uppercase`, `blocked_branches`,
  `ignore_branch_prefixes`) and anything else the agent no longer has.
- **What refuses to start**, naming the key: `chatops.enabled: true` (chat integration was removed),
  `server.protected: true` (basic authentication was removed; callers are identified by certificate) and an empty
  `server.tls.client_ca` (the plain webhook mode was removed).
- `stagehand-deploy-agent --check-config --config /path/to/webhook.yml --legacy` reports the same findings for a
  legacy file at another path.
- **The fallback is removed in agent 1.2.0.** Write the new file before then.

On Red Hat family systems, removing the old package (`rpm -e webhook-go`) may rename a modified
`/etc/voxpupuli/webhook.yml` to `/etc/voxpupuli/webhook.yml.rpmsave`. The agent does not read that name, so copy
your settings into the new file first.

To copy the old deploy history across yourself, run once as root (the old directory is root's):
`stagehand-deploy-agent import-history --from /var/lib/webhook-go/jobs`. It only reads the old records and prints
one line, `imported N; skipped N already present, N unfinished, N unreadable`. Running it again changes nothing. If
the old store held more than `deploys.max_jobs` records, a note on stderr says the oldest were not copied; that is
not a failure (exit 0).

## The API

Every call needs the allowed client certificate on the mTLS port (8088). Examples use `curl` with the client's
Puppet certificate; `stagehand-code-manager` is the usual certname.

```sh
CERT=/etc/puppetlabs/puppet/ssl/certs/stagehand-code-manager.pem
KEY=/etc/puppetlabs/puppet/ssl/private_keys/stagehand-code-manager.pem
CA=/etc/puppetlabs/puppet/ssl/certs/ca.pem
URL=https://primary.example.com:8088

curl --cert $CERT --key $KEY --cacert $CA -X POST $URL/api/v1/deploys \
  -H 'Content-Type: application/json' \
  -d '{"environments":[{"name":"production","expected_sha":"0123456789abcdef0123456789abcdef01234567"}],"wait":false}'
```

A revoked, unlisted or unpinned certificate gets no HTTP answer at all: the TLS handshake fails.

| Route | What it does |
|-------|--------------|
| `POST /api/v1/deploys` | Queue a deploy of 1 to 20 environments. |
| `GET /api/v1/deploys?limit=N` | List the newest deploy jobs (N from 1 to 100, default 20). Removals are not listed. |
| `GET /api/v1/deploys/{id}` | One deploy job. |
| `DELETE /api/v1/environments/{env}` | Queue the removal of one environment. |
| `GET /api/v1/removals/{id}` | One removal job. |
| `GET /api/v1/info` | What the agent is: version, engine, certificate dates. |

### Deploying

**Request** (`POST /api/v1/deploys`, at most 64 KiB):

- `environments`: 1 to 20 entries, no duplicates. `name` is lower-case letters, digits and `_`. `expected_sha` is the
  full 40-character lower-case commit the branch should be at. It is required, so there is no "deploy whatever is
  there" mode.
- `wait`: required.
  - `false` answers `202 Accepted` at once with the queued job and a `Location` header; you then poll
    `GET /api/v1/deploys/<id>`.
  - `true` answers `200` when the deploy has finished.

**Result** (`GET /api/v1/deploys/<id>`):

```json
{
  "id": "9f2c4d1e8a7b4c3d2e1f0a9b8c7d6e5f",
  "engine": "r10k",
  "state": "deployed",
  "created_at": "2026-10-13T20:18:00.1Z",
  "started_at": "2026-10-13T20:18:00.1Z",
  "finished_at": "2026-10-13T20:18:04.3Z",
  "results": [{
    "environment": "production",
    "state": "deployed",
    "exit_code": 0,
    "expected_sha": "0123456789abcdef0123456789abcdef01234567",
    "deployed_sha": "0123456789abcdef0123456789abcdef01234567",
    "marker_present": true,
    "marker_deploy_success": true,
    "started_at": "2026-10-13T20:18:00.1Z",
    "finished_at": "2026-10-13T20:18:04.3Z",
    "duration_ms": 4120,
    "output": "INFO\t -> Deploying environment ...",
    "output_truncated": false,
    "error": ""
  }]
}
```

Ids are 32 hex characters; treat them as opaque.

**States**

| State | Meaning |
|-------|---------|
| `queued` | Accepted; waiting for the deploy lock (another job is running). |
| `deploying` | The engine is running. |
| `deployed` | Every environment passed all four checks. |
| `failed` | At least one environment did not. Its `error` says why. |

Decide on `state` only, never on `exit_code`. `exit_code` is `-1` when the engine did not start.

**Errors in a result**

| `error` | What it means | What to do |
|---------|---------------|------------|
| `engine exited N: <stderr tail>` | The engine failed. The tail shows its last lines. | Read the tail: often a missing branch, an unreachable git remote or a bad module ref. |
| `no deploy marker` | No receipt was written. g10k does this, exiting 0, for a branch that does not exist. | Check the branch name. |
| `marker not rewritten by this run` / `the engine did not rewrite the deploy marker` | The receipt is old, so the engine did nothing. | Check the remote is reachable and the branch exists. |
| `marker finished_at unreadable: ...` / `deploy marker unreadable: ...` | The receipt could not be parsed. | Look at the marker file in the environment. |
| `marker reports deploy_success false` | The engine says it failed. | Read `output`. |
| `deployed <x>, expected <y>` | The environment is at a different commit than asked. | Someone pushed in between: fetch the new SHA and deploy again. |
| `engine timed out after 15m0s` | The run took longer than `deploys.timeout` and was stopped. | Look for a hung git remote; raise the timeout if the environment is huge. |
| `engine did not start: ...` | The engine binary is missing or not executable. | Check `engine.command_path`. |
| `interrupted by a restart` | The agent restarted while the job was in flight. | Deploy again. The environment may be half-updated. |
| `not run: the deploy service is shutting down` / `engine stopped: ...` | The agent was stopping. | Deploy again once it is back. |

**HTTP answers**

- `422` names the bad field in the request (`field` and `error` in the body).
- `400` means the JSON is malformed, or there is data after the JSON object.
- `413` means the body is over 64 KiB.
- `404` means an unknown id, or one pruned beyond `max_jobs`.
- `503` with `Retry-After: 30` means the agent is shutting down or 100 jobs are already waiting; that job's record
  reads `failed`.

**Two recorded differences from the old fork.** The fork answered `400` for a body over 64 KiB, which blames the
caller's syntax for a size problem; the agent answers `413`. The fork read the first JSON value and ignored
anything after it; the agent answers `400` for data after the object. The Stagehand Code Manager pack sends
neither case, so it is unaffected. The agent's Go test suite replays the fork's recorded answers and fails on any other
byte difference.

### Removing an environment

`DELETE /api/v1/environments/{env}` queues a removal job and answers `202 Accepted` at once with the job and a
`Location: /api/v1/removals/<id>` header. Poll `GET /api/v1/removals/<id>`; the record has the deploy record's shape
with `"kind":"remove"`, and ends `deployed` (meaning "removed") or `failed` with the reason.

- A removal takes the same lock as a deploy, so it never races one.
- The directory is first renamed to `.removing-<env>-<id>` (Puppet Server does not treat a name starting with a dot
  as an environment), then deleted. If the agent stops in between, it deletes the leftover at its next start.
- `403` if the environment is in `deploys.protected_environments` (default `production`). `404` if there is no such
  environment. `422` for a bad name. `503` as for deploys.
- **Delete the git branch first.** If the branch still exists, the next deploy (or a Stagehand sync) brings the
  environment straight back.

### What `GET /api/v1/info` tells you

Seven fields, in this order. They are a published contract from v1.0.0; renaming one is a 2.0.0 change.

| Field | Meaning |
|-------|---------|
| `version` | The agent version, for example `1.0.0`. |
| `engine` | `r10k` or `g10k`. |
| `engine_version` | The engine's own first version line, read once at start. `null` when the engine did not answer. |
| `environments_dir` | `deploys.basedir`. |
| `cert_not_after` | When the server certificate being served expires (RFC 3339, UTC). |
| `crl_next_update` | When the CRL's next update is due. `null` when no CRL is configured. |
| `config_source` | The configuration file that was loaded: the agent's own path, or `/etc/voxpupuli/webhook.yml` when the fallback was used. |

`cert_not_after` is the certificate loaded when the agent started. After the certificate is renewed on disk, the
file and this field differ until the agent is restarted; that difference shows a restart is due.

## How revocation works

When you revoke a certificate (`puppetserver ca revoke --certname <name>`), the CA adds it to its CRL. The agent
re-reads the CRL file within a minute of the file changing, and from then on refuses that certificate, including on
resumed TLS sessions. If a new CRL file does not parse, the last good list is kept and an error is logged.

Which file to point `server.tls.crl` at:
- **On the CA host:** `/etc/puppetlabs/puppetserver/ca/ca_crl.pem`. A revoke takes effect within a minute.
- **Elsewhere:** `/etc/puppetlabs/puppet/ssl/crl.pem`, the agent's copy. It is only refreshed when the Puppet agent
  runs, so a revoke can take up to one agent run interval (30 minutes by default) to reach the deploy agent.

A CRL has an expiry, its next update. A CRL that has expired makes `/readyz` report `crl` as failing.

## The ops port

The agent opens a second, **plain HTTP** port for monitoring, `ops.listen` (default `127.0.0.1:8089`). It serves
three routes and nothing else; the mTLS port serves none of them, and the ops port serves none of the deploy
routes.

It has no authentication, which is why it binds to the local machine only by default. If you set `ops.listen` to
another address, the agent logs a warning; do that only on a network where a scraper must reach it.

| Route | Answer |
|-------|--------|
| `GET /healthz` | `200 ok` whenever the process is serving. It checks nothing else, so a supervisor can tell "hung" from "serving but not ready". |
| `GET /readyz` | `200 {"ready":true,"failing":[]}` when ready, `503 {"ready":false,"failing":["..."]}` otherwise. |
| `GET /metrics` | Prometheus text. |

`/readyz` fails only on hard failures. It names the failing checks and nothing else (no paths, no error text):

| Check name | It fails when |
|------------|---------------|
| `engine` | The engine binary is missing or not executable. |
| `environments_dir` | The agent user cannot write the environments directory (including a read-only mount). |
| `certificate` | The server certificate file cannot be read or has expired. |
| `crl` | A configured CRL cannot be read or has passed its next update. |
| `draining` | The agent has been told to stop: it is still answering, but a probe should stop sending it work. |

An expiry that is only coming up never makes the agent not ready. Watch the gauges below for that.

### Metrics

All names start with `stagehand_deploy_agent_`. No environment name, job id, certname or path is ever a label, so
the number of series stays small. The names are frozen at v1.0.0.

| Name | Type | Labels | Meaning |
|------|------|--------|---------|
| `build_info` | gauge | `version`, `engine` | Always 1. |
| `ready` | gauge | | 1 when `/readyz` would answer 200. |
| `http_requests_total` | counter | `route`, `code` | Requests answered on the mTLS port. |
| `http_request_duration_seconds` | histogram | `route` | Time to answer. |
| `jobs_total` | counter | `kind` (`deploy`, `remove`), `outcome` (`deployed`, `failed`, `refused`) | Jobs finished or refused. |
| `job_duration_seconds` | histogram | `kind` | Time a job held the lock. |
| `jobs_in_flight` | gauge | | Jobs holding the lock now. |
| `jobs_queued` | gauge | | Jobs waiting for the lock. |
| `requests_refused_total` | counter | `reason` (`busy`, `shutdown`, `protected`, `invalid`) | Requests refused for a reason other than a bad certificate. |
| `tls_refusals_total` | counter | `reason` (`certname`, `revoked`, `fingerprint`) | Client certificates refused in the handshake. |
| `server_certificate_seconds_until_expiry` | gauge | | Seconds until the server certificate expires; negative once it has. |
| `crl_seconds_until_expiry` | gauge | | Seconds until the CRL's next update; negative once it has passed. Absent without a CRL. |

The two expiry gauges are in seconds, the Prometheus base unit. An alert for "less than 14 days left" is
`stagehand_deploy_agent_server_certificate_seconds_until_expiry < 14*86400`. Both gauges read the files on disk at
scrape time.

## Logs

The agent writes one JSON object per line to the journal: `journalctl -u stagehand-deploy-agent`. Every mTLS
request logs one line with `component` `http`, the matched `route`, `status`, `duration_ms` and the caller's
`certname`. A body, a query string or a path value is never logged. Refused client certificates log a WARN with
the certname and the reason. Configuration warnings are logged at start.

## Where it keeps its records

`deploys.state_dir` (default `/var/lib/stagehand-deploy-agent`, mode 0700) holds `jobs/<id>.json` for every job,
the deploy lock and per-process lock files. If the agent finds a record it cannot read, it moves it to
`jobs/.corrupt/` and logs that; it never deletes that folder, so look at what is in it and remove it yourself when
you are done. Leftover `.tmp-*` files are cleaned at start. Removal leftovers named `.removing-*` in the
environments directory are deleted at start.

## Stopping and restarting

Use `systemctl stop|restart stagehand-deploy-agent`. On SIGTERM the agent stops a running engine with every process
it started, fails queued jobs and writes their records before exiting (up to 30 seconds; the unit allows 60). The
unit has no reload command: `systemctl reload` is refused. To change settings, edit the file and restart.

**After a certificate renewal, restart the agent.** It loads the server certificate once, at start. `/readyz` and
the expiry gauge read the file on disk and look healthy, but the agent keeps serving the old certificate until it
expires. Compare `cert_not_after` from `GET /api/v1/info` with the file; a difference means a restart is due.

A deploy that was running when the agent stopped is reported as failed (`interrupted by a restart` or
`... shutting down`). r10k may have been stopped part-way, so deploy that environment again.

## The container image

`ghcr.io/puppet-stagehand/stagehand-deploy-agent:<version>` holds the agent binary and nothing else. It exists for
`--check-config`, tests and the contract harness. It is not a deploy runtime: it has no r10k, git or ssh, so it
cannot deploy Puppet code. Deploy on a primary with the deb or rpm.

## Glossary

| Word | Meaning |
|------|---------|
| control repository | The git repository holding your Puppet code; each branch is an environment. |
| environment | One branch of the control repository, deployed to its own directory on the primary. |
| deploy engine | The tool that copies a branch and its modules onto the primary: r10k or g10k. |
| r10k / g10k | Two deploy engines. r10k is Puppet's Ruby tool; g10k is a faster Go reimplementation. |
| deploy marker | The receipt file the engine writes in an environment: `.r10k-deploy.json` or `.g10k-deploy.json`. |
| mTLS | Mutual TLS: both the client and the server prove who they are with certificates. |
| certname | The name inside a Puppet certificate (its Subject CN), for example `stagehand-code-manager`. |
| allow-list | The list of certnames (`server.tls.allowed_certnames`) that may call the API. Anyone else fails the handshake. |
| CRL | Certificate revocation list: the CA's list of certificates that must no longer be trusted. |
| fingerprint | A SHA-256 hash of one exact certificate, used to pin it. |
| protected environment | An environment the agent will never remove (default `production`). |
| ops port | The plain-HTTP monitoring port (`ops.listen`) serving `/healthz`, `/readyz` and `/metrics`. |
| readiness | Whether the agent could do a deploy right now, as `/readyz` judges it. |
| credential | A secret (the Puppet key, a git deploy key) that systemd hands to the service as a private file at start, so the agent never reads the original. |
| job | One deploy or removal request with an id; its record says how each environment went. |
| cosign | A tool that signs files and checks signatures. Here it proves a release was built by this repository's release workflow. |
| sha256 | A 64-character fingerprint of a file. If even one byte changes, so does the sha256. |
