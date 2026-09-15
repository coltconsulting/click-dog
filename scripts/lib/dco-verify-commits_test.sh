#!/usr/bin/env bash
# Tests for dco_verify_commits, the check .github/workflows/dco.yml runs.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/dco-verify-commits.sh
. "$ROOT/scripts/lib/dco-verify-commits.sh"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-dco-commits-test.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

PASS=0
FAIL=0
SIGNOFF="Signed-off-by: Jane Doe <jane@example.com>"
CASE=0

# new_repo creates an isolated repo with one signed root commit and cd's into
# it. A repo per case keeps ranges unambiguous -- no branch or orphan juggling
# to get subtly wrong, which is how an earlier draft of this suite reported
# green against genuinely unsigned fixtures.
new_repo() {
	CASE=$((CASE + 1))
	local dir="$TMP_DIR/case$CASE"
	git init -q "$dir"
	cd "$dir"
	git config user.name "Jane Doe"
	git config user.email "jane@example.com"
	echo base >base.txt
	git add -A
	git commit -q -m "$(printf 'base\n\n%s\n' "$SIGNOFF")"
}

commit_signed() {
	echo "$1" >>"$1.txt"
	git add -A
	git commit -q -m "$(printf '%s\n\n%s\n' "$1" "$SIGNOFF")"
}

commit_unsigned() {
	echo "$1" >>"$1.txt"
	git add -A
	git commit -q -m "$1"
}

expect() {
	local want="$1" name="$2" base="$3" head="$4" got
	if dco_verify_commits "$base" "$head" >/dev/null 2>&1; then got=pass; else got=fail; fi
	if [ "$got" = "$want" ]; then
		echo "  PASS: $name (=> $got)"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $name -- got $got, want $want" >&2
		FAIL=$((FAIL + 1))
	fi
}

note() {
	if [ "$1" = ok ]; then
		echo "  PASS: $2"
		PASS=$((PASS + 1))
	else
		echo "  FAIL: $2" >&2
		FAIL=$((FAIL + 1))
	fi
}

echo "dco_verify_commits"

# --- linear history --------------------------------------------------------
new_repo; BASE=$(git rev-parse HEAD)
commit_signed one
expect pass "linear, signed" "$BASE" "$(git rev-parse HEAD)"

new_repo; BASE=$(git rev-parse HEAD)
commit_unsigned one
expect fail "linear, unsigned" "$BASE" "$(git rev-parse HEAD)"

new_repo; BASE=$(git rev-parse HEAD)
commit_signed one
commit_unsigned two
commit_signed three
expect fail "unsigned commit in the middle" "$BASE" "$(git rev-parse HEAD)"

# --- the regression: a merge introducing a change in neither parent --------
# build_evil_merge leaves HEAD on a merge whose tree contains merge-only.txt.
# $1 is the merge commit message, so the same shape can be tested signed and
# unsigned.
build_evil_merge() {
	new_repo
	BASE=$(git rev-parse HEAD)
	git checkout -q -b upstream
	commit_signed upstream
	git checkout -q -b feature "$BASE"
	commit_signed feature
	git merge --no-ff --no-commit upstream >/dev/null 2>&1 || true
	echo evil >merge-only.txt
	git add -A
	git commit -q --no-edit -m "$1"
}

build_evil_merge "Merge upstream into feature"
HEAD_SHA=$(git rev-parse HEAD)
if git show --stat --format= "$HEAD_SHA" | grep -q "merge-only.txt"; then
	note ok "fixture: merge introduces a file present in neither parent"
else
	note bad "fixture did not produce a merge-only change"
fi
expect fail "unsigned merge carrying a merge-only change" "$BASE" "$HEAD_SHA"

build_evil_merge "$(printf 'Merge upstream into feature\n\n%s\n' "$SIGNOFF")"
expect pass "signed merge carrying a merge-only change" "$BASE" "$(git rev-parse HEAD)"

# --- identity must match the commit author ---------------------------------
new_repo; BASE=$(git rev-parse HEAD)
echo x >>base.txt; git add -A
git commit -q -m "$(printf 'wrong identity\n\nSigned-off-by: Someone Else <other@example.com>\n')"
expect fail "trailer identity does not match author" "$BASE" "$(git rev-parse HEAD)"

