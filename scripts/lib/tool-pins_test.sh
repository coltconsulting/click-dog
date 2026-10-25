#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="$ROOT/scripts/lib/tool-pins.sh"

# shellcheck source=scripts/tool-versions.env
. "$ROOT/scripts/tool-versions.env"
# shellcheck source=scripts/lib/tool-pins.sh
. "$HELPER"

# The opt-out is under test, so one inherited from the caller (an exported
# Make variable, or the environment) must not leak into the cases below.
unset ALLOW_UNPINNED_TOOLS

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT

fail() {
	echo "FAIL: $1" >&2
	exit 1
}

assert_eq() {
	[ "$2" = "$3" ] || fail "$1 (want '$3', got '$2')"
}

assert_contains() {
	case "$2" in
		*"$3"*) ;;
		*) fail "$1 (want output containing '$3', got: $2)" ;;
	esac
}

# One fake tool and one fake `go`, each created once: macOS scans every new
# executable on its first run, at about half a second apiece. The tool prints
# whatever a case wrote to $TMP_DIR/output, so each case pins the exact text a
# real release prints, and it records every `run` (a lint, as opposed to a
# version query) so a gate can be shown to refuse it before running it.
FAKE="$TMP_DIR/bin/tool"
CALLS="$TMP_DIR/calls"
mkdir -p "$TMP_DIR/bin" "$TMP_DIR/go"
cat > "$FAKE" <<EOF
#!/bin/sh
if [ "\$1" = run ]; then
	echo "\$*" >> "$CALLS"
	exit 0
fi
cat "$TMP_DIR/output"
EOF
cat > "$TMP_DIR/go/go" <<EOF
#!/bin/sh
cat "$TMP_DIR/go-output"
EOF
chmod +x "$FAKE" "$TMP_DIR/go/go"

# says <output>: what the fake tool prints from now on.
says() {
	printf '%s\n' "$1" > "$TMP_DIR/output"
}

# check <tool> <output>: the status tool_pin_check returns for a <tool> that
# prints <output>.
check() {
	local status=0
	says "$2"
	tool_pin_check "$1" "$FAKE" || status=$?
	printf '%s\n' "$status"
}

