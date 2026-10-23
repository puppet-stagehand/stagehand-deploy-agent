#!/bin/sh
# Runs as root after the files are removed (deb postrm, rpm %postun). The user
# stagehand-deploy-agent is never deleted: it owns files under the environments
# directory and the state directory, and removing it would orphan them.
set -eu

systemctl daemon-reload || true
