#!/usr/bin/env bash
# Unit tests for install.sh pure-bash functions.
# Run: bash deploy/install_test.sh
#
# These tests source install.sh functions without executing the main flow.
# No root, systemd, or ClickHouse required.
set -euo pipefail

PASS=0
FAIL=0
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
TEST_TEMP_DIRS=()

# ── Test helpers ──────────────────────────────────────────────
# A suite that stops before its last line tested less than it reports, whatever
# status bash leaves behind.
SUITE_FINISHED="false"
finish_suite() {
    local status=$? dir
    for dir in "${TEST_TEMP_DIRS[@]+"${TEST_TEMP_DIRS[@]}"}"; do
        rm -rf "$dir"
    done
    if [[ "$SUITE_FINISHED" != "true" ]]; then
        echo "FAIL: the suite stopped before its last test"
        [[ "$status" -ne 0 ]] || status=1
    fi
    exit "$status"
}
trap finish_suite EXIT

make_temp_dir() {
    mktemp -d "${TMPDIR:-/tmp}/click-dog-test.XXXXXX"
}

# Give this run its own TMPDIR. Some assertions count temp files by name, and
# another run on the same machine would change the count mid-test.
TMPDIR=$(make_temp_dir)
export TMPDIR
TEST_TEMP_DIRS+=("$TMPDIR")

assert_eq() {
    local desc="$1" expected="$2" actual="$3"
    if [[ "$expected" == "$actual" ]]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected: $expected"
        echo "    actual:   $actual"
        FAIL=$((FAIL + 1))
    fi
}

assert_match() {
    local desc="$1" pattern="$2" actual="$3"
    if [[ "$actual" =~ $pattern ]]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    pattern:  $pattern"
        echo "    actual:   $actual"
        FAIL=$((FAIL + 1))
    fi
}

assert_not_match() {
    local desc="$1" pattern="$2" actual="$3"
    if [[ ! "$actual" =~ $pattern ]]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected NOT to match: $pattern"
        FAIL=$((FAIL + 1))
    fi
}

assert_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if [[ "$haystack" == *"$needle"* ]]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected to contain: $needle"
        FAIL=$((FAIL + 1))
    fi
}

assert_not_contains() {
    local desc="$1" needle="$2" haystack="$3"
    if [[ "$haystack" != *"$needle"* ]]; then
        echo "  PASS: $desc"
        PASS=$((PASS + 1))
    else
        echo "  FAIL: $desc"
        echo "    expected NOT to contain: $needle"
        FAIL=$((FAIL + 1))
    fi
}

assert_before() { # assert_before DESC FIRST SECOND HAYSTACK
    local before
    if [[ "$4" != *"$3"* ]]; then
        assert_contains "$1" "$3" "$4"
        return
    fi
    before=${4%%"$3"*}
    assert_contains "$1" "$2" "$before"
}

# ── Source install.sh functions ───────────────────────────────
# We need to source the functions without running the main flow.
# Extract just the function definitions.
# The globals below are read by install.sh's functions, which arrive through
# the eval at the end rather than a source, so the linter sees only the writes.
# shellcheck disable=SC2034
source_functions() {
    # Set required globals so sourced functions don't fail
    CLICKHOUSE_USER="monitoring"
    CLICKHOUSE_PASSWORD="testpass123"
    CLICKHOUSE_TLS="false"
    COLLECTOR_ADDRESS="localhost:4317"
    SYSTEMD="true"
    VERSION=""
    CLICKHOUSE_CLIENT="clickhouse-client"

    # Extract and eval functions from install.sh
    # We source everything up to the dispatch case statement
    eval "$(sed -n '1,/^# ── Dispatch/p' "$SCRIPT_DIR/install.sh" | grep -v '^set -euo pipefail')"
}

source_functions

# ── Tests ─────────────────────────────────────────────────────

echo "=== generate_password ==="

pw=$(generate_password)
assert_match "length >= 24" "^.{24,}$" "$pw"
assert_match "alphanumeric only" "^[A-Za-z0-9]+$" "$pw"

pw2=$(generate_password)
assert_eq "two calls produce different passwords" "true" "$([[ "$pw" != "$pw2" ]] && echo true || echo false)"

echo ""
echo "=== guided quickstart requires a TTY (issue #386) ==="

quickstart_tty_status=0
quickstart_tty_out=$(require_quickstart_tty </dev/null 2>&1) || quickstart_tty_status="$?"
assert_eq "non-TTY quickstart preflight exits 1" "1" "$quickstart_tty_status"
assert_contains "non-TTY error explains the terminal requirement" \
    "Guided install requires an interactive terminal" "$quickstart_tty_out"
assert_contains "non-TTY error points to the supported install command" \
    "install -c collector:4317 --systemd" "$quickstart_tty_out"

echo ""
echo "=== render_click_dog_config args ==="

# Replace the binary with a stub that just echoes the args it received.
# Pre-unification this block exercised install.sh's inline YAML renderer
# directly; now the renderer lives in the Go binary (see
# docs/development/specs/unify-config-generators.md), so install.sh's job
# is just to build the right argv. The Go tests in cmd_init_test.go
# (TestRunInit_NeverEmitsTopLevelOtel,
# TestRunInit_ProductionHasHardeningAndBlacklistOps,
# TestRunInit_CHUserFlagOverridesDefault, etc) cover what the binary
# emits given those args.
STUB_BIN=$(make_temp_dir)/click-dog-stub
TEST_TEMP_DIRS+=("$(dirname "$STUB_BIN")")
cat > "$STUB_BIN" <<'STUB'
#!/bin/sh
echo "$@"
STUB
chmod +x "$STUB_BIN"

# The earlier `eval` in source_functions evaluates the install.sh prelude
# which resets every CLI var to its install.sh default (empty COLLECTOR,
# CLICKHOUSE_TLS=true, etc.) — re-set the ones this block reads so the
# assertions exercise known values rather than stale defaults.
saved_BINARY="${BINARY:-}"
saved_TLS="$CLICKHOUSE_TLS"
saved_KEEPER="$KEEPER_HOSTS"
saved_COLLECTOR="$COLLECTOR_ADDRESS"
saved_USER="$CLICKHOUSE_USER"
BINARY="$STUB_BIN"
COLLECTOR_ADDRESS="localhost:4317"
CLICKHOUSE_USER="monitoring"

# The non-guided install default is TLS on ClickHouse's standard secure
# native-protocol port. install.sh conveys that contract by passing
# -ch-secure without -ch-port; click-dog init then auto-selects 9440 (covered
# end-to-end by TestRunInit_CHSecureAutoFlipsPort in cmd_init_test.go).
assert_eq "installer defaults ClickHouse TLS on" "true" "$CLICKHOUSE_TLS"
assert_eq "installer leaves TLS port to init's 9440 default" "" "${CLICKHOUSE_PORT:-}"
default_secure=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "default render enables ClickHouse TLS" "--ch-secure" "$default_secure"
assert_not_contains "default render delegates port to init's TLS default" "-ch-port" "$default_secure"

CLICKHOUSE_TLS="false"
KEEPER_HOSTS=""
plain=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "init subcommand"          "init"                                   "$plain"
assert_contains "collector flag passed"    "--collector localhost:4317"              "$plain"
assert_contains "service flag is canonical click-dog-monitor" "--service click-dog-monitor" "$plain"
assert_contains "ch-user threads CLICKHOUSE_USER through" "--ch-user monitoring"     "$plain"
assert_contains "output path"              "-o /etc/click-dog/click-dog.yaml"       "$plain"
assert_contains "force overwrite"          "--force"                                "$plain"
assert_not_contains "no ch-secure when TLS off" "-ch-secure"                        "$plain"
assert_not_contains "no ha-keeper when keeper empty" "-ha-keeper"                   "$plain"

CLICKHOUSE_TLS="true"
KEEPER_HOSTS="keeper1:9181,keeper2:9181"
full=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "ch-secure added when CLICKHOUSE_TLS=true" "--ch-secure"            "$full"
assert_contains "ha-keeper threads KEEPER_HOSTS"          "--ha-keeper keeper1:9181,keeper2:9181" "$full"
assert_not_contains "no ch-port when CLICKHOUSE_PORT unset" "-ch-port"              "$full"

# Quickstart probe pins CLICKHOUSE_PORT to whichever port it actually
# checked (see "Detect ClickHouse TLS" in install.sh). When set, that
# value must flow through to `-ch-port` so the rendered config matches
# the probed endpoint — preventing the "TLS on 9000 → config for 9440"
# divergence flagged in PR #164 review.
saved_PORT="${CLICKHOUSE_PORT:-}"
CLICKHOUSE_PORT="9000"
pinned=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "ch-port pinned when CLICKHOUSE_PORT=9000" "--ch-port 9000"          "$pinned"
CLICKHOUSE_PORT="$saved_PORT"

# OTLP TLS: empty means automatic, and init gets no --otel-secure so it
# renders by the loopback rule; an explicit choice is passed with '=', the
# only spelling Go's flag package reads a false from.
assert_eq "installer leaves the OTLP transport automatic by default" "" "$OTEL_SECURE"
assert_not_contains "no otel-secure when OTEL_SECURE unset" "-otel-secure" "$full"
OTEL_SECURE="true"
otel_tls=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "otel-secure=true added when OTEL_SECURE=true" "--otel-secure=true" "$otel_tls"
OTEL_SECURE="false"
otel_plain=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "otel-secure=false added when OTEL_SECURE=false" "--otel-secure=false" "$otel_plain"
assert_not_match "never a bare --otel-secure" '--otel-secure( |$)' "$otel_tls $otel_plain"

# The docker path renders through `docker run ... init`; stub docker to
# capture that argv.
docker_render_args=""
docker() { docker_render_args="$*"; }
docker_render_dir=$(make_temp_dir)
TEST_TEMP_DIRS+=("$docker_render_dir")
OTEL_SECURE="true"
render_click_dog_config_via_docker "$docker_render_dir/click-dog.yaml"
assert_contains "docker render passes otel-secure=true" "--otel-secure=true" "$docker_render_args"
OTEL_SECURE="false"
render_click_dog_config_via_docker "$docker_render_dir/click-dog.yaml"
assert_contains "docker render passes otel-secure=false" "--otel-secure=false" "$docker_render_args"
OTEL_SECURE="true"
assert_contains "docker render runs as the invoker" "run --rm --user $(id -u):$(id -g)" "$docker_render_args"
# click-dog.yaml is 0644 so the service can read it; only .env holds the password.
assert_not_contains "docker render never passes the password" "$CLICKHOUSE_PASSWORD" "$docker_render_args"
assert_not_match "docker render passes no password flag" '(^| )--?ch-password( |=|$)' "$docker_render_args"
OTEL_SECURE=""
render_click_dog_config_via_docker "$docker_render_dir/click-dog.yaml"
assert_not_contains "docker render leaves the transport automatic by default" "-otel-secure" "$docker_render_args"

# click-dog spells long options --flag (its --help and the docs do); Go's flag
# package accepts one dash too, so a regression would still run — pin it here.
# Every optional flag is on for this render so all of them are checked.
single_dash_long='(^| )-[a-z][a-z-]+'
CLICKHOUSE_TLS="true" CLICKHOUSE_PORT="9440" KEEPER_HOSTS="keeper1:9181" OTEL_SECURE="true"
all_flags=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_not_match "render passes only --long flags" "$single_dash_long" "$all_flags"
render_click_dog_config_via_docker "$docker_render_dir/click-dog.yaml"
assert_not_match "docker render passes only --long flags" "$single_dash_long" \
    "${docker_render_args#*click-dog:* }"
CLICKHOUSE_TLS="$saved_TLS" CLICKHOUSE_PORT="${saved_PORT:-}" KEEPER_HOSTS="" OTEL_SECURE=""
unset -f docker

BINARY="$saved_BINARY"
CLICKHOUSE_TLS="$saved_TLS"
KEEPER_HOSTS="$saved_KEEPER"
COLLECTOR_ADDRESS="$saved_COLLECTOR"
CLICKHOUSE_USER="$saved_USER"

echo ""
echo "=== OTLP receiver TLS: guided default ==="

# The installer keeps no copy of the loopback rule: the binary decides
# (`click-dog init --print-otel-secure`; its Go tests cover the address
# table). The guided answer on Enter is whatever it said.
assert_eq "otel_tls_default is gone" "" "$(declare -F otel_tls_default || true)"
assert_eq "ipv6_expand is gone with it" "" "$(declare -F ipv6_expand || true)"
assert_eq "no shell loopback rule is left" "0" \
    "$(grep -cE 'localhost\|\*\.localhost|127\.\*\)' deploy/install.sh || true)"

COLLECTOR_ADDRESS="otel.example.com:4317"
assert_eq "collector host split" "otel.example.com" "$(otel_collector_host)"
assert_eq "collector port split" "4317" "$(otel_collector_port)"
# No port means 443, the port grpc's dns resolver dials, so the guided
# reachability check and the handshake look at the port export will use.
COLLECTOR_ADDRESS="otel.example.com"
assert_eq "collector port defaults to 443 like grpc" "443" "$(otel_collector_port)"
# gRPC writes an IPv6 literal as [addr]:port; the host comes back bare.
COLLECTOR_ADDRESS="[::1]:4317"
assert_eq "IPv6 host split" "::1" "$(otel_collector_host)"
assert_eq "IPv6 port split" "4317" "$(otel_collector_port)"
COLLECTOR_ADDRESS="[2001:db8::5]"
assert_eq "bracketed IPv6 without a port" "2001:db8::5" "$(otel_collector_host)"
assert_eq "bracketed IPv6 port defaults to 443" "443" "$(otel_collector_port)"
assert_eq "IPv6 host:port is bracketed" "[2001:db8::5]:4317" "$(otel_hostport 2001:db8::5 4317)"
assert_eq "name host:port is not" "otel.example.com:4317" "$(otel_hostport otel.example.com 4317)"

# The answer on Enter follows the binary's choice and leaves the transport
# automatic; only an answer that differs becomes an explicit OTEL_SECURE.
OTEL_SECURE="" OTEL_USES_TLS="false"
prompt_otel_tls <<<"" >/dev/null
assert_eq "Enter on a plaintext default stays automatic" "" "$OTEL_SECURE"
assert_eq "Enter on a plaintext default is plaintext" "false" "$OTEL_USES_TLS"
OTEL_SECURE="" OTEL_USES_TLS="true"
prompt_otel_tls <<<"" >/dev/null
assert_eq "Enter on a TLS default stays automatic" "" "$OTEL_SECURE"
assert_eq "Enter on a TLS default is TLS" "true" "$OTEL_USES_TLS"
OTEL_SECURE="" OTEL_USES_TLS="true"
prompt_otel_tls <<<"y" >/dev/null
assert_eq "y on a TLS default is the default, still automatic" "" "$OTEL_SECURE"
OTEL_SECURE="" OTEL_USES_TLS="true"
prompt_otel_tls <<<"n" >/dev/null
assert_eq "n overrides the TLS default explicitly" "false" "$OTEL_SECURE"
assert_eq "n makes the transport plaintext" "false" "$OTEL_USES_TLS"
OTEL_SECURE="" OTEL_USES_TLS="false"
prompt_otel_tls <<<"y" >/dev/null
assert_eq "y overrides the plaintext default explicitly" "true" "$OTEL_SECURE"
assert_eq "y makes the transport TLS" "true" "$OTEL_USES_TLS"
OTEL_SECURE="" OTEL_USES_TLS="false"
prompt_otel_tls <<<$'maybe\nyes' >/dev/null
assert_eq "invalid answer re-asks" "true" "$OTEL_SECURE"
OTEL_SECURE="" OTEL_USES_TLS=""

echo ""
echo "=== OTLP receiver TLS: handshake probe ==="

# Every verdict comes from the click-dog binary (`click-dog test tls`), whose
# Go tests drive real TLS and gRPC receivers through each one. Here a fake
# binary replays scripted --json output and --chain-out files and records its
# argv, so these tests cover what the shell does with a verdict. With
# --ca-cert it replays FAKE_CD_OUT_CA when that exists, so a
# trust-on-first-use test can script both handshakes. FAKE_CD_MODE stands in
# for a binary that cannot run here (norun), a release without `test tls`
# (old), and one that prints nothing (silent). Certificates for chain files
# and CA checks are made with openssl, which only these tests need.
REAL_OPENSSL=$(command -v openssl)
tls_sandbox=$(make_temp_dir)
TEST_TEMP_DIRS+=("$tls_sandbox")
# fake_print_otel_secure ARGS... — a stand-in for `click-dog init
# --print-otel-secure` in the fake binaries: the flags decide, and unset, a
# loopback collector is plaintext and anything else TLS. It is only an
# answer to replay; the rule itself is the binary's, under Go tests.
FAKE_PRINT_OTEL_SECURE='
fake_print_otel_secure() {
    collector="" secure="" ca="" prev=""
    for arg in "$@"; do
        case "$prev" in --collector) collector="$arg" ;; --otel-ca-cert) ca="$arg" ;; esac
        case "$arg" in --otel-secure=*) secure="${arg#--otel-secure=}" ;; esac
        prev="$arg"
    done
    [ -n "${FAKE_CD_INIT_ARGS:-}" ] && printf "%s\n" "$*" >> "$FAKE_CD_INIT_ARGS"
    [ "${FAKE_CD_PRINT:-}" = "none" ] && { echo "flag provided but not defined: -print-otel-secure" >&2; exit 2; }
    if [ -n "$secure" ]; then echo "$secure"
    elif [ -n "$ca" ]; then echo true
    else
        case "$collector" in
            localhost:*|127.*|"[::1]"*|unix:*) echo false ;;
            *) echo true ;;
        esac
    fi
    exit 0
}'
cat > "$tls_sandbox/click-dog" <<FAKE
#!/bin/sh
$FAKE_PRINT_OTEL_SECURE
FAKE
cat >> "$tls_sandbox/click-dog" <<'FAKE'
[ "${FAKE_CD_MODE:-}" = "norun" ] && exit 126
case "$1" in
    --version) echo "click-dog v0.0.0-test"; exit 0 ;;
    init) [ "$2" = "--print-otel-secure" ] && { shift; fake_print_otel_secure "$@"; }; exit 1 ;;
    test) ;;
    *) exit 1 ;;
esac
if [ "$2" = "help" ]; then
    echo "Subcommands:"
    echo "  export     Send a synthetic span through each configured exporter"
    [ "${FAKE_CD_MODE:-}" = "old" ] || echo "  tls        Check an OTLP receiver's TLS the way the exporter would"
    exit 0
fi
if [ "$2" != "tls" ] || [ "${FAKE_CD_MODE:-}" = "old" ]; then
    echo "click-dog test: unknown command \"$2\"" >&2
    exit 2
fi
printf '%s\n' "$*" > "$FAKE_CD_ARGS"
json="$FAKE_CD_OUT"
case " $* " in
    *" --ca-cert "*) [ -f "${FAKE_CD_OUT_CA:-/nonexistent}" ] && json="$FAKE_CD_OUT_CA" ;;
esac
chain_out="" prev=""
for arg in "$@"; do
    [ "$prev" = "--chain-out" ] && chain_out="$arg"
    prev="$arg"
done
if [ -n "$chain_out" ]; then
    if [ -f "${FAKE_CD_CHAIN:-/nonexistent}" ]; then cat "$FAKE_CD_CHAIN" > "$chain_out"; else : > "$chain_out"; fi
fi
[ "${FAKE_CD_MODE:-}" = "silent" ] || cat "$json"
exit 1
FAKE
chmod +x "$tls_sandbox/click-dog"
export FAKE_CD_ARGS="$tls_sandbox/args" FAKE_CD_OUT="$tls_sandbox/out" \
    FAKE_CD_OUT_CA="$tls_sandbox/out-ca" FAKE_CD_CHAIN="$tls_sandbox/chain"
saved_BINARY="${BINARY:-}"
BINARY="$tls_sandbox/click-dog"

# make_test_cert NAME CN [SAN] [ISSUER] — a throwaway EC certificate (and
# key) in the sandbox: self-signed, or signed by the ISSUER certificate made
# earlier. A config file rather than -addext keeps it working on older
# openssl.
make_test_cert() {
    local name="$1" cn="$2" san="${3:-}" issuer="${4:-}"
    local cnf="$tls_sandbox/$name.cnf"
    {
        printf '[req]\ndistinguished_name=dn\nprompt=no\n[dn]\nCN=%s\n[ext]\n' "$cn"
        if [[ -z "$issuer" ]]; then
            printf 'basicConstraints=critical,CA:TRUE\n'
        else
            printf 'basicConstraints=CA:FALSE\n'
        fi
        if [[ -n "$san" ]]; then
            printf 'subjectAltName=%s\n' "$san"
        fi
    } > "$cnf"
    if [[ -z "$issuer" ]]; then
        "$REAL_OPENSSL" req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
            -config "$cnf" -extensions ext -keyout "$tls_sandbox/$name.key" \
            -out "$tls_sandbox/$name.crt" 2>/dev/null
    else
        "$REAL_OPENSSL" req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes \
            -config "$cnf" -keyout "$tls_sandbox/$name.key" -out "$tls_sandbox/$name.csr" 2>/dev/null
        "$REAL_OPENSSL" x509 -req -in "$tls_sandbox/$name.csr" -days 2 -set_serial 2 \
            -CA "$tls_sandbox/$issuer.crt" -CAkey "$tls_sandbox/$issuer.key" \
            -extfile "$cnf" -extensions ext -out "$tls_sandbox/$name.crt" 2>/dev/null
    fi
}
make_test_cert san   otel.example.com "DNS:otel.example.com,IP:10.0.0.5"
make_test_cert ca    "Test Collector CA"
make_test_cert leaf  otel.example.com "DNS:otel.example.com" ca

# tls_json VERDICT [REASON [MESSAGE]] — a `click-dog test tls --json` object
# in the shape the binary prints (TestRunTestTLS_JSONLinesInstallShReads pins
# it), nested certificate fields included.
tls_json() {
    printf '{\n  "address": "otel.example.com:4317",\n  "server_name": "otel.example.com",\n'
    printf '  "verdict": "%s",\n' "$1"
    if [[ -n "${2:-}" ]]; then
        printf '  "reason": "%s",\n' "$2"
    fi
    printf '  "message": "%s",\n' "${3:-}"
    printf '  "client_cert_requested": false,\n  "certificates": [\n    {\n'
    printf '      "subject": "CN=otel.example.com",\n      "sans": [\n        "DNS:otel.example.com"\n      ],\n'
    printf '      "sha256": "AB:CD"\n    }\n  ]\n}\n'
}

# tls_chain CERT... — a --chain-out file presenting CERTs in the order sent:
# each under the "# Label: value" lines click-dog writes, with subject,
# issuer and fingerprint taken from the certificate itself.
tls_chain() {
    local i=0 c line sans
    for c in "$@"; do
        i=$((i + 1))
        echo "# Certificate $i of $# sent by otel.example.com:4317"
        line=$("$REAL_OPENSSL" x509 -in "$tls_sandbox/$c.crt" -noout -subject -nameopt RFC2253)
        echo "# Subject:     ${line#subject=}"
        line=$("$REAL_OPENSSL" x509 -in "$tls_sandbox/$c.crt" -noout -issuer -nameopt RFC2253)
        echo "# Issuer:      ${line#issuer=}"
        case "$c" in
            san)  sans="DNS:otel.example.com, IP Address:10.0.0.5" ;;
            leaf) sans="DNS:otel.example.com" ;;
            *)    sans="none" ;;
        esac
        echo "# SANs:        $sans"
        echo "# Not before:  2026-10-15 00:00:00 UTC"
        echo "# Not after:   2026-10-17 00:00:00 UTC"
        line=$("$REAL_OPENSSL" x509 -in "$tls_sandbox/$c.crt" -noout -fingerprint -sha256)
        echo "# SHA-256:     ${line#*=}"
        cat "$tls_sandbox/$c.crt"
    done
}
# same_cert A B — compares two certificate files without printing either.
same_cert() { if cmp -s "$1" "$2"; then echo yes; else echo no; fi; }
OK_MESSAGE="certificate verified for otel.example.com; TLS 1.3, ALPN h2"
SELF_SIGNED="certificate signed by unknown authority (self-signed)"

tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT"
# Three fields: verdict, reason code ("-" for none), then the detail.
assert_eq "ok passes through with its detail" "ok - $OK_MESSAGE" "$(probe_otel_tls otel.example.com 4317)"
probe_args=$(cat "$FAKE_CD_ARGS")
assert_eq "probe asks the binary for a JSON verdict" "test tls --collector otel.example.com:4317 --json" "$probe_args"
assert_not_contains "system-store probe pins nothing" "--ca-cert" "$probe_args"

# IPv6: the binary gets the gRPC form, bracketed.
probe_otel_tls 2001:db8::5 4317 >/dev/null
assert_contains "probe dials an IPv6 literal in brackets" "--collector [2001:db8::5]:4317" "$(cat "$FAKE_CD_ARGS")"
probe_otel_tls ::1 4317 >/dev/null
assert_contains "probe dials IPv6 loopback in brackets" "--collector [::1]:4317" "$(cat "$FAKE_CD_ARGS")"

probe_otel_tls otel.example.com 4317 "$tls_sandbox/ca.crt" "$tls_sandbox/probe-chain" >/dev/null
probe_args=$(cat "$FAKE_CD_ARGS")
assert_contains "CA probe trusts the given file" "--ca-cert $tls_sandbox/ca.crt" "$probe_args"
assert_contains "probe asks for the chain when told where" "--chain-out $tls_sandbox/probe-chain" "$probe_args"

# Each verdict and its detail reach the callers unchanged.
for tls_case in \
    "plaintext|non-tls|otel.example.com:4317 answered the TLS handshake with non-TLS bytes" \
    "plaintext|closed|otel.example.com:4317 closed the connection without a TLS answer: not a TLS port, or a TLS proxy or load balancer that does not serve otel.example.com (SNI)" \
    "no-san||the certificate has no subjectAltName; click-dog ignores the Common Name" \
    "untrusted|hostname-mismatch|hostname mismatch; certificate names DNS:other.example.com" \
    "untrusted|expired|certificate has expired: not after 2026-01-02 00:00:00 UTC" \
    "untrusted|unknown-authority|$SELF_SIGNED" \
    "client-cert||the receiver refused the handshake without a client certificate (remote error: tls: certificate required)" \
    "no-alpn||the receiver did not select HTTP/2 (ALPN h2)" \
    "unknown|timeout|no TLS answer within 10s"; do
    IFS='|' read -r tls_verdict tls_reason tls_message <<<"$tls_case"
    tls_json "$tls_verdict" "$tls_reason" "$tls_message" > "$FAKE_CD_OUT"
    assert_eq "$tls_verdict${tls_reason:+ ($tls_reason)} passes through" "$tls_verdict ${tls_reason:--} $tls_message" \
        "$(probe_otel_tls otel.example.com 4317)"
done

# Only top-level fields count: a nested field never stands in for the verdict.
printf '{\n  "certificates": [\n    {\n      "verdict": "ok"\n    }\n  ],\n  "verdict": "untrusted",\n  "message": "x"\n}\n' > "$FAKE_CD_OUT"
assert_eq "nested fields are not read" "untrusted - x" "$(probe_otel_tls otel.example.com 4317)"

# No verdict is never a guess: a binary that cannot run here (wrong
# architecture), a release without `test tls`, or no output at all is
# unknown, which callers treat as "not verified".
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT"
assert_eq "a binary that cannot run is unknown" "unknown binary the click-dog binary cannot run on this host" \
    "$(FAKE_CD_MODE=norun probe_otel_tls otel.example.com 4317)"
assert_eq "a release without test tls is unknown" "unknown binary this click-dog release predates 'click-dog test tls'" \
    "$(FAKE_CD_MODE=old probe_otel_tls otel.example.com 4317)"
assert_eq "no verdict at all is unknown" "unknown binary 'click-dog test tls' gave no verdict" \
    "$(FAKE_CD_MODE=silent probe_otel_tls otel.example.com 4317)"

# openssl no longer decides anything about OTLP TLS.
for removed in classify_otel_tls otel_tls_handshake otel_san_matches otel_openssl_store_args otel_cert_sans otel_pem_block is_ipv4; do
    assert_eq "$removed is gone" "" "$(declare -F "$removed" || true)"
done
tls_helpers=$(awk '/^(probe_otel_tls|otel_chain_block|otel_cert_[a-z_]*|offer_otel_tofu|confirm_otel_tls_choice|preflight_otel_tls|verify_installed_otel_ca)\(\)/,/^}/' deploy/install.sh)
assert_eq "OTLP TLS helpers never run openssl" "0" \
    "$(grep -v '^ *echo ' <<<"$tls_helpers" | grep -v '^ *#' | grep -c openssl || true)"

echo ""
echo "=== OTLP receiver TLS: chain files ==="

tls_chain leaf ca > "$FAKE_CD_CHAIN"
chain_first=$(otel_chain_block first < "$FAKE_CD_CHAIN")
chain_last=$(otel_chain_block last < "$FAKE_CD_CHAIN")
otel_cert_pem <<<"$chain_first" > "$tls_sandbox/first.crt"
otel_cert_pem <<<"$chain_last" > "$tls_sandbox/last.crt"
assert_eq "first block is the leaf" "yes" "$(same_cert "$tls_sandbox/leaf.crt" "$tls_sandbox/first.crt")"
assert_eq "last block is the top of the chain" "yes" "$(same_cert "$tls_sandbox/ca.crt" "$tls_sandbox/last.crt")"
assert_contains "a block keeps its summary lines" "# Issuer:      CN=Test Collector CA" "$chain_first"
assert_not_contains "a block stops at its own certificate" "Certificate 2 of 2" "$chain_first"
leaf_summary=$(otel_cert_summary <<<"$chain_first")
assert_eq "summary is five labeled lines" "5" "$(wc -l <<<"$leaf_summary" | tr -d ' ')"
assert_match "summary leads with the subject" "^Subject:     CN=otel.example.com" "$leaf_summary"
assert_not_contains "summary leaves the fingerprint to the prompt" "SHA-256" "$leaf_summary"
ca_fp=$("$REAL_OPENSSL" x509 -in "$tls_sandbox/ca.crt" -noout -fingerprint -sha256)
assert_eq "fingerprint comes from the block" "${ca_fp#*=}" "$(otel_cert_fingerprint <<<"$chain_last")"
self_issued() { if otel_cert_self_issued; then echo yes; else echo no; fi; }
assert_eq "a root CA is self-issued" "yes" "$(self_issued <<<"$chain_last")"
assert_eq "a CA-issued leaf is not" "no" "$(self_issued <<<"$chain_first")"
assert_eq "an empty chain has no blocks" "" "$(otel_chain_block last </dev/null)"
rm -f "$FAKE_CD_CHAIN"

echo ""
echo "=== OTLP receiver TLS: CA file checks ==="

ca_file_check() {
    local rc=0 out
    out=$(validate_otel_ca_file "$1" 2>&1) || rc=$?
    echo "$rc|$out"
}
cat "$tls_sandbox/leaf.crt" "$tls_sandbox/ca.crt" > "$tls_sandbox/bundle.crt"
cat "$tls_sandbox/san.crt" "$tls_sandbox/san.key" > "$tls_sandbox/cert-and-key.crt"
"$REAL_OPENSSL" x509 -in "$tls_sandbox/san.crt" -outform der -out "$tls_sandbox/san.der"
"$REAL_OPENSSL" pkey -in "$tls_sandbox/san.key" -pubout -out "$tls_sandbox/san.pub" 2>/dev/null
assert_match "a certificate is accepted" '^0\|$' "$(ca_file_check "$tls_sandbox/san.crt")"
assert_match "a bundle is accepted" '^0\|$' "$(ca_file_check "$tls_sandbox/bundle.crt")"
assert_match "a private key is refused" '^1\|.*PRIVATE KEY' "$(ca_file_check "$tls_sandbox/san.key")"
assert_match "a certificate with its key is refused" '^1\|.*PRIVATE KEY' "$(ca_file_check "$tls_sandbox/cert-and-key.crt")"
assert_match "DER is refused with the conversion" '^1\|.*not PEM.*-inform der' "$(ca_file_check "$tls_sandbox/san.der")"
assert_match "PEM without a certificate is refused" '^1\|.*no certificate' "$(ca_file_check "$tls_sandbox/san.pub")"
assert_match "a missing file is refused" '^1\|.*not found' "$(ca_file_check "$tls_sandbox/absent.crt")"

echo ""
echo "=== OTLP receiver TLS: unattended preflight ==="

COLLECTOR_ADDRESS="otel.example.com:4317"
OTEL_CA_CERT=""

# preflight_check — run the gate, capturing its exit status and output.
preflight_check() {
    preflight_rc=0
    preflight_out=$(preflight_otel_tls 2>&1) || preflight_rc=$?
}

tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT"
preflight_check
assert_eq "trusted TLS passes the preflight" "0" "$preflight_rc"
assert_contains "trusted TLS is reported" "TLS handshake ok, certificate trusted" "$preflight_out"

tls_json plaintext non-tls "otel.example.com:4317 answered the TLS handshake with non-TLS bytes" > "$FAKE_CD_OUT"
preflight_check
assert_eq "plaintext receiver fails the preflight" "1" "$preflight_rc"
assert_contains "plaintext failure names the cause" "does not speak TLS on otel.example.com:4317" "$preflight_out"
# The transport here is automatic (no flag): TLS, because the collector is
# not loopback. Dropping a flag is no fix then, so the fix is the explicit
# plaintext spelling.
assert_contains "an automatic choice says why it is TLS" \
    "Error: TLS is the default for otel.example.com:4317, which is not loopback, but the TLS handshake with otel.example.com:4317 failed." "$preflight_out"
assert_contains "plaintext failure on an automatic choice names --otel-secure=false" \
    "To export in plaintext, pass --otel-secure=false." "$preflight_out"
assert_not_contains "plaintext failure never says to drop a flag" "Drop --otel-secure" "$preflight_out"
OTEL_SECURE="true"
preflight_check
assert_contains "an explicit choice names its flag" "Error: --otel-secure was given, but the TLS handshake" "$preflight_out"
assert_contains "plaintext failure on --otel-secure names --otel-secure=false" \
    "pass --otel-secure=false in place of --otel-secure" "$preflight_out"
OTEL_SECURE=""

# A silent close is a plaintext port or a TLS proxy without a certificate
# for this name: both are named, and dropping TLS is not the only fix.
CLOSED_MESSAGE="otel.example.com:4317 closed the connection without a TLS answer: not a TLS port, or a TLS proxy or load balancer that does not serve otel.example.com (SNI)"
tls_json plaintext closed "$CLOSED_MESSAGE" > "$FAKE_CD_OUT"
preflight_check
assert_eq "a silent close fails the preflight" "1" "$preflight_rc"
assert_contains "a silent close names a plaintext port" "does not speak TLS on this port" "$preflight_out"
assert_contains "a silent close names a TLS proxy without the name" "serves no certificate for 'otel.example.com' (SNI)" "$preflight_out"
assert_contains "a silent close gives both fixes" "if it is a TLS proxy" "$preflight_out"
assert_contains "a silent close names --otel-secure=false for a plaintext port" \
    "If the receiver is a plaintext port, pass --otel-secure=false;" "$preflight_out"
assert_not_contains "a silent close does not just say plaintext" "To export in plaintext" "$preflight_out"

tls_json no-alpn "" "the receiver did not select HTTP/2 (ALPN h2)" > "$FAKE_CD_OUT"
preflight_check
assert_eq "receiver without ALPN h2 fails the preflight" "1" "$preflight_rc"
assert_contains "no-ALPN failure names HTTP/2" "does not select HTTP/2 (ALPN h2)" "$preflight_out"

tls_json unknown handshake-ended "TLS handshake ended before a certificate (remote error: tls: protocol version not supported)" > "$FAKE_CD_OUT"
preflight_check
assert_eq "an early TLS alert is only a note" "0" "$preflight_rc"
assert_not_contains "an early TLS alert never says plaintext" "does not speak TLS" "$preflight_out"
assert_contains "the note carries the binary's reason" "not verified (TLS handshake ended before a certificate" "$preflight_out"

# A CA file click-dog cannot parse is not an inconclusive check: the
# exporter fails at startup on the same file, so the install must not go on.
OTEL_CA_CERT="$tls_sandbox/ca.crt"
# With --ca-cert the fake replays FAKE_CD_OUT_CA when it exists.
rm -f "$FAKE_CD_OUT_CA"
tls_json unknown config "failed to parse OTEL CA certificate" > "$FAKE_CD_OUT"
assert_eq "a config verdict is its own failure, not unknown" "config config failed to parse OTEL CA certificate" \
    "$(probe_otel_tls otel.example.com 4317 "$OTEL_CA_CERT")"
preflight_check
assert_eq "an unusable CA fails the preflight" "1" "$preflight_rc"
assert_not_contains "an unusable CA is never just a note" "not verified" "$preflight_out"
assert_contains "an unusable CA names the file and the cause" "cannot use $tls_sandbox/ca.crt as the collector's CA (failed to parse OTEL CA certificate)" "$preflight_out"
assert_contains "an unusable CA is not called a failed handshake" "cannot use its TLS settings" "$preflight_out"
assert_not_contains "an unusable CA claims no handshake" "TLS handshake with" "$preflight_out"
OTEL_CA_CERT=""

