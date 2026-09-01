#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-public-release.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

command -v goreleaser >/dev/null 2>&1 || {
	echo "error: goreleaser is required for the public release snapshot test" >&2
	exit 2
}

# shellcheck source=scripts/lib/build-export-tree.sh
. "$ROOT/scripts/lib/build-export-tree.sh"

# Materialize the same export-ignore tree used by the public publisher while
# including uncommitted worktree changes. A release snapshot against the full
# internal checkout would miss absent-input failures at the public boundary.
build_export_tree "$ROOT" "$TMP_DIR/export"

git -C "$TMP_DIR/export" init -q
git -C "$TMP_DIR/export" config user.name "public-release-test"
git -C "$TMP_DIR/export" config user.email "public-release-test@example.invalid"
git -C "$TMP_DIR/export" add -A
git -C "$TMP_DIR/export" commit -q -m snapshot

(
	cd "$TMP_DIR/export"
	CLICK_DOG_IMAGE_REPOSITORY=ghcr.io/coltconsulting/click-dog \
		CLICK_DOG_SOURCE_REPOSITORY=https://github.com/coltconsulting/click-dog \
		GOCACHE="$TMP_DIR/go-build" \
		goreleaser release --snapshot --clean --skip=docker,sign
)

shopt -s nullglob
archives=("$TMP_DIR"/export/dist/*.tar.gz)
if [ "${#archives[@]}" -ne 2 ]; then
	echo "FAIL: expected two public release archives, found ${#archives[@]}" >&2
	exit 1
fi

for archive in "${archives[@]}"; do
	contents="$TMP_DIR/$(basename "$archive").txt"
	tar -tzf "$archive" > "$contents"
	for profile in minimal production paranoid; do
		required="examples/click-dog-${profile}.yaml"
		if ! grep -Fxq "$required" "$contents"; then
			echo "FAIL: $(basename "$archive") is missing $required" >&2
			exit 1
		fi
	done
done

echo "public release snapshot: two archives contain all starter configurations"
