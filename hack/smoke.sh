#!/bin/sh
# Binary-level end-to-end check: build the agent and the fake engine, start the
# agent with a strict YAML config, deploy one environment over mutual TLS with
# an allow-listed client certificate, poll until the record reads "deployed",
# and check that a certificate whose certname is not allow-listed is refused in
# the TLS handshake. It also removes an environment (DELETE) and checks that the
# protected default, production, is refused. It probes the loopback ops listener
# (/healthz, /readyz) and checks that the mTLS port serves none of the ops routes.
# Prints "SMOKE OK" as its last line, or
# "SMOKE FAIL: <step>" and exits 1.
set -eu

ROOT=$(cd "$(dirname "$0")/.." && pwd)
SHA=0123456789abcdef0123456789abcdef01234567
STEP=setup
DONE=0
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stagehand-smoke.XXXXXX")
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
		echo "SMOKE FAIL: $STEP"
		rm -rf "$TMP"
		exit 1
	fi
	rm -rf "$TMP"
	exit "$rc"
}
trap cleanup EXIT

# free_port prints a high port nothing is listening on.
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
CN = Smoke CA
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
  basedir: $TMP/environments
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
mtls() { curl -sS --max-time 10 --cacert ca.pem --cert allowed.pem --key allowed.key "$@"; }

STEP="agent did not become ready"
i=0
until mtls -o /dev/null "$URL/api/v1/deploys?limit=1" 2>/dev/null; do
	i=$((i + 1))
	[ "$i" -lt 100 ] || exit 1
	kill -0 "$AGENT_PID" 2>/dev/null || exit 1
	sleep 0.1
done

STEP="ops listener: /healthz is ok"
OPS="http://127.0.0.1:$OPS_PORT"
[ "$(curl -sS --max-time 10 "$OPS/healthz")" = ok ]

STEP="ops listener: /readyz is ready"
curl -sS --max-time 10 -o ready.json -w '%{http_code}' "$OPS/readyz" >ready.code
[ "$(cat ready.code)" = 200 ]
grep -qx '{"ready":true,"failing":\[\]}' ready.json

STEP="ops listener: the deploy API is not served"
[ "$(curl -sS --max-time 10 -o /dev/null -w '%{http_code}' "$OPS/api/v1/deploys")" = 404 ]

STEP="mTLS port: /readyz, /healthz and /metrics are 404 for the allowed certificate"
for p in readyz healthz metrics; do
	[ "$(mtls -o /dev/null -w '%{http_code}' "$URL/$p")" = 404 ]
done

STEP="POST /api/v1/deploys"
CODE=$(mtls -o post.json -D post.headers -w '%{http_code}' -X POST \
	-H 'Content-Type: application/json' \
	-d "{\"environments\":[{\"name\":\"production\",\"expected_sha\":\"$SHA\"}],\"wait\":false}" \
	"$URL/api/v1/deploys")
[ "$CODE" = 202 ]
ID=$(sed -n 's/.*"id":"\([0-9a-f]\{32\}\)".*/\1/p' post.json)
[ -n "$ID" ]
grep -qi "^location: /api/v1/deploys/$ID" post.headers

STEP="GET /api/v1/deploys/$ID reaches deployed"
i=0
while :; do
	mtls -o get.json "$URL/api/v1/deploys/$ID"
	if grep -q '"state":"deployed","created_at"' get.json; then break; fi
	if grep -q '"state":"failed","created_at"' get.json; then cat get.json; exit 1; fi
	i=$((i + 1))
	[ "$i" -lt 300 ] || exit 1
	sleep 0.1
done
grep -q "\"deployed_sha\":\"$SHA\"" get.json

STEP="DELETE a protected environment is refused with 403"
CODE=$(mtls -o protected.json -w '%{http_code}' -X DELETE "$URL/api/v1/environments/production")
[ "$CODE" = 403 ]
grep -qx '{"error":"environment production is protected","field":"environment"}' protected.json
[ -d "$TMP/environments/production" ]

STEP="POST /api/v1/deploys feature_x"
CODE=$(mtls -o post2.json -w '%{http_code}' -X POST \
	-H 'Content-Type: application/json' \
	-d "{\"environments\":[{\"name\":\"feature_x\",\"expected_sha\":\"$SHA\"}],\"wait\":true}" \
	"$URL/api/v1/deploys")
[ "$CODE" = 200 ]
grep -q '"state":"deployed","created_at"' post2.json
[ -d "$TMP/environments/feature_x" ]

STEP="DELETE /api/v1/environments/feature_x"
CODE=$(mtls -o del.json -D del.headers -w '%{http_code}' -X DELETE "$URL/api/v1/environments/feature_x")
[ "$CODE" = 202 ]
RID=$(sed -n 's/.*"id":"\([0-9a-f]\{32\}\)".*/\1/p' del.json)
[ -n "$RID" ]
grep -q '"kind":"remove"' del.json
grep -qi "^location: /api/v1/removals/$RID" del.headers

STEP="GET /api/v1/removals/$RID reaches deployed"
i=0
while :; do
	mtls -o rem.json "$URL/api/v1/removals/$RID"
	if grep -q '"state":"deployed","created_at"' rem.json; then break; fi
	if grep -q '"state":"failed","created_at"' rem.json; then cat rem.json; exit 1; fi
	i=$((i + 1))
	[ "$i" -lt 300 ] || exit 1
	sleep 0.1
done
[ ! -e "$TMP/environments/feature_x" ]
for leftover in "$TMP"/environments/.removing-*; do
	[ -e "$leftover" ] && exit 1
done

STEP="a removal is not listed as a deploy"
CODE=$(mtls -o gone.json -w '%{http_code}' "$URL/api/v1/deploys/$RID")
[ "$CODE" = 404 ]

STEP="a non-listed certname must be refused in the handshake"
set +e
curl -sS --max-time 10 --cacert ca.pem --cert intruder.pem --key intruder.key \
	-o /dev/null "$URL/api/v1/deploys?limit=1" 2>/dev/null
RC=$?
set -e
[ "$RC" -eq 35 ] || [ "$RC" -eq 56 ]

STEP="stop the agent"
kill "$AGENT_PID"
wait "$AGENT_PID" 2>/dev/null || true
AGENT_PID=""

DONE=1
echo "SMOKE OK"