# A gRPC target with a scheme is split wrongly by the host:port helpers, and
# the exporter accepts it, so the check is skipped with a note, never failed.
for scheme_addr in "passthrough:///otel.example.com:4317" "dns:///otel.example.com:4317" "unix:///run/otel.sock"; do
    COLLECTOR_ADDRESS="$scheme_addr"
    : > "$FAKE_CD_ARGS"
    assert_match "a $scheme_addr target is not checked" "^unknown target " \
        "$(probe_otel_tls "$(otel_collector_host)" "$(otel_collector_port)")"
    assert_eq "a $scheme_addr target never runs the binary" "" "$(cat "$FAKE_CD_ARGS")"
    preflight_check
    assert_eq "a $scheme_addr target passes the preflight with a note" "0" "$preflight_rc"
    assert_contains "the note says what was skipped" "checks host:port collector addresses only" "$preflight_out"
done
COLLECTOR_ADDRESS="otel.example.com:4317"

tls_json untrusted unknown-authority "$SELF_SIGNED" > "$FAKE_CD_OUT"
preflight_check
assert_eq "untrusted certificate fails the preflight" "1" "$preflight_rc"
assert_contains "untrusted failure says so" "Certificate not trusted" "$preflight_out"
assert_contains "untrusted failure gives the reason" "($SELF_SIGNED)" "$preflight_out"
assert_contains "untrusted failure links the TLS docs" "integrations/generic-otlp/#tls-configuration" "$preflight_out"
assert_contains "untrusted failure routes to --otel-ca-cert" "Pass --otel-ca-cert FILE" "$preflight_out"
assert_contains "unattended never trusts on first use" "never trust a certificate on first use" "$preflight_out"
assert_not_contains "unattended offers no trust prompt" "Trust this certificate" "$preflight_out"

tls_json untrusted hostname-mismatch "hostname mismatch; certificate names DNS:other.example.com" > "$FAKE_CD_OUT"
preflight_check
assert_eq "wrong-name certificate fails the preflight" "1" "$preflight_rc"
assert_contains "wrong-name failure names the certificate's names" "is not issued for 'otel.example.com'" "$preflight_out"
assert_not_contains "a CA would not fix a wrong name" "Pass --otel-ca-cert" "$preflight_out"

tls_json untrusted expired "certificate has expired: not after 2026-01-02 00:00:00 UTC" > "$FAKE_CD_OUT"
preflight_check
assert_eq "an expired certificate fails the preflight" "1" "$preflight_rc"
assert_contains "an expired certificate is called invalid now" "is not valid now (certificate has expired" "$preflight_out"
assert_not_contains "a CA would not fix an expired certificate" "Pass --otel-ca-cert" "$preflight_out"

# Decisions follow the reason code, never the wording: an unknown CA whose
# message happens to say "hostname mismatch" and "expired" still routes to
# --otel-ca-cert, and a mismatch reworded without that phrase does not.
tls_json untrusted unknown-authority "hostname mismatch? expired? neither: unknown issuer" > "$FAKE_CD_OUT"
preflight_check
assert_contains "an unknown CA is decided by its reason" "Pass --otel-ca-cert FILE" "$preflight_out"
assert_contains "the reworded detail is still shown" "(hostname mismatch? expired? neither: unknown issuer)" "$preflight_out"
tls_json untrusted hostname-mismatch "names other hosts entirely" > "$FAKE_CD_OUT"
preflight_check
assert_contains "a reworded mismatch is still a mismatch" "is not issued for 'otel.example.com'" "$preflight_out"
assert_not_contains "a reworded mismatch gets no CA advice" "Pass --otel-ca-cert" "$preflight_out"

tls_json no-san "" "the certificate has no subjectAltName; click-dog ignores the Common Name" > "$FAKE_CD_OUT"
preflight_check
assert_eq "CN-only certificate fails the preflight" "1" "$preflight_rc"
assert_contains "CN-only failure explains Go ignores the CN" "ignores the Common Name" "$preflight_out"
assert_contains "CN-only failure links the self-signed guide" "#self-signed-collector" "$preflight_out"

tls_json client-cert "" "the receiver refused the handshake without a client certificate (remote error: tls: certificate required)" > "$FAKE_CD_OUT"
preflight_check
assert_eq "mTLS receiver fails the preflight" "1" "$preflight_rc"
assert_contains "mTLS failure points to client_cert" "exporters.otel[].client_cert and client_key" "$preflight_out"
assert_contains "mTLS failure routes to install -f" "install.sh install -f click-dog.yaml" "$preflight_out"

tls_json unknown unreachable "no answer from otel.example.com:4317 (connect: connection refused)" > "$FAKE_CD_OUT"
preflight_check
assert_eq "unverifiable TLS does not block install" "0" "$preflight_rc"
assert_contains "unverifiable TLS leaves a note" "not verified" "$preflight_out"

FAKE_CD_MODE=norun preflight_check
assert_eq "a binary that cannot run does not block install" "0" "$preflight_rc"
assert_contains "it says why nothing was checked" "cannot run on this host" "$preflight_out"

# --otel-ca-cert: the handshake runs against that file alone.
OTEL_CA_CERT="$tls_sandbox/san.crt"
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
preflight_check
assert_eq "operator CA that verifies passes" "0" "$preflight_rc"
assert_contains "operator CA is named on success" "certificate verified by $tls_sandbox/san.crt" "$preflight_out"
assert_contains "operator CA is what the probe trusts" "--ca-cert $tls_sandbox/san.crt" "$(cat "$FAKE_CD_ARGS")"

tls_json untrusted unknown-authority "certificate signed by unknown authority" > "$FAKE_CD_OUT_CA"
preflight_check
assert_eq "operator CA that does not verify fails" "1" "$preflight_rc"
assert_contains "wrong CA failure names the flag" "--otel-ca-cert was given" "$preflight_out"
assert_contains "wrong CA failure names the file" "$tls_sandbox/san.crt does not verify" "$preflight_out"

tls_json no-san "" "the certificate has no subjectAltName; click-dog ignores the Common Name" > "$FAKE_CD_OUT_CA"
preflight_check
assert_eq "operator CA cannot rescue a CN-only certificate" "1" "$preflight_rc"
rm -f "$FAKE_CD_OUT_CA"
OTEL_CA_CERT=""

# A CA that meets a plaintext port: the fix replaces the CA flag.
OTEL_CA_CERT="$tls_sandbox/san.crt"
tls_json plaintext non-tls "otel.example.com:4317 answered the TLS handshake with non-TLS bytes" > "$FAKE_CD_OUT_CA"
preflight_check
assert_contains "plaintext failure with a CA names --otel-secure=false in its place" \
    "pass --otel-secure=false in place of --otel-ca-cert" "$preflight_out"
rm -f "$FAKE_CD_OUT_CA"
OTEL_CA_CERT=""

echo ""
echo "=== OTLP receiver TLS: transport from the binary ==="

# resolve_otel_transport asks the click-dog being installed, passing the
# address and only the flags given; the fake replays an answer.
export FAKE_CD_INIT_ARGS="$tls_sandbox/init-args"
transport_of() {
    : > "$FAKE_CD_INIT_ARGS"
    OTEL_USES_TLS=""
    COLLECTOR_ADDRESS="$1" OTEL_SECURE="${2:-}" OTEL_CA_CERT="${3:-}"
    resolve_otel_transport >/dev/null 2>&1 || { echo "error"; return 0; }
    echo "$OTEL_USES_TLS"
}
assert_eq "a remote collector is TLS" "true" "$(transport_of otel.example.com:4317)"
assert_eq "the binary is asked with the address alone" \
    "--print-otel-secure --collector otel.example.com:4317" "$(cat "$FAKE_CD_INIT_ARGS")"
assert_eq "a loopback collector is plaintext" "false" "$(transport_of localhost:4317)"
assert_eq "--otel-secure=false makes a remote collector plaintext" "false" "$(transport_of otel.internal:4317 false)"
assert_eq "an explicit choice reaches the binary with =" \
    "--print-otel-secure --collector otel.internal:4317 --otel-secure=false" "$(cat "$FAKE_CD_INIT_ARGS")"
assert_eq "--otel-secure=true makes loopback TLS" "true" "$(transport_of localhost:4317 true)"
assert_eq "a CA makes a loopback collector TLS" "true" "$(transport_of localhost:4317 "" ca.pem)"
assert_eq "the CA reaches the binary" \
    "--print-otel-secure --collector localhost:4317 --otel-ca-cert ca.pem" "$(cat "$FAKE_CD_INIT_ARGS")"
assert_eq "a release without --print-otel-secure is an error" "error" \
    "$(FAKE_CD_PRINT=none transport_of otel.example.com:4317)"
transport_rc=0
transport_out=$(COLLECTOR_ADDRESS=otel.example.com:4317 FAKE_CD_PRINT=none resolve_otel_transport 2>&1) || transport_rc=$?
assert_eq "no answer stops the run" "1" "$transport_rc"
assert_contains "no answer names the command" "'click-dog init --print-otel-secure'" "$transport_out"
assert_contains "no answer points at the latest release" "releases/latest" "$transport_out"
assert_contains "no answer shows the binary's own error" "flag provided but not defined" "$transport_out"

# The guided step: a flag settles the question; otherwise the binary's
# answer is the default, and accepting it leaves the transport automatic.
guided_transport() {
    : > "$FAKE_CD_INIT_ARGS"
    OTEL_USES_TLS=""
    COLLECTOR_ADDRESS="$1" OTEL_SECURE="${2:-}" OTEL_CA_CERT="${3:-}"
    guided_transport_out=$(guided_otel_transport <<<"${4:-}" 2>&1; echo "secure=${OTEL_SECURE} tls=${OTEL_USES_TLS}")
}
guided_transport otel.example.com:4317
assert_contains "guided Enter on a remote collector keeps it automatic TLS" "secure= tls=true" "$guided_transport_out"
assert_contains "the guided default comes from the binary" "--print-otel-secure --collector otel.example.com:4317" "$(cat "$FAKE_CD_INIT_ARGS")"
guided_transport localhost:4317
assert_contains "guided Enter on loopback keeps it automatic plaintext" "secure= tls=false" "$guided_transport_out"
guided_transport otel.example.com:4317 "" "" n
assert_contains "a guided n on a TLS default is explicit plaintext" "secure=false tls=false" "$guided_transport_out"
guided_transport otel.example.com:4317 true "" n
assert_contains "--otel-secure=true is not asked again" "Using TLS (from --otel-secure)" "$guided_transport_out"
assert_contains "--otel-secure=true stays explicit" "secure=true tls=true" "$guided_transport_out"
guided_transport localhost:4317 false "" y
assert_contains "--otel-secure=false is not asked again" "Using plaintext (from --otel-secure=false)" "$guided_transport_out"
assert_contains "--otel-secure=false stays explicit" "secure=false tls=false" "$guided_transport_out"
guided_transport localhost:4317 "" "$tls_sandbox/san.crt" n
assert_contains "a CA is not asked about" "Using TLS with CA $tls_sandbox/san.crt" "$guided_transport_out"
assert_contains "a CA makes loopback TLS, leaving --otel-secure unset" "secure= tls=true" "$guided_transport_out"
guided_rc=0
( guided_transport localhost:4317 "" "$tls_sandbox/san.key" ) >/dev/null 2>&1 || guided_rc=$?
assert_eq "the guided step refuses a key given as the CA" "1" "$guided_rc"

# docker asks the image, run with the host network, for the transport and
# the handshake: the same answers, read the same way.
run_args_file="$tls_sandbox/docker-run-args"
docker() {
    printf '%s\n' "$*" >> "$run_args_file"
    [[ "$1 $2 $3 $4" == "run --rm --network host" ]] || return 1
    shift 5
    "$tls_sandbox/click-dog" "$@"
}
: > "$run_args_file"
# Read by run_click_dog, which arrives via eval.
# shellcheck disable=SC2034
CLICK_DOG_RUN_IMAGE="ghcr.io/coltconsulting/click-dog:26.04.1"
assert_eq "the image decides the docker transport" "true" "$(transport_of otel.example.com:4317)"
assert_eq "the image runs with the host network" \
    "run --rm --network host ghcr.io/coltconsulting/click-dog:26.04.1 init --print-otel-secure --collector otel.example.com:4317" \
    "$(head -1 "$run_args_file")"
: > "$run_args_file"
tls_json plaintext non-tls "otel.example.com:4317 answered the TLS handshake with non-TLS bytes" > "$FAKE_CD_OUT"
COLLECTOR_ADDRESS="otel.example.com:4317" OTEL_SECURE="" OTEL_CA_CERT=""
preflight_check
assert_eq "a plaintext receiver stops docker" "1" "$preflight_rc"
assert_contains "docker names --otel-secure=false as the fix" "pass --otel-secure=false" "$preflight_out"
assert_contains "the docker handshake runs in the image with the host network" \
    "run --rm --network host ghcr.io/coltconsulting/click-dog:26.04.1 test tls --collector otel.example.com:4317 --json" \
    "$(cat "$run_args_file")"
docker() { return 125; }
preflight_check
assert_eq "an image docker cannot run is only a note" "0" "$preflight_rc"
assert_contains "the note names the image" "docker could not run ghcr.io/coltconsulting/click-dog:26.04.1" "$preflight_out"
# shellcheck disable=SC2034
CLICK_DOG_RUN_IMAGE=""
unset -f docker transport_of guided_transport
unset FAKE_CD_INIT_ARGS
OTEL_SECURE="" OTEL_CA_CERT="" OTEL_USES_TLS=""

echo ""
echo "=== OTLP receiver TLS: guided mismatch handling ==="

tls_json plaintext non-tls "otel.example.com:4317 answered the TLS handshake with non-TLS bytes" > "$FAKE_CD_OUT"
OTEL_SECURE="true"
confirm_otel_tls_choice otel.example.com 4317 <<<"p" >/dev/null
assert_eq "[p] on a plaintext receiver switches to plaintext" "false" "$OTEL_SECURE"
OTEL_SECURE="true"
confirm_otel_tls_choice otel.example.com 4317 <<<"c" >/dev/null
assert_eq "[c] keeps the TLS answer" "true" "$OTEL_SECURE"
guided_rc=0
guided_out=$(confirm_otel_tls_choice otel.example.com 4317 <<<"q" 2>&1) || guided_rc=$?
assert_eq "[q] exits cleanly" "0" "$guided_rc"
assert_contains "[q] aborts the guided install" "Aborted." "$guided_out"
assert_contains "guided check explains the plaintext receiver" "does not speak TLS" "$guided_out"
assert_not_contains "a non-TLS answer needs no proxy caveat" "Plaintext is right only if" "$guided_out"

# A silent close does not lead with plaintext: both causes are named, the
# prompt offers continuing with TLS first, and [p] still works.
tls_json plaintext closed "$CLOSED_MESSAGE" > "$FAKE_CD_OUT"
OTEL_SECURE="true"
guided_out=$(confirm_otel_tls_choice otel.example.com 4317 <<<"c" 2>&1)
assert_contains "guided silent close names a TLS proxy" "serves no certificate for 'otel.example.com' (SNI)" "$guided_out"
assert_contains "guided silent close says when plaintext is right" "Plaintext is right only if this port really is plaintext" "$guided_out"
assert_eq "[c] after a silent close keeps TLS" "true" "$OTEL_SECURE"
confirm_otel_tls_choice otel.example.com 4317 <<<"p" >/dev/null
assert_eq "[p] after a silent close still switches" "false" "$OTEL_SECURE"
assert_contains "a silent close offers TLS first" '[c]ontinue with TLS, [p]laintext instead, or [q]uit?' \
    "$(grep -F '[c]ontinue with TLS, [p]laintext instead' deploy/install.sh)"

# Without a terminal there is no one to read the certificate: no trust on
# first use, only today's message. stdin here is a pipe.
tls_json untrusted unknown-authority "$SELF_SIGNED" > "$FAKE_CD_OUT"
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
tls_chain san > "$FAKE_CD_CHAIN"
OTEL_SECURE="true"
guided_out=$(confirm_otel_tls_choice otel.example.com 4317 <<<"y" 2>&1)
assert_not_contains "no trust on first use without a TTY" "Trust this certificate" "$guided_out"
assert_contains "guided untrusted cert names the fix" "exporters.otel[].ca_cert" "$guided_out"
assert_contains "guided untrusted cert says where to add it" "/etc/click-dog/click-dog.yaml" "$guided_out"
assert_not_contains "plaintext is not offered for a TLS receiver" "[p]laintext" "$guided_out"
confirm_otel_tls_choice otel.example.com 4317 <<<"y" >/dev/null
assert_eq "continuing keeps TLS" "true" "$OTEL_SECURE"
assert_eq "continuing pins nothing" "" "$OTEL_CA_CERT"

tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT"
guided_out=$(confirm_otel_tls_choice otel.example.com 4317 </dev/null 2>&1)
assert_contains "trusted TLS needs no answer" "ok, certificate trusted" "$guided_out"

tls_json unknown timeout "no TLS answer within 10s" > "$FAKE_CD_OUT"
guided_out=$(confirm_otel_tls_choice otel.example.com 4317 </dev/null 2>&1)
assert_contains "an inconclusive check is skipped with its reason" "skipped (no TLS answer within 10s)" "$guided_out"

echo ""
echo "=== OTLP receiver TLS: guided trust on first use ==="

# Pretend a person is at the terminal; answers still come from here-strings.
stdin_is_tty() { return 0; }
tofu_reset() { OTEL_SECURE="true"; OTEL_CA_CERT=""; OTEL_CA_PIN_TMP=""; }
tofu_out="$tls_sandbox/tofu.out"
tofu_temp_count() {
    find "${TMPDIR:-/tmp}" -maxdepth 1 \( -name 'click-dog-otel-ca.*' -o -name 'click-dog-otel-chain.*' \) 2>/dev/null | wc -l | tr -d ' '
}

# Self-signed with a matching SAN: details, fingerprint, the check command,
# then an explicit yes pins the certificate itself.
tls_json untrusted unknown-authority "$SELF_SIGNED" > "$FAKE_CD_OUT"
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
tls_chain san > "$FAKE_CD_CHAIN"
san_fp=$("$REAL_OPENSSL" x509 -in "$tls_sandbox/san.crt" -noout -fingerprint -sha256)
san_fp="${san_fp#*=}"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "trust prompt shows the subject" "Subject:     CN=otel.example.com" "$tofu_text"
assert_contains "trust prompt shows the issuer" "Issuer:" "$tofu_text"
assert_contains "trust prompt shows the SANs" "DNS:otel.example.com" "$tofu_text"
assert_contains "trust prompt shows validity" "Not after:" "$tofu_text"
assert_contains "trust prompt shows the SHA-256 fingerprint" "SHA-256:     $san_fp" "$tofu_text"
assert_contains "trust prompt says how to check it" "openssl x509 -in <cert> -noout -fingerprint -sha256" "$tofu_text"
assert_not_contains "trust prompt does not echo chain-file headers" "# Certificate" "$tofu_text"
assert_contains "yes pins the certificate" "Will install it as /etc/click-dog/tls/ca.pem" "$tofu_text"
assert_eq "yes sets the CA to the pin" "$OTEL_CA_PIN_TMP" "$OTEL_CA_CERT"
assert_eq "self-signed pin is the certificate itself" "yes" "$(same_cert "$tls_sandbox/san.crt" "$OTEL_CA_CERT")"
assert_contains "the pin is proven with a handshake trusting it alone" "--ca-cert $OTEL_CA_CERT" "$(cat "$FAKE_CD_ARGS")"
assert_not_contains "a self-signed pin gets no renewal note" "stops verifying when it is" "$tofu_text"
rm -f "$OTEL_CA_PIN_TMP"

# A CA-issued leaf sent without its chain anchors itself, as it does for Go,
# so it can be pinned, but renewal breaks that pin: say so before the prompt.
tls_json untrusted unknown-authority "certificate signed by unknown authority" > "$FAKE_CD_OUT"
tls_chain leaf > "$FAKE_CD_CHAIN"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"n"$'\ny' > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "a leaf-only pin warns about renewal" "export stops verifying when it is" "$tofu_text"
assert_contains "a leaf-only pin points to the issuing CA" "--otel-ca-cert FILE" "$tofu_text"

# read -p shows its prompt only on a real terminal, so the [y/N] text is
# asserted from the source; behavior: Enter declines, and so does n. Both
# fall back to today's message and leave no temp file behind.
assert_contains "trust prompt defaults to no" 'Trust this certificate? [y/N]' "$(grep -F 'Trust this certificate?' deploy/install.sh)"
tls_chain san > "$FAKE_CD_CHAIN"
tofu_tmp_before=$(tofu_temp_count)
for tofu_answer in "" "n"; do
    tofu_reset
    confirm_otel_tls_choice otel.example.com 4317 <<<"${tofu_answer}"$'\ny' > "$tofu_out" 2>&1
    assert_eq "answer '${tofu_answer:-Enter}' pins nothing" "" "$OTEL_CA_CERT"
    assert_contains "answer '${tofu_answer:-Enter}' keeps today's message" \
        "To finish, add those files under exporters.otel" "$(cat "$tofu_out")"
done
assert_eq "declined pins leave no temp file" "$tofu_tmp_before" "$(tofu_temp_count)"

# A chain pins its top-most certificate, which survives leaf rotation.
tls_chain leaf ca > "$FAKE_CD_CHAIN"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "chain shows the certificate to trust" "the top of the chain it sent" "$tofu_text"
assert_contains "chain shows the top certificate's subject" "Subject:     CN=Test Collector CA" "$tofu_text"
assert_eq "chain pin is the top certificate sent" "yes" "$(same_cert "$tls_sandbox/ca.crt" "$OTEL_CA_CERT")"
rm -f "$OTEL_CA_PIN_TMP"

# Expired or not-yet-valid: shown, never offered.
for tofu_case in "expired|certificate has expired: not after 2026-01-02 00:00:00 UTC|has expired" \
    "not-yet-valid|certificate is not yet valid: not before 2030-01-02 00:00:00 UTC|is not valid yet"; do
    IFS='|' read -r tofu_reason tofu_message tofu_says <<<"$tofu_case"
    tls_json untrusted "$tofu_reason" "$tofu_message" > "$FAKE_CD_OUT"
    tls_json untrusted "$tofu_reason" "$tofu_message" > "$FAKE_CD_OUT_CA"
    tls_chain san > "$FAKE_CD_CHAIN"
    tofu_reset
    confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
    tofu_text=$(cat "$tofu_out")
    assert_contains "refusal explains: it $tofu_says" "Not offering to trust it: it $tofu_says ($tofu_message)" "$tofu_text"
    assert_not_contains "no trust prompt when it $tofu_says" "Will install it as" "$tofu_text"
    assert_not_contains "no CA advice when it $tofu_says" "To finish, add those files" "$tofu_text"
    assert_eq "nothing pinned when it $tofu_says" "" "$OTEL_CA_CERT"
done

# CN-only: the binary says no-san straight away, so nothing is offered —
# pinning cannot fix a missing SAN.
tls_json no-san "" "the certificate has no subjectAltName; click-dog ignores the Common Name" > "$FAKE_CD_OUT"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "CN-only certificate is explained" "has no subjectAltName" "$tofu_text"
assert_not_contains "CN-only certificate is never offered" "Not offering to trust it" "$tofu_text"
assert_not_contains "CN-only certificate gets no trust prompt" "Will install it as" "$tofu_text"
assert_eq "CN-only certificate is never pinned" "" "$OTEL_CA_CERT"

