#!/bin/sh
# Prove the packages, locally, without publishing or signing anything.
#
#  1. A goreleaser snapshot build (tag v1.0.0, no signing, no SBOM, no publish)
#     produces a binary whose `version` prints "stagehand-deploy-agent 1.0.0-snapshot".
#  2. The deb and rpm install the unit at /usr/lib/systemd/system and write nothing
#     under /etc; both declare that they replace/conflict with webhook-go.
#  3. In debian:12, installing the agent .deb over the fork's v2.15.1-stagehand.1
#     .deb removes webhook-go. The package creates the user (primary group puppet,
#     home /var/lib/stagehand-deploy-agent, shell /usr/sbin/nologin) and never
#     enables or starts the service. Removing it stops the service but keeps the
#     user; reinstalling over itself (an upgrade) touches nothing.
#  4. In rockylinux:9, `rpm -i` of the agent while the fork is installed fails and
#     names the conflict; what `rpm -U` does is printed as INFO (RESEARCH A6); after
#     `rpm -e webhook-go` the agent installs with the same user checks.
#  5. The image built from the snapshot binary runs `version`, runs as 65532:65532
#     and says it is not a deploy runtime.
#
# Needs docker, goreleaser, curl and network access (the fork's release assets and
# the container images). The containers are linux/amd64 (the fork's pinned asset).
# Prints "PACKAGE SMOKE OK" as its last line, or "PACKAGE SMOKE FAIL: <step>" and
# exits 1.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
FORK_TAG=v2.15.1-stagehand.1
FORK_BASE=https://github.com/puppet-stagehand/webhook-go/releases/download/$FORK_TAG
FORK_DEB=webhook-go_2.15.1-stagehand.1_linux_amd64.deb
FORK_RPM=webhook-go_2.15.1-stagehand.1_linux_amd64.rpm
FORK_DEB_SHA256=432e1ae4cce129ab87d4a3b63e336a467123d8378d29f2691f2e737922f1b22e
VERSION=1.0.0-snapshot
IMAGE_TAG=stagehand-deploy-agent-smoke:$VERSION
STEP=setup
DONE=0
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stagehand-package-smoke.XXXXXX")

cleanup() {
	rc=$?
	docker rmi -f "$IMAGE_TAG" >/dev/null 2>&1 || true
	rm -rf "$TMP"
	if [ "$DONE" -ne 1 ]; then
		echo "PACKAGE SMOKE FAIL: $STEP"
		exit "${rc:-1}"
	fi
}
trap cleanup EXIT
trap 'exit 1' INT TERM

step() {
	STEP=$1
	echo "== $STEP"
}

need() {
	if ! command -v "$1" >/dev/null 2>&1; then
		STEP="$1 is not installed"
		exit 1
	fi
}

sha256_of() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d' ' -f1
	else
		shasum -a 256 "$1" | cut -d' ' -f1
	fi
}

need docker
need goreleaser
need curl

# --- 1. snapshot build -------------------------------------------------------
step "goreleaser snapshot build"
cd "$ROOT"
# GORELEASER_CURRENT_TAG alone is not enough in a clone with no remote: goreleaser
# cannot read a remote URL, falls back to v0.0.0 and ignores the variable. A remote
# URL supplied through git's environment config satisfies it without touching the
# repository's own configuration.
GIT_CONFIG_COUNT=1 \
	GIT_CONFIG_KEY_0=remote.origin.url \
	GIT_CONFIG_VALUE_0=https://github.com/puppet-stagehand/stagehand-deploy-agent.git \
	GORELEASER_CURRENT_TAG=v1.0.0 \
	goreleaser release --snapshot --clean --skip=sign,sbom,publish >"$TMP/goreleaser.log" 2>&1 || {
	cat "$TMP/goreleaser.log"
	exit 1
}
DIST=$ROOT/dist
DEB=stagehand-deploy-agent_${VERSION}_linux_amd64.deb
RPM=stagehand-deploy-agent_${VERSION}_linux_amd64.rpm
for f in "$DEB" "$RPM" "stagehand-deploy-agent_${VERSION}_linux_arm64.deb" "stagehand-deploy-agent_${VERSION}_linux_arm64.rpm" \
	"stagehand-deploy-agent_${VERSION}_linux_amd64.tar.gz" "stagehand-deploy-agent_${VERSION}_linux_arm64.tar.gz" checksums.txt; do
	[ -s "$DIST/$f" ] || {
		STEP="snapshot did not produce $f"
		exit 1
	}
done
echo "snapshot assets:"
(cd "$DIST" && find . -maxdepth 1 -type f \( -name '*.deb' -o -name '*.rpm' -o -name '*.tar.gz' -o -name checksums.txt \) | sed 's|^\./|  |' | sort)

