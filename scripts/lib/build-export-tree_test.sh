#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="$ROOT/scripts/lib/build-export-tree.sh"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-export-tree-test.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

SCRATCH_ROOT="$TMP_DIR/scratch"
BAD_MANIFEST="$TMP_DIR/manifest-is-a-directory"
mkdir -p "$SCRATCH_ROOT" "$BAD_MANIFEST"

# Run the helper in a separate shell so testing its status does not put the
# function itself in an `if` condition, which would suppress errexit inside it.
if TMPDIR="$SCRATCH_ROOT" bash -c '
	set -euo pipefail
	. "$1"
	build_export_tree "$2" "$3" "$4"
' _ "$HELPER" "$ROOT" "$TMP_DIR/export" "$BAD_MANIFEST" >/dev/null 2>&1; then
	echo "FAIL: build_export_tree succeeded after the manifest write failed" >&2
	exit 1
fi

if [ -n "$(find "$SCRATCH_ROOT" -mindepth 1 -print -quit)" ]; then
	echo "FAIL: build_export_tree left its scratch directory after failure" >&2
	exit 1
fi

echo "build export tree tests: PASS"