# A pin the binary cannot check is not offered either.
tls_json untrusted unknown-authority "$SELF_SIGNED" > "$FAKE_CD_OUT"
tls_json unknown config "failed to parse OTEL CA certificate" > "$FAKE_CD_OUT_CA"
tls_chain san > "$FAKE_CD_CHAIN"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y"$'\ny' > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "an unproven pin is refused with a reason" "click-dog could not prove it would verify the collector (failed to parse" "$tofu_text"
assert_eq "an unproven pin pins nothing" "" "$OTEL_CA_CERT"

# A CA file click-dog cannot use ends the guided run: continuing would only
# install a config the service cannot start with.
# With --ca-cert the fake replays FAKE_CD_OUT_CA.
tls_json unknown config "failed to parse OTEL CA certificate" > "$FAKE_CD_OUT_CA"
tofu_reset
OTEL_CA_CERT="$tls_sandbox/ca.crt"
config_rc=0
( confirm_otel_tls_choice otel.example.com 4317 <<<"y" ) > "$tofu_out" 2>&1 || config_rc=$?
tofu_text=$(cat "$tofu_out")
assert_eq "an unusable CA aborts the guided run" "1" "$config_rc"
assert_contains "the guided run says why" "cannot use $tls_sandbox/ca.crt as the collector's CA" "$tofu_text"
assert_not_contains "an unusable CA is not offered as continuable" "Continue with TLS?" "$tofu_text"
OTEL_CA_CERT=""

# No certificate arrived for the second look: nothing to show or pin.
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
rm -f "$FAKE_CD_CHAIN"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y"$'\ny' > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_not_contains "no chain, no trust prompt" "Trust this certificate" "$tofu_text"
assert_contains "no chain keeps today's message" "To finish, add those files under exporters.otel" "$tofu_text"
assert_eq "no chain pins nothing" "" "$OTEL_CA_CERT"

# A wrong name is not a trust problem either.
tls_json untrusted hostname-mismatch "hostname mismatch; certificate names DNS:other.example.com" > "$FAKE_CD_OUT"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
tofu_text=$(cat "$tofu_out")
assert_contains "wrong-name certificate is explained" "is not issued for 'otel.example.com'" "$tofu_text"
assert_not_contains "wrong-name certificate is never offered" "Will install it as" "$tofu_text"

# Trust on first use follows reason codes too. Reworded details change
# nothing: an expired pin is refused as expired, a mismatch is never
# offered, and an unknown CA described in alarming words is still offered.
tls_json untrusted expired "the validity window closed" > "$FAKE_CD_OUT"
tls_json untrusted expired "the validity window closed" > "$FAKE_CD_OUT_CA"
tls_chain san > "$FAKE_CD_CHAIN"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
assert_contains "a reworded expiry is refused as expired" "Not offering to trust it: it has expired (the validity window closed)" "$(cat "$tofu_out")"
tls_json untrusted hostname-mismatch "names other hosts entirely" > "$FAKE_CD_OUT"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
assert_not_contains "a reworded mismatch is never offered" "The collector presented" "$(cat "$tofu_out")"
tls_json untrusted unknown-authority "hostname mismatch, expired, not yet valid: none of these" > "$FAKE_CD_OUT"
tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
tofu_reset
confirm_otel_tls_choice otel.example.com 4317 <<<"y" > "$tofu_out" 2>&1
assert_contains "an unknown CA is offered whatever its wording" "Will install it as /etc/click-dog/tls/ca.pem" "$(cat "$tofu_out")"
rm -f "$OTEL_CA_PIN_TMP"

unset -f stdin_is_tty tofu_reset tofu_temp_count
rm -f "$FAKE_CD_OUT_CA" "$FAKE_CD_CHAIN"
OTEL_CA_CERT="" OTEL_CA_PIN_TMP=""

# verify_installed_otel_ca re-runs the handshake against the installed file.
(
    OTEL_CA_CERT_PATH="$tls_sandbox/installed-ca.pem"
    tls_json ok "" "$OK_MESSAGE" > "$FAKE_CD_OUT_CA"
    verify_installed_otel_ca otel.example.com 4317 && echo "rc=0"
    echo "args=$(cat "$FAKE_CD_ARGS")"
    tls_json untrusted unknown-authority "certificate signed by unknown authority" > "$FAKE_CD_OUT_CA"
    verify_installed_otel_ca otel.example.com 4317 || echo "rc=$?"
    tls_json unknown unreachable "no answer from otel.example.com:4317 (connect: connection refused)" > "$FAKE_CD_OUT_CA"
    verify_installed_otel_ca otel.example.com 4317 && echo "unknown-rc=0"
    tls_json unknown config "failed to parse OTEL CA certificate" > "$FAKE_CD_OUT_CA"
    verify_installed_otel_ca otel.example.com 4317 || echo "config-rc=$?"
    rm -f "$FAKE_CD_OUT_CA"
) > "$tls_sandbox/verify-installed.out" 2>&1
verify_installed_out=$(cat "$tls_sandbox/verify-installed.out")
assert_contains "the installed CA is re-checked" "args=test tls --collector otel.example.com:4317 --json --ca-cert $tls_sandbox/installed-ca.pem" "$verify_installed_out"
assert_contains "a re-check that passes returns 0" "rc=0" "$verify_installed_out"
assert_contains "a re-check that fails returns 1" "rc=1" "$verify_installed_out"
assert_contains "a failed re-check explains itself" "does not verify the certificate" "$verify_installed_out"
assert_contains "an inconclusive re-check is only a note" "unknown-rc=0" "$verify_installed_out"
assert_contains "an unusable installed CA returns 2, reachable or not" "config-rc=2" "$verify_installed_out"

echo ""
echo "=== OTLP receiver TLS: installing the CA ==="

# The guided path's EXIT handler removes every temp file it made: the
# password-hash setup files, a pinned CA's copy, and its config temp files.
(
    BINARY_AUTO_DOWNLOADED="false"
    cleanup_dir="$tls_sandbox/exit-cleanup"
    mkdir -p "$cleanup_dir"
    for f in setup.sql user.xml pin.pem config secret unit; do : > "$cleanup_dir/$f"; done
    clickhouse_setup_tempfiles=("$cleanup_dir/setup.sql" "$cleanup_dir/user.xml")
    OTEL_CA_PIN_TMP="$cleanup_dir/pin.pem"
    CONFIG_TMPFILE="$cleanup_dir/config" SECRET_TMPFILE="$cleanup_dir/secret" UNIT_TMPFILE="$cleanup_dir/unit"
    quickstart_exit_cleanup
    echo "left=$(find "$cleanup_dir" -type f | wc -l | tr -d ' ')"
    # Nothing made yet: still a clean no-op under set -u. The handler that
    # reads these arrives via eval, so the linter sees only the writes.
    # shellcheck disable=SC2034
    clickhouse_setup_tempfiles=()
    # shellcheck disable=SC2034
    OTEL_CA_PIN_TMP="" CONFIG_TMPFILE="" SECRET_TMPFILE="" UNIT_TMPFILE=""
    quickstart_exit_cleanup && echo "empty=ok"
) > "$tls_sandbox/exit-cleanup.out" 2>&1
assert_contains "guided exit removes its temp files" "left=0" "$(cat "$tls_sandbox/exit-cleanup.out")"
assert_contains "guided exit with nothing to remove is a no-op" "empty=ok" "$(cat "$tls_sandbox/exit-cleanup.out")"

# resolve_binary's trap replaces the caller's while it downloads, so it runs
# the caller's handler too, and hands the trap back when it is done.
(
    _RESOLVE_BINARY_ARCHIVE_TMP="" _RESOLVE_BINARY_VERIFY_DIR="" _RESOLVE_BINARY_DOWNLOAD_DIR=""
    outer_ran() { echo "outer ran"; }
    # shellcheck disable=SC2034  # read by _resolve_binary_cleanup, via eval
    OUTER_EXIT_CLEANUP=outer_ran
    _resolve_binary_cleanup
) > "$tls_sandbox/outer-cleanup.out"
assert_eq "a failed download runs the caller's EXIT handler" "outer ran" "$(cat "$tls_sandbox/outer-cleanup.out")"
resolve_body=$(awk '/^resolve_binary\(\)/,/^}/' deploy/install.sh)
assert_contains "resolve_binary hands the trap back" 'trap "$OUTER_EXIT_CLEANUP" EXIT' "$resolve_body"
tofu_body=$(awk '/^offer_otel_tofu\(\)/,/^}/' deploy/install.sh)
assert_not_contains "trust on first use sets no trap of its own" "trap " "$tofu_body"

# A failed guided reinstall puts back the CA the previous config names.
(
    OTEL_CA_CERT_PATH="$tls_sandbox/restore/ca.pem"
    mkdir -p "${OTEL_CA_CERT_PATH%/*}"
    echo new > "$OTEL_CA_CERT_PATH"
    echo old > "${OTEL_CA_CERT_PATH}.bak"
    restore_otel_ca_backup
    echo "ca=$(cat "$OTEL_CA_CERT_PATH") bak=$([[ -e "${OTEL_CA_CERT_PATH}.bak" ]] && echo kept || echo gone)"
    rm -f "$OTEL_CA_CERT_PATH"
    restore_otel_ca_backup && echo "no-backup=ok"
) > "$tls_sandbox/restore.out"
assert_contains "restore puts the previous CA back" "ca=old bak=gone" "$(cat "$tls_sandbox/restore.out")"
(
    OTEL_CA_CERT_PATH="$tls_sandbox/backup/ca.pem"
    mkdir -p "${OTEL_CA_CERT_PATH%/*}"
    echo current > "$OTEL_CA_CERT_PATH"
    backup_otel_ca
    echo "bak=$(cat "${OTEL_CA_CERT_PATH}.bak")"
    rm -f "$OTEL_CA_CERT_PATH"
    backup_otel_ca
    echo "stale=$([[ -e "${OTEL_CA_CERT_PATH}.bak" ]] && echo kept || echo dropped)"
) > "$tls_sandbox/backup.out"
assert_contains "backup keeps the current CA" "bak=current" "$(cat "$tls_sandbox/backup.out")"
assert_contains "no current CA drops a stale backup" "stale=dropped" "$(cat "$tls_sandbox/backup.out")"
assert_contains "restore without a backup is a no-op" "no-backup=ok" "$(cat "$tls_sandbox/restore.out")"

# install_otel_ca_cert places the file the config names, for the runtime
# user. chown needs root and a click-dog account, so it is stubbed; the
# modes are real.
(
    chown() { :; }
    OTEL_CA_CERT_PATH="$tls_sandbox/etc/tls/ca.pem"
    OTEL_CA_CERT="$tls_sandbox/san.crt"
    install_otel_ca_cert
    echo "copied=$(same_cert "$OTEL_CA_CERT" "$OTEL_CA_CERT_PATH")"
    echo "file=$(find "$OTEL_CA_CERT_PATH" -perm 0640 | wc -l | tr -d ' ')"
    echo "dir=$(find "${OTEL_CA_CERT_PATH%/*}" -prune -perm 0750 | wc -l | tr -d ' ')"
    # Re-running with the installed file itself must not trip cp.
    OTEL_CA_CERT="$OTEL_CA_CERT_PATH"
    install_otel_ca_cert && echo "same-file=ok"
) > "$tls_sandbox/install-ca.out" 2>&1 || true
install_ca_out=$(cat "$tls_sandbox/install-ca.out")
assert_contains "CA is copied where the config points" "copied=yes" "$install_ca_out"
assert_contains "CA file is 0640" "file=1" "$install_ca_out"
assert_contains "CA directory is 0750" "dir=1" "$install_ca_out"
assert_contains "re-installing the installed file is a no-op copy" "same-file=ok" "$install_ca_out"

unset -f make_test_cert tls_json tls_chain ca_file_check same_cert self_issued preflight_check
BINARY="$saved_BINARY"
unset FAKE_CD_ARGS FAKE_CD_OUT FAKE_CD_OUT_CA FAKE_CD_CHAIN REAL_OPENSSL
OTEL_SECURE=""
COLLECTOR_ADDRESS="$saved_COLLECTOR"

echo ""
echo "=== OTLP receiver TLS: wiring and flag scope ==="

install_body=$(awk '/^do_install\(\)/,/^}/' deploy/install.sh)
line_of() { grep -nF -- "$1" <<<"$install_body" | head -1 | cut -d: -f1; }
ca_check_line=$(line_of 'validate_otel_ca_file "$OTEL_CA_CERT" || exit 1')
preflight_line=$(line_of 'preflight_otel_tls || exit 1')
download_line=$(grep -n '^    resolve_binary$' <<<"$install_body" | cut -d: -f1)
install_trap_line=$(line_of "trap 'rm -f \"\$CONFIG_TMPFILE\"")
backup_ca_line=$(line_of '        backup_otel_ca')
place_ca_line=$(line_of '        install_otel_ca_cert')
validate_line=$(line_of ' validate --config /etc/click-dog/click-dog.yaml')
in_order() {
    local prev=0 n
    for n in "$@"; do
        if [[ -z "$n" || "$n" -le "$prev" ]]; then
            echo false
            return
        fi
        prev="$n"
    done
    echo true
}
# The handshake runs the resolved binary, so it follows the download; the
# EXIT trap that removes a download is set before it can stop the install.
assert_eq "install checks the CA file, downloads, sets its trap, asks the binary, then checks the handshake" "true" \
    "$(in_order "$ca_check_line" "$download_line" "$install_trap_line" \
        "$(line_of 'resolve_otel_transport || exit 1')" "$preflight_line")"
assert_contains "install checks the handshake whenever the transport is TLS" \
    'if [[ "$OTEL_USES_TLS" == "true" ]]; then' "$install_body"
assert_eq "install checks the handshake before writing anything" "true" \
    "$(in_order "$preflight_line" "$(line_of 'useradd --system')" "$backup_ca_line")"
assert_eq "install backs up, places, then validates the CA" "true" \
    "$(in_order "$backup_ca_line" "$place_ca_line" "$validate_line")"
assert_contains "install restores the CA when validation fails" \
    '[[ -n "$OTEL_CA_CERT" ]] && restore_otel_ca_backup' "$install_body"
assert_contains "install drops the CA backup once validation passes" \
    'rm -f "${OTEL_CA_CERT_PATH}.bak"' "$install_body"
assert_contains "install guards the new init flag" 'require_init_flag otel-ca-cert' "$install_body"

guided_body=$(awk '/^do_quickstart\(\)/,/^# ── Dispatch/' deploy/install.sh)
guided_line_of() { grep -nF -- "$1" <<<"$guided_body" | head -1 | cut -d: -f1; }
assert_contains "guided collector step chooses the transport" 'guided_otel_transport' "$guided_body"
guided_transport_body=$(awk '/^guided_otel_transport\(\)/,/^}/' deploy/install.sh)
assert_contains "guided transport step asks the binary" 'resolve_otel_transport || exit 1' "$guided_transport_body"
assert_contains "guided transport step asks about TLS" 'prompt_otel_tls' "$guided_transport_body"
assert_contains "guided reachability check proves a TLS answer" 'confirm_otel_tls_choice "$col_host" "$col_port"' "$guided_body"
assert_contains "guided handshake follows the transport, not the flag" 'if [[ "$OTEL_USES_TLS" == "true" ]]; then' "$guided_body"
assert_eq "guided install chooses the transport after fetching click-dog" "true" \
    "$(in_order "$(guided_line_of 'Fetching click-dog')" "$(guided_line_of '    guided_otel_transport')")"
# The binary the collector check runs is fetched once, before step 2.
assert_eq "guided install fetches click-dog before the collector step" "true" \
    "$(in_order "$(guided_line_of 'Fetching click-dog')" "$(guided_line_of 'Step 2/3 — OTEL collector')" \
        "$(guided_line_of 'confirm_otel_tls_choice "$col_host" "$col_port"')")"
assert_eq "guided install resolves the binary once outside Update" "1" \
    "$(grep -c '^    resolve_binary$' <<<"$guided_body")"
assert_contains "guided path checks an operator CA file" 'validate_otel_ca_file "$OTEL_CA_CERT" || exit 1' "$guided_transport_body"
assert_contains "guided install places the CA" 'install_otel_ca_cert' "$guided_body"
assert_contains "guided install re-runs the handshake against it" 'verify_installed_otel_ca "$col_host" "$col_port"' "$guided_body"
# An unusable CA stops the guided install even when the collector was
# unreachable and no pin exists: only the pin case used to stop it.
assert_contains "guided install stops on an unusable CA, pin or not" '[[ "$verify_rc" -eq 2 ]]' "$guided_body"
assert_contains "guided install cleans up an unused pin" '"${OTEL_CA_PIN_TMP:-}"' \
    "$(awk '/^quickstart_exit_cleanup\(\)/,/^}/' deploy/install.sh)"
assert_eq "guided install backs up the CA before replacing it" "true" \
    "$(in_order "$(guided_line_of '        backup_otel_ca')" \
        "$(guided_line_of '        install_otel_ca_cert')")"
assert_contains "guided install sets its EXIT handler" 'trap quickstart_exit_cleanup EXIT' "$guided_body"
assert_eq "the not-running ending keeps the setup files it names" "true" \
    "$(in_order "$(guided_line_of 'clickhouse_setup_tempfiles=()' | tail -1)" \
        "$(guided_line_of 'Installed, but not yet running')")"
assert_eq "guided temp file list is reset in two places" "2" \
    "$(grep -c '^ *clickhouse_setup_tempfiles=()$' <<<"$guided_body")"
# The unusable-CA stop, the pin that no longer verifies, and the failed
# config validation each restore the CA the install replaced.
assert_eq "every guided failure exit restores the CA" "3" \
    "$(grep -c '^ *restore_otel_ca_backup$' <<<"$guided_body")"
assert_contains "guided success drops the CA backup" '"${OTEL_CA_CERT_PATH}.bak"' \
    "$(grep -F 'rm -f /etc/click-dog/click-dog.yaml.bak' <<<"$guided_body")"

# Render: the config names the installed path, never the source file.
saved_BINARY="${BINARY:-}"
BINARY="$STUB_BIN"
OTEL_SECURE="true" OTEL_CA_CERT="/root/collector-ca.pem"
ca_render=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "render names the installed CA" "--otel-ca-cert /etc/click-dog/tls/ca.pem" "$ca_render"
assert_not_contains "render never names the source CA" "/root/collector-ca.pem" "$ca_render"
OTEL_SECURE="" OTEL_CA_CERT=""
BINARY="$saved_BINARY"

# require_init_flag refuses a release whose init lacks the flag.
flag_stub_dir=$(make_temp_dir)
TEST_TEMP_DIRS+=("$flag_stub_dir")
printf '#!/bin/sh\necho "  --otel-secure"\n' > "$flag_stub_dir/old"
printf '#!/bin/sh\necho "  --otel-secure"\necho "  --otel-ca-cert string"\n' > "$flag_stub_dir/new"
chmod +x "$flag_stub_dir/old" "$flag_stub_dir/new"
flag_rc=0
flag_out=$(BINARY="$flag_stub_dir/old"; require_init_flag otel-ca-cert 2>&1) || flag_rc=$?
assert_eq "release without --otel-ca-cert is refused" "1" "$flag_rc"
assert_contains "refusal names the missing flag" "predates 'click-dog init --otel-ca-cert'" "$flag_out"
flag_rc=0
(BINARY="$flag_stub_dir/new"; require_init_flag otel-ca-cert) >/dev/null 2>&1 || flag_rc=$?
assert_eq "release with --otel-ca-cert is accepted" "0" "$flag_rc"

# kubernetes takes --otel-secure (do_kubernetes passes it to the generator;
# argv covered below). Run without -c so it stops before Docker either way:
# what matters is that the option parser does not refuse it.
flag_out=$(bash "$SCRIPT_DIR/install.sh" kubernetes --otel-secure 2>&1) || true
assert_not_contains "kubernetes accepts --otel-secure" "Error: --otel-secure" "$flag_out"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" kubernetes update --otel-secure 2>&1) || flag_rc=$?
assert_eq "kubernetes update rejects --otel-secure" "1" "$flag_rc"
assert_contains "kubernetes update rejection names kubernetes" "install, docker, kubernetes" "$flag_out"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" update --otel-secure 2>&1) || flag_rc=$?
assert_eq "update rejects --otel-secure" "1" "$flag_rc"
assert_contains "update rejection names the valid commands" "only valid with install, docker" "$flag_out"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" docker update --otel-secure 2>&1) || flag_rc=$?
assert_eq "docker update rejects --otel-secure" "1" "$flag_rc"

flag_ca="$tls_sandbox/san.crt"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" docker -c otel:4317 --otel-ca-cert "$flag_ca" 2>&1) || flag_rc=$?
assert_eq "docker rejects --otel-ca-cert" "1" "$flag_rc"
assert_contains "docker rejection explains the mount" "mounts only click-dog.yaml" "$flag_out"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" kubernetes --otel-ca-cert "$flag_ca" 2>&1) || flag_rc=$?
assert_eq "kubernetes rejects --otel-ca-cert" "1" "$flag_rc"
assert_contains "kubernetes CA refusal points at the mounted ConfigMap" "click-dog-otel-ca" "$flag_out"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" update --otel-ca-cert "$flag_ca" 2>&1) || flag_rc=$?
assert_eq "update rejects --otel-ca-cert" "1" "$flag_rc"

flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel:4317 --otel-ca-cert 2>&1) || flag_rc=$?
assert_eq "--otel-ca-cert without a file is refused" "1" "$flag_rc"
assert_contains "missing file is named" "--otel-ca-cert needs a FILE" "$flag_out"

byo_config="$tls_sandbox/byo.yaml"
: > "$byo_config"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -f "$byo_config" --otel-secure 2>&1) || flag_rc=$?
assert_eq "install -f rejects --otel-secure" "1" "$flag_rc"
assert_contains "-f rejection points at the file" "$byo_config instead" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -f "$byo_config" --otel-ca-cert "$flag_ca" 2>&1) || flag_rc=$?
assert_eq "install -f rejects --otel-ca-cert" "1" "$flag_rc"
assert_contains "-f rejection names ca_cert" "(and ca_cert)" "$flag_out"

# --otel-secure is parsed strictly: '=true', '=false' or bare (true). A
# separate word is refused before anything runs, because Go's flag package
# would read `--otel-secure false` as true plus a stray word.
for accepted in "--otel-secure" "--otel-secure=true" "--otel-secure=false"; do
    flag_out=$(bash "$SCRIPT_DIR/install.sh" kubernetes "$accepted" 2>&1) || true
    assert_not_contains "kubernetes accepts $accepted" "Error: --otel-secure" "$flag_out"
    assert_not_contains "$accepted leaves nothing over" "unexpected argument" "$flag_out"
done
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel.internal:4317 --otel-secure false 2>&1) || flag_rc=$?
assert_eq "--otel-secure false is refused" "1" "$flag_rc"
assert_contains "the refusal names the = form" "write --otel-secure=false" "$flag_out"
assert_not_contains "the refusal comes before anything runs" "click-dog install:" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel.internal:4317 --otel-secure TRUE 2>&1) || flag_rc=$?
assert_eq "--otel-secure TRUE is refused" "1" "$flag_rc"
assert_contains "the refusal lower-cases the value" "write --otel-secure=true" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel.internal:4317 --otel-secure=yes 2>&1) || flag_rc=$?
assert_eq "--otel-secure=yes is refused" "1" "$flag_rc"
assert_contains "the refusal names both values" "use --otel-secure=true or --otel-secure=false" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" update --otel-secure=false 2>&1) || flag_rc=$?
assert_eq "update rejects --otel-secure=false" "1" "$flag_rc"
assert_contains "the = form is scoped like the bare flag" "only valid with install, docker" "$flag_out"

# Anything left after the options is refused: getopts stops at the first
# word, and every option after it would be dropped silently.
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel.internal:4317 stray --systemd 2>&1) || flag_rc=$?
assert_eq "a leftover argument is refused" "1" "$flag_rc"
assert_contains "the refusal names it" "unexpected argument 'stray'" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c otel.internal:4317 false -v 26.04.1 2>&1) || flag_rc=$?
assert_eq "a leftover false is refused" "1" "$flag_rc"
assert_contains "a leftover false points to the = form" "e.g. --otel-secure=false" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" -c localhost:4317 extra 2>&1) || flag_rc=$?
assert_eq "the guided path refuses leftovers too" "1" "$flag_rc"
assert_contains "the guided refusal names it" "unexpected argument 'extra'" "$flag_out"

# A CA implies TLS: with an explicit false it is refused whichever comes first.
for conflict in "--otel-secure=false --otel-ca-cert $tls_sandbox/san.crt" \
                "--otel-ca-cert $tls_sandbox/san.crt --otel-secure=false"; do
    flag_rc=0
    # shellcheck disable=SC2086  # the case is two flags and their values
    flag_out=$(bash "$SCRIPT_DIR/install.sh" install -c localhost:4317 $conflict 2>&1) || flag_rc=$?
    assert_eq "CA with false is refused ($conflict)" "1" "$flag_rc"
    assert_contains "the CA refusal says why ($conflict)" "--otel-ca-cert implies TLS, so it cannot combine with --otel-secure=false" "$flag_out"
done
# ...while a CA with an explicit true is consistent.
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -f /nonexistent --otel-ca-cert "$tls_sandbox/san.crt" --otel-secure=true 2>&1) || true
assert_not_contains "a CA with --otel-secure=true is no conflict" "cannot combine with --otel-secure=false" "$flag_out"
flag_rc=0
flag_out=$(bash "$SCRIPT_DIR/install.sh" install -f "$byo_config" --otel-secure=false 2>&1) || flag_rc=$?
assert_eq "install -f rejects --otel-secure=false" "1" "$flag_rc"
assert_eq "the -f refusal comes first" "0" "$(grep -c 'implies TLS' <<<"$flag_out" || true)"

usage_out=$(bash "$SCRIPT_DIR/install.sh" -h 2>&1) || true
assert_contains "help documents --otel-secure" "--otel-secure" "$usage_out"
assert_contains "help documents --otel-ca-cert" "--otel-ca-cert FILE" "$usage_out"

echo ""
echo "=== click-dog invocations use --long flags ==="

# The binary's --version and --config flags are passed from many call sites;
# none may use the one-dash spelling, and validation is the validate
# subcommand, never a -validate flag. Comments are
# skipped; the init -h help probe greps for a substring, not a flag.
one_dash_calls=$(grep -nE '(^|[ "])-(version|validate|config)( |$|")' deploy/install.sh \
    | grep -vE '^[0-9]+:[[:space:]]*#' || true)
assert_eq "no one-dash -version/-validate/-config call" "" "$one_dash_calls"

echo ""
echo "=== legacy installer uninstall delegates safely ==="

# The compatibility verb must delegate confirmation and removal to the
# installed Go command. In particular, the shell must never bypass its
# installer-ownership checks with direct userdel/groupdel calls.
uninstall_sandbox=$(make_temp_dir)
TEST_TEMP_DIRS+=("$uninstall_sandbox")
uninstall_delegate_log="$uninstall_sandbox/delegate.log"
cat > "$uninstall_sandbox/click-dog" <<'STUB'
#!/bin/sh
if [ "${UNINSTALL_UNSUPPORTED:-}" = "1" ]; then
    exit 2
fi
if [ "$*" = "deploy uninstall --help" ]; then
    exit 0
fi
printf '%s\n' "$*" > "$UNINSTALL_DELEGATE_LOG"
STUB
chmod +x "$uninstall_sandbox/click-dog"

cat > "$uninstall_sandbox/run-uninstall.sh" <<'HARNESS'
#!/usr/bin/env bash
set -euo pipefail

if ! grep -q '^# ── Dispatch' "$INSTALL_SCRIPT"; then
    echo "installer dispatch marker not found" >&2
    exit 1
fi
installer_source=$(sed -n '1,/^# ── Dispatch/p' "$INSTALL_SCRIPT" \
    | grep -v '^set -euo pipefail')
eval "$installer_source"

CLICK_DOG_BINARY="$UNINSTALL_BINARY"
UNINSTALL_YES="${UNINSTALL_YES_OVERRIDE:-false}"
do_uninstall
HARNESS
chmod +x "$uninstall_sandbox/run-uninstall.sh"

uninstall_status=0
uninstall_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" \
    UNINSTALL_BINARY="$uninstall_sandbox/click-dog" \
    UNINSTALL_DELEGATE_LOG="$uninstall_delegate_log" \
    bash "$uninstall_sandbox/run-uninstall.sh" 2>&1) || uninstall_status="$?"
assert_eq "legacy uninstall propagates delegate success" "0" "$uninstall_status"
assert_eq "legacy uninstall adds no competing confirmation flow" "" "$uninstall_out"
assert_eq "legacy uninstall delegates exact command" "deploy uninstall" "$(<"$uninstall_delegate_log")"
uninstall_body=$(sed -n '/^do_uninstall() {/,/^}/p' "$SCRIPT_DIR/install.sh")
assert_contains "legacy uninstall can forward non-interactive confirmation" "--yes" "$uninstall_body"
assert_not_contains "legacy uninstall never deletes a pre-existing user directly" "userdel" "$uninstall_body"
assert_not_contains "legacy uninstall never deletes a pre-existing group directly" "groupdel" "$uninstall_body"

uninstall_yes_status=0
uninstall_yes_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" \
    UNINSTALL_BINARY="$uninstall_sandbox/click-dog" \
    UNINSTALL_DELEGATE_LOG="$uninstall_delegate_log" \
    UNINSTALL_YES_OVERRIDE=true \
    bash "$uninstall_sandbox/run-uninstall.sh" 2>&1) || uninstall_yes_status="$?"
assert_eq "legacy uninstall --yes propagates delegate success" "0" "$uninstall_yes_status"
assert_eq "legacy uninstall --yes adds no competing confirmation flow" "" "$uninstall_yes_out"
assert_eq "legacy uninstall --yes forwards confirmation" "deploy uninstall --yes" "$(<"$uninstall_delegate_log")"

install_source=$(<"$SCRIPT_DIR/install.sh")
assert_contains "legacy uninstall parser accepts --yes" '--yes)' "$install_source"
assert_contains "legacy uninstall parser scopes --yes" '--yes is only valid with uninstall' "$install_source"

unsupported_status=0
unsupported_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" \
    UNINSTALL_BINARY="$uninstall_sandbox/click-dog" \
    UNINSTALL_DELEGATE_LOG="$uninstall_delegate_log" \
    UNINSTALL_UNSUPPORTED=1 \
    bash "$uninstall_sandbox/run-uninstall.sh" 2>&1) || unsupported_status="$?"
assert_eq "legacy uninstall rejects an unsupported installed binary" "1" "$unsupported_status"
assert_contains "legacy uninstall gives an actionable upgrade command" \
    "sudo bash install.sh update" "$unsupported_out"

echo ""
echo "=== install -f flag (bring your own YAML) ==="

# When -f PATH is set, do_install should skip the require-collector check
# and skip the render_click_dog_config call entirely. We can't exercise
# the full do_install (it needs root + systemd), but we can verify the
# flag plumbing: CONFIG_FROM_FILE is set, the getopts string accepts -f,
# and the usage docstring mentions the flag.

# getopts string must include f: so the flag accepts an argument.
# Use grep -m1 (exit-on-first-match) rather than `| head -1` — under
# `set -euo pipefail` a head-closed pipe can SIGPIPE the producer and
# kill the script intermittently.
getopts_str=$(grep -m1 -E "^while getopts " deploy/install.sh)
assert_contains "getopts accepts -f with arg" "f:" "$getopts_str"

# Usage docstring must document -f as the "bring your own YAML" flag.
usage_lines=$(sed -n '1,45p' deploy/install.sh)
assert_contains "usage documents -f flag" "-f PATH" "$usage_lines"
assert_contains "usage flag mentions wizard pairing" "wizard" "$usage_lines"

# The do_install function must branch on CONFIG_FROM_FILE — either the
# copy branch or the render-via-binary branch fires, never both.
install_body=$(awk '/^do_install\(\)/,/^}/' deploy/install.sh)
assert_contains "do_install copies CONFIG_FROM_FILE when set" 'cp "$CONFIG_FROM_FILE" "$CONFIG_TMPFILE"' "$install_body"
assert_contains "do_install keeps the render path for the no-flag case" "render_click_dog_config" "$install_body"
assert_contains "do_install validates -f path exists" "Error: -f file not found" "$install_body"

echo ""
echo "=== successful install/update exit status (issue #382) ==="

# Drive the real do_install/do_update functions to completion without root
# access. The command shims redirect staging work into a private temp directory
# and no-op only the writes to system paths. This catches failures that occur
# after the completion message — specifically EXIT traps that dereference
# function-local variables after the command function has returned.
install_sandbox=$(make_temp_dir)
TEST_TEMP_DIRS+=("$install_sandbox")
printf 'monitor:\n  enabled: true\n' > "$install_sandbox/input.yaml"
printf '[Unit]\nDescription=click-dog test unit\n' > "$install_sandbox/click-dog.service"
# The stub answers --print-otel-secure as the fake TLS binary above does,
# logs each render's init argv to $HARNESS_LOG, and logs each handshake.
cat > "$install_sandbox/click-dog" <<STUB
#!/bin/sh
$FAKE_PRINT_OTEL_SECURE
STUB
cat >> "$install_sandbox/click-dog" <<'STUB'
case "$1" in
    --version|validate) exit 0 ;;
    init)
        shift
        case "$1" in
            --print-otel-secure) fake_print_otel_secure "$@" ;;
            -h) echo "  --ch-user string"; echo "  --otel-ca-cert string"; exit 0 ;;
        esac
        echo "render $*" >> "${HARNESS_LOG:-/dev/null}"
        exit 0
        ;;
    test)
        echo "handshake $*" >> "${HARNESS_LOG:-/dev/null}"
        printf '{\n  "verdict": "%s",\n  "reason": "%s",\n  "message": "%s",\n  "certificates": []\n}\n' \
            "${HARNESS_TLS_VERDICT:-ok}" "${HARNESS_TLS_REASON:-}" "${HARNESS_TLS_MESSAGE:-}"
        exit 1
        ;;
    *) exit 1 ;;
esac
STUB
chmod +x "$install_sandbox/click-dog"

cat > "$install_sandbox/run-install.sh" <<'HARNESS'
#!/usr/bin/env bash
set -euo pipefail

sandbox="$INSTALL_SANDBOX"

# Load the real installer prelude and functions without its dispatch block.
# Fail closed if the extraction boundary changes: sed would otherwise print
# the whole script and eval its live command dispatch.
if ! grep -q '^# ── Dispatch' "$INSTALL_SCRIPT"; then
    echo "installer dispatch marker not found" >&2
    exit 1
fi
# do_update's systemd-unit existence guard is the only direct filesystem test
# that command shims cannot intercept. Point that guard at the fake unit while
# leaving the production function otherwise unchanged.
installer_source=$(sed -n '1,/^# ── Dispatch/p' "$INSTALL_SCRIPT" \
    | grep -v '^set -euo pipefail' \
    | sed "s|/etc/systemd/system/click-dog.service|$sandbox/click-dog.service|g")
# The guide needs root and looks for an installed binary.
if [[ "$HARNESS_ACTION" == "quickstart" ]]; then
    installer_source=$(sed -e 's/\$EUID -ne 0/0 -ne 0/' -e "s|/usr/local/bin/click-dog|$sandbox/bin/click-dog|g" <<<"$installer_source")
fi
eval "$installer_source"

CONFIG_FROM_FILE="$sandbox/input.yaml"
CLICKHOUSE_PASSWORD="${HARNESS_CH_PASSWORD-testpass123}"
COLLECTOR_ADDRESS=""
SYSTEMD="false"
BINARY="$sandbox/click-dog"
BINARY_AUTO_DOWNLOADED="false"
BINARY_AUTO_DOWNLOAD_DIR=""

uname() { echo "Linux"; }
file() { echo "ELF 64-bit LSB executable"; }
id() { return 0; }
useradd() { return 0; }
chown() { return 0; }
# A service that fails its restart reports active until it has had time to die.
rm -f "$sandbox/restarted" "$sandbox/settled"
systemctl() {
    case "$1" in
        restart) : > "$sandbox/restarted" ;;
        is-active)
            [[ -z "${HARNESS_SERVICE_STOPPED:-}" ]] || return 3
            [[ -z "${HARNESS_RESTART_FAILS:-}" || ! -e "$sandbox/settled" ]] || return 3
            ;;
    esac
    return 0
}
sleep() {
    if [[ -e "$sandbox/restarted" ]]; then
        : > "$sandbox/settled"
    fi
    return 0
}

mkdir() {
    if [[ "$*" == *"/etc/click-dog"* || "$*" == *"/usr/local/bin"* ]]; then
        echo "mkdir $*" >> "$sandbox/system-writes"
        return 0
    fi
    command mkdir "$@"
}
chmod() {
    local target="${!#}"
    if [[ "$target" == "$sandbox/"* ]]; then
        command chmod "$@"
    fi
}
mktemp() {
    if [[ "${1:-}" == "-d" ]]; then
        case "${2:-}" in
            /usr/local/bin/.click-dog-install.XXXXXX)
                command mktemp -d "$sandbox/install-stage.XXXXXX"
                return
                ;;
            /usr/local/bin/.click-dog-update.XXXXXX)
                command mktemp -d "$sandbox/update-stage.XXXXXX"
                return
                ;;
        esac
    fi
    if [[ "${1:-}" == /tmp/click-dog-quickstart-update.* ]]; then
        command mktemp "$sandbox/quickstart-update.XXXXXX"
        return
    fi
    command mktemp "$@"
}
cp() {
    local target="${!#}"
    if [[ "$target" == /etc/* || "$target" == /usr/local/bin/* ]]; then
        echo "cp $target" >> "$sandbox/system-writes"
        return 0
    fi
    command cp "$@"
}
mv() {
    if [[ "${2:-}" == "/usr/local/bin/click-dog" ]]; then
        command rm -f "$1"
        return 0
    fi
    command mv "$@"
}

case "$HARNESS_ACTION" in
    install) do_install ;;
    install-tls)
        CONFIG_FROM_FILE=""
        COLLECTOR_ADDRESS="${HARNESS_COLLECTOR:-otel.example.com:4317}"
        # Unset means --otel-secure; set and empty means no flag.
        OTEL_SECURE="${HARNESS_OTEL_SECURE-true}"
        OTEL_CA_CERT="${HARNESS_CA:-}"
        do_install
        ;;
    update)  do_update ;;
    quickstart)
        require_quickstart_tty() { return 0; }
        openssl() { return 1; }
        docker() { return "${HARNESS_LOGIN_RC:-0}"; }
        CLICKHOUSE_USERS_DIR="$sandbox/ch/users.d"
        CLICKHOUSE_CLIENT="${HARNESS_CLIENT:-$sandbox/fake-login}"
        do_quickstart
        ;;
    *) echo "unknown harness action: $HARNESS_ACTION" >&2; exit 1 ;;
esac
HARNESS

install_status=0
install_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=install \
    bash "$install_sandbox/run-install.sh" 2>&1) || install_status="$?"
assert_eq "successful do_install exits 0" "0" "$install_status"
assert_contains "successful do_install reaches completion" "Install complete." "$install_out"
assert_not_contains "successful do_install has no unbound trap variable" "unbound variable" "$install_out"

# --otel-secure gates the install on the handshake the resolved binary makes:
# a refusal stops it before anything reaches /etc or /usr/local/bin.
rm -f "$install_sandbox/system-writes"
tls_gate_status=0
tls_gate_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=install-tls HARNESS_TLS_VERDICT=untrusted HARNESS_TLS_REASON=unknown-authority \
    HARNESS_TLS_MESSAGE="certificate signed by unknown authority (self-signed)" \
    bash "$install_sandbox/run-install.sh" 2>&1) || tls_gate_status="$?"
assert_eq "a refused handshake stops do_install" "1" "$tls_gate_status"
assert_contains "the refusal names the flag and receiver" \
    "Error: --otel-secure was given, but the TLS handshake with otel.example.com:4317 failed." "$tls_gate_out"
assert_contains "the refusal carries the binary's reason" "(certificate signed by unknown authority (self-signed))" "$tls_gate_out"
assert_contains "the refusal routes an unknown CA to --otel-ca-cert" "Pass --otel-ca-cert FILE" "$tls_gate_out"
assert_eq "nothing is written before the gate" "no" \
    "$([[ -s "$install_sandbox/system-writes" ]] && echo yes || echo no)"

# run_tls_install VAR=VALUE... — the unattended install against a fake
# receiver, logging renders and handshakes to $harness_log.
harness_log="$install_sandbox/harness.log"
run_tls_install() {
    rm -f "$install_sandbox/system-writes"
    : > "$harness_log"
    tls_install_status=0
    tls_install_out=$(env INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
        HARNESS_ACTION=install-tls HARNESS_LOG="$harness_log" "$@" \
        bash "$install_sandbox/run-install.sh" 2>&1) || tls_install_status="$?"
    tls_install_log=$(cat "$harness_log")
}

# install -c otel.example.com:4317 with no --otel-secure: the binary says TLS,
# the handshake runs before anything is written, and init gets no flag, so it
# renders `secure: true` by the rule (cmd_init_test.go covers that output).
run_tls_install HARNESS_OTEL_SECURE=
assert_eq "an automatic TLS install completes" "0" "$tls_install_status"
assert_contains "the automatic choice is checked by handshake" \
    "handshake test tls --collector otel.example.com:4317 --json" "$tls_install_log"
assert_eq "the handshake comes before the render" "true" \
    "$(in_order "$(grep -n '^handshake' <<<"$tls_install_log" | cut -d: -f1)" "$(grep -n '^render' <<<"$tls_install_log" | cut -d: -f1)")"
assert_not_contains "an automatic choice passes no --otel-secure to init" "-otel-secure" "$(grep '^render' <<<"$tls_install_log")"
assert_contains "the status line names the automatic TLS choice" "(OTLP: TLS, automatic;" "$tls_install_out"

# ...and a plaintext receiver fails it, naming --otel-secure=false.
run_tls_install HARNESS_OTEL_SECURE= HARNESS_TLS_VERDICT=plaintext HARNESS_TLS_REASON=non-tls \
    HARNESS_TLS_MESSAGE="otel.example.com:4317 answered the TLS handshake with non-TLS bytes"
assert_eq "a plaintext receiver stops an automatic TLS install" "1" "$tls_install_status"
assert_contains "the stop names --otel-secure=false as the fix" "pass --otel-secure=false" "$tls_install_out"
assert_eq "nothing is written before the automatic gate" "no" \
    "$([[ -s "$install_sandbox/system-writes" ]] && echo yes || echo no)"
assert_not_contains "nothing is rendered before the automatic gate" "render" "$tls_install_log"

# --otel-secure=false to a remote collector: rendered explicitly, no handshake.
run_tls_install HARNESS_OTEL_SECURE=false HARNESS_COLLECTOR=otel.internal:4317
assert_eq "an explicit plaintext install completes" "0" "$tls_install_status"
assert_not_contains "explicit plaintext makes no handshake" "handshake" "$tls_install_log"
assert_contains "explicit plaintext reaches init with =" "--otel-secure=false" "$(grep '^render' <<<"$tls_install_log")"
assert_contains "the status line names the explicit choice" "(OTLP: plaintext, --otel-secure=false;" "$tls_install_out"

# A loopback collector left automatic: plaintext, no handshake, no flag.
run_tls_install HARNESS_OTEL_SECURE= HARNESS_COLLECTOR=localhost:4317
assert_eq "an automatic loopback install completes" "0" "$tls_install_status"
assert_not_contains "automatic loopback makes no handshake" "handshake" "$tls_install_log"
assert_not_contains "automatic loopback passes no --otel-secure" "-otel-secure" "$(grep '^render' <<<"$tls_install_log")"

# install -c localhost:4317 --otel-ca-cert ca.pem: the CA makes it TLS,
# checked against that CA, and init renders the installed CA path.
run_tls_install HARNESS_OTEL_SECURE= HARNESS_COLLECTOR=localhost:4317 HARNESS_CA="$tls_sandbox/san.crt"
assert_eq "a loopback install with a CA completes" "0" "$tls_install_status"
assert_contains "a CA makes loopback TLS, checked against it" \
    "handshake test tls --collector localhost:4317 --json --ca-cert $tls_sandbox/san.crt" "$tls_install_log"
assert_contains "a CA reaches init as the installed path" "--otel-ca-cert /etc/click-dog/tls/ca.pem" "$(grep '^render' <<<"$tls_install_log")"
assert_not_contains "a CA alone passes no --otel-secure" "-otel-secure" "$(grep '^render' <<<"$tls_install_log")"

# A release that cannot answer --print-otel-secure stops before writing.
run_tls_install HARNESS_OTEL_SECURE= FAKE_CD_PRINT=none
assert_eq "a binary without --print-otel-secure stops the install" "1" "$tls_install_status"
assert_contains "the stop names the missing command" "'click-dog init --print-otel-secure'" "$tls_install_out"
assert_eq "nothing is written without a transport" "no" \
    "$([[ -s "$install_sandbox/system-writes" ]] && echo yes || echo no)"

update_status=0
update_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=update \
    bash "$install_sandbox/run-install.sh" 2>&1) || update_status="$?"
assert_eq "successful do_update exits 0" "0" "$update_status"
assert_contains "successful do_update reaches completion" "Update complete." "$update_out"
assert_not_contains "successful do_update has no unbound trap variable" "unbound variable" "$update_out"

# A service that fails after the restart looks active at first.
update_status=0
update_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=update HARNESS_RESTART_FAILS=1 \
    bash "$install_sandbox/run-install.sh" 2>&1) || update_status="$?"
assert_eq "an update whose service dies exits non-zero" "1" "$update_status"
assert_contains "an update whose service dies says so" "click-dog is not running" "$update_out"
assert_not_contains "an update whose service dies claims nothing" "Update complete." "$update_out"

update_status=0
update_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=update HARNESS_SERVICE_STOPPED=1 \
    bash "$install_sandbox/run-install.sh" 2>&1) || update_status="$?"
assert_eq "an update of a stopped service exits 0" "0" "$update_status"
assert_contains "an update of a stopped service says so" "so it stays stopped" "$update_out"
assert_eq "an update leaves a stopped service stopped" "no" \
    "$([[ -e "$install_sandbox/restarted" ]] && echo yes || echo no)"

echo ""
echo "=== the guide's stops, on the real do_quickstart ==="

mkdir -p "$install_sandbox/ch/users.d" "$install_sandbox/bin"
printf '#!/bin/sh\nexit "${HARNESS_LOGIN_RC:-0}"\n' > "$install_sandbox/fake-login"
chmod +x "$install_sandbox/fake-login"
# run_guide ANSWERS VAR=VALUE... — runs the guide until it stops or stdin ends.
run_guide() {
    local answers="$1"
    shift
    guide_status=0
    guide_out=$(printf '%b' "$answers" | env INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
        HARNESS_ACTION=quickstart "$@" bash "$install_sandbox/run-install.sh" 2>&1) || guide_status="$?"
}
guide_bad_file="$install_sandbox/ch/users.d/click-dog-old.xml"

echo '<query>GRANT SELECT ON system.query_log TO mon</query>' > "$guide_bad_file"
run_guide '\n'
assert_eq "step 1 stops on a file ClickHouse cannot load" "1" "$guide_status"
assert_contains "step 1 names the file" "$guide_bad_file" "$guide_out"
assert_contains "step 1 says to delete it" "rm FILE" "$guide_out"
assert_not_contains "step 1 stops before the span-log check" "Checking ClickHouse OTEL span logging" "$guide_out"
rm -f "$guide_bad_file"
run_guide '\n'
assert_contains "step 1 goes on when no file stops ClickHouse" "Checking ClickHouse OTEL span logging" "$guide_out"
assert_contains "step 1 says when it cannot check the files" "cannot check that ClickHouse can read its config files" "$guide_out"

# CLICKHOUSE_PASSWORD replaces .secret in step 3, so a refused one stops here.
run_guide '\ns\n' HARNESS_LOGIN_RC=4
assert_eq "a refused CLICKHOUSE_PASSWORD stops the guide" "1" "$guide_status"
assert_contains "a refused CLICKHOUSE_PASSWORD says so" "ClickHouse refused user 'click_dog_monitor' with CLICKHOUSE_PASSWORD" "$guide_out"
assert_not_contains "a refused CLICKHOUSE_PASSWORD stops before the span-log check" "Verifying span logging" "$guide_out"
assert_not_contains "a refused CLICKHOUSE_PASSWORD stops before [w] can restart ClickHouse" "Checking ClickHouse OTEL span logging" "$guide_out"
assert_contains "a refused CLICKHOUSE_PASSWORD names the client config" "A password in the client's config beats CLICKHOUSE_PASSWORD" "$guide_out"
run_guide '\ns\n' HARNESS_LOGIN_RC=0
assert_contains "an accepted CLICKHOUSE_PASSWORD goes on" "Verifying span logging" "$guide_out"
run_guide '\ns\n' HARNESS_LOGIN_RC=210
assert_contains "no answer from ClickHouse does not stop the guide" "Verifying span logging" "$guide_out"
run_guide '\ns\n' HARNESS_LOGIN_RC=4 "HARNESS_CLIENT=docker exec c clickhouse-client"
assert_contains "a wrapper that drops the password does not stop the guide" "Verifying span logging" "$guide_out"
run_guide '\ns\n' HARNESS_LOGIN_RC=4 "HARNESS_CLIENT=docker exec -e CLICKHOUSE_PASSWORD c clickhouse-client"
assert_eq "a wrapper that forwards the password can stop the guide" "1" "$guide_status"

# The guide's update option, with the same dying service.
cp "$install_sandbox/click-dog" "$install_sandbox/bin/click-dog"
run_guide 'u\n'
assert_eq "the guide's update finishes" "0" "$guide_status"
assert_contains "the guide's update reports a service that stays up" "click-dog is running." "$guide_out"
cp "$install_sandbox/click-dog" "$install_sandbox/bin/click-dog"
run_guide 'u\n' HARNESS_RESTART_FAILS=1
assert_contains "the guide's update reports a service that dies" "service did not start" "$guide_out"
assert_eq "the guide's update exits 1 when the service dies" "1" "$guide_status"
assert_contains "the guide's update names the prior binary" "click-dog.prev" "$guide_out"
assert_not_contains "the guide's update claims nothing for a service that dies" "click-dog is running." "$guide_out"

# A reinstall says what happens to the saved password.
cp "$install_sandbox/click-dog" "$install_sandbox/bin/click-dog"
echo '<query>GRANT SELECT ON system.query_log TO mon</query>' > "$guide_bad_file"
run_guide 'r\n\n'
assert_contains "a reinstall with CLICKHOUSE_PASSWORD says it replaces the saved one" "CLICKHOUSE_PASSWORD replaces" "$guide_out"
assert_not_contains "a reinstall with CLICKHOUSE_PASSWORD does not say it is kept" "are kept so the" "$guide_out"
run_guide 'r\n\n' HARNESS_CH_PASSWORD=
assert_contains "a reinstall with no password keeps the saved one" "are kept so the" "$guide_out"
assert_contains "the menu with no password keeps the saved one" "[r] Reinstall — overwrite config and service; keep existing credentials" "$guide_out"
run_guide 'q\n'
assert_contains "the menu with CLICKHOUSE_PASSWORD says it replaces the saved one" "[r] Reinstall — overwrite config and service; CLICKHOUSE_PASSWORD replaces the saved password" "$guide_out"
assert_not_contains "the menu with CLICKHOUSE_PASSWORD does not say it keeps the saved one" "keep existing credentials" "$guide_out"
rm -f "$guide_bad_file" "$install_sandbox/bin/click-dog"

echo ""
echo "=== the installer's environment ==="

env_out=$(env CLICKHOUSE_HOST=ops CLICKHOUSE_USER=ops INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" bash -c '
    eval "$(sed -n "1,/^# ── Dispatch/p" "$INSTALL_SCRIPT" | grep -v "^set -euo pipefail")"
    printenv CLICKHOUSE_HOST || echo unset
    printenv CLICKHOUSE_USER || echo unset
')
assert_eq "the client does not inherit the script's host and user" "unset
unset" "$env_out"

echo ""
echo "=== install_secret_file ==="

sf_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$sf_tmp")
printf 'pw' > "$sf_tmp/src"
chmod 644 "$sf_tmp/src"
sf_log=$(
    chown() { echo "chown $*"; }
    # shellcheck disable=SC2034 # read by the eval'd function
    SECRET_FILE="$sf_tmp/.secret"
    install_secret_file "$sf_tmp/src"
)
assert_eq "the password file goes to click-dog's user" "chown click-dog:click-dog $sf_tmp/.secret" "$sf_log"
# shellcheck disable=SC2012 # one known file
assert_eq "the password file is private" "-rw-------" "$(ls -l "$sf_tmp/.secret" | cut -c1-10)"
assert_eq "the password file holds the password" "pw" "$(cat "$sf_tmp/.secret")"

echo ""
echo "=== require_unified_init_binary ==="

# Pre-unified click-dog releases don't know -ch-user / -ch-secure /
# -ha-keeper. require_unified_init_binary probes `$BINARY init -h` for
# one of those flag names and refuses if absent, so the user sees a
# version-skew message instead of Go's "flag provided but not defined"
# halfway through an install.
saved_BINARY="${BINARY:-}"

# Pre-unified stub: prints help that lacks the new flags.
OLD_STUB=$(make_temp_dir)/click-dog-old
TEST_TEMP_DIRS+=("$(dirname "$OLD_STUB")")
cat > "$OLD_STUB" <<'STUB'
#!/bin/sh
echo "Usage: click-dog init [flags]"
echo "  -ch-host string"
echo "  -collector string"
echo "  -service string"
STUB
chmod +x "$OLD_STUB"
BINARY="$OLD_STUB"
old_out=$(VERSION=v25.04.1 require_unified_init_binary 2>&1) || old_status="$?"
assert_eq "pre-unified binary refused (exit nonzero)" "1" "${old_status:-0}"
assert_contains "diagnostic mentions the version-skew origin" "predates this install.sh" "$old_out"

# Unified stubs: early unified releases print one-dash flags in their help,
# current ones --flag (printFlagDefaults); both must be accepted.
for help_dash in "-" "--"; do
    NEW_STUB=$(make_temp_dir)/click-dog-new
    TEST_TEMP_DIRS+=("$(dirname "$NEW_STUB")")
    cat > "$NEW_STUB" <<STUB
#!/bin/sh
echo "Usage: click-dog init [flags]"
echo "  ${help_dash}ch-user string"
echo "  ${help_dash}ch-secure"
echo "  ${help_dash}ha-keeper string"
STUB
    chmod +x "$NEW_STUB"
    BINARY="$NEW_STUB"
    new_status="0"
    require_unified_init_binary >/dev/null 2>&1 || new_status="$?"
    assert_eq "unified binary with ${help_dash}ch-user help accepted (exit 0)" "0" "$new_status"
done

unset old_status new_status
BINARY="$saved_BINARY"

echo ""
echo "=== generate_systemd_unit ==="

unit=$(generate_systemd_unit)
assert_contains "has ExecStart" "ExecStart=/usr/local/bin/click-dog --config /etc/click-dog/click-dog.yaml" "$unit"
assert_contains "has Restart=on-failure" "Restart=on-failure" "$unit"
assert_contains "has StartLimitBurst" "StartLimitBurst=5" "$unit"
assert_contains "start limits live in [Unit]" $'StartLimitBurst=5\n\n[Service]' "$unit"
# The secret is read by click-dog via clickhouse.password_file, not injected
# into the service environment, so the unit must NOT carry an EnvironmentFile.
assert_not_contains "no EnvironmentFile (password_file instead)" "EnvironmentFile" "$unit"
assert_contains "has User=click-dog" "User=click-dog" "$unit"
assert_not_contains "no insecure-no-tls flag" "insecure-no-tls" "$unit"
assert_not_contains "no Restart=always" "Restart=always" "$unit"

echo ""
echo "=== installer-owned runtime identity markers ==="

identity_sandbox=$(make_temp_dir)
TEST_TEMP_DIRS+=("$identity_sandbox")
saved_state_dir="$INSTALL_STATE_DIR"
saved_user_marker="$INSTALLER_CREATED_USER_MARKER"
saved_group_marker="$INSTALLER_CREATED_GROUP_MARKER"
INSTALL_STATE_DIR="$identity_sandbox/state"
INSTALLER_CREATED_USER_MARKER="$INSTALL_STATE_DIR/installer-created-user"
INSTALLER_CREATED_GROUP_MARKER="$INSTALL_STATE_DIR/installer-created-group"

(
    id() { echo "1201"; }
    getent() { echo "click-dog:x:1202:"; }
    chown() { :; }
    record_installer_created_runtime_identity "false"
)
assert_eq "created user marker records numeric uid" "1201" "$(<"$INSTALLER_CREATED_USER_MARKER")"
assert_eq "created private-group marker records numeric gid" "1202" "$(<"$INSTALLER_CREATED_GROUP_MARKER")"

rm -rf "$INSTALL_STATE_DIR"
(
    id() { echo "1301"; }
    getent() { echo "click-dog:x:1302:"; }
    chown() { :; }
    record_installer_created_runtime_identity "true"
)
assert_eq "created user is still recorded with a preexisting group" "1301" "$(<"$INSTALLER_CREATED_USER_MARKER")"
if [[ ! -e "$INSTALLER_CREATED_GROUP_MARKER" ]]; then
    assert_eq "preexisting group is not marked installer-owned" "absent" "absent"
else
    assert_eq "preexisting group is not marked installer-owned" "absent" "present"
fi

INSTALL_STATE_DIR="$saved_state_dir"
INSTALLER_CREATED_USER_MARKER="$saved_user_marker"
INSTALLER_CREATED_GROUP_MARKER="$saved_group_marker"

installer_text=$(<"$SCRIPT_DIR/install.sh")
assert_contains "guided handoff uses installed uninstaller" \
    "To remove Click-Dog later: sudo click-dog deploy uninstall" "$installer_text"
assert_not_contains "guided handoff drops checkout-only uninstall path" \
    "To undo everything: ./deploy/install.sh uninstall" "$installer_text"

echo ""
echo "=== quickstart update keeps update safeguards ==="

quickstart_body=$(awk '/^do_quickstart\(\)/,/^# ── Dispatch/' deploy/install.sh)
assert_contains "quickstart update stages the binary before swap" \
    "click-dog-quickstart-update" "$quickstart_body"
assert_contains "quickstart update rejects downgrades" \
    "refusing to downgrade" "$quickstart_body"
assert_contains "quickstart update validates existing config" \
    "validate --config /etc/click-dog/click-dog.yaml" "$quickstart_body"
assert_contains "quickstart update keeps a .prev rollback binary" \
    "click-dog.prev" "$quickstart_body"
assert_contains "quickstart update cleans auto-downloaded binary" \
    'cleanup_auto_downloaded_binary' "$quickstart_body"

echo ""
echo "=== install.sh temp staging hardening (issue #293 M1/L3) ==="

resolve_body=$(awk '/^resolve_binary\(\)/,/^}/' deploy/install.sh)
assert_contains "resolve_binary uses a private random download dir" \
    'mktemp -d "${TMPDIR:-/tmp}/click-dog-binary.XXXXXX"' "$resolve_body"
assert_contains "resolve_binary extracts inside the private dir" \
    'tar xzf "$archive_tmp" -C "$download_dir" click-dog' "$resolve_body"
assert_not_contains "resolve_binary no longer uses pid-based /tmp binary" \
    'BINARY="/tmp/click-dog-$$"' "$resolve_body"
assert_not_contains "resolve_binary no longer extracts fixed /tmp/click-dog" \
    '"/tmp/click-dog"' "$resolve_body"

install_body=$(awk '/^do_install\(\)/,/^}/' deploy/install.sh)
assert_contains "do_install stages under destination dir" \
    'mktemp -d /usr/local/bin/.click-dog-install.XXXXXX' "$install_body"
assert_not_contains "do_install no fixed /tmp install binary" \
    '/tmp/click-dog-install' "$install_body"

update_body=$(awk '/^do_update\(\)/,/^}/' deploy/install.sh)
assert_contains "do_update stages under destination dir" \
    'mktemp -d /usr/local/bin/.click-dog-update.XXXXXX' "$update_body"
assert_not_contains "do_update no fixed /tmp update binary" \
    '/tmp/click-dog-update' "$update_body"

assert_contains "secure temp helper chmods temp files 600" \
    'chmod 600 "$tmp"' "$(awk '/^secure_temp_file\(\)/,/^}/' deploy/install.sh)"
assert_contains "quickstart setup SQL uses secure temp helper" \
    'secure_temp_file "click-dog-setup"' "$quickstart_body"
assert_not_contains "quickstart no fixed setup SQL path" \
    '/tmp/click-dog-setup.sql' "$quickstart_body"
assert_not_contains "quickstart no fixed user XML path" \
    '/tmp/click-dog-${CLICKHOUSE_USER}-user.xml' "$quickstart_body"
assert_not_contains "quickstart no fixed span-log config path" \
    '/tmp/click-dog-opentelemetry' "$quickstart_body"
assert_contains "manual span-log copy keeps loadable extension" \
    '$(clickhouse_file_install_cmd) ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}' "$quickstart_body"
assert_contains "manual users.d copy keeps loadable extension" \
    '$(clickhouse_file_install_cmd) ${user_xml} /etc/clickhouse-server/users.d/click-dog-${CLICKHOUSE_USER}.xml' "$quickstart_body"
# ClickHouse refuses to start with an unreadable file in config.d or users.d,
# and the temp files are 0600: a plain cp as root carries that over.
assert_not_contains "nothing copies a temp file into ClickHouse's config with cp" \
    'cp "$otel_cfg_file"' "$quickstart_body"
assert_not_contains "nothing copies the user overlay with cp" \
    'cp "$user_xml"' "$quickstart_body"
assert_eq "ClickHouse files go to its group, unreadable to others" \
    "install -m 640 -o root -g clickhouse" \
    "$(systemctl() { return 1; }; getent() { return 0; }; clickhouse_file_install_cmd)"
assert_eq "without a clickhouse group the file has to be world-readable" \
    "install -m 644 -o root -g root" \
    "$(systemctl() { return 1; }; getent() { return 2; }; clickhouse_file_install_cmd)"
assert_eq "files for ClickHouse go to the group its unit runs as" \
    "install -m 640 -o root -g chsvc" \
    "$(systemctl() { [[ "$3" == "Group" ]] && echo chsvc; }; getent() { return 0; }; clickhouse_file_install_cmd)"
assert_eq "a unit with only a user gives that user's group" \
    "install -m 640 -o root -g svcgroup" \
    "$(systemctl() { [[ "$3" == "User" ]] && echo chsvc; }; id() { echo svcgroup; }; getent() { return 0; }; clickhouse_file_install_cmd)"
assert_eq "a unit naming a group this host lacks falls back to clickhouse" "install -m 640 -o root -g clickhouse" \
    "$(systemctl() { [[ "$3" == "Group" ]] && echo nosuch; }; getent() { [[ "$2" == "clickhouse" ]]; }; clickhouse_file_install_cmd)"
assert_eq "a unit naming a group this host lacks, and no clickhouse group, is world-readable" "install -m 644 -o root -g root" \
    "$(systemctl() { [[ "$3" == "Group" ]] && echo nosuch; }; getent() { return 2; }; clickhouse_file_install_cmd)"

echo ""
echo "=== do_kubernetes (delegates to click-dog deploy kubernetes) ==="

# do_kubernetes now runs the single Go generator via the published image rather
# than rendering bash/envsubst templates (the manifest content itself is covered
# by cmd_deploy_generate_test.go). Stub `docker` to capture the argv it would
# invoke — `command -v docker` finds the function and `docker run` calls it.
# The stub answers the image's `init -h` with $k8s_init_help and its
# `init --print-otel-secure` through the fake binary above (logging both to
# $k8s_log), and records every other call as the generator run, also
# announced on stdout so a subshell run can tell whether it happened.
k8s_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$k8s_tmp")
k8s_log="$k8s_tmp/docker.log"
k8s_init_help="  --print-otel-secure"
k8s_docker_args=""
docker() {
    case " $* " in
        *" init -h "*)
            echo "help $*" >> "$k8s_log"
            printf '%s\n' "$k8s_init_help"
            return 0
            ;;
        *" init --print-otel-secure "*)
            echo "print $*" >> "$k8s_log"
            shift 5
            "$tls_sandbox/click-dog" "$@"
            return
            ;;
    esac
    k8s_docker_args="$*"
    echo "stub: generator ran"
}
# No terminal unless a case says so.
stdin_is_tty() { return 1; }

# do_kubernetes reads these; it comes from the eval in source_functions, so the
# linter sees the writes and none of the reads.
# shellcheck disable=SC2034
OUTPUT_DIR="$k8s_tmp/out"
COLLECTOR_ADDRESS="otel-collector:4317"
CLICKHOUSE_PASSWORD="testpass123"
CLICKHOUSE_USER="click_dog_monitor"
CLICKHOUSE_HOST="clickhouse.clickhouse.svc.cluster.local"
# shellcheck disable=SC2034
CLICKHOUSE_CLUSTER="main"
VERSION="26.04.1"
# shellcheck disable=SC2034
SUBCOMMAND=""
OTEL_SECURE=""

: > "$k8s_log"
do_kubernetes > /dev/null

# Must delegate to the single generator via the published image, threading the
# ClickHouse Service host + cluster (the #199 fix) — never the old DaemonSet.
assert_contains "runs the published image"        "ghcr.io/coltconsulting/click-dog:26.04.1"        "$k8s_docker_args"
assert_contains "invokes deploy kubernetes"        "deploy kubernetes"                                "$k8s_docker_args"
assert_contains "threads --ch-host Service DNS"    "--ch-host clickhouse.clickhouse.svc.cluster.local" "$k8s_docker_args"
assert_contains "threads --cluster"                "--cluster main"                                   "$k8s_docker_args"
assert_contains "writes into the mounted /work"    "-o /work"                                         "$k8s_docker_args"
assert_contains "passes collector"                 "-c otel-collector:4317"                           "$k8s_docker_args"
assert_contains "passes username"                  "-u click_dog_monitor"                             "$k8s_docker_args"
assert_not_match "deploy kubernetes gets only --long flags" '(^| )-[a-z][a-z-]+' \
    "${k8s_docker_args#*deploy kubernetes }"
assert_contains "password via env, not argv"       "-e CLICKHOUSE_PASSWORD"                            "$k8s_docker_args"
assert_not_contains "password not baked into argv" "testpass123"                                      "$k8s_docker_args"
# Without the flag the generator decides (and refuses a collector off
# loopback, below); install.sh adds nothing of its own.
assert_not_contains "no --otel-secure unless given" "-otel-secure"                                    "$k8s_docker_args"
assert_contains "generator runs as the invoker"    "run --rm --user $(id -u):$(id -g)"                "$k8s_docker_args"
assert_contains "the image guard checks init for --print-otel-secure" \
    "help run --rm ghcr.io/coltconsulting/click-dog:26.04.1 init -h" "$(cat "$k8s_log")"
assert_not_contains "no terminal, no transport question to the image" "print " "$(cat "$k8s_log")"

# --otel-secure (bare) and --otel-secure=false reach the generator with '='.
OTEL_SECURE="true"
do_kubernetes > /dev/null
assert_contains "passes --otel-secure=true"        "--otel-secure=true"                               "${k8s_docker_args#*deploy kubernetes }"
assert_not_match "secure argv keeps --long flags"  '(^| )-[a-z][a-z-]+'                               "${k8s_docker_args#*deploy kubernetes }"
OTEL_SECURE="false"
do_kubernetes > /dev/null
assert_contains "passes --otel-secure=false"       "--otel-secure=false"                              "${k8s_docker_args#*deploy kubernetes }"

# kubernetes -c otel-collector.monitoring:4317 with no flag and no terminal:
# the generator's own refusal stops the run, in its words. The stub replays
# that refusal for a generator run without --otel-secure=.
OTEL_SECURE=""
COLLECTOR_ADDRESS="otel-collector.monitoring:4317"
k8s_refuse_rc=0
k8s_refuse_out=$(
    docker() {
        if [[ " $* " == *" init -h "* ]]; then
            echo "  --print-otel-secure"
            return 0
        fi
        if [[ " $* " == *" --otel-secure="* ]]; then
            echo "stub: generator ran"
            return 0
        fi
        echo "Error: collector otel-collector.monitoring:4317 is not on loopback, and the generator cannot see its network to choose a transport." >&2
        echo "  Pass --otel-secure=true for a TLS receiver, or --otel-secure=false for plaintext." >&2
        echo "  In-cluster and Compose collectors (an OpenTelemetry Collector or Datadog Agent on 4317) usually serve plaintext" >&2
        return 1
    }
    do_kubernetes 2>&1
) || k8s_refuse_rc=$?
assert_eq "a remote collector without a transport fails off a terminal" "1" "$k8s_refuse_rc"
assert_contains "the error is the generator's, naming both values" \
    "Pass --otel-secure=true for a TLS receiver, or --otel-secure=false for plaintext." "$k8s_refuse_out"
assert_contains "the error says in-cluster collectors usually serve plaintext" "usually serve plaintext" "$k8s_refuse_out"
assert_not_contains "nothing is reported as generated" "Kubernetes manifests generated" "$k8s_refuse_out"

# On a terminal, install.sh asks instead, for a collector the image's
# --print-otel-secure leaves to the operator (it says TLS: not loopback).
stdin_is_tty() { return 0; }
: > "$k8s_log"
k8s_docker_args=""
do_kubernetes <<<"p" > "$k8s_tmp/prompt.out"
assert_contains "the terminal question asks for the transport" "[t]ls or [p]laintext" \
    "$(grep -F '[t]ls or [p]laintext' deploy/install.sh)"
assert_contains "the image decides whether to ask" \
    "print run --rm --network host ghcr.io/coltconsulting/click-dog:26.04.1 init --print-otel-secure --collector otel-collector.monitoring:4317" \
    "$(cat "$k8s_log")"
assert_contains "the question says in-cluster collectors usually serve plaintext" "usually serve plaintext" "$(cat "$k8s_tmp/prompt.out")"
assert_contains "a plaintext answer reaches the generator" "--otel-secure=false" "${k8s_docker_args#*deploy kubernetes }"
OTEL_SECURE=""
do_kubernetes <<<$'x\nt' > /dev/null
assert_contains "a TLS answer reaches the generator, after a re-ask" "--otel-secure=true" "${k8s_docker_args#*deploy kubernetes }"
# A loopback collector is settled: no question, and the generator renders
# the rule comment for it.
OTEL_SECURE=""
COLLECTOR_ADDRESS="localhost:4317"
k8s_docker_args=""
do_kubernetes </dev/null > "$k8s_tmp/prompt.out"
assert_not_contains "a loopback collector is not asked about" "Which transport" "$(cat "$k8s_tmp/prompt.out")"
assert_not_contains "a loopback collector gets no --otel-secure" "-otel-secure" "$k8s_docker_args"
# A flag settles it on a terminal too.
OTEL_SECURE="true"
COLLECTOR_ADDRESS="otel-collector.monitoring:4317"
: > "$k8s_log"
do_kubernetes </dev/null > "$k8s_tmp/prompt.out"
assert_not_contains "a given flag is not asked about" "Which transport" "$(cat "$k8s_tmp/prompt.out")"
assert_not_contains "a given flag needs no answer from the image" "print " "$(cat "$k8s_log")"
stdin_is_tty() { return 1; }
OTEL_SECURE=""
COLLECTOR_ADDRESS="otel-collector:4317"

# An image whose init predates --print-otel-secure (and so --otel-secure=
# values) is refused before any manifest is generated, rather than failing
# inside Go's flag parser.
k8s_init_help="  --otel-secure"
k8s_skew_rc=0
k8s_skew_out=$(do_kubernetes 2>&1) || k8s_skew_rc=$?
assert_eq "image without --print-otel-secure is refused" "1" "$k8s_skew_rc"
assert_contains "refusal names the missing flag" "predates
       'click-dog init --print-otel-secure'" "$k8s_skew_out"
assert_contains "refusal points to the latest release" "https://github.com/coltconsulting/click-dog/releases/latest" "$k8s_skew_out"
assert_not_contains "generator never ran for the old image" "stub: generator ran" "$k8s_skew_out"
k8s_init_help="  --print-otel-secure"
k8s_body=$(awk '/^do_kubernetes\(\)/,/^}/' deploy/install.sh)
assert_eq "image guard runs before the password prompt" "true" \
    "$(in_order "$(grep -nF '    require_otel_transport_image' <<<"$k8s_body" | head -1 | cut -d: -f1)" \
        "$(grep -nF '    resolve_password' <<<"$k8s_body" | cut -d: -f1)")"
assert_not_contains "kubernetes checks no handshake" "preflight_otel_tls" "$k8s_body"
docker() { k8s_docker_args="$*"; }

# --ch-host is required: a standalone click-dog pod can't reach ClickHouse via
# localhost. Subshell so the function's `exit 1` doesn't kill the suite.
k8s_noch_rc=0
# shellcheck disable=SC2034  # read by do_kubernetes, which arrives via eval
( CLICKHOUSE_HOST=""; do_kubernetes >/dev/null 2>&1 ) || k8s_noch_rc=$?
assert_eq "kubernetes without --ch-host fails" "1" "$k8s_noch_rc"

# `kubernetes update` rewrites deployment.yaml and writes its .bak through the
# same image, so it needs the same identity. Its preflight validates the
# ConfigMap's config as the image's own user, so the copy must be readable
# even under umask 077; the stub records the mode of the file it would mount.
k8s_update_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$k8s_update_tmp")
: > "$k8s_update_tmp/deployment.yaml"
cat > "$k8s_update_tmp/configmap.yaml" <<'CM'
data:
  click-dog.yaml: |
    clickhouse:
      host: ch
CM
k8s_validate_mode=""
docker() {
    local argv="$*" src
    case " $argv " in
        *" validate --config "*)
            src="${argv#* -v }"
            src="${src%%:/etc/click-dog/*}"
            k8s_validate_mode=$(ls -l "$src" | cut -c1-10) ;;
        *) k8s_docker_args="$argv" ;;
    esac
}
k8s_docker_args=""
k8s_umask=$(umask)
umask 077
do_kubernetes_update "$k8s_update_tmp" > /dev/null
umask "$k8s_umask"
assert_eq "update validates a copy the image's user can read" "-rw-r--r--" "$k8s_validate_mode"
assert_contains "update runs as the invoker" "run --rm --user $(id -u):$(id -g)" "$k8s_docker_args"
assert_contains "update mounts the directory" "-v $(cd "$k8s_update_tmp" && pwd):/work " "$k8s_docker_args"
assert_contains "update delegates the tag bump" "deploy kubernetes --update -v 26.04.1 -o /work" "$k8s_docker_args"

unset -f docker

echo ""
echo "=== extract_configmap_yaml ==="

# The update path validates the *embedded* config against the target image, so
# it must pull the click-dog.yaml block back out as a standalone document — no
# ConfigMap wrapper, four-space block indent stripped. do_kubernetes no longer
# renders a configmap locally (it delegates to the image), so write a fixture.
cat > "$k8s_tmp/configmap.yaml" <<'CM'
apiVersion: v1
kind: ConfigMap
metadata:
  name: click-dog-config
data:
  click-dog.yaml: |
    clickhouse:
      host: clickhouse.clickhouse.svc.cluster.local
      port: 9000
    exporters:
      otel:
        - collector_address: otel-collector:4317
    monitor:
      enabled: true
    health:
      enabled: true
      listen_address: ":8686"
CM
extracted=$(extract_configmap_yaml "$k8s_tmp/configmap.yaml")
assert_contains "extracted config keeps clickhouse section" "clickhouse:" "$extracted"
assert_contains "extracted config keeps otel section"       "otel:"       "$extracted"
assert_contains "extracted config keeps monitor section"    "monitor:"    "$extracted"
assert_contains "extracted config keeps health section"     "health:"     "$extracted"
assert_not_contains "extracted config drops apiVersion wrapper" "apiVersion:"     "$extracted"
assert_not_contains "extracted config drops ConfigMap kind"     "kind: ConfigMap" "$extracted"
assert_not_contains "extracted config drops data: key"          "data:"           "$extracted"
# Block indent stripped: top-level keys land at column 0 (else validation
# against the image would choke on a 4-space-indented document).
assert_match "extracted top-level keys de-indented to column 0" '^clickhouse:' "$extracted"

echo ""
echo "=== preflight_validate_config ==="

pf_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$pf_tmp")
echo "log_level: info" > "$pf_tmp/click-dog.yaml"  # contents irrelevant; the fake docker decides pass/fail

# Fake docker that validates clean → preflight returns 0 quietly.
ok_bin=$(make_temp_dir)
TEST_TEMP_DIRS+=("$ok_bin")
cat > "$ok_bin/docker" <<'STUB'
#!/bin/sh
exit 0
STUB
chmod +x "$ok_bin/docker"
pf_status=0
VERSION=26.04.1 PATH="$ok_bin:$PATH" preflight_validate_config "$pf_tmp/click-dog.yaml" "install.sh docker" >/dev/null 2>&1 || pf_status=$?
assert_eq "preflight passes when image validates config" "0" "$pf_status"

# Fake docker that rejects the config → preflight must fail closed + hint.
bad_bin=$(make_temp_dir)
TEST_TEMP_DIRS+=("$bad_bin")
cat > "$bad_bin/docker" <<'STUB'
#!/bin/sh
echo "configuration validation failed" >&2
exit 1
STUB
chmod +x "$bad_bin/docker"
pf_status=0
pf_out=$(VERSION=26.04.1 PATH="$bad_bin:$PATH" preflight_validate_config "$pf_tmp/click-dog.yaml" "install.sh docker" 2>&1) || pf_status=$?
assert_eq "preflight fails closed when config does not validate" "1" "$pf_status"
assert_contains "preflight surfaces re-render hint on failure" "install.sh docker" "$pf_out"
assert_contains "preflight explains a permission failure" "make it mode 0644" "$pf_out"

# Docker absent → warn + skip (return 0) so the kubectl-only k8s workflow
# isn't forced to install docker just to bump an image tag.
nodocker_bin=$(make_temp_dir)
TEST_TEMP_DIRS+=("$nodocker_bin")
for t in awk sed dirname basename mktemp; do
    s=$(command -v "$t" 2>/dev/null) && ln -sf "$s" "$nodocker_bin/$t"
done
pf_status=0
pf_out=$(VERSION=26.04.1 PATH="$nodocker_bin" preflight_validate_config "$pf_tmp/click-dog.yaml" "install.sh docker" 2>&1) || pf_status=$?
assert_eq "preflight skips (exit 0) when docker absent" "0" "$pf_status"
assert_contains "preflight notes the docker-absent skip" "skipping config validation" "$pf_out"

echo ""
echo "=== docker_run_mounted ==="

# Docker Engine on Linux keeps host ownership on a bind mount. The stub answers
# `docker info` with $mount_security, or fails it when that is "fail", logs
# each probe to $mount_probes (it runs in a command substitution), and records
# the run argv. `id` is stubbed too, so the uid:gid and
# 0:0 cases differ even when the suite runs as root.
mount_security=""
mount_probes="$(make_temp_dir)/probes"
TEST_TEMP_DIRS+=("${mount_probes%/probes}")
mount_args=""
docker() {
    if [[ "$1" == "info" ]]; then
        echo probe >> "$mount_probes"
        [[ "$mount_security" == "fail" ]] && return 1
        printf '%s\n' "$mount_security"
        return 0
    fi
    mount_args="$*"
}
id() {
    case "$1" in
        -u) echo 1234 ;;
        -g) echo 5678 ;;
        -G) echo "5678 27 999" ;;
    esac
}
mounted() {
    mount_security="$1"
    DOCKER_SECURITY_PROBED=""
    shift
    docker_run_mounted "$@"
}
invoker="--user 1234:5678 --group-add 27 --group-add 999"

mounted "[name=seccomp,profile=builtin name=cgroupns]" write -v /x:/work img init
assert_eq "write runs as the invoker, supplementary groups included" \
    "run --rm $invoker -v /x:/work img init" "$mount_args"
mounted "[name=seccomp,profile=builtin name=rootless name=cgroupns]" write -v /x:/work img init
assert_eq "rootless write runs as container root, the invoker" \
    "run --rm --user 0:0 -v /x:/work img init" "$mount_args"
mounted "[name=seccomp,profile=builtin name=userns]" write -v /x:/work img init
assert_eq "userns-remap write leaves the user namespace" \
    "run --rm $invoker --userns=host -v /x:/work img init" "$mount_args"
mounted "[name=seccomp,profile=builtin name=selinux]" write -v /x:/work img init
assert_eq "SELinux write lifts the label instead of relabeling" \
    "run --rm --security-opt label=disable $invoker -v /x:/work img init" "$mount_args"
mounted "fail" write -v /x:/work img init
assert_eq "failed docker info falls back to uid:gid" "run --rm $invoker -v /x:/work img init" "$mount_args"
mounted "[name=seccomp,profile=builtin name=rootless]" read -v /x:/y:ro img -h
assert_eq "read keeps the image's own user" "run --rm -v /x:/y:ro img -h" "$mount_args"
mounted "[name=selinux]" read -v /x:/y:ro img -h
assert_eq "SELinux read lifts the label" "run --rm --security-opt label=disable -v /x:/y:ro img -h" "$mount_args"

# Probed once per run, however many containers it starts.
: > "$mount_probes"
mounted "[name=rootless]" write -v /x:/work img init
docker_run_mounted write -v /x:/work img init
assert_eq "docker info runs once per run" "1" "$(wc -l < "$mount_probes" | tr -d ' ')"
assert_eq "the cached answer still applies" "run --rm --user 0:0 -v /x:/work img init" "$mount_args"

# The update preflight validates as the image's own user, mounting the file
# where docker-compose.yml does, so a config the service cannot read fails.
mount_security=""
# Re-probed by the next docker_run_mounted, which arrives via eval.
# shellcheck disable=SC2034
DOCKER_SECURITY_PROBED=""
VERSION=26.04.1 validate_config_via_docker "$pf_tmp/click-dog.yaml"
assert_eq "config validation keeps the image's own user and mounts only the file" \
    "run --rm -v $(cd "$pf_tmp" && pwd)/click-dog.yaml:/etc/click-dog/click-dog.yaml:ro ghcr.io/coltconsulting/click-dog:26.04.1 validate --config /etc/click-dog/click-dog.yaml" \
    "$mount_args"
# ...which only holds while the compose template mounts it there.
assert_contains "compose mounts the config where validation does" \
    "./click-dog.yaml:/etc/click-dog/click-dog.yaml:ro" "$(cat "$SCRIPT_DIR/../internal/deploy/template/docker/docker-compose.yml.tmpl")"

# TLS files live where the deployment mounts them, never next to the config,
# so each absolute path the config names gets an empty read-only stand-in:
# without one, every private-CA or mTLS config would fail the update.
tls_cfg_dir=$(make_temp_dir)
TEST_TEMP_DIRS+=("$tls_cfg_dir")
tls_cfg="$tls_cfg_dir/click-dog.yaml"
cat > "$tls_cfg" <<'YAML'
clickhouse:
  secure: true
  ca_cert: "/etc/click-dog/ch-ca.pem"
exporters:
  otel:
    - collector_address: otel.example.com:4317
      secure: true
      # ca_cert: /etc/click-dog/tls/old.pem
      ca_cert: '/etc/click-dog/tls/ca.pem'  # collector CA
      client_cert: /etc/click-dog/tls/client.crt
      client_key: /etc/click-dog/tls/client.key
    - collector_address: other.example.com:4317
      secure: true
      ca_cert: /etc/click-dog/tls/ca.pem
      client_cert: ${CLIENT_CERT}
      client_key: relative/client.key
YAML
assert_eq "TLS paths are the absolute ones, once each" \
    "/etc/click-dog/ch-ca.pem /etc/click-dog/tls/ca.pem /etc/click-dog/tls/client.crt /etc/click-dog/tls/client.key" \
    "$(config_tls_file_paths "$tls_cfg" | tr '\n' ' ' | sed 's/ $//')"
# shellcheck disable=SC2034
DOCKER_SECURITY_PROBED=""
VERSION=26.04.1 validate_config_via_docker "$tls_cfg"
# The stand-in sits beside the config, which the daemon can already mount.
tls_cfg_abs=$(cd "$tls_cfg_dir" && pwd)
tls_stub_re="$tls_cfg_abs/\.click-dog-tls-stub\.[A-Za-z0-9]+"
assert_match "TLS validation mounts a stand-in for each file, read-only" \
    "^run --rm -v $tls_cfg_abs/click-dog.yaml:/etc/click-dog/click-dog.yaml:ro -v ${tls_stub_re}:/etc/click-dog/ch-ca.pem:ro -v ${tls_stub_re}:/etc/click-dog/tls/ca.pem:ro -v ${tls_stub_re}:/etc/click-dog/tls/client.crt:ro -v ${tls_stub_re}:/etc/click-dog/tls/client.key:ro ghcr.io/coltconsulting/click-dog:26.04.1 validate --config /etc/click-dog/click-dog.yaml$" \
    "$mount_args"
assert_not_contains "a commented-out path gets no stand-in" "old.pem" "$mount_args"
assert_eq "the stand-in is removed after validation" "click-dog.yaml" "$(ls -A "$tls_cfg_dir")"
unset -f docker id mounted
# Re-probed by the next docker_run_mounted, which arrives via eval.
# shellcheck disable=SC2034
DOCKER_SECURITY_PROBED=""

# `init` writes click-dog.yaml 0600 and the invoker owns it, but the service
# reads it as the image's own user. The stub writes it 0600 into the mounted
# directory, as `init` would, and answers the image's `init -h`. A subshell
# keeps the inputs, and any `exit`, to this case.
docker_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$docker_tmp")
# The image also answers its `init -h` guard, `init --print-otel-secure` and
# `test tls` (both through the fake binary above, run with the host network,
# as do_docker must), logging each call to $docker_log. $docker_help stands in
# for the image's init help.
docker_log="$docker_tmp/docker.log"
docker_help="  --ch-user string
  --print-otel-secure"
# docker_tls VERDICT [REASON [MESSAGE]] — the image's scripted `test tls`
# answer (the TLS section's helpers are gone by now).
export FAKE_CD_ARGS="$docker_tmp/tls-args" FAKE_CD_OUT="$docker_tmp/tls-out"
docker_tls() {
    printf '{\n  "verdict": "%s",\n  "reason": "%s",\n  "message": "%s"\n}\n' "$1" "${2:-}" "${3:-}" > "$FAKE_CD_OUT"
}
docker_run() {
    local out_dir="$1"
    shift
    docker() {
        local argv="$*" mount
        echo "$argv" >> "$docker_log"
        case " $argv " in
            *" info "*) ;;
            *" init -h "*) printf '%s\n' "$docker_help" ;;
            *" --network host "*)
                shift 5
                "$tls_sandbox/click-dog" "$@"
                return
                ;;
            *" deploy docker "*)
                mount="${argv#* -v }"
                mount="${mount%%:/work*}"
                : > "$mount/docker-compose.yml"
                : > "$mount/.gitignore"
                : > "$mount/click-dog.yaml"
                ( umask 077; : > "$mount/.env" ) ;;
            *" init "*)
                mount="${argv#* -v }"
                mount="${mount%%:/work*}"
                ( umask 077; : > "$mount/click-dog.yaml" ) ;;
        esac
    }
    # do_docker reads these; it arrives via the eval in source_functions.
    # shellcheck disable=SC2034
    OUTPUT_DIR="$out_dir" SUBCOMMAND="" VERSION="26.04.1"
    # shellcheck disable=SC2034
    CLICKHOUSE_USER="click_dog_monitor" CLICKHOUSE_PASSWORD="testpass123"
    do_docker
}
docker_rc=0
: > "$docker_log"
(
    # shellcheck disable=SC2034
    COLLECTOR_ADDRESS="otel:4317" OTEL_SECURE=""
    docker_tls ok "" "certificate verified for otel; TLS 1.3, ALPN h2"
    docker_run "$docker_tmp/out" > /dev/null
) || docker_rc=$?
assert_eq "do_docker succeeds" "0" "$docker_rc"
assert_eq "docker-mode config is readable by the image's user" \
    "-rw-r--r--" "$(ls -l "$docker_tmp/out/click-dog.yaml" | cut -c1-10)"
docker_calls=$(cat "$docker_log")
assert_contains "docker generates its files with the image's deploy docker" \
    "deploy docker -c otel:4317 -u click_dog_monitor -v 26.04.1 -o /work --otel-secure=true" "$docker_calls"
assert_contains "the password reaches the generator through the environment" \
    "-e CLICKHOUSE_PASSWORD -v" "$docker_calls"
assert_not_contains "the password stays out of the docker command line" "testpass123" "$docker_calls"
assert_contains "docker asks the image for the transport, with the host network" \
    "run --rm --network host ghcr.io/coltconsulting/click-dog:26.04.1 init --print-otel-secure --collector otel:4317" "$docker_calls"
assert_contains "docker checks an automatic TLS choice in the image" \
    "run --rm --network host ghcr.io/coltconsulting/click-dog:26.04.1 test tls --collector otel:4317 --json" "$docker_calls"
assert_not_contains "an automatic choice reaches the image's init without a flag" "-otel-secure" \
    "$(grep -v -- '--print-otel-secure' <<<"$docker_calls" | grep ' init ' || true)"

# docker -c otel.example.com:4317 against a plaintext receiver: the image's
# verdict stops the run, naming --otel-secure=false, before any file exists.
docker_stop_rc=0
: > "$docker_log"
docker_stop_out=$(
    # shellcheck disable=SC2034
    COLLECTOR_ADDRESS="otel.example.com:4317" OTEL_SECURE=""
    docker_tls plaintext non-tls "otel.example.com:4317 answered the TLS handshake with non-TLS bytes"
    docker_run "$docker_tmp/stop" 2>&1
) || docker_stop_rc=$?
assert_eq "a plaintext receiver stops docker" "1" "$docker_stop_rc"
assert_contains "the docker stop names --otel-secure=false" "pass --otel-secure=false" "$docker_stop_out"
assert_eq "nothing is generated before the docker gate" "no" "$([[ -e "$docker_tmp/stop" ]] && echo yes || echo no)"

# --otel-secure=false: the image renders it explicitly and checks nothing.
: > "$docker_log"
docker_plain_rc=0
(
    # shellcheck disable=SC2034
    COLLECTOR_ADDRESS="otel.internal:4317" OTEL_SECURE="false"
    docker_run "$docker_tmp/plain" > /dev/null
) || docker_plain_rc=$?
assert_eq "explicit plaintext docker succeeds" "0" "$docker_plain_rc"
assert_not_contains "explicit plaintext docker makes no handshake" "test tls" "$(cat "$docker_log")"
assert_contains "explicit plaintext reaches the image's init with =" "--otel-secure=false" \
    "$(grep -v -- '--print-otel-secure' "$docker_log" | grep ' init ' || true)"

# An image whose init predates --print-otel-secure is refused before anything.
docker_skew_rc=0
docker_skew_out=$(
    # shellcheck disable=SC2034
    COLLECTOR_ADDRESS="otel:4317" OTEL_SECURE=""
    docker_help="  --ch-user string"
    docker_run "$docker_tmp/skew" 2>&1
) || docker_skew_rc=$?
assert_eq "docker refuses an image without --print-otel-secure" "1" "$docker_skew_rc"
assert_contains "the docker refusal points to the latest release" "releases/latest" "$docker_skew_out"
assert_eq "nothing is generated for the old image" "no" "$([[ -e "$docker_tmp/skew" ]] && echo yes || echo no)"
assert_not_contains "a trusted image handshake is no note" "not verified" \
    "$(COLLECTOR_ADDRESS="otel:4317" OTEL_SECURE="" docker_run "$docker_tmp/out" 2>&1 >/dev/null || true)"
unset -f docker_run docker_tls
unset FAKE_CD_ARGS FAKE_CD_OUT

docker_body=$(awk '/^do_docker\(\)/,/^}/' "$SCRIPT_DIR/install.sh")
assert_eq "docker guards the image, asks it, then checks, all before the password" "true" \
    "$(in_order "$(grep -nF '    require_otel_transport_image' <<<"$docker_body" | head -1 | cut -d: -f1)" \
        "$(grep -nF 'resolve_otel_transport || exit 1' <<<"$docker_body" | head -1 | cut -d: -f1)" \
        "$(grep -nF 'preflight_otel_tls || exit 1' <<<"$docker_body" | head -1 | cut -d: -f1)" \
        "$(grep -nF '    resolve_password' <<<"$docker_body" | head -1 | cut -d: -f1)")"
assert_eq "init replaces the generator's config" "true" \
    "$(in_order "$(grep -nF 'deploy docker \' <<<"$docker_body" | cut -d: -f1)" \
        "$(grep -nF 'render_click_dog_config_via_docker "$out_dir/click-dog.yaml"' <<<"$docker_body" | cut -d: -f1)")"

# A `docker run` that bind-mounts outside docker_run_mounted would regress on
# Linux Docker Engine, and a writable mount must use `write`.
run_cmds=$(awk '
    /^[[:space:]]*#/ { next }
    /\\$/ { cmd = cmd $0 " "; next }
    { print cmd $0; cmd = "" }
' "$SCRIPT_DIR/install.sh")
mount_flag='(^|[[:space:]])(-v|--volume|--mount)([[:space:]=]|$)'
assert_eq "no bare docker run bind-mounts" "" \
    "$(grep -E '(^|[[:space:]])docker run ' <<<"$run_cmds" | grep -E -- "$mount_flag" || true)"
assert_eq "finds the docker_run_mounted calls" "true" \
    "$([[ $(grep -cE '^[[:space:]]*docker_run_mounted (read|write) ' <<<"$run_cmds") -ge 4 ]] && echo true || echo false)"
assert_eq "every read mount is read-only" "" \
    "$(grep -E '^[[:space:]]*docker_run_mounted read ' <<<"$run_cmds" | grep -vE ':ro"' || true)"

echo ""
echo "=== verify_release_archive ==="

# Stage a verify_dir + archive that would normally pass verification.
# Returns absolute paths via globals: VR_DIR, VR_ARCHIVE, VR_NAME.
stage_valid_release() {
    VR_DIR=$(make_temp_dir)
    TEST_TEMP_DIRS+=("$VR_DIR")
    VR_NAME="click-dog_99.99.0_linux_amd64.tar.gz"
    VR_ARCHIVE="$VR_DIR/$VR_NAME"
    # The archive doesn't have to be a real tarball — verification only
    # cares about its bytes' SHA256.
    printf 'fake-archive-bytes-for-test\n' > "$VR_ARCHIVE"
    local hash
    hash=$(sha256_file "$VR_ARCHIVE")
    cat > "$VR_DIR/checksums.txt" <<CHECKSUMS_EOF
${hash}  ${VR_NAME}
deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  click-dog_99.99.0_linux_arm64.tar.gz
CHECKSUMS_EOF
    printf 'fake-sig\n' > "$VR_DIR/checksums.txt.sig"
    printf 'fake-pem\n' > "$VR_DIR/checksums.txt.pem"
}

# Build a sandbox PATH dir that contains only the externals
# verify_release_archive needs (awk plus a SHA256 tool, and
# `timeout` if available so the bounded path is exercised). Returns the
# directory via stdout; caller registers it for cleanup. Using an
# explicit sandbox — not /usr/bin — means a host with cosign installed
# in /usr/bin can't accidentally satisfy the "missing cosign" path.
make_tool_sandbox() {
    local dir
    dir=$(make_temp_dir)
    local tool src
    # `sleep` is included so the hanging-cosign fake (which calls sleep)
    # works under this minimal PATH; without it, sleep fails fast and
    # the timeout test would never reach the timeout-bounded path.
    for tool in awk sha256sum shasum timeout sleep; do
        src=$(command -v "$tool" 2>/dev/null) || continue
        ln -sf "$src" "$dir/$tool"
    done
    echo "$dir"
}

# Variant of make_tool_sandbox that omits `timeout` (and sleep, which is
# only needed for the hanging-cosign fake). Forces verify to take its
# unbounded-cosign fallback branch — the path stock macOS hosts hit.
make_tool_sandbox_no_timeout() {
    local dir
    dir=$(make_temp_dir)
    local tool src
    for tool in awk sha256sum shasum; do
        src=$(command -v "$tool" 2>/dev/null) || continue
        ln -sf "$src" "$dir/$tool"
    done
    echo "$dir"
}

# Variant that omits any SHA256 tool. Forces verify to hit its
# "neither sha256sum nor shasum" early-return branch. Needed because
# the default sandbox would happily pass through whichever the host has.
make_tool_sandbox_no_sha256() {
    local dir
    dir=$(make_temp_dir)
    local tool src
    for tool in awk timeout sleep; do
        src=$(command -v "$tool" 2>/dev/null) || continue
        ln -sf "$src" "$dir/$tool"
    done
    echo "$dir"
}

# Tool sandbox for the mocked resolve_binary tests. Shell-function stubs cover
# curl, tar, release resolution, and asset downloads; these are the external
# filesystem/checksum tools the real orchestration still needs.
make_resolve_tool_sandbox() {
    local dir
    dir=$(make_temp_dir)
    local tool src
    for tool in awk sha256sum shasum timeout mktemp chmod rm; do
        src=$(command -v "$tool" 2>/dev/null) || continue
        ln -sf "$src" "$dir/$tool"
    done
    echo "$dir"
}

# Drop a fake cosign into <dir>/cosign that exits with <exit_code>. Uses
# `#!/bin/sh` (not `/usr/bin/env`) so it runs even when PATH is the
# minimal sandbox we set up for these tests.
add_fake_cosign() {
    local dir="$1" exit_code="$2"
    cat > "$dir/cosign" <<COSIGN_EOF
#!/bin/sh
exit ${exit_code}
COSIGN_EOF
    chmod +x "$dir/cosign"
}

# Drop a hanging fake cosign that sleeps <sleep_s> before exiting 0. Used
# to exercise the timeout-bounded verify path.
add_hanging_cosign() {
    local dir="$1" sleep_s="$2"
    cat > "$dir/cosign" <<COSIGN_EOF
#!/bin/sh
sleep ${sleep_s}
exit 0
COSIGN_EOF
    chmod +x "$dir/cosign"
}

# Run verify_release_archive with PATH set to *only* <tool_dir>.
# Captures combined stdout+stderr in CAPTURED and exit status in RC.
run_verify() {
    local tool_dir="$1"
    local forced_reason="${2:-}"
    local saved_path="$PATH"
    local output_file cosign_path="" checksum_reason="missing"
    output_file=$(make_temp_dir)/verify-output
    TEST_TEMP_DIRS+=("$(dirname "$output_file")")
    if [[ "$forced_reason" == "ignored" ]]; then
        checksum_reason="ignored"
    elif [[ -x "$tool_dir/cosign" ]]; then
        cosign_path="$tool_dir/cosign"
        checksum_reason=""
    fi
    PATH="$tool_dir"
    set +e
    verify_release_archive "$VR_DIR" "$VR_ARCHIVE" "$VR_NAME" \
        "$cosign_path" "$checksum_reason" >"$output_file" 2>&1
    RC=$?
    set -e
    PATH="$saved_path"
    CAPTURED=$(<"$output_file")
}

# --- success: cosign exit 0 + matching hash ---
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "valid signature + hash returns 0" "0" "$RC"
assert_eq "valid signature reports signed mode" "signed" "$RELEASE_VERIFICATION_MODE"

# --- cosign verification failure (exit 1) → fail closed, no extraction ---
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 1
run_verify "$SANDBOX"
assert_eq "cosign failure returns nonzero" "1" "$RC"
assert_contains "cosign failure mentions verify-blob" "cosign verify-blob failed" "$CAPTURED"
assert_contains "cosign failure refuses install" "Refusing to install" "$CAPTURED"
assert_contains "cosign failure explains explicit checksum-only retry" "--dangerously-ignore-cosign" "$CAPTURED"
assert_eq "cosign failure reports no successful mode" "" "$RELEASE_VERIFICATION_MODE"

# --- cosign not on PATH → checksum-only verification succeeds with warning ---
# Sandbox built with no cosign symlink/script; PATH is set to that sandbox
# only, so a real cosign installed elsewhere on the host cannot satisfy
# `command -v cosign` and accidentally bypass the missing-cosign path.
stage_valid_release
rm -f "$VR_DIR/checksums.txt.sig" "$VR_DIR/checksums.txt.pem"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
run_verify "$SANDBOX"
assert_eq "missing cosign permits checksum-verified install" "0" "$RC"
assert_contains "missing cosign emits explicit warning" "cosign not found" "$CAPTURED"
assert_contains "missing cosign explains weaker trust" "does not authenticate the publisher" "$CAPTURED"
assert_eq "missing cosign reports checksum mode" "checksum-no-cosign" "$RELEASE_VERIFICATION_MODE"

# --- explicit dangerous override skips an installed but failing cosign ---
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 1
run_verify "$SANDBOX" ignored
assert_eq "dangerous override permits checksum-verified install" "0" "$RC"
assert_contains "dangerous override emits prominent warning" "DANGER: --dangerously-ignore-cosign" "$CAPTURED"
assert_eq "dangerous override reports ignored mode" "checksum-cosign-ignored" "$RELEASE_VERIFICATION_MODE"

# --- neither sha256sum nor shasum on PATH → fail closed ---
# Defends the second tool-availability check against silent breakage if
# the early-return branches are ever restructured.
stage_valid_release
SANDBOX=$(make_tool_sandbox_no_sha256); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "missing sha256 tools returns nonzero" "1" "$RC"
assert_contains "missing sha256 tools error names both" "neither sha256sum nor shasum" "$CAPTURED"

# --- archive hash mismatch (cosign passes, hash doesn't) ---
stage_valid_release
# Corrupt the archive so its SHA no longer matches checksums.txt.
printf 'tampered-bytes\n' > "$VR_ARCHIVE"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "checksum mismatch returns nonzero" "1" "$RC"
assert_contains "checksum mismatch error explicit" "archive checksum mismatch" "$CAPTURED"

# --- archive name absent from checksums.txt ---
stage_valid_release
# Rewrite checksums.txt with only an unrelated entry.
cat > "$VR_DIR/checksums.txt" <<CHECKSUMS_EOF
deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  some-other-archive.tar.gz
CHECKSUMS_EOF
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "missing archive entry returns nonzero" "1" "$RC"
assert_contains "missing archive entry error explicit" "not found in release checksums.txt" "$CAPTURED"

# --- missing checksums.txt asset ---
stage_valid_release
rm -f "$VR_DIR/checksums.txt"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "missing checksums.txt returns nonzero" "1" "$RC"
assert_contains "missing checksums.txt error names asset" "checksums.txt" "$CAPTURED"

# --- missing checksums.txt.sig asset ---
stage_valid_release
rm -f "$VR_DIR/checksums.txt.sig"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "missing signature returns nonzero" "1" "$RC"
assert_contains "missing signature error names asset" "checksums.txt.sig" "$CAPTURED"

# --- missing checksums.txt.pem asset ---
stage_valid_release
rm -f "$VR_DIR/checksums.txt.pem"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "missing certificate returns nonzero" "1" "$RC"
assert_contains "missing certificate error names asset" "checksums.txt.pem" "$CAPTURED"

# --- empty checksums.txt (zero-byte) is rejected like missing ---
stage_valid_release
: > "$VR_DIR/checksums.txt"
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "empty checksums.txt returns nonzero" "1" "$RC"
assert_contains "empty checksums.txt error mentions empty" "missing or empty" "$CAPTURED"

# --- archive_path doesn't exist on disk ---
# Simulates the verification function being called with a stale or
# wrong path after the assets are valid. Defends against a future
# refactor that drops or reorders the existence check.
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
saved_archive="$VR_ARCHIVE"
VR_ARCHIVE="$VR_DIR/does-not-exist.tar.gz"
run_verify "$SANDBOX"
VR_ARCHIVE="$saved_archive"
assert_eq "missing archive returns nonzero" "1" "$RC"
assert_contains "missing archive error names path" "archive not found" "$CAPTURED"

# --- cosign verify-blob bounded by timeout (Rekor/Fulcio simulated stall) ---
# Skip if `timeout` (coreutils) isn't on PATH — install.sh falls back to
# unbounded cosign on those hosts (rare on Linux, common on stock macOS).
if command -v timeout &>/dev/null; then
    stage_valid_release
    SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
    add_hanging_cosign "$SANDBOX" 5
    saved_timeout="${COSIGN_VERIFY_TIMEOUT_S:-}"
    COSIGN_VERIFY_TIMEOUT_S=1
    start=$SECONDS
    run_verify "$SANDBOX"
    elapsed=$((SECONDS - start))
    COSIGN_VERIFY_TIMEOUT_S="$saved_timeout"
    assert_eq "hanging cosign returns nonzero" "1" "$RC"
    assert_contains "hanging cosign error mentions timeout" "timed out" "$CAPTURED"
    assert_contains "hanging cosign error names sigstore endpoints" "rekor.sigstore.dev" "$CAPTURED"
    # 5s window (not 1 + epsilon) so a heavily loaded CI runner's spawn
    # overhead doesn't false-fail. The point is "bounded, not hanging" —
    # any value < the 5s sleep proves the timeout fired.
    assert_eq "hanging cosign aborted within timeout (<= 5s)" "true" "$([[ $elapsed -le 5 ]] && echo true || echo false)"
fi

# --- no-`timeout` fallback path: success and failure still fail closed ---
# Helper is defined at the top with the other sandbox builders. We can't
# simulate a hang here (no `timeout` to break out of one), but we can
# prove cosign still runs and the success/failure semantics match the
# bounded path. This is the path stock macOS hosts hit.
stage_valid_release
SANDBOX=$(make_tool_sandbox_no_timeout); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "no-timeout fallback: valid signature returns 0" "0" "$RC"

stage_valid_release
SANDBOX=$(make_tool_sandbox_no_timeout); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 1
run_verify "$SANDBOX"
assert_eq "no-timeout fallback: cosign failure returns nonzero" "1" "$RC"
assert_contains "no-timeout fallback: cosign failure mentions verify-blob" "cosign verify-blob failed" "$CAPTURED"

echo ""
echo "=== resolve_binary verification branches ==="

# Exercise resolve_binary orchestration without network or a real tarball.
# The real verify_release_archive runs; only release lookup/download/extraction
# are stubbed. Results are returned through CAPTURED/RC and an asset log.
run_resolve_branch() {
    local tool_dir="$1" asset_log="$2" ignore_cosign="${3:-false}"
    local saved_path="$PATH" output_file
    output_file=$(mktemp "${TMPDIR:-/tmp}/click-dog-resolve-output.XXXXXX")
    PATH="$tool_dir"
    set +e
    # resolve_binary reads these script globals through functions extracted
    # dynamically from install.sh.
    # shellcheck disable=SC2034
    (
        VERSION=""
        BINARY=""
        BINARY_AUTO_DOWNLOADED="false"
        BINARY_AUTO_DOWNLOAD_DIR=""
        DANGEROUSLY_IGNORE_COSIGN="$ignore_cosign"
        _RESOLVE_BINARY_ARCHIVE_TMP=""
        _RESOLVE_BINARY_VERIFY_DIR=""
        _RESOLVE_BINARY_DOWNLOAD_DIR=""
        RB_ARCHIVE=""

        resolve_version() {
            VERSION="99.99.0"
            # Bash 3.2 treats an empty array expansion as unset under nounset.
            CURL_ARGS=(-fsSL)
        }
        resolve_target_arch() { printf 'amd64\n'; }
        curl() { printf '{"assets":[]}\n'; }
        download_release_asset() {
            local _json="$1" name="$2" dest="$3"
            printf '%s\n' "$name" >> "$asset_log"
            case "$name" in
                click-dog_*.tar.gz)
                    printf 'mock archive bytes\n' > "$dest"
                    RB_ARCHIVE="$dest"
                    ;;
                checksums.txt)
                    local hash
                    hash=$(sha256_file "$RB_ARCHIVE")
                    printf '%s  click-dog_99.99.0_linux_amd64.tar.gz\n' "$hash" > "$dest"
                    ;;
                checksums.txt.sig|checksums.txt.pem)
                    printf 'mock signature asset\n' > "$dest"
                    ;;
            esac
        }
        tar() {
            local dest=""
            while (($#)); do
                if [[ "$1" == "-C" ]]; then
                    shift
                    dest="$1"
                fi
                shift
            done
            printf '#!/bin/sh\nexit 0\n' > "$dest/click-dog"
        }

        resolve_binary
        printf 'MODE=%s\n' "$RELEASE_VERIFICATION_MODE"
    ) >"$output_file" 2>&1
    RC=$?
    CAPTURED=$(<"$output_file")
    rm -f "$output_file"
    set -e
    PATH="$saved_path"
}

# No Cosign: fetch only the archive + manifest and report checksum mode.
SANDBOX=$(make_resolve_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
ASSET_LOG="$SANDBOX/assets.log"
run_resolve_branch "$SANDBOX" "$ASSET_LOG"
assert_eq "resolve without cosign succeeds" "0" "$RC"
assert_contains "resolve without cosign reports checksum mode" "MODE=checksum-no-cosign" "$CAPTURED"
assert_contains "resolve without cosign prints final checksum summary" "Publisher signature: not verified" "$CAPTURED"
assert_not_contains "resolve without cosign skips signature asset" "checksums.txt.sig" "$(<"$ASSET_LOG")"
assert_not_contains "resolve without cosign skips certificate asset" "checksums.txt.pem" "$(<"$ASSET_LOG")"

# Cosign present: fetch both signature assets and report signed mode.
SANDBOX=$(make_resolve_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
ASSET_LOG="$SANDBOX/assets.log"
run_resolve_branch "$SANDBOX" "$ASSET_LOG"
assert_eq "resolve with cosign succeeds" "0" "$RC"
assert_contains "resolve with cosign reports signed mode" "MODE=signed" "$CAPTURED"
assert_contains "resolve with cosign prints signed summary" "Publisher signature: verified with cosign" "$CAPTURED"
assert_contains "resolve with cosign fetches signature asset" "checksums.txt.sig" "$(<"$ASSET_LOG")"
assert_contains "resolve with cosign fetches certificate asset" "checksums.txt.pem" "$(<"$ASSET_LOG")"

# Failed Cosign is fatal unless the operator explicitly chooses the dangerous
# checksum-only override on the next run.
SANDBOX=$(make_resolve_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 1
ASSET_LOG="$SANDBOX/assets.log"
run_resolve_branch "$SANDBOX" "$ASSET_LOG"
assert_eq "resolve with failing cosign fails closed" "1" "$RC"
assert_contains "resolve with failing cosign shows explicit retry" "--dangerously-ignore-cosign" "$CAPTURED"

ASSET_LOG="$SANDBOX/ignored-assets.log"
run_resolve_branch "$SANDBOX" "$ASSET_LOG" true
assert_eq "dangerous resolve override succeeds" "0" "$RC"
assert_contains "dangerous resolve override reports ignored mode" "MODE=checksum-cosign-ignored" "$CAPTURED"
assert_contains "dangerous resolve override remains visible" "DANGER: publisher signature deliberately skipped" "$CAPTURED"
assert_not_contains "dangerous resolve override skips signature download" "checksums.txt.sig" "$(<"$ASSET_LOG")"

# Release summaries preserve stdout for successful/source information and use
# stderr for reduced-trust warnings, including when indented inside a banner.
SUMMARY_DIR=$(mktemp -d); TEST_TEMP_DIRS+=("$SUMMARY_DIR")
RELEASE_VERIFICATION_MODE="signed"
print_release_verification_summary "  " >"$SUMMARY_DIR/stdout" 2>"$SUMMARY_DIR/stderr"
assert_contains "signed summary accepts banner prefix" "  Publisher signature: verified with cosign" "$(<"$SUMMARY_DIR/stdout")"
assert_eq "signed summary leaves stderr empty" "" "$(<"$SUMMARY_DIR/stderr")"

RELEASE_VERIFICATION_MODE="checksum-no-cosign"
print_release_verification_summary >"$SUMMARY_DIR/stdout" 2>"$SUMMARY_DIR/stderr"
assert_eq "missing cosign summary leaves stdout empty" "" "$(<"$SUMMARY_DIR/stdout")"
assert_contains "missing cosign summary uses stderr" "Publisher signature: not verified" "$(<"$SUMMARY_DIR/stderr")"

RELEASE_VERIFICATION_MODE="checksum-cosign-ignored"
print_release_verification_summary >"$SUMMARY_DIR/stdout" 2>"$SUMMARY_DIR/stderr"
assert_eq "dangerous summary leaves stdout empty" "" "$(<"$SUMMARY_DIR/stdout")"
assert_contains "dangerous summary uses stderr" "DANGER: publisher signature deliberately skipped" "$(<"$SUMMARY_DIR/stderr")"

RELEASE_VERIFICATION_MODE="provided"
print_release_verification_summary >"$SUMMARY_DIR/stdout" 2>"$SUMMARY_DIR/stderr"
assert_contains "operator-provided summary uses stdout" "Binary source: operator-provided" "$(<"$SUMMARY_DIR/stdout")"
assert_eq "operator-provided summary leaves stderr empty" "" "$(<"$SUMMARY_DIR/stderr")"

# --- pinned identity matches the regex used by self-updater + docs ---
expected_identity='^https://github\.com/coltconsulting/click-dog/\.github/workflows/release\.yml@refs/tags/v.+$'
expected_issuer='https://token.actions.githubusercontent.com'
assert_eq "cosign identity regex matches Go and docs" "$expected_identity" "$COSIGN_IDENTITY_REGEXP"
assert_eq "cosign OIDC issuer matches Go and docs" "$expected_issuer" "$COSIGN_OIDC_ISSUER"

echo ""
echo "=== build_curl_args + curl_auth_config: token never in argv (#229) ==="

# Save/restore real env tokens so the suite is hermetic on CI runners that set one.
_sv_ght="${GITHUB_TOKEN:-}"; _sv_ghtk="${GH_TOKEN:-}"; _sv_proxy="$HTTPS_PROXY_FLAG"
HTTPS_PROXY_FLAG=""
unset GITHUB_TOKEN GH_TOKEN

# CURL_ARGS carries -fsSL + https-only hardening, and NEVER the token (a token in
# curl's argv is world-readable via ps/proc — the leak this design avoids).
build_curl_args
assert_contains "curl uses -fsSL"                  "-fsSL"                "${CURL_ARGS[*]}"
assert_contains "curl is https-only"               "--proto =https"       "${CURL_ARGS[*]}"
assert_contains "curl redirect is https-only"      "--proto-redir =https" "${CURL_ARGS[*]}"
assert_not_contains "no auth header in args (no token)" "Authorization"    "${CURL_ARGS[*]}"

GITHUB_TOKEN="ghp_secrettoken123"
build_curl_args
assert_not_contains "token never enters CURL_ARGS, even when set" "ghp_secrettoken123" "${CURL_ARGS[*]}"
assert_not_contains "no Authorization in CURL_ARGS, even when set" "Authorization"      "${CURL_ARGS[*]}"
# The token rides in the curl --config FD content instead.
assert_contains "curl_auth_config carries the bearer header" 'header = "Authorization: Bearer ghp_secrettoken123"' "$(curl_auth_config)"
assert_eq "github_token reads GITHUB_TOKEN" "ghp_secrettoken123" "$(github_token)"
unset GITHUB_TOKEN

GH_TOKEN="gho_fallback456"
assert_contains "GH_TOKEN is the documented fallback" "Authorization: Bearer gho_fallback456" "$(curl_auth_config)"
unset GH_TOKEN

# No token → empty config (a harmless no-op for `curl --config`).
assert_eq "curl_auth_config empty without a token" "" "$(curl_auth_config)"

if [[ -n "$_sv_ght" ]];  then export GITHUB_TOKEN="$_sv_ght"; else unset GITHUB_TOKEN 2>/dev/null || true; fi
if [[ -n "$_sv_ghtk" ]]; then export GH_TOKEN="$_sv_ghtk";    else unset GH_TOKEN 2>/dev/null || true; fi
HTTPS_PROXY_FLAG="$_sv_proxy"

echo ""
echo "=== validate_version_format (#229) ==="
for v in 26.04.6 26.06.2-beta 1.0.0 26.06.10-alpha.1; do
    vf_rc=0; ( validate_version_format "$v" ) >/dev/null 2>&1 || vf_rc=$?
    assert_eq "accepts valid version $v" "0" "$vf_rc"
done
for v in "" "v26.04.6" "26.04" "latest" "26.4.x" "26.04.6 oops"; do
    vf_rc=0; ( validate_version_format "$v" ) >/dev/null 2>&1 || vf_rc=$?
    assert_eq "rejects invalid version '$v'" "1" "$vf_rc"
done

echo ""
echo "=== extract_asset_id (#229) ==="

# Mirror GitHub's pretty-printed release JSON: per asset, "id" precedes "name";
# author/uploader objects also carry "id" (must not be mistaken for an asset id).
PRETTY_JSON=$(cat <<'JSON'
{
  "id": 9001,
  "tag_name": "v26.06.2-beta",
  "name": "v26.06.2-beta",
  "author": { "login": "coltnz", "id": 4242 },
  "assets": [
    {
      "url": "https://api.github.com/repos/coltconsulting/click-dog/releases/assets/111",
      "id": 111,
      "node_id": "RA_aaa",
      "name": "click-dog_26.06.2-beta_linux_amd64.tar.gz",
      "uploader": { "login": "coltnz", "id": 4242 }
    },
    {
      "url": "https://api.github.com/repos/coltconsulting/click-dog/releases/assets/112",
      "id": 112,
      "node_id": "RA_bbb",
      "name": "click-dog_26.06.2-beta_linux_arm64.tar.gz",
      "uploader": { "login": "coltnz", "id": 4242 }
    },
    {
      "url": "https://api.github.com/repos/coltconsulting/click-dog/releases/assets/222",
      "id": 222,
      "node_id": "RA_ccc",
      "name": "checksums.txt",
      "uploader": { "login": "coltnz", "id": 4242 }
    },
    {
      "url": "https://api.github.com/repos/coltconsulting/click-dog/releases/assets/223",
      "id": 223,
      "node_id": "RA_ddd",
      "name": "checksums.txt.sig",
      "uploader": { "login": "coltnz", "id": 4242 }
    }
  ]
}
JSON
)
assert_eq "amd64 asset id"            "111" "$(printf '%s' "$PRETTY_JSON" | extract_asset_id 'click-dog_26.06.2-beta_linux_amd64.tar.gz')"
assert_eq "arm64 asset id"            "112" "$(printf '%s' "$PRETTY_JSON" | extract_asset_id 'click-dog_26.06.2-beta_linux_arm64.tar.gz')"
# checksums.txt must not collide with the checksums.txt.sig prefix.
assert_eq "checksums.txt id (not .sig)" "222" "$(printf '%s' "$PRETTY_JSON" | extract_asset_id 'checksums.txt')"
assert_eq "checksums.txt.sig id"      "223" "$(printf '%s' "$PRETTY_JSON" | extract_asset_id 'checksums.txt.sig')"
assert_eq "missing asset yields empty" ""   "$(printf '%s' "$PRETTY_JSON" | extract_asset_id 'click-dog_99_linux_ppc64.tar.gz')"

# Same data minified onto one line: tr ',' normalization keeps the parse working.
COMPACT_JSON='{"id":9001,"tag_name":"v26.06.2-beta","assets":[{"url":"x","id":111,"node_id":"a","name":"click-dog_26.06.2-beta_linux_amd64.tar.gz","uploader":{"login":"c","id":4242}},{"url":"y","id":222,"name":"checksums.txt"}]}'
assert_eq "amd64 id (compact json)"     "111" "$(printf '%s' "$COMPACT_JSON" | extract_asset_id 'click-dog_26.06.2-beta_linux_amd64.tar.gz')"
assert_eq "checksums id (compact json)" "222" "$(printf '%s' "$COMPACT_JSON" | extract_asset_id 'checksums.txt')"

echo ""
echo "=== download_release_asset: asset API URL + token NOT in argv (#229) ==="

# Stub curl to capture its argv. download_release_asset resolves the id from the
# JSON, then must GET the asset API URL with Accept: application/octet-stream
# rather than relying on a browser redirect URL, because only the API path
# supports tokens. The token must NEVER appear in the captured argv (it rides
# the --config FD).
_sv_dl_tok="${GITHUB_TOKEN:-}"
CURL_ARGS=(-fsSL)
DL_ARGS=""
curl() { DL_ARGS="$*"; }
GITHUB_TOKEN="ghp_secrettoken123"
download_release_asset "$PRETTY_JSON" "checksums.txt" "/tmp/click-dog-dl-test.$$"
unset -f curl
if [[ -n "$_sv_dl_tok" ]]; then export GITHUB_TOKEN="$_sv_dl_tok"; else unset GITHUB_TOKEN 2>/dev/null || true; fi
assert_contains "hits the asset API URL"          "releases/assets/222"               "$DL_ARGS"
assert_contains "requests octet-stream"           "Accept: application/octet-stream"  "$DL_ARGS"
assert_contains "writes to the -o path"           "-o /tmp/click-dog-dl-test.$$"      "$DL_ARGS"
assert_contains "auth supplied via --config FD"   "--config"                          "$DL_ARGS"
assert_not_contains "token never in curl argv"    "ghp_secrettoken123"                "$DL_ARGS"
assert_not_contains "no -H Authorization in argv" "Authorization"                     "$DL_ARGS"
assert_not_contains "never the browser download URL" "releases/download"              "$DL_ARGS"

# A missing asset fails closed (nonzero) with a clear message, no curl call.
CURL_ARGS=(-fsSL)
called="no"; curl() { called="yes"; }
dra_rc=0
dra_out=$(download_release_asset "$PRETTY_JSON" "click-dog_does_not_exist.tar.gz" "/tmp/x" 2>&1) || dra_rc=$?
unset -f curl
assert_eq "missing asset returns nonzero"  "1"   "$dra_rc"
assert_eq "missing asset skips the download" "no" "$called"
assert_contains "missing asset names the artifact" "click-dog_does_not_exist.tar.gz" "$dra_out"

echo ""
echo "=== extract_first_prerelease_tag: skips a newer GA (#229) ==="
pre_pretty=$(cat <<'JSON'
[
  { "tag_name": "v26.07.0", "name": "26.07.0", "prerelease": false },
  { "tag_name": "v26.06.9-beta", "name": "26.06.9-beta", "prerelease": true }
]
JSON
)
assert_eq "first prerelease after a newer GA (pretty)" "26.06.9-beta" "$(printf '%s' "$pre_pretty" | extract_first_prerelease_tag)"
assert_eq "first prerelease (compact)" "26.06.9-beta" "$(printf '%s' '[{"tag_name":"v26.07.0","prerelease":false},{"tag_name":"v26.06.9-beta","prerelease":true}]' | extract_first_prerelease_tag)"
assert_eq "no prerelease in list yields empty" "" "$(printf '%s' '[{"tag_name":"v26.07.0","prerelease":false}]' | extract_first_prerelease_tag)"

# The live /releases page passed the 64 KiB pipe buffer at five releases. The
# newest prerelease comes first, so an extractor that stops reading there
# leaves its writers to SIGPIPE, and pipefail turns that into exit 141.
pre_big_pad=$(printf '%*s' 4096 '' | tr ' ' 'x')
pre_big='[{"tag_name":"v26.10.1-beta.1","prerelease":true}'
for _ in {1..64}; do
    pre_big+=",{\"tag_name\":\"v26.09.1\",\"prerelease\":false,\"body\":\"$pre_big_pad\"}"
done
pre_big+=']'
pre_big_status=0
pre_big_tag=$(printf '%s' "$pre_big" | extract_first_prerelease_tag) || pre_big_status=$?
assert_eq "first prerelease from a page past the pipe buffer" "26.10.1-beta.1" "$pre_big_tag"
assert_eq "extractor reads the whole page, so pipefail sees no SIGPIPE" "0" "$pre_big_status"

echo ""
echo "=== resolve_version GA vs prerelease selection (#229) ==="

# Stub curl: write the requested URL to a file (survives the command-subst
# subshell resolve_version runs curl in) and emit a canned body + the http_code
# the -w probe appends. resolve_version runs in THIS shell so VERSION persists.
# (The token, if any, rides the --config FD; the captured last arg is the URL.)
rv_tmp=$(make_temp_dir); TEST_TEMP_DIRS+=("$rv_tmp")
rv_url_file="$rv_tmp/url"
_sv_pre="$PRERELEASE"; _sv_ver="$VERSION"; _sv_proxy2="$HTTPS_PROXY_FLAG"
HTTPS_PROXY_FLAG=""

VERSION=""; PRERELEASE="false"
curl() { printf '%s' "${@: -1}" > "$rv_url_file"; printf '%s\n200' '{"tag_name": "v26.04.6"}'; }
resolve_version >/dev/null 2>&1
assert_eq "GA resolves the latest tag (v stripped)" "26.04.6" "$VERSION"
assert_contains "GA uses the releases/latest endpoint" "/releases/latest" "$(cat "$rv_url_file")"

# Prerelease must pick the first PRERELEASE even when a newer GA precedes it.
VERSION=""; PRERELEASE="true"
rv_mixed='[{"tag_name":"v26.07.0","prerelease":false},{"tag_name":"v26.06.9-beta","prerelease":true},{"tag_name":"v26.06.1-alpha","prerelease":true}]'
curl() { printf '%s' "${@: -1}" > "$rv_url_file"; printf '%s\n200' "$rv_mixed"; }
resolve_version >/dev/null 2>&1
assert_eq "prerelease skips the newer GA, picks newest beta" "26.06.9-beta" "$VERSION"
assert_contains "prerelease uses the list endpoint" "/releases?per_page=20" "$(cat "$rv_url_file")"

# Under set -e the old extractor's 141 ended install.sh silently right after
# assigning VERSION. Reproduce that in a subshell that really has errexit on:
# $() drops it, and so does a subshell on the left of ||.
#
# The page comes from cat, as it would from real curl. In bash 3.2 a builtin
# printf past the pipe buffer fails with EINTR when the <(curl_auth_config) child
# exits mid-write, which made this test flake on macOS.
VERSION=""; PRERELEASE="true"
printf '%s\n200' "$pre_big" > "$rv_tmp/big_page"
curl() { printf '%s' "${@: -1}" > "$rv_url_file"; cat "$rv_tmp/big_page"; }
set +e
(set -e; resolve_version >/dev/null 2>&1; printf '%s' "$VERSION" > "$rv_tmp/big_version")
rv_big_status=$?
set -e
assert_eq "--prerelease survives set -e on a page past the pipe buffer" "0" "$rv_big_status"
assert_eq "--prerelease resolves from a page past the pipe buffer" "26.10.1-beta.1" "$(cat "$rv_tmp/big_version" 2>/dev/null || true)"

# No GA published at all (public beta: every release is a prerelease). The
# default channel must fall back to the newest beta, not dead-end on the 404
# that /releases/latest returns when no non-prerelease release exists.
VERSION=""; PRERELEASE="false"
rv_all_pre='[{"tag_name":"v26.09.1-beta.2","prerelease":true},{"tag_name":"v26.08.3-beta.1","prerelease":true}]'
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    case "$url" in
        */releases/latest) printf '%s\n404' '{"message":"Not Found"}' ;;
        *)                 printf '%s\n200' "$rv_all_pre" ;;
    esac
}
resolve_version >/dev/null 2>&1
assert_eq "no GA falls back to the newest prerelease" "26.09.1-beta.2" "$VERSION"
assert_contains "fallback re-probes the list endpoint" "/releases?per_page=20" "$(cat "$rv_url_file")"

