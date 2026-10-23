#!/bin/sh
# Runs as root before the files are unpacked (deb preinst, rpm %pre), on a new
# install and on an upgrade, so it must be safe to run twice.
#
# Creates the system user the unit runs as. Its primary group is puppet, a group
# the puppetserver package owns, so this package cannot create it: if it is
# missing the install stops here with the reason. The service is never started or
# enabled by a package script; the Puppet module (or the operator) does that after
# writing the configuration.
set -eu

if ! getent group puppet >/dev/null 2>&1; then
	echo "stagehand-deploy-agent needs the puppet group (install puppetserver first)" >&2
	exit 1
fi

if ! getent passwd stagehand-deploy-agent >/dev/null 2>&1; then
	useradd --system --gid puppet \
		--home-dir /var/lib/stagehand-deploy-agent --no-create-home \
		--shell /usr/sbin/nologin stagehand-deploy-agent
fi