step "version subcommand prints the snapshot version"
set -- "$DIST"/*linux_amd64*/stagehand-deploy-agent
[ -x "$1" ] || {
	STEP="no linux_amd64 binary under dist"
	exit 1
}
GOT=$(docker run --rm --platform linux/amd64 -v "$DIST:/dist:ro" debian:12 "/dist/$(basename "$(dirname "$1")")/stagehand-deploy-agent" version)
echo "version: $GOT"
[ "$GOT" = "stagehand-deploy-agent $VERSION" ] || {
	STEP="version printed '$GOT', want 'stagehand-deploy-agent $VERSION'"
	exit 1
}

# --- fork assets -------------------------------------------------------------
step "download and check the fork's release assets"
mkdir "$TMP/fork"
for a in "$FORK_DEB" "$FORK_RPM" checksums.txt; do
	curl -fsSL -o "$TMP/fork/$a" "$FORK_BASE/$a"
done
[ "$(sha256_of "$TMP/fork/$FORK_DEB")" = "$FORK_DEB_SHA256" ] || {
	STEP="the fork's deb does not match the pinned sha256"
	exit 1
}
LISTED=$(grep " $FORK_RPM\$" "$TMP/fork/checksums.txt" | cut -d' ' -f1)
[ -n "$LISTED" ] && [ "$(sha256_of "$TMP/fork/$FORK_RPM")" = "$LISTED" ] || {
	STEP="the fork's rpm does not match the release's checksums.txt"
	exit 1
}
echo "fork deb sha256 matches the pin; fork rpm matches checksums.txt ($LISTED)"

# --- inner scripts (run as root inside the containers) -----------------------
cat >"$TMP/common.sh" <<'INNER'
# Sourced by the deb and rpm checks. Stubs systemctl (a container has no systemd),
# recording every call so the checks can see what the package scripts asked for.
fail() {
	echo "CHECK FAIL: $*" >&2
	exit 1
}
mkdir -p /var/log
: >/var/log/systemctl-calls
cat >/usr/bin/systemctl <<'STUB'
#!/bin/sh
echo "$*" >>/var/log/systemctl-calls
exit 0
STUB
chmod 0755 /usr/bin/systemctl
groupadd --system puppet

# After the agent package is installed: the user, the files, and no start or enable.
check_installed_agent() {
	line=$(getent passwd stagehand-deploy-agent) || fail "user stagehand-deploy-agent was not created"
	gid=$(getent group puppet | cut -d: -f3)
	echo "user: $line"
	[ "$(echo "$line" | cut -d: -f4)" = "$gid" ] || fail "primary group is not puppet (gid $gid)"
	[ "$(echo "$line" | cut -d: -f6)" = /var/lib/stagehand-deploy-agent ] || fail "home is not /var/lib/stagehand-deploy-agent"
	[ "$(echo "$line" | cut -d: -f7)" = /usr/sbin/nologin ] || fail "shell is not /usr/sbin/nologin"
	[ -x /usr/bin/stagehand-deploy-agent ] || fail "binary missing"
	[ -f /usr/lib/systemd/system/stagehand-deploy-agent.service ] || fail "unit missing"
	[ -f /usr/share/doc/stagehand-deploy-agent/config.example.yml ] || fail "config.example.yml missing"
	[ -f /usr/share/doc/stagehand-deploy-agent/ssh_config.example ] || fail "ssh_config.example missing"
	[ ! -e /etc/stagehand-deploy-agent ] || fail "the package created /etc/stagehand-deploy-agent (no conffile, PD-25)"
	[ ! -e /etc/systemd/system/multi-user.target.wants/stagehand-deploy-agent.service ] || fail "the service was enabled"
	if grep -E '(^| )(start|enable|restart|preset|reload-or-restart)( |$)' /var/log/systemctl-calls | grep -q stagehand-deploy-agent; then
		cat /var/log/systemctl-calls
		fail "a package script started or enabled the service"
	fi
	[ "$(/usr/bin/stagehand-deploy-agent version)" = "stagehand-deploy-agent 1.0.0-snapshot" ] || fail "installed binary prints the wrong version"
}
INNER

cat >"$TMP/deb.sh" <<'INNER'
set -eu
. /work/common.sh
export DEBIAN_FRONTEND=noninteractive

