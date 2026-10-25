#!/usr/bin/env bash
# vulncheck.sh GOVULNCHECK — govulncheck ./..., then again against the upstream
# release of every module the root go.mod replaces with a local directory.
#
# govulncheck looks advisories up by a module's replacement path, so a module
# built from third_party/ (see third_party/go-zookeeper/README.md) matches no
# advisory at all. The second pass drops those replaces in a temporary copy of
# go.mod, so an advisory for the upstream version the patch is based on still
# fails the gate. Used by `make check-vuln` and the CI govulncheck job.
set -euo pipefail

GOVULNCHECK="${1:-govulncheck}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"

"$GOVULNCHECK" ./...

# Local replaces: old path whose new path is a directory (./ or ../).
replaced="$(go mod edit -json | awk '
	/"Replace": \[/ { inrep = 1 }
	inrep && /"Old": \{/ { side = "old" }
	inrep && /"New": \{/ { side = "new" }
	inrep && /"Path":/ {
		p = $0; sub(/.*"Path": "/, "", p); sub(/".*/, "", p)
		if (side == "old") old = p
		else if (p ~ /^\.\.?\//) print old
	}')"
[[ -n "$replaced" ]] || exit 0

tmp="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-vulncheck.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT
# govulncheck loads packages itself and refuses -modfile, so the second pass
# scans a copy of the module (tracked and new files, without third_party/),
# whose go.mod builds the replaced modules from their upstream releases.
git ls-files -z --cached --others --exclude-standard -- . ':(exclude)third_party' |
	while IFS= read -r -d '' f; do [[ -e "$f" ]] && printf '%s\0' "$f"; done |
	tar -c --null -T - -f - | tar -x -f - -C "$tmp"
while IFS= read -r mod; do
	(cd "$tmp" && go mod edit -dropreplace="$mod")
	echo "govulncheck: also scanning upstream $mod (replaced by a local directory)"
done <<<"$replaced"
# The upstream module's checksum is not in go.sum while the replace is active.
# shellcheck disable=SC2086 # one module path per word
(cd "$tmp" && GOFLAGS=-mod=mod go mod download $replaced && "$GOVULNCHECK" ./...)
