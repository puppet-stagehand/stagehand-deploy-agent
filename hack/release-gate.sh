#!/bin/sh
# The local release gate: every automated check the agent has, in order, on the
# commit that will be tagged. Run it from a clean tree before you push a tag.
#
# Usage: hack/release-gate.sh [path-to-stagehand-pack-code-manager-checkout]
#        (default: ../stagehand-pack-code-manager, next to this repository)
#
# After each check it prints "GATE <name>: ok". At the first failure it prints
# "GATE <name>: FAILED" and exits 1. Only when all thirteen pass is the last line
# "RELEASE GATE OK". The pack checkout is only read (hack/pack-contract.sh
# refuses a dirty or moved checkout and restores it).
#
# govulncheck reports the standard library of the Go toolchain on THIS machine.
# If that toolchain is older than the newest patch it lists standard-library
# findings. Those are recorded and pass only when EVERY finding is in the
# standard library (the release workflow builds with the newest patch of the
# 1.25 line); any finding in a module fails. GATE_STRICT_VULN=1 makes any
# finding fail.
#
# Needs: go, git, docker, goreleaser, actionlint, shellcheck, network access.
set -eu

# Keep in step with .github/workflows/ci.yml (checked below).
VULN_VERSION=v1.1.4

ROOT=$(cd "$(dirname "$0")/.." && pwd)
PACK_ARG=${1:-"$ROOT/../stagehand-pack-code-manager"}
TMP=$(mktemp -d "${TMPDIR:-/tmp}/stagehand-release-gate.XXXXXX")
trap 'rm -rf "$TMP"' EXIT

cd "$ROOT"

for tool in go git docker goreleaser actionlint shellcheck; do
	if ! command -v "$tool" >/dev/null 2>&1; then
		echo "GATE prerequisites: FAILED ($tool is not installed)"
		exit 1
	fi
done
if ! PACK=$(cd "$PACK_ARG" 2>/dev/null && pwd); then
	echo "GATE prerequisites: FAILED (no pack checkout at $PACK_ARG)"
	exit 1
fi

echo "release candidate: $(git rev-parse HEAD) ($(git rev-parse --abbrev-ref HEAD))"
echo "go: $(go version)"

# gate NAME COMMAND [ARGS...]: run one check, print its verdict.
gate() {
	name=$1
	shift
	echo "== $name"
	if "$@"; then
		echo "GATE $name: ok"
	else
		echo "GATE $name: FAILED"
		exit 1
	fi
}

# vuln_check: govulncheck, with the standard-library rule described above.
vuln_check() {
	if ! grep -q "govulncheck@$VULN_VERSION" .github/workflows/ci.yml; then
		echo "ci.yml does not pin govulncheck@$VULN_VERSION; update VULN_VERSION in this script" >&2
		return 1
	fi
	if go run "golang.org/x/vuln/cmd/govulncheck@$VULN_VERSION" ./... >"$TMP/vuln.txt" 2>&1; then
		cat "$TMP/vuln.txt"
		return 0
	fi
	cat "$TMP/vuln.txt"
	total=$(grep -c '^Vulnerability #' "$TMP/vuln.txt" || true)
	stdlib=$(grep -c '^  Standard library$' "$TMP/vuln.txt" || true)
	if [ "${GATE_STRICT_VULN:-}" != "1" ] && [ "$total" -gt 0 ] && [ "$total" -eq "$stdlib" ]; then
		ids=$(grep '^Vulnerability #' "$TMP/vuln.txt" | sed 's/^Vulnerability #[0-9]*: //' | tr '\n' ' ')
		echo "govulncheck: $total finding(s), all in the Go standard library of $(go version | cut -d' ' -f3): $ids"
		echo "govulncheck: recorded, not hidden; the release build uses the newest patch of Go 1.25"
		return 0
	fi
	return 1
}

gate vet go vet ./...
gate test-race go test -race -count=1 ./...
gate govulncheck vuln_check
gate golden go test -count=1 -run TestGolden ./contract
gate pack-contract sh hack/pack-contract.sh "$PACK"
gate smoke sh hack/smoke.sh
gate unit-verify sh hack/unit-verify.sh
gate package-smoke sh hack/package-smoke.sh
gate promtool sh hack/promtool-check.sh
gate goreleaser-check goreleaser check
gate actionlint actionlint
gate shellcheck sh -c 'shellcheck hack/*.sh packaging/scripts/*.sh'

# The tree must be clean, so the result describes the commit and nothing else.
clean_tree() {
	status=$(git status --porcelain)
	if [ -n "$status" ]; then
		echo "$status"
		return 1
	fi
}
gate clean-tree clean_tree

echo "release candidate: $(git rev-parse HEAD)"
echo "RELEASE GATE OK"