# A real GA still wins when one exists — the fallback must not fire.
VERSION=""; PRERELEASE="false"
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    case "$url" in
        */releases/latest) printf '%s\n200' '{"tag_name": "v26.10.0"}' ;;
        *)                 printf '%s\n200' "$rv_all_pre" ;;
    esac
}
resolve_version >/dev/null 2>&1
assert_eq "an existing GA is preferred over any beta" "26.10.0" "$VERSION"
assert_contains "no fallback probe when a GA exists" "/releases/latest" "$(cat "$rv_url_file")"

# A failed GA lookup is NOT "no GA exists". A 5xx (or a malformed 200) leaves
# VERSION empty just like a 404 does, but switching channels there would hand a
# beta to someone who asked for GA while a GA may well be published. Both must
# fail loudly instead.
for rv_bad_code in 500 502; do
    VERSION=""; PRERELEASE="false"
    curl() {
        local url="${!#}"
        printf '%s' "$url" > "$rv_url_file"
        case "$url" in
            */releases/latest) printf '%s\n'"$rv_bad_code" '{"message":"Server Error"}' ;;
            *)                 printf '%s\n200' "$rv_all_pre" ;;
        esac
    }
    # resolve_version must run in a SUBSHELL here because it exits on failure —
    # which also means its VERSION assignment cannot reach this shell. Asserting
    # on the parent's VERSION would therefore be vacuous (it is "" either way),
    # so the "did not pick a beta" check reads the subshell's own stdout instead.
    rv_out="$( { resolve_version; echo "RESOLVED=$VERSION"; } 2>&1 )" && rv_status=0 || rv_status=$?
    assert_eq "HTTP $rv_bad_code on the GA endpoint fails instead of resolving" "1" "$rv_status"
    assert_not_contains "HTTP $rv_bad_code does not silently select a beta" "26.09.1-beta.2" "$rv_out"
    assert_not_contains "HTTP $rv_bad_code never reports a resolved version" "RESOLVED=" "$rv_out"
    assert_not_contains "HTTP $rv_bad_code does not claim no GA is published" "No GA release published yet" "$rv_out"