# ── each tool's real version shape reads as the pin ──────────────
# Release builds and `go install` builds spell versions differently (with and
# without the leading "v"), and both must be accepted as the same pin.
assert_eq "golangci-lint release build" \
	"$(check golangci-lint "golangci-lint has version ${GOLANGCI_LINT_VERSION#v} built with go1.26.9 from 5d1e709 on 2026-09-01T00:00:00Z")" 0
assert_eq "golangci-lint go install build" \
	"$(check golangci-lint "golangci-lint has version $GOLANGCI_LINT_VERSION built with go1.26.9 from (unknown, modified: ?, mod sum: \"h1:abc=\") on (unknown)")" 0
# govulncheck leads with the Go toolchain version, which must not be mistaken
# for its own.
assert_eq "govulncheck" \
	"$(check govulncheck "Go: go1.26.9
Scanner: govulncheck@$GOVULNCHECK_VERSION
DB: https://vuln.go.dev
DB updated: 2026-10-21 00:27:29 +0000 UTC")" 0
assert_eq "actionlint go install build" \
	"$(check actionlint "$ACTIONLINT_VERSION
installed by building from source
built with go1.26.9 compiler for darwin/arm64")" 0
assert_eq "actionlint release build" \
	"$(check actionlint "${ACTIONLINT_VERSION#v}
installed by downloading from release page
built with go1.26.9 compiler for linux/amd64")" 0
assert_eq "kubeconform release build" "$(check kubeconform "$KUBECONFORM_VERSION")" 0
assert_eq "shellcheck" \
	"$(check shellcheck "ShellCheck - shell script analysis tool
version: ${SHELLCHECK_VERSION#v}
license: GNU General Public License, version 3
website: https://www.shellcheck.net")" 0
assert_eq "openspec" "$(check openspec "$OPENSPEC_VERSION")" 0

# ── another version is refused, and the comparison is exact ──────
OLD_LINT="golangci-lint has version 2.10.1 built with go1.26.0 from 5d1e709 on 2026-02-17T15:21:47Z"
assert_eq "an older golangci-lint is refused" "$(check golangci-lint "$OLD_LINT")" 1
tool_pin_check golangci-lint "$FAKE" || true
assert_eq "the refused version is reported" "$TOOL_PIN_FOUND" "2.10.1"
assert_eq "the pin is reported" "$TOOL_PIN_EXPECTED" "$GOLANGCI_LINT_VERSION"
# A substring test would accept this: the pin is a prefix of it.
assert_eq "a version that merely starts with the pin is refused" \
	"$(check golangci-lint "golangci-lint has version ${GOLANGCI_LINT_VERSION#v}0 built with go1.26.9")" 1
assert_eq "an older shellcheck is refused" \
	"$(check shellcheck "ShellCheck - shell script analysis tool
version: 0.10.0")" 1
assert_eq "an older govulncheck is refused" \
	"$(check govulncheck "Go: go1.26.9
Scanner: govulncheck@v1.1.4")" 1
# A version that cannot be read cannot be proven pinned.
assert_eq "an unreadable version is refused" "$(check openspec "openspec: command failed")" 1
assert_eq "a missing tool is its own verdict" \
	"$(tool_pin_check golangci-lint "$TMP_DIR/nowhere/golangci-lint" && echo 0 || echo $?)" 2

# ── a version only in the build info is read from the build info ──
# `go install` builds of kubeconform report "development"; the fallback reads
# the main module version that every Go binary records, via `go version -m`.
printf '%s\n' "$FAKE: go1.26.9
	path	github.com/yannh/kubeconform/cmd/kubeconform
	mod	github.com/yannh/kubeconform	$KUBECONFORM_VERSION	h1:abc=
	dep	github.com/santhosh-tekuri/jsonschema/v6	v6.0.1	h1:def=" > "$TMP_DIR/go-output"
assert_eq "kubeconform go install build reads its build info" \
	"$(PATH="$TMP_DIR/go:$PATH" check kubeconform "development")" 0
printf '%s\n' "$FAKE: go1.26.9
	path	github.com/yannh/kubeconform/cmd/kubeconform
	mod	github.com/yannh/kubeconform	(devel)
	dep	github.com/santhosh-tekuri/jsonschema/v6	v6.0.1	h1:def=" > "$TMP_DIR/go-output"
assert_eq "a (devel) build info is not a version" \
	"$(PATH="$TMP_DIR/go:$PATH" check kubeconform "development")" 1

# ── the command line: refuse, explain, and honor only an exact opt-out ──
says "$OLD_LINT"
status=0
out="$(bash "$HELPER" golangci-lint "$FAKE" 2>&1)" || status=$?
assert_eq "an unpinned tool fails the gate" "$status" 1
assert_contains "the failure names the tool's version" "$out" "is 2.10.1"
assert_contains "the failure names the pin" "$out" "pins $GOLANGCI_LINT_VERSION"
assert_contains "the failure names the fix" "$out" "make dev-setup"
assert_contains "the failure names the opt-out" "$out" "ALLOW_UNPINNED_TOOLS=1"

status=0
out="$(ALLOW_UNPINNED_TOOLS=1 bash "$HELPER" golangci-lint "$FAKE" 2>&1)" || status=$?
assert_eq "ALLOW_UNPINNED_TOOLS=1 accepts the substitute" "$status" 0
assert_contains "the accepted substitute is still announced" "$out" "WARN: golangci-lint"

for value in 0 true yes " 1"; do
	status=0
	ALLOW_UNPINNED_TOOLS="$value" bash "$HELPER" golangci-lint "$FAKE" >/dev/null 2>&1 || status=$?
	assert_eq "ALLOW_UNPINNED_TOOLS='$value' is not an opt-out" "$status" 1
done

status=0
out="$(ALLOW_UNPINNED_TOOLS=1 bash "$HELPER" golangci-lint "$TMP_DIR/nowhere/golangci-lint" 2>&1)" || status=$?
assert_eq "the opt-out does not excuse a missing tool" "$status" 1
assert_contains "a missing tool names the fix" "$out" "not installed; run: make dev-setup"

says "golangci-lint has version $GOLANGCI_LINT_VERSION built with go1.26.9"
status=0
bash "$HELPER" golangci-lint "$FAKE" >/dev/null 2>&1 || status=$?
assert_eq "the pinned tool passes the gate" "$status" 0

# ── the gates call it, before the tool runs ─────────────────────
for tool in golangci-lint govulncheck actionlint kubeconform shellcheck openspec; do
	grep -q "call require_pinned,$tool," "$ROOT/Makefile.shared" ||
		fail "no gate in Makefile.shared checks the $tool pin"
done

# A clean sub-make: the parent's MAKEFLAGS would otherwise pass along its own
# command-line variables, ALLOW_UNPINNED_TOOLS among them.
sub_make() {
	env -u MAKEFLAGS -u MFLAGS -u MAKELEVEL make -s -C "$ROOT" "$@"
}

says "$OLD_LINT"
status=0
out="$(sub_make check-lint GOLANGCI_LINT="$FAKE" AGENT_CACHE_DIR="$TMP_DIR/cache" 2>&1)" || status=$?
[ "$status" -ne 0 ] || fail "make check-lint accepted an unpinned golangci-lint"
assert_contains "make check-lint names the fix" "$out" "make dev-setup"
[ ! -e "$CALLS" ] || fail "make check-lint ran the unpinned linter before refusing it"

status=0
sub_make check-lint GOLANGCI_LINT="$FAKE" AGENT_CACHE_DIR="$TMP_DIR/cache" ALLOW_UNPINNED_TOOLS=1 >/dev/null 2>&1 || status=$?
assert_eq "make check-lint ALLOW_UNPINNED_TOOLS=1 runs the substitute" "$status" 0
assert_eq "the substitute ran the lint" "$(cat "$CALLS" 2>/dev/null)" "run"

echo "tool pin tests: PASS"
