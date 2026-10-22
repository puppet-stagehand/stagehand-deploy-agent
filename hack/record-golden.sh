#!/bin/sh
# Records contract/golden/* from the webhook-go fork at v2.15.1-stagehand.1.
#
# Provenance for the golden corpus the agent is held to (plan 57.6.2-02,
# byte-compatibility of /api/v1/deploys). It does not touch the fork checkout:
# the tag is exported with `git archive` into a temp directory, the throwaway
# recorder (contract/recorder/record_test.go.txt) is dropped into the copy's api
# package, run there against the fork's own gin handlers and fake engine, and the
# copy is deleted.
#
# Usage: hack/record-golden.sh [path-to-webhook-go-checkout]   (default ../webhook-go)
# Needs: git, go (the fork's modules must be in the module cache or reachable).
set -eu

FORK_TAG="v2.15.1-stagehand.1"
here=$(cd "$(dirname "$0")/.." && pwd)
fork=${1:-"$here/../webhook-go"}
out="$here/contract/golden"

if ! git -C "$fork" rev-parse --verify --quiet "refs/tags/$FORK_TAG^{commit}" >/dev/null; then
	echo "record-golden: tag $FORK_TAG not found in $fork" >&2
	exit 2
fi
if [ ! -f "$out/cases.json" ]; then
	echo "record-golden: $out/cases.json is missing" >&2
	exit 2
fi

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

git -C "$fork" archive "$FORK_TAG" | tar -x -C "$tmp"
cp "$here/contract/recorder/record_test.go.txt" "$tmp/api/record_test.go"

(
	cd "$tmp"
	GOLDEN_OUT="$out" go test -count=1 -run TestRecordGolden ./api
)

echo "record-golden: wrote $(find "$out" -name '*.body' | wc -l | tr -d ' ') cases from $FORK_TAG into $out"