done

# A manifest outlives the run that wrote it, so the generators opt out of the
# channel switch: with no GA they must fail rather than bake a beta image tag in.
VERSION=""; PRERELEASE="false"
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    case "$url" in
        */releases/latest) printf '%s\n404' '{"message":"Not Found"}' ;;
        *)                 printf '%s\n200' "$rv_all_pre" ;;
    esac
}
rv_out="$( { resolve_version no-fallback; echo "RESOLVED=$VERSION"; } 2>&1 )" && rv_status=0 || rv_status=$?
assert_eq "no-fallback fails instead of switching channel" "1" "$rv_status"
assert_not_contains "no-fallback never resolves a beta" "26.09.1-beta.2" "$rv_out"
assert_not_contains "no-fallback does not announce a fallback" "falling back to the newest pre-release" "$rv_out"
assert_contains "no-fallback offers the beta channel" "--prerelease   use the newest public beta" "$rv_out"
assert_contains "no-fallback offers an exact version" "-v VERSION     pin an exact version" "$rv_out"
assert_not_contains "no-fallback does not suggest -b, which a generator cannot use" "-b /path/to/click-dog" "$rv_out"
# The hint must not reconstruct a command line: $* inside resolve_version is the
# function's own argument, so "$0 $*" printed `install.sh no-fallback ...`.
assert_not_contains "no-fallback never prints its own argument as a command" "no-fallback --prerelease" "$rv_out"
assert_not_contains "no-fallback prints no invented example version" "26.09.1" "$rv_out"

