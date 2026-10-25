#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHECK="$ROOT/scripts/lib/workflow-pins.sh"

# shellcheck source=scripts/tool-versions.env
. "$ROOT/scripts/tool-versions.env"

TMP_DIR="$(mktemp -d)"
trap 'rm -rf "$TMP_DIR"' EXIT
TREE="$TMP_DIR/tree"
CI="$TREE/.github/workflows/ci.yml"

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

# ── this checkout passes, and every read in it was checked ───────
status=0
out="$(bash "$CHECK" 2>&1)" || status=$?
[ "$status" -eq 0 ] || fail "this checkout's workflows do not read their pins cleanly:
$out"
# The parser skips full-line YAML comments, so the naive count does too. A
# parser that lost a step or a read would otherwise pass vacuously.
workflow_lines() {
	find "$ROOT/.github/workflows" -name '*.yml' -o -name '*.yaml' | sort | xargs cat |
		grep -v -E '^[[:space:]]*#'
}
reads="$(workflow_lines | grep -o 'steps\.pins\.outputs\.[A-Za-z0-9_-]*' | wc -l | tr -d ' ')"
steps="$(workflow_lines | grep -c -E '^[[:space:]]*id:[[:space:]]*pins[[:space:]]*$')"
installs="$(workflow_lines | grep -c -E '^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*(actions/setup-go|golangci/golangci-lint-action)@')"
[ "$steps" -gt 0 ] || fail "no workflow has a Read tool pins step to check"
[ "$installs" -gt 0 ] || fail "no workflow installs Go or golangci-lint to check"
assert_contains "every pins step and read is checked" "$out" "$reads reads of $steps Read tool pins steps resolve"
assert_contains "every Go and golangci-lint install is checked" "$out" "; $installs Go and golangci-lint installs use their pins"

# ── a mutated copy of what the check reads, one case at a time ───
# Only ci.yml is mutated: it ships in the public tree too, where this suite
# also runs and the internal-only workflows are absent.
PRISTINE="$TMP_DIR/pristine"
mkdir -p "$PRISTINE/.github" "$PRISTINE/scripts"
cp -R "$ROOT/.github/workflows" "$PRISTINE/.github/"
cp "$ROOT/scripts/tool-versions.env" "$PRISTINE/scripts/"
cp "$ROOT/go.mod" "$PRISTINE/"
fresh() {
	rm -rf "$TREE"
	cp -R "$PRISTINE" "$TREE"
}

# swap <file> <old> <new>: replaces the first <old>, a fixed string, with
# <new>. A missing <old> fails the suite, so a case cannot pass by mutating
# nothing after the workflows change.
swap() {
	OLD="$2" NEW="$3" awk '
		!done && (i = index($0, ENVIRON["OLD"])) {
			$0 = substr($0, 1, i - 1) ENVIRON["NEW"] substr($0, i + length(ENVIRON["OLD"]))
			done = 1
		}
		{ print }
		END { exit !done }' "$1" > "$1.new" || fail "swap: '$2' is not in $1"
	mv "$1.new" "$1"
}

# line_of <text>: the line of the first <text> in the mutated ci.yml.
line_of() {
	grep -n -F -m 1 -- "$1" "$CI" | cut -d: -f1
}

# refused <case> <want>: the check fails the copy and its output, left in
# OUT, names <want>.
refused() {
	local status=0
	OUT="$(bash "$CHECK" "$TREE" 2>&1)" || status=$?
	assert_eq "$1 fails the check" "$status" 1
	assert_contains "$1 is named" "$OUT" "$2"
}

# accepted <case>: the check passes the copy.
accepted() {
	local status=0 out
	out="$(bash "$CHECK" "$TREE" 2>&1)" || status=$?
	[ "$status" -eq 0 ] || fail "$1 (the check failed: $out)"
}

# read_order: "<reading step> <pins step>" from the ordering failure in OUT.
read_order() {
	local re='in step ([0-9]+), but its Read tool pins step \(line [0-9]+\) is step ([0-9]+); move that step first'
	[[ "$OUT" =~ $re ]] || fail "no ordering failure in: $OUT"
	echo "${BASH_REMATCH[1]} ${BASH_REMATCH[2]}"
}