echo "-- package metadata"
dpkg-deb -f "/pkg/$DEB" Package Version Architecture Replaces Conflicts
dpkg-deb -f "/pkg/$DEB" Replaces | grep -qw webhook-go || fail "Replaces does not name webhook-go"
dpkg-deb -f "/pkg/$DEB" Conflicts | grep -qw webhook-go || fail "Conflicts does not name webhook-go"
echo "-- package contents"
dpkg-deb -c "/pkg/$DEB"
dpkg-deb -c "/pkg/$DEB" | grep -q ' \./usr/lib/systemd/system/stagehand-deploy-agent.service$' || fail "the unit is not at /usr/lib/systemd/system"
if dpkg-deb -c "/pkg/$DEB" | awk '{print $6}' | grep -q '^\./etc/'; then fail "the deb ships a file under /etc"; fi

echo "-- the package refuses to install without the puppet group"
groupdel puppet
if dpkg -i "/pkg/$DEB" >/tmp/nogroup.log 2>&1; then fail "dpkg -i succeeded without the puppet group"; fi
grep -q 'needs the puppet group' /tmp/nogroup.log || { cat /tmp/nogroup.log; fail "the missing-group message is not shown"; }
dpkg -P stagehand-deploy-agent >/dev/null 2>&1 || true
groupadd --system puppet
[ -z "$(getent passwd stagehand-deploy-agent || true)" ] || fail "a failed install left the user behind"

echo "-- install the fork, then the agent over it"
dpkg -i "/fork/$FORK_DEB" >/dev/null
dpkg-query -W -f='${Status}\n' webhook-go | grep -q '^install ok installed$' || fail "the fork did not install"
dpkg -i "/pkg/$DEB"
status=$(dpkg-query -W -f='${Status}\n' webhook-go 2>/dev/null || true)
echo "webhook-go status after the agent install: ${status:-<unknown package>}"
case "$status" in
'install ok installed') fail "webhook-go is still installed" ;;
esac
dpkg-query -W -f='${Status}\n' stagehand-deploy-agent | grep -q '^install ok installed$' || fail "the agent is not installed"
[ ! -e /usr/bin/webhook-go ] || fail "the fork's binary is still on disk"
check_installed_agent

echo "-- reinstalling over itself (an upgrade) touches nothing"
: >/var/log/systemctl-calls
dpkg -i "/pkg/$DEB" >/dev/null
if grep -q 'disable' /var/log/systemctl-calls; then cat /var/log/systemctl-calls; fail "an upgrade disabled or stopped the service"; fi

echo "-- removal stops the service and keeps the user"
dpkg -r stagehand-deploy-agent >/dev/null
grep -qx 'disable --now stagehand-deploy-agent' /var/log/systemctl-calls || { cat /var/log/systemctl-calls; fail "removal did not disable --now the service"; }
getent passwd stagehand-deploy-agent >/dev/null || fail "removal deleted the user"
echo "DEB CHECKS OK"
INNER

cat >"$TMP/rpm.sh" <<'INNER'
set -eu
. /work/common.sh

echo "-- package metadata"
rpm -qp --queryformat '%{NAME} %{VERSION} %{ARCH}\n' "/pkg/$RPM"
echo "obsoletes: $(rpm -qp --obsoletes "/pkg/$RPM" | tr '\n' ' ')"
echo "conflicts: $(rpm -qp --conflicts "/pkg/$RPM" | tr '\n' ' ')"
rpm -qp --conflicts "/pkg/$RPM" | grep -qw webhook-go || fail "the rpm does not conflict with webhook-go"
echo "-- package contents"
rpm -qlp "/pkg/$RPM"
rpm -qlp "/pkg/$RPM" | grep -qx /usr/lib/systemd/system/stagehand-deploy-agent.service || fail "the unit is not at /usr/lib/systemd/system"
if rpm -qlp "/pkg/$RPM" | grep -q '^/etc/'; then fail "the rpm ships a file under /etc"; fi

echo "-- the package refuses to install without the puppet group"
groupdel puppet
if rpm -i "/pkg/$RPM" >/tmp/nogroup.log 2>&1; then fail "rpm -i succeeded without the puppet group"; fi
grep -q 'needs the puppet group' /tmp/nogroup.log || { cat /tmp/nogroup.log; fail "the missing-group message is not shown"; }
groupadd --system puppet
[ -z "$(getent passwd stagehand-deploy-agent || true)" ] || fail "a failed install left the user behind"

echo "-- install the fork, then try the agent beside it"
rpm -i "/fork/$FORK_RPM" >/dev/null 2>&1
rpm -q webhook-go >/dev/null || fail "the fork did not install"
if rpm -i "/pkg/$RPM" >/tmp/conflict.log 2>&1; then fail "rpm -i of the agent succeeded beside the fork"; fi
cat /tmp/conflict.log
grep -q 'webhook-go' /tmp/conflict.log || fail "the refusal does not name webhook-go"
grep -qi conflict /tmp/conflict.log || fail "the refusal does not say conflict"

