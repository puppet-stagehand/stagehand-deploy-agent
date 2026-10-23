# Stagehand deploy agent: Tester guide

Manual checks for a lab Puppet Core primary running the agent. Run them after installing a new release or changing
the config. Each step lists the expected result; anything else is a defect, so record it as an issue on
https://github.com/puppet-stagehand/stagehand-deploy-agent/issues.

The agent's automated suite (`go test -race ./...`, plus `hack/release-gate.sh`) already covers these behaviours
against fakes, a fake engine and the Stagehand Code Manager pack's real client. These steps prove them on a real
primary, with real r10k, git, ssh, systemd and the Puppet CA. Nothing here has been run for you: until a person
runs a step on a lab primary, it counts as not verified, and the release notes must not claim it.

## Before you start

You need:
- a lab Puppet Core primary (Enterprise Linux 9 or Ubuntu or Debian, with systemd 247 or newer) with the agent
  installed. The `stagehand::webhook` class does it; see the `stagehand` module's `docs/stagehand-webhook.md`;
- a control repository with a `production` branch and at least one other branch (called `feature_x` below);
- a client certificate signed by the lab CA for an allowed certname (`stagehand-code-manager` below). Create it on
  the primary with `puppetserver ca generate --certnames stagehand-code-manager`.

Set these in a shell on the machine you test from:

```sh
URL=https://<primary fqdn>:8088
OPS=http://127.0.0.1:8089          # run the ops checks on the primary itself
CA=/etc/puppetlabs/puppet/ssl/certs/ca.pem
CERT=/etc/puppetlabs/puppet/ssl/certs/stagehand-code-manager.pem
KEY=/etc/puppetlabs/puppet/ssl/private_keys/stagehand-code-manager.pem
SHA=$(git ls-remote <control repo url> refs/heads/production | cut -f1)
```

Check the agent is up: `systemctl is-active stagehand-deploy-agent` prints `active`, and
`stagehand-deploy-agent version` prints `stagehand-deploy-agent 1.0.0` (or the release under test).

## 1. A client without a certificate is refused

```sh
curl -sS --cacert $CA $URL/api/v1/info
```

**Expected:** curl fails with a TLS error (for example `alert certificate required` or `handshake failure`). No HTTP
status is printed.

## 2. A certificate whose certname is not on the list is refused

Generate a second certificate (`puppetserver ca generate --certnames not-allowed`) and use it:

```sh
curl -sS --cacert $CA --cert <not-allowed cert> --key <not-allowed key> $URL/api/v1/info
```

**Expected:** a TLS handshake error, no HTTP status. With `$CERT`/`$KEY` instead, the call answers 200 with the seven
info fields (`version`, `engine`, `engine_version`, `environments_dir`, `cert_not_after`, `crl_next_update`,
`config_source`). On the ops port, `curl -s $OPS/metrics | grep tls_refusals_total` shows the `certname` count went
up by one.

## 3. A revoked certificate is refused after the CRL refresh

