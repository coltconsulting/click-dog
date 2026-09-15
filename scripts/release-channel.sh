#!/usr/bin/env bash
set -euo pipefail

repository="${1:?usage: release-channel.sh <repository> <event> <ref-name>}"
event="${2:?usage: release-channel.sh <repository> <event> <ref-name>}"
ref_name="${3:-}"

if [ "$event" = "workflow_dispatch" ]; then
	printf '%s\n' none
	exit 0
fi
if [ "$event" != "push" ]; then
	echo "error: unsupported release event '$event'" >&2
	exit 2
fi

# Three channels:
#   alpha  internal only — private signed prerelease, never mirrored
#   beta   public prerelease — gate-only internally, built on public
#   ga     public production — gate-only internally, built on public
#
# beta and ga are classified the same way on each side on purpose: anything
# destined for the public repository is a gate internally and an artifact build
# publicly. alpha is the only channel that produces artifacts internally, and
# the only one the public side refuses outright.
ga_re='^v[0-9]{2}\.(0[1-9]|1[0-2])\.[1-9][0-9]*$'
alpha_re='^v[0-9]{2}\.(0[1-9]|1[0-2])\.[1-9][0-9]*-alpha\.[1-9][0-9]*$'
beta_re='^v[0-9]{2}\.(0[1-9]|1[0-2])\.[1-9][0-9]*-beta\.[1-9][0-9]*$'

case "$repository" in
	coltconsulting/click-dog-internal)
		if [[ "$ref_name" =~ $ga_re ]] || [[ "$ref_name" =~ $beta_re ]]; then
			printf '%s\n' gate_only
		elif [[ "$ref_name" =~ $alpha_re ]]; then
			printf '%s\n' internal_alpha
		else
			echo "error: internal release tag '$ref_name' is malformed" >&2
			exit 2
		fi
		;;
	coltconsulting/click-dog)
		if [[ "$ref_name" =~ $ga_re ]]; then
			printf '%s\n' public_ga
		elif [[ "$ref_name" =~ $beta_re ]]; then
			printf '%s\n' public_beta
		elif [[ "$ref_name" =~ $alpha_re ]]; then
			echo "error: alpha release tag '$ref_name' is internal-only and must never be published" >&2
			exit 2
		else
			echo "error: public release tag '$ref_name' is malformed" >&2
			exit 2
		fi
		;;
	*)
		echo "error: release workflow is not authorized for repository '$repository'" >&2
		exit 2
		;;
esac
