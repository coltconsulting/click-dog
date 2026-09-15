#!/usr/bin/env bash
# Fail early, and legibly, when a host port the integration cluster needs is
# already held by a container this compose project does not own.
#
# `docker compose down --remove-orphans` reclaims only containers LABELLED with
# this project, so a stray `docker run` — or scratch left behind by another tool
# — keeps its port binding and `up` dies with a raw daemon error that names an
# endpoint id rather than the offender:
#
#   Error response from daemon: failed to set up container networking: driver
#   failed programming external connectivity on endpoint clickhouse-int-1
#   (2923666d...): Bind for 0.0.0.0:18123 failed: port is already allocated
#
# This turns that into the container name and a command that frees it.
set -euo pipefail

# Host ports the compose file publishes as `PORT/PROTO`, one per line,
# deduplicated. The file writes them as `- "HOST:CONTAINER"   # comment` with an
# optional `/udp` suffix, so the leading list marker, the optional quotes, and
# that suffix are the whole grammar we need. Compose defaults to tcp when the
# suffix is absent, and so do we.
integration_published_ports() {
	local compose_file="$1"
	sed -nE 's#^[[:space:]]*-[[:space:]]*"?([0-9]+):[0-9]+(/(udp|tcp))?"?.*#\1 \3#p' "$compose_file" |
		awk '{ print $1 "/" ($2 == "" ? "tcp" : $2) }' | sort -u
}

# Names of running containers outside `project` that publish any of `ports`.
# Reads `docker ps` output on stdin as NAME|PROJECT|PORTMAP so the matching
# stays testable without a daemon.
#
# The delimiter is "|", not a tab: IFS treats runs of *whitespace* as one
# separator, so an unlabelled container's empty project field would collapse and
# shift the port map into the wrong variable — silently blinding this check to
# precisely the stray `docker run` it exists to catch. None of the three fields
# can contain "|".
integration_port_holders() {
	local project="$1" ports="$2"
	local name container_project portmap spec port proto re
	while IFS='|' read -r name container_project portmap; do
		[ -n "$name" ] || continue
		# Containers this project owns are torn down by the caller's `down`;
		# anything still holding a port is foreign by definition.
		if [ "$container_project" = "$project" ]; then
			continue
		fi
		for spec in $ports; do
			port="${spec%%/*}"
			proto="${spec##*/}"
			# Matched as one contiguous mapping, `:PORT->CONTAINERPORT/PROTO`:
			#   - the ":" anchor stops ":118123->" matching port 18123;
			#   - pinning the protocol to the SAME mapping stops a UDP binding on
			#     18123 being reported as holding TCP 18123, which it does not.
			#     A looser "contains :PORT-> and contains /tcp" would match across
			#     two different mappings in the same comma-separated list.
			re=":${port}->[0-9]+/${proto}"
			if [[ "$portmap" =~ $re ]]; then
				printf '%s|%s\n' "$spec" "$name"
			fi
		done
	done
}

main() {
	local compose_file="${1:-testing/docker-compose.integration.yml}"
	local project="${2:-testing}"

	if ! command -v docker >/dev/null 2>&1; then
		echo "integration preflight: docker not found; skipping the port check." >&2
		return 0
	fi
	if [ ! -f "$compose_file" ]; then
		echo "integration preflight: $compose_file not found; skipping the port check." >&2
		return 0
	fi

	local ports ps_out conflicts
	ports="$(integration_published_ports "$compose_file")"
	[ -n "$ports" ] || return 0

	# A daemon that cannot be reached is not this check's business to report:
	# `up` will fail with its own, clearer message a moment later.
	ps_out="$(docker ps --format '{{.Names}}|{{.Label "com.docker.compose.project"}}|{{.Ports}}' 2>/dev/null)" || return 0

	conflicts="$(printf '%s\n' "$ps_out" | integration_port_holders "$project" "$ports")"
	[ -n "$conflicts" ] || return 0

	local names
	names="$(printf '%s\n' "$conflicts" | cut -d'|' -f2 | sort -u | tr '\n' ' ')"
	names="${names% }"

	{
		echo ""
		echo "ERROR: the integration cluster cannot start — host ports it needs are already bound:"
		echo ""
		printf '%s\n' "$conflicts" | while IFS='|' read -r port name; do
			printf '  %-6s held by container %s\n' "$port" "$name"
		done
		echo ""
		echo "These containers are not part of the '$project' compose project, so the"
		echo "'docker compose down --remove-orphans' above does not reclaim them."
		echo ""
		echo "Free them (reversible — 'docker start' brings them back):"
		echo "  docker stop $names"
		echo ""
	} >&2
	return 1
}

if [ "${BASH_SOURCE[0]}" = "$0" ]; then
	main "$@"
fi
