#!/bin/sh
# Scrape a live agent's /metrics and run it through promtool's linter, so the
# hand-rolled exposition (internal/metrics) is held to the real Prometheus rules.
# The agent is started as hack/smoke.sh starts it, a deploy, a protected removal
# and a refused certificate put every family of series on the page, and the
# scrape goes to `promtool check metrics` from a pinned prom/prometheus image
# in a throwaway container (a local promtool is used when docker is absent).
# Prints "PROMTOOL OK" as its last line, or "PROMTOOL FAIL: <step>" and exits 1.
#
# No finding is accepted or filtered: the two certificate-expiry gauges are named
# *_seconds_until_expiry (the Prometheus base unit), so promtool must pass clean.
set -eu

# The pinned image: tag for the reader, digest for the guarantee.
PROMTOOL_IMAGE="prom/prometheus:v3.5.0@sha256:63805ebb8d2b3920190daf1cb14a60871b16fd38bed42b857a3182bc621f4996"

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SHA=0123456789abcdef0123456789abcdef01234567
STEP=setup
DONE=0
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stagehand-promtool.XXXXXX")
AGENT_PID=""

cleanup() {
	rc=$?
	if [ -n "$AGENT_PID" ]; then
		kill "$AGENT_PID" 2>/dev/null || true
		wait "$AGENT_PID" 2>/dev/null || true
	fi
	if [ "$DONE" -ne 1 ]; then
		if [ -f "$TMP/agent.log" ]; then
			echo "--- agent log ---"
			cat "$TMP/agent.log"
			echo "-----------------"
		fi
		echo "PROMTOOL FAIL: $STEP"
		rm -rf "$TMP"
		exit 1
	fi
	rm -rf "$TMP"
	exit "$rc"
}
trap cleanup EXIT

free_port() {
	p=$((20000 + $$ % 20000 + $1))
	while nc -z 127.0.0.1 "$p" 2>/dev/null; do p=$((p + 1)); done
	echo "$p"
}

STEP="build"
(cd "$ROOT" && go build -o "$TMP/stagehand-deploy-agent" ./cmd/stagehand-deploy-agent &&
	go build -o "$TMP/fakeengine" ./internal/jobs/fakeengine/cmd/fakeengine)

STEP="certificates"
cd "$TMP"
cat >ca.cnf <<'CNF'
[req]
distinguished_name = dn
prompt = no
x509_extensions = v3_ca
[dn]
CN = Promtool CA
[v3_ca]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
CNF
cat >leaf.ext <<'EXT'
subjectAltName = DNS:localhost,IP:127.0.0.1
extendedKeyUsage = serverAuth,clientAuth
keyUsage = digitalSignature
EXT
openssl ecparam -name prime256v1 -genkey -noout -out ca.key
openssl req -x509 -new -key ca.key -days 2 -config ca.cnf -out ca.pem
issue() { # issue <name> <cn>
	openssl ecparam -name prime256v1 -genkey -noout -out "$1.key"
	openssl req -new -key "$1.key" -subj "/CN=$2" -out "$1.csr"
	openssl x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -days 2 \
		-extfile leaf.ext -out "$1.pem" 2>/dev/null
}
issue server localhost
issue allowed stagehand-code-manager
issue intruder intruder

STEP="config"
PORT=$(free_port 0)
OPS_PORT=$(free_port 1)
mkdir -p "$TMP/environments" "$TMP/state"
cat >config.yml <<CFG
server:
  address: 127.0.0.1
  port: $PORT
  tls:
    certificate: $TMP/server.pem
    key: $TMP/server.key
    client_ca: $TMP/ca.pem
    allowed_certnames: [stagehand-code-manager]
ops:
  listen: 127.0.0.1:$OPS_PORT
engine:
  name: r10k
  command_path: $TMP/fakeengine
deploys:
  environments_dir: $TMP/environments
  state_dir: $TMP/state
  timeout: 1m
CFG

STEP="start the agent"
STAGEHAND_FAKE_ENGINE_FIXTURE="$ROOT/internal/jobs/testdata/r10k-5.0.3/deploy-success.json" \
STAGEHAND_FAKE_ENGINE_BASEDIR="$TMP/environments" \
STAGEHAND_FAKE_ENGINE_SHA="$SHA" \
	"$TMP/stagehand-deploy-agent" serve --config "$TMP/config.yml" >"$TMP/agent.log" 2>&1 &
AGENT_PID=$!

URL="https://127.0.0.1:$PORT"
OPS="http://127.0.0.1:$OPS_PORT"
mtls() { curl -sS --max-time 10 --cacert ca.pem --cert allowed.pem --key allowed.key "$@"; }

STEP="the agent did not become ready"
i=0
until [ "$(curl -sS --max-time 2 "$OPS/healthz" 2>/dev/null)" = ok ]; do
	i=$((i + 1))
	[ "$i" -lt 100 ] || exit 1
	kill -0 "$AGENT_PID" 2>/dev/null || exit 1
	sleep 0.1
done

STEP="put series on the page: a deploy, a protected removal, an invalid request"
CODE=$(mtls -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' \
	-d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":true}" \
	"$URL/api/v1/deploys")
[ "$CODE" = 200 ]
[ "$(mtls -o /dev/null -w '%{http_code}' -X DELETE "$URL/api/v1/environments/production")" = 403 ]
[ "$(mtls -o /dev/null -w '%{http_code}' -X POST -H 'Content-Type: application/json' -d '{"environments":[]}' "$URL/api/v1/deploys")" = 422 ]
set +e
curl -sS --max-time 10 --cacert ca.pem --cert intruder.pem --key intruder.key -o /dev/null "$URL/api/v1/deploys" 2>/dev/null
set -e

STEP="scrape /metrics"
curl -sS --max-time 10 -D scrape.headers -o scrape.txt "$OPS/metrics"
grep -qi '^content-type: text/plain; version=0.0.4; charset=utf-8' scrape.headers
grep -q '^stagehand_deploy_agent_jobs_total{kind="deploy",outcome="deployed"} 1$' scrape.txt
grep -q '^stagehand_deploy_agent_tls_refusals_total{reason="certname"} 1$' scrape.txt

STEP="promtool check metrics"
if docker info >/dev/null 2>&1; then
	run_promtool() { docker run --rm -i --entrypoint promtool "$PROMTOOL_IMAGE" check metrics; }
elif command -v promtool >/dev/null 2>&1; then
	run_promtool() { promtool check metrics; }
else
	echo "neither docker nor promtool is available" >&2
	exit 1
fi
set +e
run_promtool <scrape.txt >promtool.out 2>&1
PRC=$?
set -e
if [ "$PRC" -ne 0 ] || [ -s promtool.out ]; then
	cat promtool.out
	exit 1
fi

STEP="stop the agent"
kill "$AGENT_PID"
wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""

DONE=1
echo "PROMTOOL OK"
