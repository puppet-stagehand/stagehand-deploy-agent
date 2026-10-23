#!/bin/sh
# Runs as root before the files are removed (deb prerm, rpm %preun). The first
# argument says why: deb passes "remove" (also "upgrade", "deconfigure",
# "failed-upgrade"), rpm passes 0 when the last copy is erased and 1 or more on an
# upgrade. Stop and disable the service only when the package is really going
# away, never on an upgrade, so an upgrade does not interrupt a running deploy.
set -eu

case "${1:-}" in
remove | 0)
	systemctl disable --now stagehand-deploy-agent || true
	;;
*) ;;
esac