1. Confirm `$CERT` works (step 2's last line).
2. Revoke it: `puppetserver ca revoke --certname stagehand-code-manager`.
3. Wait 65 seconds. The agent re-reads the CRL at most once a minute.
4. Repeat the `/api/v1/info` call with `$CERT`.

**Expected:** a TLS handshake error. This assumes `server.tls.crl` is the CA's
`/etc/puppetlabs/puppetserver/ca/ca_crl.pem`, the default on the CA host. With the agent copy, run `puppet agent -t`
first.

Afterwards, clean up and issue a new certificate:
- `puppetserver ca clean --certname stagehand-code-manager`;
- then generate the certificate again.

If `allowed_fingerprints` is set, update it to the new certificate's fingerprint.

## 4. A deploy of a real environment returns the commit the branch points at

```sh
curl -sS --cacert $CA --cert $CERT --key $KEY -X POST $URL/api/v1/deploys \
  -H 'Content-Type: application/json' \
  -d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":true}"
```

**Expected:** HTTP 200, and the body shows:
- `"state":"deployed"`;
- `results[0].deployed_sha` equal to `$SHA`;
- `marker_present: true`;
- `marker_deploy_success: true`;
- `error: ""`.

On the primary, `/etc/puppetlabs/code/environments/production/.r10k-deploy.json` has `signature` equal to `$SHA`,
and `ls -ln /etc/puppetlabs/code/environments/production` shows the files owned by the `stagehand-deploy-agent`
user's uid with group `puppet`, directories mode 0750 and files mode 0640. Puppet Server can still compile a catalog
for the environment.

## 5. A wrong expected SHA returns failed

Repeat step 4 with `expected_sha` set to a different 40-hex commit (for example the branch's previous commit,
`git rev-parse production~1` in a clone).

**Expected:** HTTP 200 (the deploy ran) with `"state":"failed"` and `results[0].error` reading
`deployed <the branch's real SHA>, expected <the SHA you sent>`.

## 6. Two deploys at once run one after the other

Start two deploys within a second of each other with `"wait":false`:

```sh
for i in 1 2; do
  curl -sS --cacert $CA --cert $CERT --key $KEY -X POST $URL/api/v1/deploys \
    -H 'Content-Type: application/json' \
    -d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":false}" &
done; wait
```

**Expected:** both answer 202 with an `id`. Poll each with `curl ... $URL/api/v1/deploys/<id>` until both are
`deployed`. The second job's `results[0].started_at` is **at or after** the first job's `results[0].finished_at`;
the runs never overlap.

## 7. A malformed request never reaches the engine

Send `"name":"prod;rm"` or a 39-character SHA.

**Expected:** HTTP 422 with `field` naming the bad field. `GET /api/v1/deploys?limit=1` shows no new job for it. A
request body over 64 KiB answers 413, and a valid object followed by extra text answers 400.

## 8. A restart mid-deploy is reported honestly

1. Start a deploy with `"wait":false` against a large environment.
2. While it reads `deploying`, run `systemctl restart stagehand-deploy-agent`.
3. Poll the job.

**Expected:** `state: failed`. The error reads `engine stopped: the deploy service is shutting down` (a clean stop),
or `interrupted by a restart` (if the process was killed).

## 9. A fresh install through the module

On a primary that never ran the old fork, apply the `stagehand::webhook` class from the `stagehand` module (follow
its guide's Steps 1 to 5).

**Expected:**
- the Puppet run succeeds and a second run changes nothing;
- `getent passwd stagehand-deploy-agent` shows primary group `puppet`, shell `/usr/sbin/nologin`, home
  `/var/lib/stagehand-deploy-agent`;
- `systemctl show -p User -p Group -p UMask stagehand-deploy-agent` shows the agent user, `puppet` and `0027`;
- `ps -o user= -C stagehand-deploy-agent` prints `stagehand-deploy-agent`, never `root`;
- `stagehand-deploy-agent serve` run as root prints "refusing to run as root" and exits 77;
- steps 1 to 4 above pass.

## 10. A cutover from the old webhook-go service

On a lab primary that runs the old fork as root (release v2.15.1-stagehand.1) with some finished deploys in its
history, apply the new class.

**Expected:** one Puppet run stops the old service, removes the old package, installs the agent, copies the history
and starts the agent. The Puppet log shows `imported N; skipped 0 already present, ...`. The environments directory,
r10k's cache and the state directory are owned by `stagehand-deploy-agent:puppet`. The old history is still
readable through `GET /api/v1/deploys`. `/var/lib/webhook-go` and `webhook.yml.pre-agent` are left in place.

Then try the rollback in the module guide's "Rolling back to the old service" and confirm the old service starts
with its old config.

## 11. A cutover is refused while a deploy is running

Repeat step 10 on a primary where a deploy is running (start a large deploy through the old fork, then apply).

**Expected:** the Puppet run stops at the first step with exit code 75 and the line
`a deploy is running on webhook-go; apply again when it finishes (nothing was changed)`. Nothing was stopped,
removed or installed. After the deploy finishes, applying again succeeds. Also check the second guard: with a deploy
running on the agent itself, the ownership step stops with exit code 75 and `a deploy is running on the agent`.

## 12. An SSH remote with a pinned host key, even with a hostile `Host *`

1. Use a control repository reachable only over ssh. Configure the module's SSH remote settings (deploy key,
   `ssh_config`, `known_hosts`).
2. Put a conflicting block in `/etc/ssh/ssh_config` and in `/root/.ssh/config`:
   `Host *` with `StrictHostKeyChecking no` and `UserKnownHostsFile /dev/null`.
3. Run a deploy (step 4).
4. Replace the pinned line in `/etc/stagehand-deploy-agent/known_hosts` with a wrong key, restart, and deploy again.

**Expected:** step 3 succeeds. Step 4 fails with a host key error in `results[0].output`, because the agent passes
`ssh -F /etc/stagehand-deploy-agent/ssh_config`, which skips both files, so the hostile `Host *` does nothing. Root's
`~/.ssh` is not read or changed. The deploy key is not readable by the agent user as a file in `/etc`; it is only
present under `/run/credentials/stagehand-deploy-agent.service/`.

## 13. Removing an environment, and the protected refusal

Create a branch `feature_x`, deploy it (step 4 with `"name":"feature_x"`), then delete the git branch and remove the
environment:

```sh
curl -sS --cacert $CA --cert $CERT --key $KEY -X DELETE $URL/api/v1/environments/feature_x
curl -sS --cacert $CA --cert $CERT --key $KEY $URL/api/v1/removals/<id>
curl -sS --cacert $CA --cert $CERT --key $KEY -X DELETE $URL/api/v1/environments/production
curl -sS --cacert $CA --cert $CERT --key $KEY -X DELETE $URL/api/v1/environments/nosuchenv
```

**Expected:**
- the first call answers 202 with a `Location: /api/v1/removals/<id>` header; the removal record ends
  `"state":"deployed"` with `"kind":"remove"`, and `/etc/puppetlabs/code/environments/feature_x` is gone;
- the third call answers 403 naming `production` as protected, and `production` is still there;
- the fourth answers 404;
- `GET /api/v1/deploys?limit=100` does not list the removal;
- `curl -s $OPS/metrics | grep requests_refused_total` shows `reason="protected"` went up by one.

Also deploy `feature_x` again while its git branch still exists, and confirm it comes back: this is why the guide
says to delete the branch first.

## 14. Readiness fails for the right reason and recovers

On the primary, one at a time, and `curl -s -w ' %{http_code}\n' $OPS/readyz` after each:

1. **Engine removed:** `chmod -x` the engine (or move it aside). **Expected:** `503` and `"failing":["engine"]`. Put it
   back: `200`.
2. **Environments directory read-only:** remove the directory from the unit's `ReadWritePaths=` (a drop-in that
   clears and resets the list without it), run `systemctl daemon-reload` and restart. **Expected:** `503` and
   `"failing":["environments_dir"]`. Undo and restart: `200`.
