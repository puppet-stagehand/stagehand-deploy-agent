#!/bin/sh
# Check the packaged systemd unit with systemd itself, in a throwaway debian:12
# container (systemd 252): `systemd-analyze verify` must print nothing, and
# `systemd-analyze security --offline=true` must score the unit at 2.5 or lower
# (lower is better; 1.9 was measured when the unit was drafted). Needs docker and
# network access (apt installs systemd in the container). Prints "UNIT VERIFY OK"
# as its last line, or "UNIT VERIFY FAIL: <reason>" and exits 1.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
MAX_SCORE=2.5
IMAGE=${UNIT_VERIFY_IMAGE:-debian:12}

if ! command -v docker >/dev/null 2>&1; then
	echo "UNIT VERIFY FAIL: docker is not installed"
	exit 1
fi

# The inner script runs as root in the container. The unit names a user and a
# group, so create them (systemd-analyze verify looks them up); the executable
# is a stub because verify checks that ExecStart= exists and is executable.
# shellcheck disable=SC2016  # single quotes on purpose: the container's shell expands these
INNER='
set -eu
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null
apt-get install -y -qq --no-install-recommends systemd >/dev/null 2>&1
groupadd --system puppet
useradd --system --gid puppet --no-create-home --shell /usr/sbin/nologin stagehand-deploy-agent
printf "#!/bin/sh\nexit 0\n" > /usr/bin/stagehand-deploy-agent
chmod 0755 /usr/bin/stagehand-deploy-agent
mkdir -p /etc/stagehand-deploy-agent /etc/puppetlabs/code/environments /opt/puppetlabs/puppet/cache/r10k
cp /unit/stagehand-deploy-agent.service /usr/lib/systemd/system/stagehand-deploy-agent.service
echo "systemd: $(systemctl --version | head -1)"
echo "--- systemd-analyze verify ---"
out=$(systemd-analyze verify /usr/lib/systemd/system/stagehand-deploy-agent.service 2>&1) || rc=$?
printf "%s\n" "$out"
if [ -n "$out" ] || [ "${rc:-0}" -ne 0 ]; then
	echo "VERIFY-NOT-CLEAN"
	exit 1
fi
echo "(clean)"
echo "--- systemd-analyze security --offline=true ---"
systemd-analyze security --offline=true --no-pager /usr/lib/systemd/system/stagehand-deploy-agent.service
'

OUT=$(docker run --rm --platform linux/amd64 -v "$ROOT/packaging:/unit:ro" "$IMAGE" sh -c "$INNER" 2>&1) || {
	printf '%s\n' "$OUT"
	echo "UNIT VERIFY FAIL: systemd-analyze verify was not clean, or the container could not run it"
	exit 1
}
printf '%s\n' "$OUT"

LINE=$(printf '%s\n' "$OUT" | grep 'Overall exposure level' | tail -1)
if [ -z "$LINE" ]; then
	echo "UNIT VERIFY FAIL: systemd-analyze security printed no overall exposure line"
	exit 1
fi
SCORE=$(printf '%s\n' "$LINE" | sed -n 's/.*: \([0-9][0-9]*\.[0-9][0-9]*\) .*/\1/p')
if [ -z "$SCORE" ]; then
	echo "UNIT VERIFY FAIL: could not read a score from: $LINE"
	exit 1
fi
echo "exposure score: $SCORE (limit $MAX_SCORE)"
if ! awk -v s="$SCORE" -v m="$MAX_SCORE" 'BEGIN { exit (s + 0 <= m + 0) ? 0 : 1 }'; then
	echo "UNIT VERIFY FAIL: exposure $SCORE is above $MAX_SCORE"
	exit 1
fi
echo "UNIT VERIFY OK"
