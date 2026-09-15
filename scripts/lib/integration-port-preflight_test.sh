#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
HELPER="$ROOT/scripts/lib/integration-port-preflight.sh"
COMPOSE="$ROOT/testing/docker-compose.integration.yml"

# shellcheck source=scripts/lib/integration-port-preflight.sh
. "$HELPER"

fail() {
	echo "FAIL: $1" >&2
	exit 1
}

assert_eq() {
	[ "$2" = "$3" ] || fail "$1 (want '$3', got '$2')"
}

# ── the parser reads the real compose file, not a fixture ────────
ports="$(integration_published_ports "$COMPOSE")"
# Every entry in this compose file omits the protocol suffix, so all of them must
# come back defaulted to tcp — the same default Compose itself applies.
for want in 18123/tcp 19000/tcp 14317/tcp 13133/tcp 19183/tcp; do
	printf '%s\n' "$ports" | grep -qx "$want" || fail "published ports missing $want"
done
# Container-side ports must never leak into the host list: 8123 is published as
# 18123/18124/18125 and 9000 as 19000-19002, so a naive parse shows up here.
for unwanted in 8123/tcp 9000/tcp 8686/tcp 9090/tcp; do
	printf '%s\n' "$ports" | grep -qx "$unwanted" && fail "container port $unwanted parsed as a host port"
done
printf '%s\n' "$ports" | grep -qv '/' && fail "a published port came back without a protocol"
assert_eq "ports are deduplicated" "$(printf '%s\n' "$ports" | sort -u | wc -l | tr -d ' ')" "$(printf '%s\n' "$ports" | wc -l | tr -d ' ')"

# ── the matcher ignores this project and reports foreign holders ─
ps_own=$(printf 'clickhouse-int-1|testing|0.0.0.0:18123->8123/tcp, [::]:18123->8123/tcp\n')
assert_eq "a container in this project is not a conflict" \
	"$(printf '%s\n' "$ps_own" | integration_port_holders testing "18123/tcp")" ""

ps_foreign=$(printf 'clickdog-ch||0.0.0.0:18123->8123/tcp, [::]:18123->8123/tcp\n')
assert_eq "an unlabelled container holding the port is a conflict" \
	"$(printf '%s\n' "$ps_foreign" | integration_port_holders testing "18123/tcp")" \
	"18123/tcp|clickdog-ch"

ps_other_project=$(printf 'some-db|other-project|0.0.0.0:19000->9000/tcp\n')
assert_eq "a container in a different compose project is a conflict" \
	"$(printf '%s\n' "$ps_other_project" | integration_port_holders testing "19000/tcp")" \
	"19000/tcp|some-db"

# The ":" anchor: a longer port that merely contains ours must not match.
ps_superstring=$(printf 'noisy||0.0.0.0:118123->8123/tcp\n')
assert_eq "a superstring port does not match" \
	"$(printf '%s\n' "$ps_superstring" | integration_port_holders testing "18123/tcp")" ""

# A container publishing nothing we need is left alone.
ps_unrelated=$(printf 'supabase_db|next-game|0.0.0.0:54322->5432/tcp\n')
assert_eq "an unrelated published port is not a conflict" \
	"$(printf '%s\n' "$ps_unrelated" | integration_port_holders testing "18123/tcp 19000/tcp")" ""

# A UDP binding does not occupy the TCP port of the same number, so it must not
# block a run that `compose up` would start.
ps_udp=$(printf 'udp-only||0.0.0.0:18123->8123/udp\n')
assert_eq "a UDP mapping does not hold the TCP port" \
	"$(printf '%s\n' "$ps_udp" | integration_port_holders testing "18123/tcp")" ""
assert_eq "a UDP mapping does hold the UDP port" \
	"$(printf '%s\n' "$ps_udp" | integration_port_holders testing "18123/udp")" \
	"18123/udp|udp-only"

# The protocol must be read from the SAME mapping: a container binding UDP on our
# port and TCP on an unrelated one must not read as a TCP conflict.
ps_mixed=$(printf 'mixed||0.0.0.0:18123->8123/udp, 0.0.0.0:9999->99/tcp\n')
assert_eq "protocol is matched within one mapping, not across the list" \
	"$(printf '%s\n' "$ps_mixed" | integration_port_holders testing "18123/tcp")" ""

echo "integration port preflight tests: PASS"
