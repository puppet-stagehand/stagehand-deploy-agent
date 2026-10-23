#!/bin/sh
# Runs as root after the files are unpacked (deb postinst, rpm %post). It only
# tells systemd about the new unit file. It does not start, enable or restart the
# service: the agent has no configuration yet, and the Puppet module starts it
# after it writes the config and sets ownership.
set -eu

systemctl daemon-reload || true