fresh
accepted "an unmodified copy"

# ── 1. a pinned version literal in a workflow ────────────────────
PIN_NAMES="$(sed -n -E 's/^([A-Z][A-Z0-9_]*_VERSION)=.*/\1/p' "$ROOT/scripts/tool-versions.env")"
for name in GO_VERSION GOLANGCI_LINT_VERSION GOVULNCHECK_VERSION ACTIONLINT_VERSION SHELLCHECK_VERSION WRANGLER_VERSION; do
	case " $(echo "$PIN_NAMES" | tr '\n' ' ') " in
		*" $name "*) ;;
		*) fail "scripts/tool-versions.env no longer defines $name" ;;
	esac
done
# Every pin is checked, not only the ones CI installs today: one literal of
# each, on its own line, and each must be named.
fresh
for name in $PIN_NAMES; do
	printf '# installs tool@%s\n' "${!name}" >> "$CI"
done
refused "pinned version literals" "the GO_VERSION pin"
for name in $PIN_NAMES; do
	assert_contains "a $name literal is named" "$OUT" "names ${!name}, the $name pin"
done

fresh
swap "$CI" '${{ steps.pins.outputs.go_version }}' "'$GO_VERSION'"
refused "a go-version literal" \
	"ci.yml:$(line_of "go-version: '$GO_VERSION'"): names $GO_VERSION, the GO_VERSION pin"

fresh
swap "$CI" '${{ steps.pins.outputs.golangci_lint_version }}' "${GOLANGCI_LINT_VERSION#v}"
refused "a golangci-lint literal without its v" "names $GOLANGCI_LINT_VERSION, the GOLANGCI_LINT_VERSION pin"

fresh
printf '# ends a sentence with go%s.\n' "$GO_VERSION" >> "$CI"
refused "a Go version in a comment" "the GO_VERSION pin"

# A version token, not a substring: these contain the Go pin but are not it.
fresh
printf '# not the pin: %s0, 1%s, %s.1\n' "$GO_VERSION" "$GO_VERSION" "$GO_VERSION" >> "$CI"
accepted "a longer version that contains the Go pin"

