#!/usr/bin/env bash
#
# build-export-tree.sh — materialize the tree the public repo receives.
#
# Sourced, never executed. Defines one function, build_export_tree, used by
# every gate that has to look at the public tree: verify-public-boundary.sh,
# test-public-release.sh, and check-public.sh.
#
# The snapshot comes from a TEMPORARY git index, so uncommitted work is
# included and the real index is never touched — the gates run on a dirty tree.
# `git archive` then applies .gitattributes export-ignore, which is what makes
# the result the public tree rather than the internal one.
#
# Usage: build_export_tree <repo-root> <dest-dir> [<manifest-file>]
#
#   repo-root      checkout to snapshot
#   dest-dir       directory the tree is extracted into (created if absent)
#   manifest-file  optional; receives the archive's `tar -t` path listing
#
# The git environment redirection lives inside a subshell, so GIT_INDEX_FILE
# and friends never escape into the caller — a caller that runs git afterwards
# (test-public-release.sh commits inside the extracted tree) gets an ordinary
# git, not one still pointed at this function's scratch object store.

build_export_tree() {
	local root="$1" dest="$2" manifest="${3:-}"
	local scratch

	scratch="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-export-tree.XXXXXX")"

	(
		# Keep a subshell-owned copy: function locals are out of scope by the
		# time an EXIT trap runs after errexit aborts the function.
		export_tree_scratch="$scratch"
		trap 'rm -rf "$export_tree_scratch"' EXIT

		# Resolve the real object store BEFORE redirecting git's environment —
		# exporting GIT_OBJECT_DIRECTORY first makes this rev-parse fail.
		local source_objects tree
		source_objects="$(git -C "$root" rev-parse --path-format=absolute --git-common-dir)/objects"

		mkdir -p "$scratch/objects" "$dest"
		export GIT_INDEX_FILE="$scratch/index"
		export GIT_OBJECT_DIRECTORY="$scratch/objects"
		export GIT_ALTERNATE_OBJECT_DIRECTORIES="$source_objects"

		git -C "$root" read-tree HEAD
		git -C "$root" add -A -- .
		tree="$(git -C "$root" write-tree)"

		if [ -n "$manifest" ]; then
			git -C "$root" archive "$tree" | tar -tf - > "$manifest"
		fi
		git -C "$root" archive "$tree" | tar -xf - -C "$dest"
	)
}
