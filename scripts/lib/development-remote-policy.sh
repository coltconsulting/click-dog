#!/usr/bin/env bash

# Shared by doctor and its focused shell test. This helper intentionally emits
# remote names only; a remote URL can contain credentials and must not appear in
# diagnostic output.

is_official_public_repository_url() {
	local url authority host_port path port

	# GitHub treats the host, owner, and repository name case-insensitively.
	# tr keeps this compatible with the Bash 3.2 shipped by macOS.
	url="$(LC_ALL=C printf '%s' "$1" | tr '[:upper:]' '[:lower:]')"
	while [[ "$url" == */ ]]; do
		url="${url%/}"
	done

	case "$url" in
		http://* | https://* | ssh://* | git://*)
		# Split the URL at the first path slash, then take the destination
		# host after any userinfo. Userinfo may contain a colon, as in an
		# HTTPS token URL, so it must not be mistaken for a port.
		authority="${url#*://}"
		[[ "$authority" == */* ]] || return 1
		path="${authority#*/}"
		authority="${authority%%/*}"
		host_port="${authority##*@}"

		case "$host_port" in
			github.com | github.com. | www.github.com | www.github.com.)
				;;
			github.com:* | github.com.:* | www.github.com:* | www.github.com.:*)
				port="${host_port#*:}"
				case "$port" in
					*[!0-9]*) return 1 ;;
				esac
				;;
			*)
				return 1
				;;
		esac
		;;
	*://*)
		# Unknown URL schemes must not fall through to scp-like parsing.
		return 1
		;;
	*)
		# scp-like SSH syntax: [user@]github.com:owner/repository
		[[ "$url" == *:* ]] || return 1
		authority="${url%%:*}"
		path="${url#*:}"
		case "${authority##*@}" in
			github.com | github.com. | www.github.com | www.github.com.)
				;;
			*)
				return 1
				;;
		esac
		;;
	esac

	# GitHub redirects equivalent paths with redundant separators. Normalize
	# separators only after validating the destination host, and leave all
	# other path syntax untouched so sibling or nested repositories stay distinct.
	while [[ "$path" == /* ]]; do
		path="${path#/}"
	done
	while [[ "$path" == *//* ]]; do
		path="${path%%//*}/${path#*//}"
	done

	case "$path" in
		coltconsulting/click-dog | coltconsulting/click-dog.git)
			return 0
			;;
	esac
	return 1
}

push_enabled_public_remotes() {
	local root="$1" remote push_url
	while IFS= read -r remote; do
		while IFS= read -r push_url; do
			if is_official_public_repository_url "$push_url"; then
				printf '%s\n' "$remote"
				break
			fi
		done < <(git -C "$root" remote get-url --push --all "$remote" 2>/dev/null || true)
	done < <(git -C "$root" remote 2>/dev/null || true)
}