# An auth/rate-limit failure on the no-fallback path must not suggest -b either.
VERSION=""; PRERELEASE="false"
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    printf '%s\n403' '{"message":"API rate limit exceeded"}'
}
rv_out="$( { resolve_version no-fallback; } 2>&1 )" && rv_status=0 || rv_status=$?
assert_eq "no-fallback 403 fails" "1" "$rv_status"
assert_contains "no-fallback 403 offers an exact version" "re-run with -v VERSION" "$rv_out"
assert_not_contains "no-fallback 403 does not suggest -b" "-b /path/to/click-dog" "$rv_out"
# The ordinary install path still gets the -b advice, which is useful there.
VERSION=""; PRERELEASE="false"
rv_out="$( { resolve_version; } 2>&1 )" && rv_status=0 || rv_status=$?
assert_contains "the install path keeps the -b hint on 403" "-b /path/to/click-dog" "$rv_out"

# The fallback's second probe is a separate unauthenticated call and can be
# rate-limited on its own; that must read as an auth failure, not "no release".
VERSION=""; PRERELEASE="false"
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    case "$url" in
        */releases/latest) printf '%s\n404' '{"message":"Not Found"}' ;;
        *)                 printf '%s\n403' '{"message":"API rate limit exceeded"}' ;;
    esac
}
rv_out="$( { resolve_version; } 2>&1 )" && rv_status=0 || rv_status=$?
assert_eq "a rate-limited fallback probe fails" "1" "$rv_status"
assert_contains "a rate-limited fallback probe reports the auth cause" "GitHub rejected the pre-release request (HTTP 403)" "$rv_out"

# The empty-window hint belongs to the channels that actually list releases.
VERSION=""; PRERELEASE="true"
curl() {
    local url="${!#}"
    printf '%s' "$url" > "$rv_url_file"
    printf '%s\n200' '[{"tag_name":"v26.10.0","prerelease":false}]'
}
rv_out="$( { resolve_version; } 2>&1 )" && rv_status=0 || rv_status=$?
assert_eq "--prerelease with no beta in the window fails" "1" "$rv_status"
assert_contains "--prerelease keeps its own remediation hint" "drop --prerelease for the latest GA" "$rv_out"

unset -f curl
PRERELEASE="$_sv_pre"; VERSION="$_sv_ver"; HTTPS_PROXY_FLAG="$_sv_proxy2"

echo ""
echo "=== pinned -v initializes CURL_ARGS (set -u regression) (#229) ==="
# resolve_version must run build_curl_args even when VERSION is pinned, or the
# download path's `curl \"\${CURL_ARGS[@]}\"` aborts under set -u. No probe runs
# on the pinned path (early return), so curl must NOT be called here.
_sv_ver3="$VERSION"; _sv_proxy3="$HTTPS_PROXY_FLAG"
HTTPS_PROXY_FLAG="https://proxy.test:3128"
pinned_called="no"; curl() { pinned_called="yes"; }
unset CURL_ARGS 2>/dev/null || true
VERSION="26.06.2-beta"
pin_rc=0; resolve_version >/dev/null 2>&1 || pin_rc=$?
unset -f curl
assert_eq "pinned resolve_version does not abort"         "0"     "$pin_rc"
assert_eq "pinned path issues no version probe"           "no"    "$pinned_called"
assert_contains "pinned path still initializes CURL_ARGS"  "-fsSL" "${CURL_ARGS[*]:-}"
assert_contains "pinned path carries proxy into CURL_ARGS" "proxy.test:3128" "${CURL_ARGS[*]:-}"
VERSION="$_sv_ver3"; HTTPS_PROXY_FLAG="$_sv_proxy3"

echo ""
echo "=== variable initialization ==="

# Simulate the mon_pass flow with no .secret file
# This is the exact scenario that caused the unbound variable crash
(
    set -euo pipefail
    local_test_pass=""
    if [[ -f /nonexistent/path/.secret ]]; then
        local_test_pass="from_secret"
    fi
    if [[ -z "$local_test_pass" ]]; then
        local_test_pass="generated"
    fi
    assert_eq "mon_pass initialized when .secret missing" "generated" "$local_test_pass"
)

echo ""
echo "=== classify_span_log_state ==="

# The pure decision core: given (client_available, EXISTS result, recent
# count) it must emit exactly one status token. Drive every branch with
# synthetic inputs — no ClickHouse, no client, no I/O.
assert_eq "no client -> no_client"                 "no_client" "$(classify_span_log_state false 1 5)"
assert_eq "no client wins even with positive count" "no_client" "$(classify_span_log_state false 1 99)"
assert_eq "client + EXISTS 0 -> missing"           "missing"   "$(classify_span_log_state true 0 0)"
assert_eq "client + empty EXISTS -> missing"       "missing"   "$(classify_span_log_state true '' '')"
assert_eq "client + EXISTS 1 + count 0 -> empty"   "empty"     "$(classify_span_log_state true 1 0)"
assert_eq "client + EXISTS 1 + count 1 -> ok"      "ok"        "$(classify_span_log_state true 1 1)"
assert_eq "client + EXISTS 1 + big count -> ok"    "ok"        "$(classify_span_log_state true 1 4096)"
# A malformed (non-numeric) count must degrade to the safe "empty"
# warning, never a false "ok".
assert_eq "client + EXISTS 1 + garbage count -> empty" "empty" "$(classify_span_log_state true 1 'NaN')"
assert_eq "client + EXISTS 1 + empty count -> empty"   "empty" "$(classify_span_log_state true 1 '')"

echo ""
echo "=== verify_span_log_via_sql (stubbed runner) ==="

# verify_span_log_via_sql injects its ClickHouse access via
# SPAN_LOG_QUERY_RUNNER: a command taking one SQL string and printing the
# result. We stub it so each decision branch is exercised end-to-end —
# including the human-readable report and the token-on-last-line contract.
#
# The stub answers the EXISTS query with $SPAN_STUB_EXISTS and the
# recent-count query with $SPAN_STUB_COUNT. It must be a top-level
# function (not one built inside a $(...) subshell, which would never
# survive into the parent shell where verify runs).
span_stub_runner() {
    case "$1" in
        *'EXISTS TABLE'*)  echo "$SPAN_STUB_EXISTS" ;;
        *'count()'*)       echo "$SPAN_STUB_COUNT" ;;
        *)                 echo '' ;;
    esac
}

# Set the stub's answers, run verify with it, and capture the full report
# in SPAN_OUT plus the trailing token in SPAN_STATE.
run_span_verify() {
    SPAN_STUB_EXISTS="$1"
    SPAN_STUB_COUNT="$2"
    SPAN_OUT="$(SPAN_LOG_QUERY_RUNNER=span_stub_runner verify_span_log_via_sql)"
    SPAN_STATE="${SPAN_OUT##*$'\n'}"
}

# --- ok: table exists with recent rows ---
run_span_verify 1 42
assert_eq "ok branch emits ok token" "ok" "$SPAN_STATE"
assert_contains "ok branch reports verified + recent spans" "has recent spans" "$SPAN_OUT"
assert_not_contains "ok branch has no WARNING" "WARNING" "$SPAN_OUT"

# --- empty: table exists but no rows today ---
run_span_verify 1 0
assert_eq "empty branch emits empty token" "empty" "$SPAN_STATE"
assert_contains "empty branch is a WARNING (not generic success)" "WARNING" "$SPAN_OUT"
assert_contains "empty branch explains no spans recorded" "no spans" "$SPAN_OUT"
assert_contains "empty branch tells operator to run a traced query" "traced queries" "$SPAN_OUT"
assert_contains "empty branch repeats the recent-count SQL" \
    "SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();" "$SPAN_OUT"

# --- missing: table does not exist after setup ---
run_span_verify 0 0
assert_eq "missing branch emits missing token" "missing" "$SPAN_STATE"
assert_contains "missing branch warns table does NOT exist" "does NOT exist" "$SPAN_OUT"
assert_contains "missing branch prints EXISTS SQL verbatim" \
    "EXISTS TABLE system.opentelemetry_span_log;" "$SPAN_OUT"
assert_contains "missing branch prints recent-count SQL verbatim" \
    "SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();" "$SPAN_OUT"
assert_contains "missing branch defers to click-dog check" "click-dog check" "$SPAN_OUT"

# --- malformed count (table exists, count is junk) degrades to empty ---
run_span_verify 1 "error"
assert_eq "garbage count degrades to empty token" "empty" "$SPAN_STATE"
assert_contains "garbage count still warns" "WARNING" "$SPAN_OUT"

# --- regression: a runner whose queries FAIL (nonzero) must not abort under
# set -e. Now that verification runs as the monitoring user, an existing user
# can authenticate yet lack SELECT on a system table, so the EXISTS/count query
# exits nonzero. Without the `|| ...=""` guards a failing command substitution
# aborts the installer before classify/report runs. The harness eval-strips
# install.sh's own `set -euo pipefail`, so exercise it via an explicit subshell.
span_failing_runner() { return 1; }
fail_rc=0
fail_out="$(set -euo pipefail; SPAN_LOG_QUERY_RUNNER=span_failing_runner verify_span_log_via_sql)" || fail_rc=$?
assert_eq "failing query does not abort verify under set -e" "0" "$fail_rc"
assert_eq "failing query degrades to missing token" "missing" "${fail_out##*$'\n'}"
assert_contains "failing query still surfaces the warning" "does NOT exist" "$fail_out"

echo ""
echo "=== verify_span_log_via_sql (no client available) ==="

# With no SPAN_LOG_QUERY_RUNNER and a CLICKHOUSE_CLIENT that isn't on PATH,
# verify must fall back to the manual-SQL path: print BOTH queries verbatim
# and point at click-dog check. Force the client to be absent.
saved_chc="$CLICKHOUSE_CLIENT"
CLICKHOUSE_CLIENT="click-dog-nonexistent-client-binary"
unset SPAN_LOG_QUERY_RUNNER 2>/dev/null || true
nc_out="$(verify_span_log_via_sql)"
nc_state="${nc_out##*$'\n'}"
CLICKHOUSE_CLIENT="$saved_chc"
assert_eq "no-client branch emits no_client token" "no_client" "$nc_state"
assert_contains "no-client branch admits it could not verify" "Could not verify" "$nc_out"
assert_contains "no-client branch prints EXISTS SQL verbatim" \
    "EXISTS TABLE system.opentelemetry_span_log;" "$nc_out"
assert_contains "no-client branch prints recent-count SQL verbatim" \
    "SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();" "$nc_out"
assert_contains "no-client branch defers to click-dog check" "click-dog check" "$nc_out"

echo ""
echo "=== span-log SQL constants + manual fallback ==="

# The two queries must match the issue text exactly and be shared by every
# code path so the runtime queries and the printed guidance can't drift.
assert_eq "EXISTS SQL matches issue spec" \
    "EXISTS TABLE system.opentelemetry_span_log;" "$SPAN_LOG_EXISTS_SQL"
assert_eq "recent-count SQL matches issue spec" \
    "SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();" "$SPAN_LOG_RECENT_SQL"

manual="$(print_span_log_manual_sql)"
assert_contains "manual fallback prints EXISTS SQL"       "$SPAN_LOG_EXISTS_SQL" "$manual"
assert_contains "manual fallback prints recent-count SQL" "$SPAN_LOG_RECENT_SQL" "$manual"
assert_contains "manual fallback mentions check"   "click-dog check" "$manual"

echo ""
echo "=== _span_log_default_runner auth handling (#180 review) ==="

record_args() { echo "$*"; }
_sv_chc="$CLICKHOUSE_CLIENT"; _sv_u="${CLICKHOUSE_USER:-}"; _sv_p="${CLICKHOUSE_PASSWORD:-}"

# Runner: with a password, the check connects as the monitoring user even when -C
# carries an admin, so it proves what click-dog itself can read.
CLICKHOUSE_CLIENT="record_args -u admin --password secret"; CLICKHOUSE_USER="monitoring"; CLICKHOUSE_PASSWORD="monpass"
auth_out="$(_span_log_default_runner 'SELECT 1')"
assert_eq "the check replaces -C's user with the monitoring user" "-u monitoring -q SELECT 1" "$auth_out"
assert_not_contains "-C's password stays out of the check" "secret" "$auth_out"

# Runner: a password already exists (CLICKHOUSE_PASSWORD env) → authenticate as
# that user, since it exists. Record the child environment separately so the
# assertion also proves that the password did not move back into argv.
record_env_and_args() { echo "password=${CLICKHOUSE_PASSWORD:-} args=$*"; }
CLICKHOUSE_CLIENT="record_env_and_args"; CLICKHOUSE_USER="monitoring"; CLICKHOUSE_PASSWORD="monpass"
withpw_out="$(_span_log_default_runner 'SELECT 1')"
assert_contains "existing password is passed in child environment" "password=monpass" "$withpw_out"
assert_contains "existing password authenticates as that user" "args=-u monitoring -q SELECT 1" "$withpw_out"
assert_not_contains "existing password is absent from argv" "--password" "$withpw_out"

# Docker wrappers cross a process/container boundary. They must forward the
# command-scoped password by name; otherwise the installer warns instead of
# silently degrading to the no-client/manual path.
docker_warn="$(_warn_clickhouse_client_password_forwarding 'docker exec clickhouse clickhouse-client' 2>&1)"
assert_contains "docker exec without env forwarding warns" "without forwarding CLICKHOUSE_PASSWORD" "$docker_warn"
assert_contains "docker exec without -i warns" "without -i" "$docker_warn"
docker_safe="$(_warn_clickhouse_client_password_forwarding 'docker exec -i -e CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_eq "docker exec by-name env forwarding is accepted" "" "$docker_safe"
docker_long_safe="$(_warn_clickhouse_client_password_forwarding 'docker exec -it --env=CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_eq "docker exec long env forwarding is accepted" "" "$docker_long_safe"
docker_invalid_short="$(_warn_clickhouse_client_password_forwarding 'docker exec -i -e=CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_contains "invalid docker short env spelling still warns" "without forwarding CLICKHOUSE_PASSWORD" "$docker_invalid_short"
assert_contains "kubectl exec warns" "uses kubectl without forwarding" \
    "$(_warn_clickhouse_client_password_forwarding 'kubectl exec pod -- clickhouse-client' 2>&1)"
assert_contains "sudo warns" "uses sudo without forwarding" \
    "$(_warn_clickhouse_client_password_forwarding 'sudo -u ch clickhouse-client' 2>&1)"
assert_eq "sudo -E keeps the environment" "" \
    "$(_warn_clickhouse_client_password_forwarding 'sudo -E -u ch clickhouse-client' 2>&1)"
assert_eq "a client under another name is no wrapper" "" \
    "$(_warn_clickhouse_client_password_forwarding '/opt/ch/chclient --secure' 2>&1)"
assert_eq "sudo --preserve-env forwards the password" "" \
    "$(_warn_clickhouse_client_password_forwarding 'sudo --preserve-env=CLICKHOUSE_PASSWORD clickhouse-client' 2>&1)"
plain_safe="$(_warn_clickhouse_client_password_forwarding 'clickhouse-client' 2>&1)"
assert_eq "plain clickhouse-client needs no boundary warning" "" "$plain_safe"

# Runner: default quickstart — no -C auth, no password yet. The monitoring user
# isn't created until Step 2, so the runner must NOT probe as it; it uses the
# client's own default auth instead (otherwise a working single-node install
# falls to the no_client fallback and loses the SQL verification).
CLICKHOUSE_CLIENT="record_args"; CLICKHOUSE_USER="monitoring"; CLICKHOUSE_PASSWORD=""
nopw_out="$(_span_log_default_runner 'SELECT 1')"
assert_eq "no creds appended before monitoring user exists" "-q SELECT 1" "$nopw_out"
assert_not_contains "monitoring user not probed before it exists" "monitoring" "$nopw_out"

CLICKHOUSE_CLIENT="$_sv_chc"; CLICKHOUSE_USER="$_sv_u"; CLICKHOUSE_PASSWORD="$_sv_p"

echo ""
echo "=== install.sh wires SQL verification into quickstart ==="

# Regression-protect the integration point: the grep-only signal must be
# followed by a runtime SQL verification call. If verify_span_log_via_sql
# is ever dropped from do_quickstart, the grep becomes the sole proof
# again — exactly the false-confidence bug #180 set out to kill.
quickstart_body=$(awk '/^do_quickstart\(\)/,/^}/' deploy/install.sh)
assert_contains "do_quickstart still does the cheap grep first" \
    'grep -rql "opentelemetry_span_log"' "$quickstart_body"
assert_contains "do_quickstart follows grep with SQL verification" \
    "verify_span_log_via_sql" "$quickstart_body"

echo ""
echo "=== default ClickHouse user (issue #181) ==="

# The installer default must be the dedicated, namespaced name so re-running
# install can't collide with a pre-existing operator "monitoring" account.
# source_functions evals the install.sh prelude, which sets the default before
# the test harness overrides it — read it straight from the script instead.
default_user=$(grep -m1 -E '^CLICKHOUSE_USER=' deploy/install.sh)
assert_contains "default username is click_dog_monitor" 'CLICKHOUSE_USER="click_dog_monitor"' "$default_user"
assert_not_contains "default username is not bare monitoring" 'CLICKHOUSE_USER="monitoring"' "$default_user"

# -u help text documents the dedicated default and configurability.
usage_block=$(sed -n '1,45p' deploy/install.sh)
assert_contains "usage documents click_dog_monitor default" "default: click_dog_monitor" "$usage_block"

echo ""
echo "=== generate_user_setup_sql (issue #181) ==="

setup_out=$(generate_user_setup_sql "click_dog_monitor" "deadbeef")
# Idempotent + non-clobbering: IF NOT EXISTS, never OR REPLACE.
assert_contains "setup SQL uses CREATE USER IF NOT EXISTS" \
    "CREATE USER IF NOT EXISTS click_dog_monitor IDENTIFIED WITH sha256_hash BY 'deadbeef'" "$setup_out"
assert_not_contains "setup SQL never uses OR REPLACE" "OR REPLACE" "$setup_out"
# Exactly the two required SELECT grants, nothing broader.
assert_contains "setup SQL grants span log" "GRANT SELECT ON system.opentelemetry_span_log TO click_dog_monitor" "$setup_out"
assert_contains "setup SQL grants query log" "GRANT SELECT ON system.query_log TO click_dog_monitor" "$setup_out"
assert_not_contains "setup SQL grants nothing else" "GRANT SELECT ON system.query_thread_log" "$setup_out"
# Setup SQL must NOT mutate auth on its own — the executable ALTER lives in the
# separate ALTER bundle. (A comment may *mention* ALTER USER; what matters is no
# runnable statement for our user.)
assert_not_contains "setup SQL has no runnable ALTER for the user" "ALTER USER click_dog_monitor" "$setup_out"

# Honors a custom -u username.
custom_out=$(generate_user_setup_sql "ops_reader" "cafef00d")
assert_contains "setup SQL honors custom username" "CREATE USER IF NOT EXISTS ops_reader IDENTIFIED WITH sha256_hash BY 'cafef00d'" "$custom_out"

echo ""
echo "=== generate_user_alter_sql (issue #181) ==="

alter_out=$(generate_user_alter_sql "click_dog_monitor" "feedface")
assert_contains "alter SQL emits ALTER USER ... IDENTIFIED" \
    "ALTER USER click_dog_monitor IDENTIFIED WITH sha256_hash BY 'feedface'" "$alter_out"
# The ALTER bundle is password-only — it must not (re)create or grant.
assert_not_contains "alter SQL does not CREATE USER" "CREATE USER" "$alter_out"
assert_not_contains "alter SQL does not GRANT" "GRANT SELECT" "$alter_out"

echo ""
echo "=== generate_user_verify_sql (issue #181) ==="

verify_out=$(generate_user_verify_sql "click_dog_monitor")
# Verification probes BOTH tables click-dog reads, not just SELECT 1.
assert_contains "verify SQL probes span log"  "FROM system.opentelemetry_span_log" "$verify_out"
assert_contains "verify SQL probes query log" "FROM system.query_log"              "$verify_out"
assert_not_contains "verify SQL is more than SELECT 1" "SELECT 1;" "$verify_out"

echo ""
echo "=== generate_user_setup_xml (issue #181) ==="

xml_out=$(generate_user_setup_xml "click_dog_monitor" "0badc0de")
assert_contains "XML names the user element"   "<click_dog_monitor>"                          "$xml_out"
assert_contains "XML sets the password hash"   "<password_sha256_hex>0badc0de</password_sha256_hex>" "$xml_out"
assert_contains "XML defines readonly=2 profile" "<readonly>2</readonly>"                      "$xml_out"
assert_contains "XML uses click-dog profile"     "<profile>click_dog_readonly</profile>"       "$xml_out"
assert_not_contains "XML does not use built-in readonly=1 profile" "<profile>readonly</profile>" "$xml_out"
assert_contains "XML grants span log"          "<query>GRANT SELECT ON system.opentelemetry_span_log</query>" "$xml_out"
assert_contains "XML grants query log"         "<query>GRANT SELECT ON system.query_log</query>"              "$xml_out"
assert_not_contains "XML names no grantee, which ClickHouse refuses in a config file" " TO " "$xml_out"

echo ""
echo "=== user_auth_mismatch_decision (issue #181) ==="

# (exists, can_auth) → decision. Drives the existing-user branch so the
# installer never silently ALTERs credentials.
assert_eq "fresh install (no user) → create" "create" "$(user_auth_mismatch_decision false false)"
assert_eq "existing user authenticates → ok"  "ok"     "$(user_auth_mismatch_decision true true)"
assert_eq "existing user wrong password → prompt" "prompt" "$(user_auth_mismatch_decision true false)"
# Contract is exists-first: with no prior user (no .secret) we always take the
# safe non-mutating create path, even on the impossible (false,true) input.
assert_eq "no prior user → create regardless of auth flag" "create" "$(user_auth_mismatch_decision false true)"

echo ""
echo "=== user-setup flow copy + blast radius (issue #181) ==="

# Extract the quickstart body so we can assert on its operator-facing copy.
quickstart_body=$(awk '/^do_quickstart\(\)/,/^# ── Dispatch/' deploy/install.sh)
# Generated monitoring credentials must use clickhouse-client's environment
# support, not process arguments visible to other local users.
assert_not_contains "generated password is absent from client argv" '--password "$mon_pass"' "$quickstart_body"
assert_contains "generated password uses child environment" 'CLICKHOUSE_PASSWORD="$password" "$@"' "$quickstart_body"
assert_contains "manual fallback models argv-safe password handling" 'Run with: CLICKHOUSE_PASSWORD=... clickhouse-client' "$quickstart_body"
assert_contains "a password from the environment still warns about wrappers" '_warn_clickhouse_client_password_forwarding "$CLICKHOUSE_CLIENT"' "$quickstart_body"
# A failed span-log restart names the file and waits for the operator.
assert_contains "a failed restart names the file to remove" \
    '${ch_config_dir}/opentelemetry.${otel_cfg_ext} and check its log.' "$quickstart_body"
assert_contains "a failed restart waits for the operator" \
    "$(printf '%s\n' 'and check its log."' '                            echo ""' '                            read -rp "  Press Enter once done..."')" "$quickstart_body"
# The not-running ending names the setup SQL and keeps only that file.
assert_contains "the not-running ending names the setup SQL" 'clickhouse-client --multiquery < ${MONITOR_SETUP_SQL}' "$quickstart_body"
assert_contains "the not-running ending names it whenever it has one" 'elif [[ -n "${MONITOR_SETUP_SQL:-}" ]]; then' "$quickstart_body"
assert_contains "the not-running ending says what to do after [k]" 'Write the password of' "$quickstart_body"
assert_contains "the not-running ending has a step with no SQL to run" "logs in with the password in /etc/click-dog/.secret" "$quickstart_body"
assert_contains "a backup of the config keeps its owner" 'cp -p /etc/click-dog/click-dog.yaml /etc/click-dog/click-dog.yaml.bak' "$quickstart_body"
assert_contains "a backup of the password keeps its owner" 'cp -p /etc/click-dog/.secret /etc/click-dog/.secret.bak' "$quickstart_body"
assert_contains "the not-running ending removes the other files" '"$temp_file" != "${MONITOR_SETUP_SQL:-}"' "$quickstart_body"
assert_contains "a failed start reaches the not-running ending" 'restart_click_dog && service_up="true"' "$quickstart_body"
assert_contains "a failed validation restores a password saved in step 1" 'mv /etc/click-dog/.secret.bak /etc/click-dog/.secret' "$quickstart_body"
assert_contains "a failed first validation removes the password it wrote" "$(printf '%s\n' '        else' '            rm -f /etc/click-dog/.secret')" "$quickstart_body"
assert_contains "the not-running ending removes each other file" 'rm -f "$temp_file"' "$quickstart_body"
assert_contains "the environment path warns right after it says so" "$(printf '%s\n' 'echo "  Skipping ClickHouse user setup."' '        _warn_clickhouse_client_password_forwarding "$CLICKHOUSE_CLIENT"')" "$quickstart_body"
# A failed update says what happened.
update_body=$(awk '/^do_update\(\)/,/^}/' deploy/install.sh)
assert_contains "a failed update says the service is down" "the update installed the new binary, but click-dog is not running" "$update_body"
assert_contains "a failed update names the prior binary" "click-dog.prev" "$update_body"
assert_before "an update records the service's state before it swaps the binary" \
    'systemctl is-active click-dog >/dev/null 2>&1 && was_active="true"' 'mv "$staged_binary" /usr/local/bin/click-dog' "$update_body"
assert_contains "an update says how to start a stopped service" "Start it with: systemctl start click-dog" "$update_body"
# The inaccurate "Nothing else is changed" reassurance must be gone.
assert_not_contains "no false 'Nothing else is changed' copy" "Nothing else is changed" "$quickstart_body"
# Copy must accurately describe the non-clobbering CREATE USER IF NOT EXISTS posture.
assert_contains "copy explains IF NOT EXISTS preserves existing user" "CREATE USER IF NOT EXISTS" "$quickstart_body"
# Manual SQL bundle is always surfaced for review.
assert_contains "manual SQL bundle is printed for review" "Manual SQL (review before running anywhere):" "$quickstart_body"
assert_contains "existing-user path verifies real table access" 'generate_user_verify_sql' "$quickstart_body"
# Declining the reset must also drop the [x] XML path — its users.d overlay
# carries password_sha256_hex and would otherwise reset the existing user's
# credentials declaratively, undoing the operator's "no" (the clobber #181
# removes). Gate it on the same reset confirmation.
assert_contains "declined reset gates the XML path" 'offer_xml="false"' "$quickstart_body"
assert_contains "XML path keyed on declined auth-mismatch" '"$auth_decision" == "prompt" && "$AUTH_RESET_CONFIRMED" != "true"' "$quickstart_body"
assert_contains "[x] is refused when not offered" "[x] is not available for this user" "$quickstart_body"

echo ""
echo "=== install.sh docker from a lone download ==="

# A release publishes install.sh by itself, so this runs a copy with nothing
# beside it. The Docker client is fake; the script is real and unsourced.
lone_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$lone_tmp")
mkdir -p "$lone_tmp/dl" "$lone_tmp/bin"
cp "$SCRIPT_DIR/install.sh" "$lone_tmp/dl/install.sh"
cat > "$lone_tmp/bin/docker" <<'FAKE'
#!/usr/bin/env bash
argv=" $* "
mount="${argv#* -v }"
mount="${mount%%:/work*}"
case "$argv" in
    *" info "*) ;;
    *" init -h "*) echo "  --print-otel-secure" ;;
    *" init --print-otel-secure "*) echo "false" ;;
    *" deploy docker "*)
        echo "services: {}" > "$mount/docker-compose.yml"
        echo ".env" > "$mount/.gitignore"
        echo "from-deploy" > "$mount/click-dog.yaml"
        printf "CLICKHOUSE_PASSWORD='%s'\n" "$CLICKHOUSE_PASSWORD" > "$mount/.env" ;;
    *" init "*)
        [[ ! -e "$mount/click-dog.yaml" ]] || echo "existing file replaced"
        echo "from-init" > "$mount/click-dog.yaml"
        echo "Wrote /work/click-dog.yaml" ;;
    *) echo "unexpected docker call:$argv" >&2; exit 64 ;;
esac
FAKE
chmod +x "$lone_tmp/bin/docker"
lone_rc=0
lone_out=$(cd "$lone_tmp/dl" && PATH="$lone_tmp/bin:$PATH" CLICKHOUSE_PASSWORD="lonepass" \
    bash ./install.sh docker -c localhost:4317 -v 26.04.1 2>&1) || lone_rc=$?
assert_eq "a downloaded install.sh generates docker files" "0" "$lone_rc"
assert_contains "it reports where the files are" "Docker files generated in ./click-dog-docker/" "$lone_out"
assert_not_contains "the generator's container path stays hidden" "/work" "$lone_out"
for lone_file in docker-compose.yml .env .gitignore; do
    assert_eq "lone download wrote $lone_file" "yes" \
        "$([[ -f "$lone_tmp/dl/click-dog-docker/$lone_file" ]] && echo yes || echo no)"
done
assert_eq "the config is init's, not the generator's" "from-init" "$(cat "$lone_tmp/dl/click-dog-docker/click-dog.yaml")"
assert_contains "the output names the host path" "click-dog-docker/click-dog.yaml" "$lone_out"
assert_contains "the output says where the config connects" "localhost:9440 over TLS" "$lone_out"
assert_not_contains "a first run replaces nothing" "existing file replaced" "$lone_out"
lone_rc=0
lone_out=$(cd "$lone_tmp/dl" && PATH="$lone_tmp/bin:$PATH" CLICKHOUSE_PASSWORD="lonepass" \
    bash ./install.sh docker -c localhost:4317 -v 26.04.1 2>&1) || lone_rc=$?
assert_eq "a second run succeeds" "0" "$lone_rc"
assert_contains "a second run says it replaced the config" "existing file replaced" "$lone_out"

# A generator refusal stops the run in its own words, before init overwrites anything.
sed -i.bak 's|\*" deploy docker "\*)|*" deploy docker "*) echo "refused by generator" >\&2; exit 7 ;; *" never "*)|' "$lone_tmp/bin/docker"
lone_rc=0
lone_out=$(cd "$lone_tmp/dl" && PATH="$lone_tmp/bin:$PATH" CLICKHOUSE_PASSWORD="lonepass" \
    bash ./install.sh docker -c localhost:4317 -v 26.04.1 -o refused 2>&1) || lone_rc=$?
assert_eq "a generator refusal is the exit status" "7" "$lone_rc"
assert_contains "a generator refusal is shown" "refused by generator" "$lone_out"
assert_eq "init does not run after a refusal" "no" \
    "$([[ -e "$lone_tmp/dl/refused/click-dog.yaml" ]] && echo yes || echo no)"

echo ""
echo "=== set_client_connection ==="

client_words() { # client_words CLIENT [HOST [PORT]]
    CLICKHOUSE_CLIENT="$1"
    set_client_connection "${2:-}" "${3:-}"
    echo "${CLIENT_CONNECTION[*]}"
}
assert_eq "a bare client is unchanged" "clickhouse-client" "$(client_words "clickhouse-client")"
assert_eq "the client's user and password are dropped" "clickhouse-client --secure" \
    "$(client_words "clickhouse-client -u admin --password hunter2 --secure")"
assert_eq "the = spellings are dropped" "clickhouse-client --host h" \
    "$(client_words "clickhouse-client --user=admin --password=hunter2 --host h")"
assert_eq "an attached -u is dropped" "clickhouse-client --host h" \
    "$(client_words "clickhouse-client -uadmin --host h")"
assert_eq "--password with no value keeps the next flag" "clickhouse-client --secure" \
    "$(client_words "clickhouse-client --password --secure")"
assert_eq "--password with no value keeps the next flag's value" "clickhouse-client --host db1 --port 9000" \
    "$(client_words "clickhouse-client --password --host db1 --port 9000")"
assert_eq "--ask-password is dropped" "clickhouse-client --host h" \
    "$(client_words "clickhouse-client --ask-password --host h")"
assert_eq "the two-word client is found" "clickhouse client" \
    "$(client_words "clickhouse client -u admin")"
