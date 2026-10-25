#!/usr/bin/env bash
# Refuse to run a gate on a repository-managed tool that is not the version
# scripts/tool-versions.env pins.
#
# The Makefile prefers .agent-cache/bin and falls back to PATH, so a checkout
# that never ran `make dev-setup` gates with whatever the host happens to have.
# A different linter is a different gate: Homebrew golangci-lint v2.10.1
# reported six false SA5011 findings that the pinned v2.12.2 does not, and
# failed check-deep on a clean tree. This turns that silent substitution into a
# named failure.
#
#   scripts/lib/tool-pins.sh <tool> <command>
#
# exits 0 when <command> resolves and reports the pinned version of <tool>, and
# 1 otherwise. ALLOW_UNPINNED_TOOLS=1 downgrades a version mismatch (never a
# missing tool) to a warning, for restricted environments that cannot run
# `make dev-setup`. Doctor sources this file for the same verdicts.

# The pin for <tool>, as tool-versions.env spells it. The caller has sourced
# that file.
tool_pin_expected() {
	case "$1" in
		golangci-lint) printf '%s\n' "${GOLANGCI_LINT_VERSION:-}" ;;
		govulncheck) printf '%s\n' "${GOVULNCHECK_VERSION:-}" ;;
		actionlint) printf '%s\n' "${ACTIONLINT_VERSION:-}" ;;
		kubeconform) printf '%s\n' "${KUBECONFORM_VERSION:-}" ;;
		shellcheck) printf '%s\n' "${SHELLCHECK_VERSION:-}" ;;
		openspec) printf '%s\n' "${OPENSPEC_VERSION:-}" ;;
		*) return 1 ;;
	esac
}

# The version <command> reports for itself, or nothing when it reports none.
# Each tool prints its version in its own shape, and some print more than one
# version (govulncheck leads with the Go toolchain), so each gets its own
# pattern rather than "the first thing that looks like a version".
#
# A `go install` build of kubeconform reports "development", because its
# version is stamped only by the release build. Every Go binary also carries
# its main module's version in its build info, so that is the fallback.
tool_pin_reported() {
	local tool="$1" command="$2" out="" re version=""
	case "$tool" in
		golangci-lint)
			out="$("$command" version 2>&1)" || true
			re='has version (v?[0-9][^[:space:]]*)'
			;;
		govulncheck)
			out="$("$command" -version 2>&1)" || true
			re='Scanner: govulncheck@(v?[0-9][^[:space:]]*)'
			;;
		actionlint)
			out="$("$command" -version 2>&1)" || true
			re='^(v?[0-9][^[:space:]]*)'
			;;
		kubeconform)
			out="$("$command" -v 2>&1)" || true
			re='^(v?[0-9][^[:space:]]*)'
			;;
		shellcheck)
			out="$("$command" --version 2>&1)" || true
			re='version: (v?[0-9][^[:space:]]*)'
			;;
		openspec)
			out="$("$command" --version 2>&1)" || true
			re='^(v?[0-9][^[:space:]]*)'
			;;
		*) return 1 ;;
	esac
	if [[ "$out" =~ $re ]]; then
		version="${BASH_REMATCH[1]}"
	elif command -v go >/dev/null 2>&1; then
		local path
		path="$(command -v "$command" 2>/dev/null || true)"
		if [[ -n "$path" ]]; then
			out="$(GOTOOLCHAIN=local go version -m "$path" 2>/dev/null)" || true
			re=$'\n[[:space:]]*mod[[:space:]]+[^[:space:]]+[[:space:]]+(v[0-9][^[:space:]]*)'
			if [[ "$out" =~ $re ]]; then
				version="${BASH_REMATCH[1]}"
			fi
		fi
	fi
	printf '%s\n' "$version"
}

# Compares <command> against the pin for <tool>. Sets TOOL_PIN_EXPECTED and
# TOOL_PIN_FOUND (the reported version, empty when unreadable) and returns
#   0  the pinned version
#   1  another version, or a version it could not read
#   2  <command> does not resolve
# The comparison is exact once a leading "v" is dropped from both sides, so
# 2.12.20 is not 2.12.2.
tool_pin_check() {
	local tool="$1" command="$2"
	TOOL_PIN_EXPECTED="$(tool_pin_expected "$tool")"
	TOOL_PIN_FOUND=""
	if [[ -z "$command" ]] || ! command -v "$command" >/dev/null 2>&1; then
		return 2
	fi
	TOOL_PIN_FOUND="$(tool_pin_reported "$tool" "$command")"
	if [[ -n "$TOOL_PIN_FOUND" && "${TOOL_PIN_FOUND#v}" == "${TOOL_PIN_EXPECTED#v}" ]]; then
		return 0
	fi
	return 1
}

main() {
	if [[ $# -ne 2 ]]; then
		echo "usage: $0 <tool> <command>" >&2
		return 64
	fi
	local tool="$1" command="$2" root status=0 found where
	root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
	# shellcheck source=scripts/tool-versions.env
	. "$root/scripts/tool-versions.env"
	if ! tool_pin_expected "$tool" >/dev/null; then
		echo "tool-pins: no pin is defined for '$tool'" >&2
		return 64
	fi

	tool_pin_check "$tool" "$command" || status=$?
	case "$status" in
		0) return 0 ;;
		2)
			echo "$tool not installed; run: make dev-setup" >&2
			return 1
			;;
	esac

	found="${TOOL_PIN_FOUND:-an unreadable version}"
	where="$(command -v "$command")"
	if [[ "${ALLOW_UNPINNED_TOOLS:-}" == 1 ]]; then
		{
			echo "WARN: $tool at $where is $found, not the pinned $TOOL_PIN_EXPECTED."
			echo "      Continuing because ALLOW_UNPINNED_TOOLS=1: this is not the pinned"
			echo "      gate, so report its result as a run on an unpinned $tool."
		} >&2
		return 0
	fi
	{
		echo "FAIL: $tool at $where is $found; scripts/tool-versions.env pins $TOOL_PIN_EXPECTED."
		echo "      A different version is a different gate, so its findings would not"
		echo "      match CI's. Run 'make dev-setup' to install the pinned tools under"
		echo "      .agent-cache/bin, or re-run with ALLOW_UNPINNED_TOOLS=1 to accept"
		echo "      this $tool and report the run as unpinned."
	} >&2
	return 1
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	set -euo pipefail
	main "$@"
fi