# Dependabot rewrites every `uses:` ref and its "# vX.Y.Z" comment to the
# action's latest release, which can be any number, a pin's included. Each
# pin lands on a `uses:` line as a SHA ref's comment; the lines left over
# carry pins as tag refs.
fresh
PINS_LIST="$(for name in $PIN_NAMES; do echo "${!name#v}"; done)" awk '
	BEGIN { n = split(ENVIRON["PINS_LIST"], pin, "\n") }
	/^[ \t]*(-[ \t]+)?uses:[^@]*@/ {
		at = index($0, "@")
		if (done < n)
			$0 = substr($0, 1, at) "0123456789abcdef0123456789abcdef01234567 # v" pin[done + 1]
		else
			$0 = substr($0, 1, at) "v" pin[done % n + 1]
		done++
	}
	{ print }
	END { exit done <= n }' "$CI" > "$CI.new" || fail "ci.yml has too few uses: lines to carry every pin"
mv "$CI.new" "$CI"
grep -q -F "@0123456789abcdef0123456789abcdef01234567 # v$OPENSPEC_VERSION" "$CI" ||
	fail "no Dependabot-style comment names the OpenSpec pin"
accepted "Dependabot-style action versions that equal a pin"

# A docker:// image names the tool it runs, so its tag is still a literal.
fresh
printf '      - uses: docker://golang:%s\n' "$GO_VERSION" >> "$CI"
refused "a pinned Go version in a docker:// image" "names $GO_VERSION, the GO_VERSION pin"

# ── 2. a Go or golangci-lint install that does not read its pin ──
# A stale literal names no current pin, so the literal scan cannot see it.
# GitHub resolves owner/repo whatever their case, so the install is found.
fresh
swap "$CI" 'actions/setup-go@' 'Actions/Setup-Go@'
swap "$CI" '${{ steps.pins.outputs.go_version }}' "'1.26.9'"
refused "a stale go-version under a differently cased setup-go" "its go-version is 1.26.9"
for stale in "'1.26.9'" "'1.26'" stable '${{ matrix.go }}'; do
	fresh
	swap "$CI" '${{ steps.pins.outputs.go_version }}' "$stale"
	refused "a go-version of $stale" \
		"ci.yml:$(line_of "go-version: $stale"): job 'test': its go-version is ${stale//\'/}; set it to \${{ steps.pins.outputs.<name> }}, written from \$GO_VERSION"
done

fresh
swap "$CI" 'go-version: ${{ steps.pins.outputs.go_version }}' 'go-version-file: go.mod'
refused "a setup-go step with no go-version" "job 'test': its go-version is missing"

fresh
swap "$CI" '${{ steps.pins.outputs.golangci_lint_version }}' 'v2.0.0'
refused "a stale golangci-lint version" \
	"job 'lint': its version is v2.0.0; set it to \${{ steps.pins.outputs.<name> }}, written from \$GOLANGCI_LINT_VERSION"

# A read that resolves, but to something other than the pin.
fresh
swap "$CI" 'echo "go_version=$GO_VERSION"' 'echo "go_version=1.26.9"'
refused "a pins step that writes a stale Go" \
	"job 'test': its go-version reads steps.pins.outputs.go_version, which is 1.26.9, but scripts/tool-versions.env pins GO_VERSION=$GO_VERSION"

fresh
swap "$CI" '${{ steps.pins.outputs.golangci_lint_version }}' '${{ steps.pins.outputs.go_version }}'
refused "golangci-lint installed at the Go pin" \
	"job 'lint': its version reads steps.pins.outputs.go_version, which is $GO_VERSION, but scripts/tool-versions.env pins GOLANGCI_LINT_VERSION=$GOLANGCI_LINT_VERSION"

# ── 3. a pins script that would not run in CI as it runs here ────
for key in 'shell: sh' 'working-directory: scripts'; do
	fresh
	swap "$CI" '        id: pins' "        id: pins
        $key"
	refused "a pins step with $key" \
		"ci.yml:$(line_of "        $key"): job 'test': its Read tool pins step sets ${key%%:*}:"
done

fresh
swap "$CI" '  lint:' '  lint:
    defaults:
      run:
        shell: sh'
refused "a job defaults.run over a pins step" \
	"ci.yml:$(($(line_of '    defaults:') + 1)): defaults.run applies to a Read tool pins step"

# After jobs:, which YAML allows: the parser cannot rely on seeing it first.
fresh
printf 'defaults:\n  run:\n    working-directory: scripts\n' >> "$CI"
refused "a workflow defaults.run" \
	"ci.yml:$(($(wc -l < "$CI") - 1)): defaults.run applies to a Read tool pins step"

# ── 4. a read its job's pins step does not satisfy ───────────────
# actionlint passes every one of these: it cannot see what a run script
# writes to $GITHUB_OUTPUT.
fresh
swap "$CI" 'steps.pins.outputs.golangci_lint_version' 'steps.pins.outputs.golangci_version'
refused "a mistyped output name" \
	"ci.yml:$(line_of 'steps.pins.outputs.golangci_version'): job 'lint' reads steps.pins.outputs.golangci_version, which its Read tool pins step"
assert_contains "the mistyped name is not written" "$OUT" "does not write"

fresh
swap "$CI" 'echo "golangci_lint_version=$GOLANGCI_LINT_VERSION" >> "$GITHUB_OUTPUT"' ''
refused "an output the pins step stopped writing" \
	"job 'lint' reads steps.pins.outputs.golangci_lint_version, which its Read tool pins step"
assert_contains "the dropped output is not written" "$OUT" "does not write"

fresh
swap "$CI" '$GOLANGCI_LINT_VERSION"' '$GOLANGCI_LINT_VERSON"'
refused "an output written from a mistyped variable" \
	"job 'lint' reads steps.pins.outputs.golangci_lint_version, which its Read tool pins step"
assert_contains "the mistyped variable writes empty" "$OUT" "writes empty"

# The script runs in an empty environment, so a pins step that never sources
# the pin file cannot borrow the caller's copy of a pin.
fresh
swap "$CI" 'source scripts/tool-versions.env' ''
(
	export GO_VERSION
	refused "a pins step that does not source the pin file" \
		"job 'test' reads steps.pins.outputs.go_version, which its Read tool pins step"
	assert_contains "the unsourced pin writes empty" "$OUT" "writes empty"
)

fresh
swap "$CI" 'source scripts/tool-versions.env' 'source scripts/tool-versions.missing'
refused "a pins script that fails" "job 'test': its Read tool pins script exits 1"

fresh
swap "$CI" 'echo "go_version=$GO_VERSION"' 'echo "go_version=${{ env.GO_VERSION }}"'
refused "a pins script that needs CI to evaluate it" "uses a \${{ }} expression"

fresh
swap "$CI" '        id: pins' '        id: versions'
refused "a job with no pins step" "job 'test' reads steps.pins.outputs.go_version but has no step with id: pins"

fresh
swap "$CI" '      - name: Read tool pins' '      - name: Read a pin too early
        run: echo "${{ steps.pins.outputs.go_version }}"

      - name: Read tool pins'
refused "a read before the pins step runs" "job 'test' reads steps.pins.outputs.go_version in step"
read -r reader pins <<< "$(read_order)"
[ "$reader" -lt "$pins" ] || fail "the early read is reported as step $reader, the pins step as $pins"

fresh
swap "$CI" '        id: pins' '        id: pins
        env:
          GO: ${{ steps.pins.outputs.go_version }}'
refused "a read in the pins step itself" "job 'test' reads steps.pins.outputs.go_version in step"
read -r reader pins <<< "$(read_order)"
assert_eq "the pins step's own read is in the pins step" "$reader" "$pins"

# A run script is content, not YAML: CI evaluates the expressions in its shell
# comments too, so a read there counts.
fresh
swap "$CI" '          run_integration=false' '          # Go ${{ steps.pins.outputs.go_version }}
          run_integration=false'
refused "a read in a run script's comment" "job 'test' reads steps.pins.outputs.go_version in step"
read -r reader pins <<< "$(read_order)"
[ "$reader" -lt "$pins" ] || fail "the comment's read is reported as step $reader, the pins step as $pins"

# Job outputs are read after every step, so their order does not matter.
fresh
swap "$CI" '  lint:' '  lint:
    outputs:
      go_version: ${{ steps.pins.outputs.go_version }}'
accepted "a job output read from the pins step"

fresh
swap "$CI" '  lint:' '  lint:
    outputs:
      go_version: ${{ steps.pins.outputs.go }}'
refused "a mistyped job output read" "job 'lint' reads steps.pins.outputs.go, which its Read tool pins step"

fresh
swap "$CI" 'steps.pins.outputs.go_version' "steps.pins.outputs['go_version']"
refused "a bracketed read" "reads steps.pins.outputs without a .<name> after it"

# ── 5. go.mod asks for another Go ────────────────────────────────
# A pin bump that forgets go.mod: the workflows follow the pin file, go.mod
# does not.
fresh
swap "$TREE/scripts/tool-versions.env" "GO_VERSION=$GO_VERSION" "GO_VERSION=${GO_VERSION}1"
refused "a Go bump without go.mod" \
	"go.mod: toolchain is go$GO_VERSION; scripts/tool-versions.env pins GO_VERSION=${GO_VERSION}1"
assert_eq "go.mod is the only problem with the bump" "$(grep -c '^FAIL: ' <<< "$OUT")" 1

fresh
swap "$TREE/go.mod" "toolchain go$GO_VERSION" ''
refused "a go.mod without a toolchain line" "go.mod: toolchain is missing"

echo "workflow pin tests: PASS"