assert_eq "a client path is found" "/usr/bin/clickhouse-client" \
    "$(client_words "/usr/bin/clickhouse-client -u admin")"
assert_eq "a client under another name is the first word" "chclient --secure" \
    "$(client_words "chclient -u admin --password pw --secure")"
assert_eq "a wrapper's -u stays" "docker exec -u root -e CLICKHOUSE_PASSWORD ch1 clickhouse-client" \
    "$(client_words "docker exec -u root -e CLICKHOUSE_PASSWORD ch1 clickhouse-client -u admin")"
assert_eq "a wrapper's path naming clickhouse does not end the wrapper" \
    "docker exec -w /var/lib/clickhouse -u root ch1 clickhouse-client" \
    "$(client_words "docker exec -w /var/lib/clickhouse -u root ch1 clickhouse-client -u admin")"
assert_eq "a wrapper's namespace and --user stay" \
    "kubectl exec -n clickhouse --user=ops pod -- clickhouse-client" \
    "$(client_words "kubectl exec -n clickhouse --user=ops pod -- clickhouse-client --user=admin")"

# A config flag stays on the normal connection and leaves the one a refused login
# tries next.
client_no_config() {
    CLICKHOUSE_CLIENT="$1"
    set_client_connection
    echo "${CLIENT_CONNECTION_NO_CONFIG[*]}"
}
for cw_flags in "-c /x.xml" "-C /x.xml" "--config /x.xml" "--config-file /x.xml" "-c/x.xml" "-C/x.xml" \
    "--config=/x.xml" "--config-file=/x.xml" "--connection prod" "--connection=prod"; do
    assert_eq "$cw_flags stays on the connection" "clickhouse-client --secure $cw_flags" \
        "$(client_words "clickhouse-client --secure $cw_flags")"
    assert_eq "$cw_flags leaves the no-config connection" "clickhouse-client --secure" \
        "$(client_no_config "clickhouse-client --secure $cw_flags")"
done
assert_eq "other flags stay on the no-config connection" "clickhouse-client --host h --port 9 --secure" \
    "$(client_no_config "clickhouse-client --host h --port 9 --secure")"

# A typed host or port takes the place of -C's: the client uses the first --host.
for cw_flags in "--host old" "--host=old" "-h old" "-hold"; do
    assert_eq "a typed host replaces $cw_flags" "clickhouse-client --secure --host new" \
        "$(client_words "clickhouse-client $cw_flags --secure" new)"
done
for cw_flags in "--port 9001" "--port=9001"; do
    assert_eq "a typed port replaces $cw_flags" "clickhouse-client --secure --host new --port 9002" \
        "$(client_words "clickhouse-client $cw_flags --secure" new 9002)"
    assert_eq "with no typed port, $cw_flags stays" "clickhouse-client $cw_flags --host new" \
        "$(client_words "clickhouse-client $cw_flags" new)"
done
assert_eq "the no-config connection gets the typed host too" "clickhouse-client --host new --port 9002" \
    "$(CLICKHOUSE_CLIENT="clickhouse-client --host old"; set_client_connection new 9002; echo "${CLIENT_CONNECTION_NO_CONFIG[*]}")"
assert_eq "with no typed host, -C's stays" "clickhouse-client --host old" "$(client_words "clickhouse-client --host old")"

# Whether this client reaches the server whose files are on this host.
client_local() {
    CLICKHOUSE_CLIENT="$1"
    set_client_connection
    echo "$CLIENT_IS_LOCAL"
}
for cw_client in "clickhouse-client" "clickhouse-client --host localhost" "clickhouse-client --host=127.0.0.1" "clickhouse-client -h::1"; do
    assert_eq "local: $cw_client" "true" "$(client_local "$cw_client")"
done
for cw_client in "clickhouse-client --host db1" "clickhouse-client --host=db1" "clickhouse-client -h db1" "clickhouse-client -hdb1" \
    "clickhouse-client --connection prod" "clickhouse-client --connection=prod" "docker exec c clickhouse-client"; do
    assert_eq "not local: $cw_client" "false" "$(client_local "$cw_client")"
done

# Once the empty config worked, every later connection uses it.
assert_eq "the empty config sticks, in place of -C's" "clickhouse-client --secure --config-file /tmp/empty.xml" \
    "$(CLIENT_USES_EMPTY_CONFIG=true CLIENT_EMPTY_CONFIG=/tmp/empty.xml; client_words "clickhouse-client -c /x.xml --secure")"
assert_eq "the empty config stays on this host" "clickhouse-client -c /x.xml --host db1" \
    "$(CLICKHOUSE_CLIENT="clickhouse-client -c /x.xml"; CLIENT_USES_EMPTY_CONFIG=true; CLIENT_EMPTY_CONFIG=/tmp/empty.xml; set_client_connection db1; echo "${CLIENT_CONNECTION[*]}")"

# The file the empty-config login uses.
cw_empty=$(
    clickhouse_setup_tempfiles=()
    make_client_empty_config
    make_client_empty_config
    echo "$CLIENT_EMPTY_CONFIG"
    echo "${#clickhouse_setup_tempfiles[@]}"
)
cw_empty_file=$(head -1 <<<"$cw_empty")
assert_contains "the empty config has the extension the client needs" ".xml" "$cw_empty_file"
assert_eq "the empty config is empty" "<config></config>" "$(cat "$cw_empty_file")"
# shellcheck disable=SC2012 # one known file
assert_eq "the empty config is private" "-rw-------" "$(ls -l "$cw_empty_file" | cut -c1-10)"
assert_eq "the empty config is made once, and cleaned up" "1" "$(tail -1 <<<"$cw_empty")"
rm -f "$cw_empty_file"

echo ""
echo "=== monitor_user_authenticates ==="

# ma_client exits with whatever the words ask for: rc=N on the normal connection,
# and empty=N with an empty config.
ma_client() {
    echo "$*" >> "$ma_log"
    if [[ " $* " == *" --config-file "* ]]; then
        return "$ma_empty_rc"
    fi
    return "$ma_rc"
}
ma_wrapper() { ma_client "$@"; }
ma_run() { # ma_run RC EMPTY_RC [CLIENT] — prints status, then logins made
    ma_rc="$1" ma_empty_rc="$2"
    (
        clickhouse_setup_tempfiles=()
        CLICKHOUSE_CLIENT="${3:-ma_client}"
        CLICKHOUSE_USER="mon"
        set_client_connection
        rc=0
        monitor_user_authenticates "pw" || rc=$?
        echo "$rc $(wc -l < "$ma_log" | tr -d ' ') ${CLIENT_USES_EMPTY_CONFIG:-false}"
        rm -f "${clickhouse_setup_tempfiles[@]+"${clickhouse_setup_tempfiles[@]}"}"
    )
    : > "$ma_log"
}
ma_log="$cw_empty_file.log"
: > "$ma_log"
TEST_TEMP_DIRS+=("$ma_log")
assert_eq "a login that works makes one try" "0 1 false" "$(ma_run 0 0)"
assert_eq "a refusal tries the empty config, and keeps it if it works" "0 2 true" "$(ma_run 4 0)"
assert_eq "a refusal on both is a refusal" "1 2 false" "$(ma_run 4 4)"
assert_eq "a refusal then no answer is still a refusal" "1 2 false" "$(ma_run 4 210)"
assert_eq "no answer makes no second try" "2 1 false" "$(ma_run 210 0)"
assert_eq "any other status is no answer" "2 1 false" "$(ma_run 1 0)"
assert_eq "a wrapper's client gets no second try" "1 1 false" "$(ma_run 4 0 "ma_wrapper clickhouse-client")"
assert_eq "a client on another host gets no second try" "1 1 false" "$(ma_run 4 0 "ma_client --host db1")"
assert_eq "a named connection gets no second try" "1 1 false" "$(ma_run 4 0 "ma_client --connection prod")"
assert_eq "a client on this host gets the second try" "0 2 true" "$(ma_run 4 0 "ma_client --host localhost")"

echo ""
echo "=== clickhouse_unreadable_files ==="

ur_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$ur_tmp")
mkdir -p "$ur_tmp/config.d" "$ur_tmp/conf.d" "$ur_tmp/users.d/subdir"
: > "$ur_tmp/conf.d/unreadable-g.xml"
: > "$ur_tmp/users.d/unreadable-h.XML"
: > "$ur_tmp/config.d/unreadable-i.YAML"
: > "$ur_tmp/users.d/unreadable-j.Conf"
: > "$ur_tmp/config.d/fine.xml"
: > "$ur_tmp/config.d/unreadable-a.xml"
: > "$ur_tmp/config.d/unreadable-b.yml"
: > "$ur_tmp/users.d/fine.yaml"
: > "$ur_tmp/users.d/unreadable-c.conf"
: > "$ur_tmp/users.d/unreadable-d.yaml"
: > "$ur_tmp/users.d/unreadable-e.xml.bak"
: > "$ur_tmp/users.d/.unreadable-f.xml"
# ur_run UNIT_USER UNIT_GROUP [SUPPLEMENTARY] — runs the check with stderr in
# $ur_tmp/note. Group IDs: chgrp 2002, chmain 3003 (chsvc's main group), extra 5005,
# suppx 4004, root 0. getent initgroups lists chsvc's member groups from UR_MEMBERS
# (default: extra). setpriv accepts only "--regid=X --groups=Y" equal to UR_EXPECT,
# and fails for files named unreadable. UR_NO_SETPRIV=1 runs with no setpriv at all.
ur_run() {
    (
        ur_user="$1" ur_group="$2" ur_supp="${3:-}"
        systemctl() {
            case "$3" in
                User) echo "$ur_user" ;;
                Group) echo "$ur_group" ;;
                SupplementaryGroups) echo "$ur_supp" ;;
            esac
        }
        id() {
            case "$1 $2" in
                "-gn chsvc") echo chmain ;;
                *) return 1 ;;
            esac
        }
        getent() {
            case "$1 $2" in
                "passwd 1001") echo "chsvc:x:1001:3003::/:/bin/false" ;;
                "group 2002"|"group chgrp") echo "chgrp:x:2002:" ;;
                "group chmain") echo "chmain:x:3003:" ;;
                "group root") echo "root:x:0:" ;;
                "group suppx") echo "suppx:x:4004:" ;;
                "group blankgid") echo "blankgid:x::" ;;
                "initgroups chsvc") echo "chsvc                 ${UR_MEMBERS-5005}" ;;
                *) return 2 ;;
            esac
        }
        if [[ -n "${UR_NO_SETPRIV:-}" ]]; then
            # shellcheck disable=SC2123 # no setpriv anywhere on the search path
            PATH=/nonexistent
        else
            setpriv() {
                echo "$*" >> "$ur_tmp/setpriv.log"
                [[ "$1" == "--reuid=chsvc" && "$2 $3" == "${UR_EXPECT:---regid=chgrp --groups=2002,5005}" ]] || return 1
                shift 3
                [[ "$1" == "true" ]] && return 0
                [[ "$1 $2" == "test -r" && "$3" != *unreadable* ]]
            }
        fi
        CLICKHOUSE_USERS_DIR="$ur_tmp/users.d"
        clickhouse_unreadable_files 2>"$ur_tmp/note"
    )
}
ur_note() { cat "$ur_tmp/note"; }
ur_ran() { # ur_ran OUTPUT — "yes" when the check ran and found the unreadable files
    if [[ "$1" == *unreadable-a.xml* ]]; then echo yes; else echo no; fi
}
ur_out=$(ur_run chsvc chgrp)
for ur_name in config.d/unreadable-a.xml config.d/unreadable-b.yml users.d/unreadable-c.conf users.d/unreadable-d.yaml \
    conf.d/unreadable-g.xml users.d/unreadable-h.XML config.d/unreadable-i.YAML users.d/unreadable-j.Conf; do
    assert_contains "unreadable: names $ur_name" "$ur_tmp/$ur_name" "$ur_out"
done
assert_not_contains "unreadable: leaves a readable file out" "fine" "$ur_out"
assert_not_contains "unreadable: ignores a file ClickHouse does not load" ".bak" "$ur_out"
assert_not_contains "unreadable: ignores a hidden file" ".unreadable-f" "$ur_out"
assert_eq "unreadable: a check that runs adds no note" "" "$(ur_note)"
assert_not_contains "unreadable: the main group is left out when the user is not its member" "3003" "$(cat "$ur_tmp/setpriv.log")"
rm -f "$ur_tmp/setpriv.log"
assert_eq "unreadable: a user listed in its main group keeps it" "yes" \
    "$(ur_ran "$(UR_MEMBERS="3003 5005" UR_EXPECT="--regid=chgrp --groups=2002,3003,5005" ur_run chsvc chgrp)")"
assert_eq "unreadable: with no unit group, the main group counts" "yes" \
    "$(ur_ran "$(UR_EXPECT="--regid=chmain --groups=3003,5005" ur_run chsvc "")")"
assert_eq "unreadable: group 0 adds no member groups" "yes" \
    "$(ur_ran "$(UR_EXPECT="--regid=root --groups=0" ur_run chsvc root)")"
assert_eq "unreadable: SupplementaryGroups= counts, by name or number" "yes" \
    "$(ur_ran "$(UR_EXPECT="--regid=chgrp --groups=2002,5005,4004,4242" ur_run chsvc chgrp "suppx 4242")")"
assert_eq "unreadable: group 0 still counts SupplementaryGroups=" "yes" \
    "$(ur_ran "$(UR_EXPECT="--regid=root --groups=0,4004" ur_run chsvc root suppx)")"
assert_eq "unreadable: a group listed twice counts once" "yes" \
    "$(ur_ran "$(UR_MEMBERS="2002 5005" ur_run chsvc chgrp)")"
assert_eq "unreadable: a user with no member groups" "yes" \
    "$(ur_ran "$(UR_MEMBERS="" UR_EXPECT="--regid=chgrp --groups=2002" ur_run chsvc chgrp)")"
assert_eq "unreadable: a numeric User= becomes its name" "yes" "$(ur_ran "$(ur_run 1001 chgrp)")"
assert_eq "unreadable: a numeric Group= becomes its name" "yes" "$(ur_ran "$(ur_run chsvc 2002)")"
rm -f "$ur_tmp/setpriv.log"

assert_eq "unreadable: no unit user, no check" "" "$(ur_run "" chgrp)"
assert_contains "unreadable: no unit user, says so" "systemd names no user for clickhouse-server" "$(ur_note)"
assert_eq "unreadable: a group it cannot find, no check" "" "$(ur_run nogrp "")"
assert_contains "unreadable: a group it cannot find, says so" "cannot find the group clickhouse-server runs as" "$(ur_note)"
assert_eq "unreadable: no user or group, setpriv never runs" "no" "$([[ -e "$ur_tmp/setpriv.log" ]] && echo yes || echo no)"
assert_eq "unreadable: an unknown numeric User=, no check" "" "$(ur_run 4242 chgrp)"
assert_eq "unreadable: an unknown SupplementaryGroups= entry, no check" "" "$(ur_run chsvc chgrp nosuch)"
assert_contains "unreadable: an unknown SupplementaryGroups= entry, says so" "cannot find every group clickhouse-server starts with" "$(ur_note)"
assert_eq "unreadable: a SupplementaryGroups= entry with no ID, no check" "" "$(ur_run chsvc chgrp blankgid)"
assert_contains "unreadable: a SupplementaryGroups= entry with no ID, says so" "cannot find every group" "$(ur_note)"
assert_eq "unreadable: a unit group with no ID, no check" "" "$(ur_run chsvc blankgid)"
assert_contains "unreadable: a unit group with no ID, says so" "cannot find every group" "$(ur_note)"
assert_eq "unreadable: no setpriv, no check" "" "$(UR_NO_SETPRIV=1 ur_run chsvc chgrp)"
assert_contains "unreadable: no setpriv, says so" "setpriv is not installed" "$(ur_note)"
assert_eq "unreadable: a setpriv that fails, no check" "" "$(UR_EXPECT="never" ur_run chsvc chgrp)"
assert_contains "unreadable: a setpriv that fails, says so" "setpriv cannot run as chsvc" "$(ur_note)"
assert_contains "unreadable: the note says what was skipped" "cannot check that ClickHouse can read its config files" "$(ur_note)"

echo ""
echo "=== check_clickhouse_files ==="

cc_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$cc_tmp")
mkdir -p "$cc_tmp/users.d"
# cc_run UNREADABLE GROUP — prints the output, then the status.
cc_run() {
    (
        cc_unreadable="$1" cc_group="$2"
        clickhouse_unreadable_files() { [[ -z "$cc_unreadable" ]] || echo "$cc_unreadable"; }
        clickhouse_server_group() { echo "$cc_group"; }
        # shellcheck disable=SC2034 # read by the eval'd function
        CLICKHOUSE_USERS_DIR="$cc_tmp/users.d"
        rc=0
        check_clickhouse_files 2>&1 || rc=$?
        echo "status=$rc"
    )
}
assert_eq "clean: nothing stops the guide" "status=0" "$(cc_run "" chgrp)"
cc_out=$(cc_run "/etc/x/a.xml" chgrp)
assert_contains "an unreadable file stops the guide" "status=1" "$cc_out"
assert_contains "an unreadable file is named" "/etc/x/a.xml" "$cc_out"
assert_contains "the fix uses the server's group" "chown root:chgrp FILE && chmod 640 FILE" "$cc_out"
cc_out=$(cc_run "/etc/x/a.xml" "")
assert_contains "with no group, the fix is world-readable" "chmod 644 FILE" "$cc_out"
assert_not_contains "with no group, no chown" "chown" "$cc_out"

echo '<query>GRANT SELECT ON system.query_log TO mon</query>' > "$cc_tmp/users.d/click-dog-any-name.xml"
cc_out=$(cc_run "$cc_tmp/users.d/click-dog-any-name.xml" chgrp)
assert_contains "a grantee stops the guide" "status=1" "$cc_out"
assert_contains "a grantee file is named" "$cc_tmp/users.d/click-dog-any-name.xml" "$cc_out"
assert_contains "the fix is to delete it" "rm FILE" "$cc_out"
assert_contains "the guide says it never loaded" "never loaded" "$cc_out"
assert_not_contains "a chown cannot fix a grantee" "chown" "$cc_out"
echo '<query>grant select on system.query_log to mon</query>' > "$cc_tmp/users.d/click-dog-any-name.xml"
assert_contains "a grantee in lower case stops the guide" "status=1" "$(cc_run "" chgrp)"
rm -f "$cc_tmp/users.d/click-dog-any-name.xml"
echo '<query>GRANT SELECT ON system.query_log</query>' > "$cc_tmp/users.d/click-dog-mon.xml"
assert_eq "a grant with no grantee is fine" "status=0" "$(cc_run "" chgrp)"
printf '%s\n' '<!-- <query>GRANT SELECT ON system.query_log TO mon</query> -->' '<query>GRANT SELECT ON system.query_log</query>' > "$cc_tmp/users.d/click-dog-mon.xml"
assert_eq "a grantee in a comment is fine" "status=0" "$(cc_run "" chgrp)"
printf '%s\n' '<!--' '<query>GRANT SELECT ON system.query_log TO mon</query>' '-->' > "$cc_tmp/users.d/click-dog-mon.xml"
assert_eq "a grantee in a comment over lines is fine" "status=0" "$(cc_run "" chgrp)"
printf '%s\n' '<!-- old --> <query>GRANT SELECT ON system.query_log TO mon</query> <!-- x -->' > "$cc_tmp/users.d/click-dog-mon.xml"
assert_contains "a grantee between comments stops the guide" "status=1" "$(cc_run "" chgrp)"
printf '%s\n' '<!-- a' 'b -->' '<query>GRANT SELECT ON system.query_log TO mon</query>' > "$cc_tmp/users.d/click-dog-mon.xml"
assert_contains "a grantee after a comment over lines stops the guide" "status=1" "$(cc_run "" chgrp)"
printf '%s\n' '<query>GRANT SELECT ON system.query_log</query>' > "$cc_tmp/users.d/click-dog-mon.xml"
echo '<query>GRANT SELECT ON system.query_log TO mon</query>' > "$cc_tmp/users.d/ops.xml"
assert_eq "an operator's own file is left to the unreadable check" "status=0" "$(cc_run "" chgrp)"

echo ""
echo "=== docker render output ==="

dr_tmp="$(make_temp_dir)"
TEST_TEMP_DIRS+=("$dr_tmp")
mkdir -p "$dr_tmp/out#a&b"
dr_out=$(
    docker_run_mounted() { echo "Wrote /work/click-dog.yaml and /work/other"; echo "no path here"; }
    VERSION=1 CLICKHOUSE_TLS=false KEEPER_HOSTS="" OTEL_SECURE=""
    render_click_dog_config_via_docker "$dr_tmp/out#a&b/click-dog.yaml"
)
dr_dir=$(cd "$dr_tmp/out#a&b" && pwd)
assert_eq "a # or & in the output path prints as typed" \
    "Wrote $dr_dir/click-dog.yaml and $dr_dir/other
no path here" "$dr_out"
dr_rc=0
(
    docker_run_mounted() { return 7; }
    VERSION=1 CLICKHOUSE_TLS=false KEEPER_HOSTS="" OTEL_SECURE=""
    render_click_dog_config_via_docker "$dr_tmp/out#a&b/click-dog.yaml"
) >/dev/null || dr_rc=$?
assert_eq "a failed render keeps its status" "7" "$dr_rc"
assert_eq "a render leaves no temp file" "0" "$(find "$TMPDIR" -maxdepth 1 -name 'click-dog-render*' | wc -l | tr -d ' ')"

echo ""
echo "=== guide_read_secret ==="

# shellcheck disable=SC2154 # guide_read_secret sets it
grs_value=$(guide_read_secret grs_answer "" <<<"  with spaces  " >/dev/null; printf '%s' "$grs_answer")
assert_eq "a typed password keeps its spaces" "  with spaces  " "$grs_value"
assert_contains "a typed password does not echo" 'IFS= read -rsp' "$(awk '/^guide_read_secret\(\)/,/^}/' deploy/install.sh)"

echo ""
echo "=== run_sql_file ==="

rs_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$rs_tmp")
echo "SELECT 1;" > "$rs_tmp/sql"
rs_answers() { sed -n "s/^SELECT '\(.*\)';/\1/p"; }
rs_runs() { rs_answers; }
rs_no_stdin() { :; }
rs_fails() { echo "Code: 516" >&2; return 4; }
rs_rc=0; rs_err=$(run_sql_file "$rs_tmp/sql" rs_runs 2>&1) || rs_rc=$?
assert_eq "SQL that came back succeeds" "0" "$rs_rc"
rs_rc=0; rs_err=$(run_sql_file "$rs_tmp/sql" rs_no_stdin 2>&1) || rs_rc=$?
assert_eq "a client without stdin fails" "1" "$rs_rc"
assert_contains "a client without stdin says so" "did not reach the client" "$rs_err"
rs_rc=0; rs_err=$(run_sql_file "$rs_tmp/sql" rs_fails 2>&1) || rs_rc=$?
assert_eq "a failing client keeps its status" "4" "$rs_rc"
assert_contains "a failing client's error shows" "Code: 516" "$rs_err"

echo ""
echo "=== setup_monitoring_user against a fake ClickHouse ==="

# Users live in files, as hash + storage. Like the real server: an overlay in
# users.d wins over a SQL user of the same name, an overlay naming a grantee does
# not load, SQL cannot change an XML user, CREATE USER IF NOT EXISTS leaves an
# existing user alone, a repeated --user is a usage error, and a password in the
# client's config beats CLICKHOUSE_PASSWORD.
mu_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$mu_tmp")
cat > "$mu_tmp/clickhouse-client" <<'FAKE'
#!/usr/bin/env bash
echo "$*" >> "$FAKE_CH_STATE/argv"
[[ -z "${FAKE_CH_DOWN:-}" ]] || { echo "Code: 210. Connection refused" >&2; exit 210; }
user="" query="" multi="false" config="" connection=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        -u|--user)
            if [[ -n "$user" ]]; then
                echo "Bad arguments: option '--user' cannot be specified more than once" >&2
                exit 1
            fi
            user="$2"
            shift 2
            ;;
        --config-file|--config|-c|-C) config="$2"; shift 2 ;;
        --config-file=*|--config=*) config="${1#*=}"; shift ;;
        -c*|-C*) config="${1:2}"; shift ;;
        --connection) connection="$2"; shift 2 ;;
        --password|--host|--port) shift 2 ;;
        -q) query="$2"; shift 2 ;;
        --multiquery) multi="true"; shift ;;
        *) shift ;;
    esac
done
[[ -n "$config" ]] || config="$HOME/.clickhouse-client/config.xml"
# Any password element, even an empty one, beats the environment.
if [[ -f "$config" ]] && grep -q '<password' "$config"; then
    CLICKHOUSE_PASSWORD=$(sed -n 's|.*<password[^>]*>\([^<]*\)</password>.*|\1|p' "$config")
fi
[[ -z "$connection" ]] || CLICKHOUSE_PASSWORD="connection-password"
sha() {
    if command -v sha256sum >/dev/null; then
        printf '%s' "$1" | sha256sum | awk '{print $1}'
    else
        printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
    fi
}
overlay() { echo "$FAKE_CH_USERS_DIR/click-dog-$1.xml"; }
overlay_loaded() {
    [[ -z "${FAKE_CH_IGNORE_OVERLAY:-}" && -f "$(overlay "$1")" ]] && ! grep -q ' TO ' "$(overlay "$1")"
}
stored_hash() {
    if overlay_loaded "$1"; then
        sed -n 's|.*<password_sha256_hex>\(.*\)</password_sha256_hex>.*|\1|p' "$(overlay "$1")"
    elif [[ -f "$FAKE_CH_STATE/$1.xml-hash" ]]; then
        cat "$FAKE_CH_STATE/$1.xml-hash"
    elif [[ -f "$FAKE_CH_STATE/$1.hash" ]]; then
        cat "$FAKE_CH_STATE/$1.hash"
    fi
}
may_read() {
    if overlay_loaded "$1"; then
        grep -q GRANT "$(overlay "$1")"
    else
        [[ -f "$FAKE_CH_STATE/$1.grants" ]]
    fi
}
# The monitoring user, reached through the same client.
if [[ -n "$user" && "$user" != "admin" && "$user" != "default" ]]; then
    if [[ -n "${FAKE_CH_LOGIN_FAILS:-}" ]]; then
        logins=$(cat "$FAKE_CH_STATE/logins" 2>/dev/null || echo 0)
        echo $((logins + 1)) > "$FAKE_CH_STATE/logins"
        if ((logins < FAKE_CH_LOGIN_FAILS)); then
            echo "Code: 516. Authentication failed" >&2
            exit 4
        fi
    fi
    if [[ "$(stored_hash "$user")" != "$(sha "${CLICKHOUSE_PASSWORD:-}")" ]]; then
        echo "Code: 516. Authentication failed" >&2
        exit 4
    fi
    if [[ "$multi" == "true" ]]; then
        [[ -z "${FAKE_CH_NO_STDIN:-}" ]] || exit 0
        if [[ -n "${FAKE_CH_NO_SPAN_TABLE:-}" ]]; then
            echo "Code: 60. Unknown table expression identifier 'system.opentelemetry_span_log'. (UNKNOWN_TABLE)" >&2
            exit 60
        fi
        if [[ -n "${FAKE_CH_NO_QUERY_LOG:-}" ]]; then
            echo "Code: 60. Unknown table expression identifier 'system.query_log'. (UNKNOWN_TABLE)" >&2
            exit 60
        fi
        if ! may_read "$user"; then
            echo "Code: 497. Not enough privileges" >&2
            exit 1
        fi
        echo click-dog-sql-ran
    fi
    exit 0
fi
# The administrator.
if [[ -n "${FAKE_CH_ADMIN_BLANK_ONLY:-}" && "$multi" == "true" && -n "${CLICKHOUSE_PASSWORD:-}" ]]; then
    echo "Code: 516. Authentication failed" >&2
    exit 4
fi
if [[ -n "${FAKE_CH_ADMIN_NEEDS_PASSWORD:-}" && "${CLICKHOUSE_PASSWORD:-}" != "$FAKE_CH_ADMIN_NEEDS_PASSWORD" ]]; then
    echo "Code: 516. Authentication failed" >&2
    exit 4
fi
if [[ "$query" == *system.users* ]]; then
    if [[ -n "${FAKE_CH_NO_SHOW_USERS:-}" ]]; then
        echo "Code: 497. Not enough privileges" >&2
        exit 1
    fi
    # A file ClickHouse has yet to poll: this many answers leave it out.
    if [[ -n "${FAKE_CH_SLOW_LOAD:-}" ]]; then
        polls=$(cat "$FAKE_CH_STATE/polls" 2>/dev/null || echo 0)
        echo $((polls + 1)) > "$FAKE_CH_STATE/polls"
        ((polls >= FAKE_CH_SLOW_LOAD)) || exit 0
    fi
    if overlay_loaded "$FAKE_CH_USER" || [[ -f "$FAKE_CH_STATE/$FAKE_CH_USER.xml-hash" ]]; then
        echo "users_xml"
    fi
    if [[ -f "$FAKE_CH_STATE/$FAKE_CH_USER.hash" ]]; then
        echo "local_directory"
    fi
    exit 0
fi
if [[ "$multi" == "true" ]]; then
    [[ -z "${FAKE_CH_NO_STDIN:-}" ]] || exit 0
    while IFS= read -r line; do
        hash=$(sed -n "s/.* BY '\(.*\)';/\1/p" <<<"$line")
        case "$line" in
            "CREATE USER IF NOT EXISTS "*)
                [[ -f "$FAKE_CH_STATE/$FAKE_CH_USER.hash" ]] || echo "$hash" > "$FAKE_CH_STATE/$FAKE_CH_USER.hash"
                ;;
            "ALTER USER "*)
                if [[ -n "${FAKE_CH_ALTER_DENIED:-}" ]]; then
                    echo "Code: 497. Not enough privileges" >&2
                    exit 1
                fi
                echo "$hash" > "$FAKE_CH_STATE/$FAKE_CH_USER.hash"
                echo altered >> "$FAKE_CH_STATE/log"
                ;;
            "GRANT "*) : > "$FAKE_CH_STATE/$FAKE_CH_USER.grants" ;;
            "SELECT 'click-dog-sql-ran';") echo click-dog-sql-ran ;;
        esac
    done
fi
exit 0
FAKE
chmod +x "$mu_tmp/clickhouse-client"

export FAKE_CH_STATE="$mu_tmp/state" FAKE_CH_USERS_DIR="$mu_tmp/users.d" FAKE_CH_USER="click_dog_monitor"
mu_hash() { monitor_password_hash "$1"; }
mu_file="$FAKE_CH_USERS_DIR/click-dog-$FAKE_CH_USER.xml"
# mu_reset: no users, no overlay, no .secret, an administrator who can do anything.
mu_reset() {
    rm -rf "$FAKE_CH_STATE" "$FAKE_CH_USERS_DIR" "${mu_tmp:?}/.secret" "${mu_tmp:?}/final" "${mu_tmp:?}/home" "${mu_tmp:?}/setup_sql" "${mu_tmp:?}/password_kept" "${mu_tmp:?}/chown" "${mu_tmp:?}/conf.d"
    mkdir -p "$FAKE_CH_STATE" "$FAKE_CH_USERS_DIR" "$mu_tmp/home/.clickhouse-client"
}
mu_sql_user() { mu_hash "$1" > "$FAKE_CH_STATE/$FAKE_CH_USER.hash"; : > "$FAKE_CH_STATE/$FAKE_CH_USER.grants"; }
mu_overlay() { generate_user_setup_xml "$FAKE_CH_USER" "$(mu_hash "$1")" > "$mu_file"; }
# mu_overlay_no_grants: as a file edited by hand, without the two GRANTs.
mu_overlay_no_grants() { mu_overlay "$1"; sed -i.bak '/GRANT/d' "$mu_file"; rm -f "$mu_file.bak"; }
# mu_broken_overlay: a file ClickHouse refuses to load.
mu_broken_overlay() { mu_overlay "$1"; sed -i.bak 's|<grants>|<grants><query>GRANT SELECT ON system.query_log TO x</query>|' "$mu_file"; rm -f "$mu_file.bak"; }
# mu_xml_user: another XML file defines the user, as an operator might.
mu_xml_user() {
    mu_hash "$1" > "$FAKE_CH_STATE/$FAKE_CH_USER.xml-hash"
    echo "<clickhouse><users><$FAKE_CH_USER></$FAKE_CH_USER></users></clickhouse>" > "$FAKE_CH_USERS_DIR/ops.xml"
}
# mu_client_config: root's client config carries an admin's credentials.
mu_client_config() {
    echo '<config><user>admin</user><password>adminpw</password></config>' > "$mu_tmp/home/.clickhouse-client/config.xml"
}
# mu_auth PASSWORD — would click-dog connect with this password now?
mu_auth() {
    if CLICKHOUSE_PASSWORD="$1" "$mu_tmp/clickhouse-client" -u "$FAKE_CH_USER" -q "SELECT 1" 2>/dev/null; then
        echo yes
    else
        echo no
    fi
}
# mu_run ANSWER... — run the setup with these answers on stdin. Sets mu_out and
# mu_rc; the password it settled on, the one step 3 writes, lands in mu_final.
mu_run() {
    mu_rc=0
    mu_out=$(
        # The eval'd setup appends to this.
        # shellcheck disable=SC2034
        clickhouse_setup_tempfiles=()
        clickhouse_file_install_cmd() { echo "${MU_INSTALL_CMD:-cp}"; }
        sleep() { :; }
        docker() { return 1; }
        chown() { echo "$*" >> "$mu_tmp/chown"; }
        # The globals go to the eval'd functions.
        # shellcheck disable=SC2034
        CLICKHOUSE_CLIENT="${MU_CLIENT:-$mu_tmp/clickhouse-client}" CLICKHOUSE_USER="$FAKE_CH_USER" \
            CLICKHOUSE_PASSWORD="" SECRET_FILE="$mu_tmp/.secret" CLICKHOUSE_USERS_DIR="$FAKE_CH_USERS_DIR" \
            HOME="$mu_tmp/home"
        if [[ -n "${MU_EXPORT_PASSWORD:-}" ]]; then
            export CLICKHOUSE_PASSWORD
        fi
        if [[ -n "${MU_STOP_IN_WAIT:-}" ]]; then
            wait_for_monitor_user() { exit 130; }
        fi
        setup_monitoring_user <<<"$(printf '%s\n' "$@")" 2>&1
        printf '%s' "$CLICKHOUSE_PASSWORD" > "$mu_tmp/final"
        printf '%s' "${MONITOR_SETUP_SQL:-}" > "$mu_tmp/setup_sql"
        printf '%s' "${MONITOR_PASSWORD_KEPT:-}" > "$mu_tmp/password_kept"
    ) || mu_rc=$?
    mu_final=$(cat "$mu_tmp/final" 2>/dev/null || true)
}
mu_exists() { if [[ -e "$1" ]]; then echo yes; else echo no; fi; }

# No user at all: the same password lands on both sides.
mu_reset
mu_run r
assert_eq "a new user: setup succeeds" "0" "$mu_rc"
assert_contains "a new user: the copy says the guide sets the password" "The guide sets the same password" "$mu_out"
assert_not_contains "a new user: nothing asks about an existing one" "already exists" "$mu_out"
assert_eq "a new user: the generated password connects" "yes" "$(mu_auth "$mu_final")"
assert_eq "a new user: the password is saved at once" "$mu_final" "$(cat "$mu_tmp/.secret")"
# shellcheck disable=SC2012 # one known file
assert_eq "a new user: the saved password is private" "-rw-------" "$(ls -l "$mu_tmp/.secret" | cut -c1-10)"
assert_eq "a new user: the not-running ending can name the SQL" "yes" "$(mu_exists "$(cat "$mu_tmp/setup_sql")")"
assert_contains "a new user: the saved password goes to click-dog's user" "click-dog:click-dog $mu_tmp/.secret" "$(cat "$mu_tmp/chown")"

# SQL cannot change a user the overlay defines.
mu_reset
mu_run x
assert_eq "a new user, [x]: the not-running ending offers no SQL" "" "$(cat "$mu_tmp/setup_sql")"

# A closed stdin stops the guide instead of looping.
mu_reset
mu_run
assert_eq "closed stdin: the guide stops" "1" "$mu_rc"
assert_contains "closed stdin: says why" "stdin closed" "$mu_out"

# The reinstall: `deploy uninstall` removed .secret and left the SQL user.
mu_reset; mu_sql_user "oldpass"
mu_run e wrongpass e oldpass
assert_eq "reinstall: entering the password succeeds" "0" "$mu_rc"
assert_contains "reinstall: names the cause" "already exists, and this host has no" "$mu_out"
assert_not_contains "reinstall: never says it sets the password" "The guide sets the same password" "$mu_out"
assert_contains "reinstall: a wrong password is refused" "did not authenticate" "$mu_out"
assert_eq "reinstall: step 3 gets the entered password" "oldpass" "$mu_final"
assert_contains "reinstall: nothing further to do" "nothing to do" "$mu_out"
assert_eq "reinstall: entering a password changes nothing in ClickHouse" "" "$(cat "$FAKE_CH_STATE/log" 2>/dev/null || true)"