# --- a trailer is a trailer, not prose -------------------------------------
new_repo; BASE=$(git rev-parse HEAD)
echo x >>base.txt; git add -A
git commit -q -m "$(printf 'prose\n\nI would write %s here.\n\nBut it is prose.\n' "$SIGNOFF")"
expect fail "Signed-off-by in prose is not a trailer" "$BASE" "$(git rev-parse HEAD)"

# --- empty range -----------------------------------------------------------
new_repo; BASE=$(git rev-parse HEAD)
expect pass "empty range" "$BASE" "$BASE"

# --- merges are flagged as an import hazard, without failing ---------------
build_evil_merge "$(printf 'Merge upstream into feature\n\n%s\n' "$SIGNOFF")"
WARN="$(dco_warn_merges "$BASE" "$(git rev-parse HEAD)" 2>&1 || true)"
case "$WARN" in
*"omits merges"*) note ok "merges reported as an import hazard" ;;
*) note bad "merge hazard not reported (got: ${WARN:-<empty>})" ;;
esac

new_repo; BASE=$(git rev-parse HEAD)
commit_signed one
WARN="$(dco_warn_merges "$BASE" "$(git rev-parse HEAD)" 2>&1 || true)"
if [ -z "$WARN" ]; then
	note ok "linear history produces no merge warning"
else
	note bad "linear history wrongly warned about merges (got: $WARN)"
fi

# --- the gate must not be rewritable by the PR it judges --------------------
# A contributor who can supply the verifier the workflow runs can make their
# own unsigned PR pass. This asserts the threat is real (a tampered copy does
# accept an unsigned commit) so the workflow configuration below is understood
# as load-bearing, not decoration.
new_repo; BASE=$(git rev-parse HEAD)
commit_unsigned one
HEAD_SHA=$(git rev-parse HEAD)

TAMPERED="$TMP_DIR/tampered-verifier.sh"
cat "$ROOT/scripts/lib/dco-verify-commits.sh" >"$TAMPERED"
cat >>"$TAMPERED" <<'TAMPER'
# What a malicious PR would append to the file the workflow sources.
dco_verify_commits() { return 0; }
TAMPER

# $TAMPERED is written above at a mktemp path, so there is no fixed file to
# resolve -- and resolving it would be wrong anyway: this fixture exists
# precisely to be the untrusted copy. Sourced in a subshell so the override
# cannot leak into the assertions that follow.
# shellcheck source=/dev/null
if (. "$TAMPERED"; dco_verify_commits "$BASE" "$HEAD_SHA" >/dev/null 2>&1); then
	note ok "a tampered verifier does accept an unsigned commit (threat is real)"
else
	note bad "tampered verifier unexpectedly rejected -- fixture is not exercising the threat"
fi
expect fail "the trusted verifier rejects that same commit" "$BASE" "$HEAD_SHA"

# --- workflow configuration that keeps the verifier trusted ----------------
# These are the properties that stop the above from being exploitable. They are
# asserted here because a future edit "simplifying" the trigger back to
# pull_request would silently reintroduce it: under pull_request both the
# workflow file and the tree come from the PR.
WF="$ROOT/.github/workflows/dco.yml"

if grep -qE '^[[:space:]]+pull_request_target:' "$WF"; then
	note ok "workflow triggers on pull_request_target (workflow + tree come from base)"
else
	note bad "workflow no longer triggers on pull_request_target"
fi

if grep -qE '^[[:space:]]+pull_request:' "$WF"; then
	note bad "workflow triggers on pull_request -- the PR would supply its own verifier"
else
	note ok "workflow does not trigger on plain pull_request"
fi

# actions/checkout with no `ref:` takes the base branch under
# pull_request_target. Any ref: naming the PR would put contributor files in
# the workspace the verifier is sourced from.
if grep -qE '^[[:space:]]+ref:' "$WF"; then
	note bad "workflow checkout pins a ref -- must default to the base branch"
else
	note ok "workflow checkout takes the base branch (no ref: override)"
fi

if grep -qE '^[[:space:]]*contents:[[:space:]]*read' "$WF"; then
	note ok "workflow token is read-only"
else
	note bad "workflow does not restrict the token to contents: read"
fi

echo
if [ "$FAIL" -ne 0 ]; then
	echo "dco-verify-commits: $PASS passed, $FAIL failed" >&2
	exit 1
fi
echo "dco-verify-commits: $PASS passed, 0 failed"
