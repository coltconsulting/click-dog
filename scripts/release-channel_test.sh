#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/release-channel.sh"

assert_channel() {
	local want="$1"
	shift
	local got
	got="$($SCRIPT "$@")"
	if [ "$got" != "$want" ]; then
		echo "FAIL: channel for '$*' was '$got', want '$want'" >&2
		exit 1
	fi
}

assert_rejected() {
	if "$SCRIPT" "$@" >/dev/null 2>&1; then
		echo "FAIL: release channel accepted '$*'" >&2
		exit 1
	fi
}

assert_channel none coltconsulting/click-dog-internal workflow_dispatch ""

# Internal: alpha is the only channel that builds artifacts here. beta and ga
# are both gates, because both are built on the public side.
assert_channel internal_alpha coltconsulting/click-dog-internal push v26.08.3-alpha.1
assert_channel internal_alpha coltconsulting/click-dog-internal push v26.08.3-alpha.12
assert_channel gate_only coltconsulting/click-dog-internal push v26.08.3-beta.1
assert_channel gate_only coltconsulting/click-dog-internal push v26.08.3

# Public: beta builds a prerelease, ga builds the production release, and alpha
# must never arrive at all.
assert_channel public_ga coltconsulting/click-dog push v26.08.3
assert_channel public_beta coltconsulting/click-dog push v26.08.3-beta.1
assert_channel public_beta coltconsulting/click-dog push v26.08.3-beta.7
assert_rejected coltconsulting/click-dog push v26.08.3-alpha.1

# The retired qualifiers must not classify anywhere.
assert_rejected coltconsulting/click-dog-internal push v26.08.3-uat.1
assert_rejected coltconsulting/click-dog-internal push v26.08.3-test.1
assert_rejected coltconsulting/click-dog push v26.08.3-uat.1
# Malformed ordinals are not a channel either.
assert_rejected coltconsulting/click-dog-internal push v26.08.3-alpha.0
assert_rejected coltconsulting/click-dog-internal push v26.08.3-alpha
assert_rejected coltconsulting/click-dog-internal push v26.08.3-test
assert_rejected coltconsulting/click-dog-internal push v26.8.3
assert_rejected coltconsulting/click-dog-internal push v26.13.1
assert_rejected somebody/click-dog push v26.08.3

echo "release channel tests: PASS"