mu_reset; mu_sql_user "oldpass"
mu_run n r
assert_eq "reinstall: a new password succeeds" "0" "$mu_rc"
assert_contains "reinstall: the ALTER is shown before it runs" "ALTER USER click_dog_monitor" "$mu_out"
assert_eq "reinstall: the new password connects" "yes" "$(mu_auth "$mu_final")"
assert_eq "reinstall: the old password stops" "no" "$(mu_auth "oldpass")"
assert_eq "reinstall: the new password is saved at once" "$mu_final" "$(cat "$mu_tmp/.secret")"

# [n] replaces a password that just failed.
mu_reset; mu_sql_user "current"
printf 'stale' > "$mu_tmp/.secret"
mu_run n r
assert_eq "stale secret, [n]: the password is new" "no" "$([[ "$mu_final" == "stale" ]] && echo yes || echo no)"
assert_eq "stale secret, [n]: the new password connects" "yes" "$(mu_auth "$mu_final")"
assert_eq "stale secret, [n]: the new password is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"
assert_not_contains "stale secret, [n]: claims no reuse" "Reusing password from" "$mu_out"

mu_reset; mu_sql_user "current"
printf 'stale' > "$mu_tmp/.secret"
MU_CLIENT="/nonexistent/clickhouse-client"
mu_run r "$mu_tmp/clickhouse-client" n
assert_eq "stale secret, [n] after the SQL: the password is new" "no" "$([[ "$mu_final" == "stale" ]] && echo yes || echo no)"
assert_eq "stale secret, [n] after the SQL: the new password connects" "yes" "$(mu_auth "$mu_final")"
unset MU_CLIENT

# After [e], the password in use is the one typed.
mu_reset; mu_sql_user "current"
rm -f "$FAKE_CH_STATE/$FAKE_CH_USER.grants"
printf 'stale' > "$mu_tmp/.secret"
mu_run e current r
assert_not_contains "stale secret, [e]: claims no reuse" "Reusing password from" "$mu_out"

mu_reset; mu_sql_user "oldpass"
mu_run n s
assert_contains "reinstall: skipping after [n] says nothing changed" "did not change" "$mu_out"
assert_eq "reinstall: skipping after [n] leaves the password" "yes" "$(mu_auth "oldpass")"
assert_eq "reinstall: skipping after [n] saves nothing" "no" "$(mu_exists "$mu_tmp/.secret")"
assert_not_contains "reinstall: skipping after [n] leaves no ALTER for the ending" "ALTER USER click_dog_monitor" "$(cat "$(cat "$mu_tmp/setup_sql")")"

# The client's config password beats the saved one on another host, so a correct
# saved password looks wrong. [n] then [s] must not replace it.
mu_reset; mu_sql_user "stored"; mu_client_config
printf 'stored' > "$mu_tmp/.secret"
MU_CLIENT="$mu_tmp/clickhouse-client --host ch1.example.com --secure"
mu_run n s
assert_contains "another host, config password: says the saved one can be right" "the saved"$'\n'"  password can be right" "$mu_out"
assert_eq "another host, config password: [n] then [s] keeps the saved password" "stored" "$mu_final"
assert_contains "another host, config password: says .secret keeps it" "keeps the password it had" "$mu_out"
assert_eq "another host, config password: the saved password still works" "yes" "$(mu_auth "$mu_final")"
assert_contains "another host, config password: the ending's SQL holds the saved password" \
    "$(mu_hash "stored")" "$(cat "$(cat "$mu_tmp/setup_sql")")"
unset MU_CLIENT

mu_reset; mu_sql_user "current"
printf 'stale' > "$mu_tmp/.secret"
mu_run k s
assert_not_contains "this host: no note about the client config" "can be right" "$mu_out"

mu_reset; mu_sql_user "oldpass"
mu_run k r
assert_eq "reinstall: keeping the user succeeds" "0" "$mu_rc"
assert_contains "reinstall: keeping says what is left to do" "cannot connect until" "$mu_out"
assert_eq "reinstall: keeping leaves the password" "yes" "$(mu_auth "oldpass")"
assert_eq "reinstall: keeping runs no ALTER" "" "$(cat "$FAKE_CH_STATE/log" 2>/dev/null || true)"
assert_not_contains "reinstall: keeping withdraws the overlay" "[x] Write XML config (no" "$mu_out"
assert_eq "reinstall: keeping saves nothing" "no" "$(mu_exists "$mu_tmp/.secret")"
assert_eq "reinstall: keeping is noted for the ending" "true" "$(cat "$mu_tmp/password_kept")"
assert_eq "reinstall: keeping leaves no SQL for the ending" "" "$(cat "$mu_tmp/setup_sql")"

# A SQL user never gets the overlay: it would hide the user's settings and grants.
mu_reset; mu_sql_user "oldpass"
rm -f "$FAKE_CH_STATE/$FAKE_CH_USER.grants"
mu_run e oldpass x r
assert_eq "SQL user: setup succeeds" "0" "$mu_rc"
assert_not_contains "SQL user: the menu has no overlay" "[x] Write XML config" "$mu_out"
assert_contains "SQL user: [x] is refused" "[x] is not available" "$mu_out"
assert_eq "SQL user: no overlay appears" "no" "$(mu_exists "$mu_file")"
assert_eq "SQL user: [r] adds the grants" "yes" "$(mu_exists "$FAKE_CH_STATE/$FAKE_CH_USER.grants")"

mu_reset; mu_sql_user "oldpass"
mu_run n x r
assert_contains "SQL user, new password: [x] is refused" "[x] is not available" "$mu_out"
assert_eq "SQL user, new password: no overlay appears" "no" "$(mu_exists "$mu_file")"
assert_eq "SQL user, new password: [r] sets it" "yes" "$(mu_auth "$mu_final")"

# Another XML file defines the user. A second definition stops ClickHouse.
mu_reset; mu_xml_user "oppass"
mu_run n k
assert_eq "operator XML: keeping succeeds" "0" "$mu_rc"
assert_contains "operator XML: names the file" "ops.xml defines it" "$mu_out"
assert_not_contains "operator XML: no new password on offer" "[n] Set a new password" "$mu_out"
assert_contains "operator XML: [n] is refused" "Please enter one of e/k" "$mu_out"
assert_not_contains "operator XML: no overlay on offer" "[x] Write XML config" "$mu_out"
assert_not_contains "operator XML: no SQL on offer" "[r] Run SQL" "$mu_out"
assert_eq "operator XML: no overlay appears" "no" "$(mu_exists "$mu_file")"
assert_eq "operator XML: the password stands" "yes" "$(mu_auth "oppass")"
assert_eq "operator XML: the ending offers no SQL" "" "$(cat "$mu_tmp/setup_sql")"

mu_reset; mu_xml_user "oppass"
: > "$FAKE_CH_STATE/$FAKE_CH_USER.grants"
mu_run e oppass
assert_contains "operator XML with grants: nothing to do" "nothing to do" "$mu_out"
assert_eq "operator XML with grants: step 3 gets the password" "oppass" "$mu_final"

mu_reset; mu_xml_user "oppass"
mu_run e oppass
assert_eq "operator XML without grants: succeeds with no menu" "0" "$mu_rc"
assert_contains "operator XML without grants: says so" "MISSING SELECT" "$mu_out"
assert_contains "operator XML without grants: says what to add" "add these lines" "$mu_out"
assert_contains "operator XML without grants: gives the grant" "GRANT SELECT ON system.query_log" "$mu_out"
assert_not_contains "operator XML without grants: promises no SQL" "The setup SQL adds" "$mu_out"
assert_eq "operator XML without grants: no overlay appears" "no" "$(mu_exists "$mu_file")"

mu_reset; mu_xml_user "oppass"
export FAKE_CH_NO_SHOW_USERS=1
mu_run k
assert_eq "operator XML, unreadable system.users: the file shows it" "0" "$mu_rc"
assert_contains "operator XML, unreadable system.users: names the file" "ops.xml defines it" "$mu_out"
assert_eq "operator XML, unreadable system.users: no overlay appears" "no" "$(mu_exists "$mu_file")"
unset FAKE_CH_NO_SHOW_USERS

# The guide's own file exists and ClickHouse does not report it. The guide warns
# and leaves it: it cannot tell a broken file from a healthy one it reads wrongly.
mu_reset; mu_broken_overlay "oldpass"
mu_run r
assert_eq "file not loaded: setup succeeds" "0" "$mu_rc"
assert_contains "file not loaded: warns" "WARNING: ClickHouse does not report" "$mu_out"
assert_contains "file not loaded: says what it risks" "does not start with a users.d file it cannot load" "$mu_out"
assert_not_contains "file not loaded: offers no delete" "Delete $mu_file" "$mu_out"
assert_eq "file not loaded: the file stays" "yes" "$(mu_exists "$mu_file")"
assert_not_contains "file not loaded: no claim the user exists" "already exists" "$mu_out"
assert_eq "file not loaded: the new user connects" "yes" "$(mu_auth "$mu_final")"

mu_reset; mu_broken_overlay "oldpass"
mu_run x
assert_eq "file not loaded, [x]: setup succeeds" "0" "$mu_rc"
assert_contains "file not loaded, [x]: says it replaces the whole file" "This replaces the whole of" "$mu_out"
assert_contains "file not loaded, [x]: names what else goes" "anything else in it" "$mu_out"
assert_not_contains "file not loaded, [x]: leaves no grantee" " TO " "$(cat "$mu_file")"
assert_eq "file not loaded, [x]: connects" "yes" "$(mu_auth "$mu_final")"

mu_reset; mu_broken_overlay "oldpass"; mu_sql_user "sqlpass"
mu_run e sqlpass
assert_contains "file not loaded, SQL user: the SQL user is found" "already exists" "$mu_out"
assert_eq "file not loaded, SQL user: step 3 gets its password" "sqlpass" "$mu_final"
assert_eq "file not loaded, SQL user: the setup SQL can still help" "yes" "$(mu_exists "$(cat "$mu_tmp/setup_sql")")"

# A healthy file ClickHouse has yet to poll is not reported as broken.
mu_reset; mu_overlay "oldpass"
export FAKE_CH_SLOW_LOAD=3
mu_run e oldpass
assert_not_contains "a file just written: no warning" "does not report" "$mu_out"
assert_contains "a file just written: found once loaded" "nothing to do" "$mu_out"
assert_eq "a file just written: asks until it shows" "4" "$(cat "$FAKE_CH_STATE/polls")"
unset FAKE_CH_SLOW_LOAD

# A client aimed at another server says nothing about a file on this host.
mu_reset; mu_overlay "oldpass"
export FAKE_CH_IGNORE_OVERLAY=1
MU_CLIENT="$mu_tmp/clickhouse-client --host otherhost"
mu_run s
assert_not_contains "another server: no warning about this host's file" "does not report" "$mu_out"
assert_eq "another server: the file stays" "yes" "$(mu_exists "$mu_file")"
assert_contains "the state query asks for the directory's type" "system.user_directories" "$(cat "$FAKE_CH_STATE/argv")"
unset MU_CLIENT FAKE_CH_IGNORE_OVERLAY

# Every file ClickHouse loads from users.d counts, in any spelling of the key.
# shellcheck disable=SC2034 # read by the eval'd function
mu_scan() { CLICKHOUSE_USERS_DIR="$FAKE_CH_USERS_DIR" CLICKHOUSE_USER="$FAKE_CH_USER"; monitor_user_xml_file; }
for mu_case in "ops.xml|<$FAKE_CH_USER></$FAKE_CH_USER>" "ops.conf|<$FAKE_CH_USER></$FAKE_CH_USER>" \
    "ops.yaml|  $FAKE_CH_USER:" "ops.yml|  $FAKE_CH_USER:" \
    "double.yml|  \"$FAKE_CH_USER\":" "single.yaml|  '$FAKE_CH_USER':"; do
    mu_reset
    printf '%s\n' "${mu_case#*|}" > "$FAKE_CH_USERS_DIR/${mu_case%%|*}"
    assert_eq "the scan finds a user in ${mu_case%%|*}: ${mu_case#*|}" "$FAKE_CH_USERS_DIR/${mu_case%%|*}" "$(mu_scan)"
done
for mu_case in users.xml users.yaml users.yml; do
    mu_reset
    echo "  $FAKE_CH_USER:" > "$mu_tmp/$mu_case"
    assert_eq "the scan finds a user in $mu_case" "$mu_tmp/$mu_case" "$(mu_scan)"
    rm -f "${mu_tmp:?}/$mu_case"
done
mu_reset
echo "<$FAKE_CH_USER></$FAKE_CH_USER>" > "$FAKE_CH_USERS_DIR/ops.xml.bak"
assert_eq "the scan skips a file ClickHouse does not load" "" "$(mu_scan)"
mu_reset
mkdir -p "$mu_tmp/conf.d"
echo "<$FAKE_CH_USER></$FAKE_CH_USER>" > "$mu_tmp/conf.d/ops.xml"
assert_eq "the scan finds a user in conf.d" "$mu_tmp/conf.d/ops.xml" "$(mu_scan)"
mu_reset
echo "<$FAKE_CH_USER></$FAKE_CH_USER>" > "$FAKE_CH_USERS_DIR/OPS.XML"
assert_eq "the scan finds a user in an upper-case extension" "$FAKE_CH_USERS_DIR/OPS.XML" "$(mu_scan)"
mu_reset
mu_overlay "x"
assert_eq "the scan skips the guide's own file" "" "$(mu_scan)"

# A client that cannot connect says nothing about the password.
mu_reset; mu_sql_user "stored"
printf 'stored' > "$mu_tmp/.secret"
export FAKE_CH_DOWN=1
mu_run s
assert_eq "server down: setup succeeds" "0" "$mu_rc"
assert_contains "server down: says it could not connect" "could not connect" "$mu_out"
assert_not_contains "server down: blames no password" "did NOT authenticate" "$mu_out"
assert_not_contains "server down: no second login" "--config-file" "$(cat "$FAKE_CH_STATE/argv")"
unset FAKE_CH_DOWN

# The client may not read system.users, so the mismatch only shows after the SQL.
mu_reset; mu_sql_user "oldpass"
export FAKE_CH_NO_SHOW_USERS=1
mu_run r n
assert_eq "unknown state: setup succeeds" "0" "$mu_rc"
assert_not_contains "unknown state: no false promise" "The guide sets the same password" "$mu_out"
assert_contains "unknown state: says it could not tell" "cannot read system.users" "$mu_out"
assert_contains "unknown state: the check after the SQL finds it" "the user already existed" "$mu_out"
assert_eq "unknown state: the new password connects" "yes" "$(mu_auth "$mu_final")"

mu_reset; mu_sql_user "oldpass"
mu_run r e oldpass
assert_eq "unknown state: entering the password succeeds" "0" "$mu_rc"
assert_eq "unknown state: step 3 gets the entered password" "oldpass" "$mu_final"
assert_eq "unknown state: entering runs no ALTER" "" "$(cat "$FAKE_CH_STATE/log" 2>/dev/null || true)"

mu_reset
mu_run r
assert_eq "unknown state, no user: setup succeeds" "0" "$mu_rc"
assert_not_contains "unknown state, no user: the check stays quiet" "the user already existed" "$mu_out"
assert_eq "unknown state, no user: the password connects" "yes" "$(mu_auth "$mu_final")"

# [x] in the unknown state may replace a user nobody can see, so it asks first.
mu_reset; mu_sql_user "oldpass"
mu_run x n s
assert_contains "unknown state, [x]: says what it replaces" "[x] replaces that user's password" "$mu_out"
assert_eq "unknown state, [x] declined: no overlay appears" "no" "$(mu_exists "$mu_file")"
assert_eq "unknown state, [x] declined: the password stands" "yes" "$(mu_auth "oldpass")"
assert_eq "unknown state, [x] declined: nothing is saved" "no" "$(mu_exists "$mu_tmp/.secret")"

mu_reset; mu_sql_user "oldpass"
mu_run x y
assert_eq "unknown state, [x] accepted: writes the overlay" "yes" "$(mu_exists "$mu_file")"
assert_eq "unknown state, [x] accepted: the new password connects" "yes" "$(mu_auth "$mu_final")"
assert_eq "unknown state, [x] accepted: the password is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"

# The guide's own file, and a client that cannot read system.users.
mu_reset; mu_overlay "oldpass"
mu_run n x
assert_eq "unknown state, own file: setup succeeds" "0" "$mu_rc"
assert_not_contains "unknown state, own file: no warning" "does not report" "$mu_out"
assert_eq "unknown state, own file: asks once, with nothing to wait for" "1" "$(grep -c 'system.users' "$FAKE_CH_STATE/argv")"
assert_not_contains "unknown state, own file: SQL cannot change it" "[r] Run SQL" "$mu_out"

# After the SQL shows a mismatch, the choice works the same way.
mu_reset; mu_sql_user "oldpass"
mu_run r n
assert_eq "unknown state, [n] after the SQL: the password is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"
mu_reset; mu_sql_user "oldpass"
mu_run r e oldpass
assert_contains "unknown state, [e] after the SQL: the SQL holds the entered password" \
    "$(mu_hash "oldpass")" "$(cat "$(cat "$mu_tmp/setup_sql")")"
mu_reset; mu_sql_user "oldpass"
mu_run r k
assert_eq "unknown state, [k] after the SQL: noted for the ending" "true" "$(cat "$mu_tmp/password_kept")"
assert_eq "unknown state, [k] after the SQL: no SQL for the ending" "" "$(cat "$mu_tmp/setup_sql")"

# A stale .secret is evidence of a user when ClickHouse cannot say.
mu_reset; mu_sql_user "current"
printf 'stale' > "$mu_tmp/.secret"
mu_run k s
assert_contains "unknown state, stale secret: asks about the password" "did NOT authenticate" "$mu_out"
assert_not_contains "unknown state, stale secret: keeping withdraws the overlay" "[x] Write XML config" "$mu_out"
unset FAKE_CH_NO_SHOW_USERS

# A user defined by the installer's own overlay: SQL cannot touch it.
mu_reset; mu_overlay "oldpass"
mu_run n x
assert_eq "overlay: a new password succeeds" "0" "$mu_rc"
# read shows no prompt without a terminal, so the menu's lines are the evidence.
assert_contains "overlay: the menu offers the overlay" "[x] Write XML config" "$mu_out"
assert_not_contains "overlay: the menu drops SQL, which cannot change it" "[r] Run SQL" "$mu_out"
assert_not_contains "overlay: no SQL to review" "Manual SQL" "$mu_out"
assert_contains "overlay: the install waits for it to load" "Waiting for ClickHouse to load it... ok" "$mu_out"
assert_contains "overlay: says [x] sets the new password" "[x] below writes the new password" "$mu_out"
assert_not_contains "overlay: no ALTER on offer" "[r] below runs" "$mu_out"
assert_contains "overlay: [x] says it replaces the whole file" "This replaces the whole of" "$mu_out"
assert_eq "overlay: the new password connects" "yes" "$(mu_auth "$mu_final")"
assert_eq "overlay: the old password stops" "no" "$(mu_auth "oldpass")"
assert_eq "overlay: the new password is saved at once" "$mu_final" "$(cat "$mu_tmp/.secret")"
assert_eq "overlay: the ending offers no SQL, which cannot change it" "" "$(cat "$mu_tmp/setup_sql")"

mu_reset; mu_overlay "oldpass"
mu_run n r x
assert_contains "overlay: [r] is refused" "[r] is not available" "$mu_out"
assert_not_contains "overlay: [r] runs no SQL" "--multiquery" "$(cat "$FAKE_CH_STATE/argv")"

mu_reset; mu_overlay "oldpass"
mu_overlay_before=$(cat "$mu_file")
mu_run k
assert_eq "overlay: keeping needs no menu" "0" "$mu_rc"
assert_not_contains "overlay: keeping shows no menu" "[s] Skip" "$mu_out"
assert_eq "overlay: keeping leaves the file" "$mu_overlay_before" "$(cat "$mu_file")"

mu_reset; mu_overlay "oldpass"
mu_run e oldpass
assert_eq "overlay: entering the password succeeds" "0" "$mu_rc"
assert_eq "overlay: step 3 gets the entered password" "oldpass" "$mu_final"
assert_contains "overlay: nothing further to do" "nothing to do" "$mu_out"

# [x] after [e] writes the entered password, not the generated one.
mu_reset; mu_overlay_no_grants "oldpass"
mu_run e oldpass x
assert_eq "overlay without grants: setup succeeds" "0" "$mu_rc"
assert_contains "overlay without grants: says [x] fixes it" "[x] rewrites" "$mu_out"
assert_contains "overlay without grants: [x] adds the grants" "GRANT SELECT" "$(cat "$mu_file")"
assert_eq "overlay without grants: the entered password still connects" "yes" "$(mu_auth "oldpass")"
assert_eq "overlay without grants: step 3 gets the entered password" "oldpass" "$mu_final"

# ClickHouse takes a few polls to load the file.
mu_reset
export FAKE_CH_LOGIN_FAILS=5
mu_run x
assert_contains "[x] waits for the file to load" "Waiting for ClickHouse to load it... ok" "$mu_out"
unset FAKE_CH_LOGIN_FAILS

# No users.d to write to: the operator copies the file, and the password is saved.
mu_reset
rmdir "$FAKE_CH_USERS_DIR"
mu_run x x
assert_contains "no users.d: says to copy the file" "Copy this file manually" "$mu_out"
assert_eq "no users.d: the password is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"

# A blank answer to [e] is no password.
mu_reset; mu_sql_user ""
rm -f "$mu_tmp/.secret"
mu_run e "" k
assert_contains "[e] with a blank password is refused" "did not authenticate" "$mu_out"

# A wrapper that drops the password is warned about.
mu_reset
MU_CLIENT="docker exec c clickhouse-client"
mu_run s
assert_contains "the guide warns about a wrapper" "WARNING: -C uses docker" "$mu_out"
unset MU_CLIENT

# A file that does not load after [x]: reported, and the operator decides.
mu_reset
export FAKE_CH_IGNORE_OVERLAY=1
mu_run x y
assert_contains "[x] not loading: reports it" "Waiting for ClickHouse to load it... FAILED" "$mu_out"
assert_contains "[x] not loading: says what it risks" "does not start with a users.d file it cannot load" "$mu_out"
assert_eq "[x] not loading, delete: the file is gone" "no" "$(mu_exists "$mu_file")"
assert_eq "[x] not loading, delete: the next run reuses the password" "$mu_final" "$(cat "$mu_tmp/.secret")"

mu_reset
mu_run x n
assert_eq "[x] not loading, keep: the file stays" "yes" "$(mu_exists "$mu_file")"
assert_eq "[x] not loading, keep: the password is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"

# Stopping during the wait keeps the password the file already holds.
mu_reset
MU_STOP_IN_WAIT=1
mu_run x
assert_eq "[x] stopped in the wait: stops" "130" "$mu_rc"
assert_contains "[x] stopped in the wait: the saved password is the file's" "$(mu_hash "$(cat "$mu_tmp/.secret")")" "$(cat "$mu_file")"
unset MU_STOP_IN_WAIT FAKE_CH_IGNORE_OVERLAY

# A password in root's client config beats CLICKHOUSE_PASSWORD.
mu_reset; mu_sql_user "stored"; mu_client_config
printf 'stored' > "$mu_tmp/.secret"
mu_run
assert_contains "client config: a correct saved password works" "nothing to do" "$mu_out"
assert_contains "client config: the login after the refusal uses an empty config" "--config-file" "$(cat "$FAKE_CH_STATE/argv")"

# The form of the password does not matter: the guide does not read the config.
for mu_shape in '<password/>' '<password></password>' '<password replace="1">adminpw</password>' '<password >adminpw</password>'; do
    mu_reset; mu_sql_user "stored"
    printf 'stored' > "$mu_tmp/.secret"
    echo "<config>$mu_shape</config>" > "$mu_tmp/home/.clickhouse-client/config.xml"
    mu_run
    assert_contains "client config $mu_shape: a correct saved password works" "nothing to do" "$mu_out"
done
for mu_flag in "-c $mu_tmp/named.xml" "-C $mu_tmp/named.xml" "--config $mu_tmp/named.xml" "--config-file=$mu_tmp/named.xml" "-c$mu_tmp/named.xml"; do
    mu_reset; mu_sql_user "stored"
    printf 'stored' > "$mu_tmp/.secret"
    echo '<config><password>adminpw</password></config>' > "$mu_tmp/named.xml"
    MU_CLIENT="$mu_tmp/clickhouse-client $mu_flag"
    mu_run
    assert_contains "client config in -C, $mu_flag: a correct saved password works" "nothing to do" "$mu_out"
done
unset MU_CLIENT

# A named connection can point anywhere, so a refused login stays refused.
mu_reset; mu_sql_user "stored"
printf 'stored' > "$mu_tmp/.secret"
MU_CLIENT="$mu_tmp/clickhouse-client --connection prod"
mu_run k s
assert_contains "--connection: a refused login gets no second try" "did NOT authenticate" "$mu_out"
assert_not_contains "--connection: the config stays" "--config-file" "$(cat "$FAKE_CH_STATE/argv")"
unset MU_CLIENT

# The empty config does not turn a wrong password into a right one.
mu_reset; mu_sql_user "stored"; mu_client_config
printf 'stale' > "$mu_tmp/.secret"
mu_run k s
assert_contains "client config: a wrong password stays wrong" "did NOT authenticate" "$mu_out"
assert_eq "client config: keeping is noted for the ending" "true" "$(cat "$mu_tmp/password_kept")"

# A wrapper's client reads its own config, which this host cannot change.
mu_reset; mu_sql_user "stored"; mu_client_config
printf 'stored' > "$mu_tmp/.secret"
MU_CLIENT="env $mu_tmp/clickhouse-client"
mu_run k s
assert_not_contains "wrapped client: no second login" "--config-file" "$(cat "$FAKE_CH_STATE/argv")"
unset MU_CLIENT

# A password in the client's config beats the one typed at the retry. The guide
# says so and keeps the config, which can carry TLS.
mu_reset
echo '<config><password>stale</password></config>' > "$mu_tmp/home/.clickhouse-client/config.xml"
export FAKE_CH_ADMIN_NEEDS_PASSWORD="adminpw"
mu_run r "" "" adminpw "" "" adminpw x
assert_contains "client config, retry: says the config's password wins" "beats the one typed here" "$mu_out"
assert_contains "client config, retry: gives the manual command" "Run the SQL manually" "$mu_out"
assert_not_contains "client config, retry: keeps the config" "--config-file" "$(grep -e '-u default' "$FAKE_CH_STATE/argv")"
unset FAKE_CH_ADMIN_NEEDS_PASSWORD

mu_reset; mu_sql_user "oldpass"; mu_client_config
mu_run e oldpass
assert_eq "client config: [e] accepts the right password" "oldpass" "$mu_final"

mu_reset; mu_client_config
mu_run r
assert_contains "client config: [r] uses the config's admin" "Done." "$mu_out"
assert_not_contains "client config: a new user is no mismatch" "the user already existed" "$mu_out"
assert_eq "client config: the new user connects" "yes" "$(mu_auth "$mu_final")"

# A wrapper without stdin runs nothing and exits 0. That is no success.
mu_reset
export FAKE_CH_NO_STDIN=1
mu_run r ""
assert_contains "no stdin: [r] says the SQL did not run" "did not reach the client" "$mu_out"
assert_not_contains "no stdin: [r] claims nothing" "Done." "$mu_out"
assert_not_contains "no stdin: [r] asks for no admin details" "Enter connection" "$mu_out"
assert_contains "no stdin: [r] gives the manual command" "Run the SQL manually" "$mu_out"
mu_reset; mu_sql_user "stored"
printf 'stored' > "$mu_tmp/.secret"
rm -f "$FAKE_CH_STATE/$FAKE_CH_USER.grants"
mu_run s
assert_contains "no stdin: the access check says it could not run" "could not" "$mu_out"
assert_not_contains "no stdin: the access check claims nothing" "nothing to do" "$mu_out"
unset FAKE_CH_NO_STDIN

# ClickHouse creates the span log at its first span, which is not a missing grant.
mu_reset; mu_sql_user "oldpass"
export FAKE_CH_NO_SPAN_TABLE=1
mu_run e oldpass s
assert_eq "no span table yet: setup succeeds" "0" "$mu_rc"
assert_contains "no span table yet: says why it cannot check" "when it records its first span" "$mu_out"
assert_contains "no span table yet: names it" "access to system.opentelemetry_span_log yet" "$mu_out"
assert_not_contains "no span table yet: blames no grant" "MISSING SELECT" "$mu_out"
unset FAKE_CH_NO_SPAN_TABLE

mu_reset; mu_sql_user "oldpass"
export FAKE_CH_NO_QUERY_LOG=1
mu_run e oldpass s
assert_contains "no query log yet: names it" "access to system.query_log yet" "$mu_out"
assert_contains "no query log yet: says when ClickHouse creates it" "when it logs its first query" "$mu_out"
assert_not_contains "no query log yet: leaves the span log out" "opentelemetry_span_log yet" "$mu_out"
unset FAKE_CH_NO_QUERY_LOG

# A real missing grant is still reported.
mu_reset; mu_sql_user "oldpass"
rm -f "$FAKE_CH_STATE/$FAKE_CH_USER.grants"
mu_run e oldpass r
assert_eq "missing grant: setup succeeds" "0" "$mu_rc"
assert_contains "missing grant: says so" "MISSING SELECT" "$mu_out"
assert_eq "missing grant: [r] adds it" "yes" "$(mu_exists "$FAKE_CH_STATE/$FAKE_CH_USER.grants")"

# A stored password still works: the normal re-run.
mu_reset; mu_sql_user "stored"
printf 'stored' > "$mu_tmp/.secret"
mu_run
assert_eq "re-run: succeeds with no questions" "0" "$mu_rc"
assert_contains "re-run: nothing to do" "nothing to do" "$mu_out"
assert_eq "re-run: keeps the stored password" "stored" "$mu_final"

# -C carries the admin's auth: probing as the monitoring user must replace it.
mu_reset; mu_sql_user "stored"
printf 'stored' > "$mu_tmp/.secret"
MU_CLIENT="$mu_tmp/clickhouse-client -u admin --password adminpw"
mu_run
assert_contains "-C auth: the probe replaces the client's user" "nothing to do" "$mu_out"

# -C's password never reaches the screen.
mu_reset
mu_run s
assert_contains "-C auth: the SQL's command still shows" "Run with: clickhouse-client --multiquery <" "$mu_out"
assert_not_contains "-C auth: the password does not show" "adminpw" "$mu_out"
export FAKE_CH_ADMIN_NEEDS_PASSWORD="adminpw"
for mu_secret_client in "$mu_tmp/clickhouse-client --password -hunter2" "$mu_tmp/clickhouse-client --jwt hunter2" \
    "$mu_tmp/clickhouse-client --ssh-key-passphrase hunter2" "sshpass -p hunter2 clickhouse-client" \
    "CLICKHOUSE_PASSWORD=hunter2 clickhouse-client" "$mu_tmp/clickhouse-client --password 'with hunter2'"; do
    for mu_answers in "s" "r"; do
        mu_reset
        MU_CLIENT="$mu_secret_client"
        mu_run $mu_answers "" "" "" "" "" "" ""
        assert_not_contains "-C never prints: $mu_secret_client [$mu_answers]" "hunter2" "$mu_out"
    done
done
unset MU_CLIENT FAKE_CH_ADMIN_NEEDS_PASSWORD

# A stale .secret: the password fails, the operator picks what to do.
mu_reset; mu_sql_user "current"
printf 'stale' > "$mu_tmp/.secret"
mu_run e current
assert_contains "stale secret: names the stored password" "stored in /etc/click-dog/.secret" "$mu_out"
assert_eq "stale secret: step 3 gets the entered password" "current" "$mu_final"

# .secret kept, user dropped by hand: ClickHouse's answer beats the file.
mu_reset
printf 'stored-after-drop' > "$mu_tmp/.secret"
mu_run r
assert_not_contains "dropped user: nothing asks about an existing one" "[e] Enter" "$mu_out"
assert_eq "dropped user: recreated with the stored password" "yes" "$(mu_auth "stored-after-drop")"

# The bare client needs admin details, which the retry collects.
mu_reset
export FAKE_CH_ADMIN_NEEDS_PASSWORD="adminpw"
mu_run r "" "" adminpw
assert_eq "admin retry: succeeds" "0" "$mu_rc"
assert_contains "admin retry: shows why the bare client failed" "Authentication failed" "$mu_out"
assert_contains "admin retry: runs the SQL" "Done." "$mu_out"
assert_eq "admin retry: the password connects" "yes" "$(mu_auth "$mu_final")"
assert_not_contains "admin retry: the typed password does not show" "adminpw" "$mu_out"

# A blank admin password is a password: the exported monitoring one must not stand in.
unset FAKE_CH_ADMIN_NEEDS_PASSWORD
mu_reset
export FAKE_CH_ADMIN_BLANK_ONLY=1 MU_EXPORT_PASSWORD=1
mu_run r "" "" "" x
assert_contains "admin retry, blank password: runs the SQL" "Done." "$mu_out"
assert_not_contains "admin retry, blank password: first try works" "That did not work" "$mu_out"
unset FAKE_CH_ADMIN_BLANK_ONLY MU_EXPORT_PASSWORD
export FAKE_CH_ADMIN_NEEDS_PASSWORD="adminpw"

# Both retries fail: each shows the server's error, then the manual SQL.
mu_reset
mu_run r "" "" wrong "" "" wrong x
assert_contains "admin retry failing: says it did not work" "That did not work:" "$mu_out"
assert_not_contains "admin retry: the password stays off the command line" "--password" "$(grep -e '-u default' "$FAKE_CH_STATE/argv")"
assert_eq "admin retry failing: each attempt shows the server's error" "3" \
    "$(grep -c 'Authentication failed' <<<"$mu_out")"
assert_contains "admin retry failing: gives the manual command" "Run with: CLICKHOUSE_PASSWORD=..." "$mu_out"
assert_eq "admin retry failing: finishes" "0" "$mu_rc"
assert_eq "admin retry failing: the password the SQL sets is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"

# The retry keeps -C's connection flags, drops its user, and the check uses them.
mu_reset
MU_CLIENT="$mu_tmp/clickhouse-client --secure -u admin --password stale"
mu_run r dbhost:9001 "" adminpw
assert_eq "admin retry with -C flags: succeeds" "0" "$mu_rc"
assert_contains "admin retry with -C flags: keeps them" "--secure --host dbhost --port 9001 -u default" "$(cat "$FAKE_CH_STATE/argv")"
assert_contains "admin retry with -C flags: the check after it uses them" \
    "--secure --host dbhost --port 9001 -u click_dog_monitor" "$(cat "$FAKE_CH_STATE/argv")"

# clickhouse-client uses the first --host, so -C's must go.
mu_reset
MU_CLIENT="$mu_tmp/clickhouse-client --host oldhost --port 1 -u admin --password stale"
mu_run r newhost:9001 "" adminpw
assert_eq "admin retry over -C's host: succeeds" "0" "$mu_rc"
assert_contains "admin retry over -C's host: uses the typed one" "--host newhost --port 9001 -u default" "$(cat "$FAKE_CH_STATE/argv")"
assert_not_contains "admin retry over -C's host: drops -C's" "--host oldhost" "$(grep -e '-u default' "$FAKE_CH_STATE/argv")"
mu_reset
mu_run r newhost "" adminpw
assert_contains "admin retry, host only: keeps -C's port" "--port 1 --host newhost -u default" "$(cat "$FAKE_CH_STATE/argv")"
unset MU_CLIENT FAKE_CH_ADMIN_NEEDS_PASSWORD

# No client at all, even after asking: the printed SQL still sets this password.
mu_reset
MU_CLIENT="/nonexistent/clickhouse-client"
mu_run r "/also/nonexistent" x
assert_contains "no client: says so" "Still not found" "$mu_out"
assert_eq "no client: the password the SQL sets is saved" "$mu_final" "$(cat "$mu_tmp/.secret")"
unset MU_CLIENT

# A client typed at the prompt is the one the check uses.
mu_reset
MU_CLIENT="/nonexistent/clickhouse-client"
mu_run r "$mu_tmp/clickhouse-client"
assert_eq "typed client: setup succeeds" "0" "$mu_rc"
assert_contains "typed client: runs the SQL" "Done." "$mu_out"
assert_not_contains "typed client: the check reaches the user" "the user already existed" "$mu_out"
assert_eq "typed client: the password connects" "yes" "$(mu_auth "$mu_final")"
unset MU_CLIENT

# ALTER fails after the check: the server's error and the statement both show.
mu_reset; mu_sql_user "oldpass"
export FAKE_CH_NO_SHOW_USERS=1 FAKE_CH_ALTER_DENIED=1
mu_run r n
assert_contains "ALTER denied: says so" "Could not set it:" "$mu_out"
assert_contains "ALTER denied: shows the server's error" "Not enough privileges" "$mu_out"
assert_contains "ALTER denied: gives the statement" "Run this as a ClickHouse administrator" "$mu_out"
unset FAKE_CH_NO_SHOW_USERS FAKE_CH_ALTER_DENIED

# An overlay that never authenticates is reported, not assumed.
mu_reset
MU_INSTALL_CMD="true"
mu_run x
assert_contains "an overlay that does not load is reported" "Waiting for ClickHouse to load it... FAILED" "$mu_out"
unset MU_INSTALL_CMD

unset FAKE_CH_STATE FAKE_CH_USERS_DIR FAKE_CH_USER

echo ""
echo "════════════════════════════════════════════"
echo "  Results: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════"

SUITE_FINISHED="true"
[[ $FAIL -eq 0 ]] || exit 1