3. **Expired CRL:** point `server.tls.crl` at a CRL whose next update has passed. Make one with `openssl ca -gencrl`
   and a very short `-crldays`, or wait out a short one. Evidence for the record:
   `openssl crl -in <file> -noout -nextupdate`. **Expected:** `503` and `"failing":["crl"]`; the
   `crl_seconds_until_expiry` gauge is negative. Replace the file with a current CRL: within a minute `200`.
4. **Draining:** run `systemctl stop stagehand-deploy-agent` while a long deploy is in flight and poll `$OPS/readyz`
   quickly. **Expected:** `"failing":["draining"]` appears before the process exits.

In every case the body names the check only: no path, no error text.

## 15. `puppet generate types` works as the agent user

After a deploy with `engine.generate_types: true` (the default), look at the environment's
`.resource_types` directory and the deploy output.

**Expected:** `results[0].output` shows no permission error from `puppet generate types`, and
`/etc/puppetlabs/code/environments/production/.resource_types` exists, owned by the agent user. If it fails, the
agent user cannot write Puppet's cache or the environment, and the engine's `generate_types` step is silently
incomplete; record the exact error.

## 16. SELinux on Enterprise Linux 9

On an Enterprise Linux 9 primary with SELinux enforcing (`getenforce` prints `Enforcing`):

**Expected:** steps 4 and 13 pass; `ausearch -m avc -ts recent` shows no denial for `stagehand-deploy-agent`,
`r10k`, `git` or `ssh`; the agent can bind its two ports; and `/readyz` answers 200. If a denial appears, record
the full `ausearch` line; do not switch SELinux to permissive to make the check pass.

## 17. The unit's system call filter against real r10k, git and ssh

The unit sets `SystemCallFilter=@system-service` with `SystemCallErrorNumber=EPERM`, tested against fakes only.

**Expected:** a deploy of an environment with a `Puppetfile` that lists modules from a Git remote and from the
Forge (HTTPS) and a second over ssh succeeds, and `journalctl -u stagehand-deploy-agent` plus `dmesg` show no
`Operation not permitted` from git, ssh or Ruby. If a call is blocked, record which tool failed and its message;
the fix is a drop-in widening the filter or adding `AF_NETLINK`, decided by the maintainer.

## 18. Puppet Server ignores the removal leftovers

While a removal is in progress (or after killing the agent with `kill -9` between the rename and the delete, which
leaves `/etc/puppetlabs/code/environments/.removing-<env>-<id>`), ask Puppet Server for the environment list:

```sh
curl -s --cacert $CA https://localhost:8140/puppet/v3/environments --cert <a puppet cert> --key <its key>
```

**Expected:** the list does not contain a name starting with `.removing-`. After restarting the agent, the leftover
directory is gone.

## Recording results

Note the release tag, the primary's OS and the r10k version, plus pass or fail for each step. Record each result in
the console repository's `.planning/HUMAN-TESTS-DEFERRED.md` under Phase 57.6.2: a passed step with its date and
evidence, a step you could not run with the reason. Do not mark a step done on the strength of the automated suite.
