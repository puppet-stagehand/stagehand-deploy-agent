#!/bin/sh
# Runs the Code Manager pack's real deploy client against the real agent binary.
#
# Usage: hack/pack-contract.sh <path-to-stagehand-pack-code-manager-checkout>
#
# The pack repository is private and is never modified by this script (D-08,
# D-11). It:
#   1. refuses (exit 2) unless the checkout's HEAD starts with 855c652 and
#      `git status --porcelain` is empty;
#   2. builds the agent and the fake engine into a temp directory;
#   3. copies contract/pack/agent_contract_test.go.txt into the pack as
#      internal/deploy/webhook/agent_contract_test.go, and removes it on ANY exit
#      (trap), so a failed run cannot leave it behind;
#   4. runs `go test -run TestAgentContract -v ./internal/deploy/webhook` in the pack;
#   5. re-checks HEAD and an empty `git status --porcelain`, then prints
#      PACK CONTRACT OK as its last line.
# Any failure prints PACK CONTRACT FAIL (or a refusal) and exits non-zero.
set -eu

PACK_COMMIT="855c652"
here=$(cd "$(dirname "$0")/.." && pwd)

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <pack-checkout>" >&2
	exit 2
fi
pack=$(cd "$1" 2>/dev/null && pwd) || {
	echo "pack-contract: $1 is not a directory" >&2
	exit 2
}
overlay_dir="$pack/internal/deploy/webhook"
overlay="$overlay_dir/agent_contract_test.go"

check_pack() { # check_pack <when>
	head=$(git -C "$pack" rev-parse HEAD 2>/dev/null) || {
		echo "pack-contract: $pack is not a git checkout" >&2
		return 2
	}
	case "$head" in
	"$PACK_COMMIT"*) ;;
	*)
		echo "pack-contract: $1: the pack checkout is at $head, not $PACK_COMMIT" >&2
		return 2
		;;
	esac
	dirty=$(git -C "$pack" status --porcelain)
	if [ -n "$dirty" ]; then
		echo "pack-contract: $1: the pack checkout is not clean:" >&2
		echo "$dirty" >&2
		return 2
	fi
	return 0
}

check_pack "before" || exit 2
if [ ! -d "$overlay_dir" ]; then
	echo "pack-contract: $overlay_dir does not exist" >&2
	exit 2
fi
if [ -e "$overlay" ]; then
	echo "pack-contract: $overlay already exists; refusing to overwrite it" >&2
	exit 2
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/stagehand-pack-contract.XXXXXX")
COPIED=0
cleanup() {
	if [ "$COPIED" -eq 1 ]; then
		rm -f "$overlay"
	fi
	rm -rf "$tmp"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

(
	cd "$here"
	go build -o "$tmp/stagehand-deploy-agent" ./cmd/stagehand-deploy-agent
	go build -o "$tmp/fakeengine" ./internal/jobs/fakeengine/cmd/fakeengine
)

cp "$here/contract/pack/agent_contract_test.go.txt" "$overlay"
COPIED=1

rc=0
(
	cd "$pack"
	STAGEHAND_AGENT_BIN="$tmp/stagehand-deploy-agent" \
	STAGEHAND_FAKE_ENGINE_BIN="$tmp/fakeengine" \
	STAGEHAND_FAKE_ENGINE_FIXTURE_DIR="$here/internal/jobs/testdata/r10k-5.0.3" \
		go test -count=1 -run TestAgentContract -v ./internal/deploy/webhook
) || rc=$?

rm -f "$overlay"
COPIED=0

if ! check_pack "after"; then
	echo "PACK CONTRACT FAIL: the pack checkout changed"
	exit 1
fi
if [ "$rc" -ne 0 ]; then
	echo "PACK CONTRACT FAIL: TestAgentContract exited $rc"
	exit 1
fi
echo "PACK CONTRACT OK"