echo "-- INFO (RESEARCH A6): rpm -U of the agent while the fork is installed"
if rpm -U "/pkg/$RPM" >/tmp/upgrade.log 2>&1; then
	echo "INFO rpm -U: succeeded"
	if rpm -q webhook-go >/dev/null 2>&1; then echo "INFO rpm -U: webhook-go is still installed"; else echo "INFO rpm -U: webhook-go was removed (Obsoletes honoured)"; fi
	rpm -q stagehand-deploy-agent >/dev/null 2>&1 && echo "INFO rpm -U: the agent is installed"
	rpm -e stagehand-deploy-agent >/dev/null 2>&1 || true
else
	echo "INFO rpm -U: failed:"
	sed 's/^/INFO rpm -U:   /' /tmp/upgrade.log
fi
rpm -q webhook-go >/dev/null 2>&1 || rpm -i "/fork/$FORK_RPM" >/dev/null 2>&1

echo "-- erase the fork, then install the agent"
rpm -e webhook-go >/dev/null 2>&1
rpm -q webhook-go >/dev/null 2>&1 && fail "webhook-go is still installed after rpm -e"
: >/var/log/systemctl-calls
rpm -i "/pkg/$RPM"
rpm -q stagehand-deploy-agent >/dev/null || fail "the agent is not installed"
check_installed_agent

echo "-- upgrading over itself touches nothing"
: >/var/log/systemctl-calls
rpm -U --replacepkgs "/pkg/$RPM" >/dev/null
if grep -q 'disable' /var/log/systemctl-calls; then cat /var/log/systemctl-calls; fail "an upgrade disabled or stopped the service"; fi

echo "-- erase stops the service and keeps the user"
rpm -e stagehand-deploy-agent
grep -qx 'disable --now stagehand-deploy-agent' /var/log/systemctl-calls || { cat /var/log/systemctl-calls; fail "erase did not disable --now the service"; }
getent passwd stagehand-deploy-agent >/dev/null || fail "erase deleted the user"
echo "RPM CHECKS OK"
INNER

# --- 3. debian:12 -------------------------------------------------------------
step "debian:12 deb checks"
docker run --rm --platform linux/amd64 \
	-v "$DIST:/pkg:ro" -v "$TMP/fork:/fork:ro" -v "$TMP:/work:ro" \
	-e "DEB=$DEB" -e "FORK_DEB=$FORK_DEB" \
	debian:12 sh /work/deb.sh 2>&1 | tee "$TMP/deb.out"
grep -qx 'DEB CHECKS OK' "$TMP/deb.out" || exit 1

# --- 4. rockylinux:9 ----------------------------------------------------------
step "rockylinux:9 rpm checks"
docker run --rm --platform linux/amd64 \
	-v "$DIST:/pkg:ro" -v "$TMP/fork:/fork:ro" -v "$TMP:/work:ro" \
	-e "RPM=$RPM" -e "FORK_RPM=$FORK_RPM" \
	rockylinux:9 sh /work/rpm.sh 2>&1 | tee "$TMP/rpm.out"
grep -qx 'RPM CHECKS OK' "$TMP/rpm.out" || exit 1

# --- 5. image -----------------------------------------------------------------
step "image built from the snapshot binary"
mkdir -p "$TMP/imgctx/amd64" "$TMP/imgctx/arm64"
tar -xzf "$DIST/stagehand-deploy-agent_${VERSION}_linux_amd64.tar.gz" -C "$TMP/imgctx/amd64" stagehand-deploy-agent
tar -xzf "$DIST/stagehand-deploy-agent_${VERSION}_linux_arm64.tar.gz" -C "$TMP/imgctx/arm64" stagehand-deploy-agent
docker build --quiet --platform linux/amd64 -f "$ROOT/Dockerfile" -t "$IMAGE_TAG" "$TMP/imgctx" >/dev/null
GOT=$(docker run --rm --platform linux/amd64 "$IMAGE_TAG" version)
echo "image version: $GOT"
[ "$GOT" = "stagehand-deploy-agent $VERSION" ] || {
	STEP="the image printed '$GOT'"
	exit 1
}
[ "$(docker image inspect -f '{{.Config.User}}' "$IMAGE_TAG")" = 65532:65532 ] || {
	STEP="the image does not run as 65532:65532"
	exit 1
}
docker image inspect -f '{{index .Config.Labels "org.opencontainers.image.description"}}' "$IMAGE_TAG" | grep -q 'Not a deploy runtime' || {
	STEP="the image label does not say it is not a deploy runtime"
	exit 1
}
echo "image size: $(docker image inspect -f '{{.Size}}' "$IMAGE_TAG") bytes"

DONE=1
echo "PACKAGE SMOKE OK"
