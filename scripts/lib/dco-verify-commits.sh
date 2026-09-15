#!/usr/bin/env bash
# Verify DCO sign-off on every commit in a revision range.
#
# Published: .github/workflows/dco.yml runs this on the public repo, so it must
# reach the public archive. The mbox counterpart (scripts/lib/dco-verify-patch.sh)
# is internal-only because only the import path consumes a .patch.

# dco_verify_commits <base-sha> <head-sha>
#
# Exits non-zero, listing offenders, unless every commit in base..head carries a
# Signed-off-by trailer whose identity equals that commit's own author.
#
# Merge commits are NOT exempt. CONTRIBUTING.md requires a sign-off on every
# commit, and a merge is not necessarily content-free: a conflict resolution or
# an "evil merge" can introduce changes present in neither parent, which no
# parent's sign-off covers. Skipping merges let an unsigned contribution through.
#
# %(trailers) parses only a real trailer block, so a "Signed-off-by" written in
# the prose of a message body does not count.
dco_verify_commits() {
	local base="$1" head="$2"
	local rc=0 sha author subject

	while IFS= read -r sha; do
		[ -n "$sha" ] || continue
		author="$(git show -s --format='%an <%ae>' "$sha")"
		subject="$(git show -s --format='%s' "$sha")"
		if git show -s --format='%(trailers:key=Signed-off-by,valueonly)' "$sha" |
			grep -qxF "$author"; then
			continue
		fi
		rc=1
		echo "  unsigned: ${sha:0:12} ${subject}" >&2
		echo "            want 'Signed-off-by: ${author}'" >&2
	done < <(git rev-list "$base".."$head")

	return "$rc"
}

# dco_warn_merges <base-sha> <head-sha>
#
# A PR is imported with `git am` over the GitHub .patch, and that patch is
# format-patch output, which omits merge commits entirely. Anything a merge
# introduced that is in neither parent is therefore silently dropped on import:
# the tree ends up different from the PR that was reviewed. Signing the merge
# satisfies the DCO but does not fix that, so say so separately. Advisory only.
dco_warn_merges() {
	local base="$1" head="$2" merges
	merges="$(git rev-list --merges "$base".."$head")"
	[ -n "$merges" ] || return 0
	echo "note: this branch contains merge commits:" >&2
	echo "$merges" | while IFS= read -r sha; do
		[ -n "$sha" ] && echo "  $(git show -s --format='%h %s' "$sha")" >&2
	done
	echo "      Importing uses 'git am' over the .patch, which omits merges, so" >&2
	echo "      any change a merge introduced that is in neither parent is lost." >&2
	echo "      Rebase onto the base branch instead of merging into your branch." >&2
	return 0
}
