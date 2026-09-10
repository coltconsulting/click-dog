#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT="$ROOT/scripts/next-release-tag.sh"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-version-test.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

git -C "$TMP_DIR" init -q
git -C "$TMP_DIR" config user.name "release-version-test"
git -C "$TMP_DIR" config user.email "release-version-test@example.invalid"
git -C "$TMP_DIR" commit -q --allow-empty -m init

assert_tag() {
	local want="$1"
	shift
	local got
	got="$(cd "$TMP_DIR" && "$SCRIPT" "$@")"
	if [ "$got" != "$want" ]; then
		echo "FAIL: next tag for '$*' was '$got', want '$want'" >&2
		exit 1
	fi
}

# No qualifier means alpha: the everyday channel, and the one that cannot
# reach the public repository. GA has to be asked for by name.
assert_tag v26.08.1-alpha.1 "" "" 26 08
assert_tag v26.08.1 ga "" 26 08
git -C "$TMP_DIR" tag v26.08.1
git -C "$TMP_DIR" tag v26.08.2
git -C "$TMP_DIR" tag v26.08.9-alpha.1

# Qualified tags do not consume the GA index. The next release candidate is
# based on max GA + 1 and receives an independent per-qualifier ordinal.
assert_tag v26.08.3 ga "" 26 08
assert_tag v26.08.3-alpha.1 alpha "" 26 08
git -C "$TMP_DIR" tag v26.08.3-alpha.1
git -C "$TMP_DIR" tag v26.08.3-alpha.3
assert_tag v26.08.3-alpha.4 alpha "" 26 08
assert_tag v26.08.3-alpha.4 "" "" 26 08
assert_tag v26.08.3-beta.1 beta "" 26 08
assert_tag v26.08.3 ga "" 26 08

# Explicit bases use the same ordinal rule and do not move or reuse tags.
assert_tag v26.08.7-beta.1 beta 26.08.7 26 08
git -C "$TMP_DIR" tag v26.08.7-beta.1
assert_tag v26.08.7-beta.2 beta 26.08.7 26 08
assert_tag v26.08.7 ga 26.08.7 26 08

for retired in rc uat test; do
	if (cd "$TMP_DIR" && "$SCRIPT" "$retired" "" 26 08 >/dev/null 2>&1); then
		echo "FAIL: retired/invalid qualifier '$retired' was accepted" >&2
		exit 1
	fi
done
if (cd "$TMP_DIR" && "$SCRIPT" rc "" 26 08 >/dev/null 2>&1); then
	echo "FAIL: invalid qualifier was accepted" >&2
	exit 1
fi
if (cd "$TMP_DIR" && "$SCRIPT" alpha 26.8.3 26 08 >/dev/null 2>&1); then
	echo "FAIL: non-canonical explicit version was accepted" >&2
	exit 1
fi

echo "release version tests: PASS"
