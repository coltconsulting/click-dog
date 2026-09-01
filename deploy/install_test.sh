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
cleanup_temp_dirs() {
    local dir
    for dir in "${TEST_TEMP_DIRS[@]+"${TEST_TEMP_DIRS[@]}"}"; do
        rm -rf "$dir"
    done
}
trap cleanup_temp_dirs EXIT

make_temp_dir() {
    mktemp -d "${TMPDIR:-/tmp}/click-dog-test.XXXXXX"
}

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
assert_contains "default render enables ClickHouse TLS" "-ch-secure" "$default_secure"
assert_not_contains "default render delegates port to init's TLS default" "-ch-port" "$default_secure"

CLICKHOUSE_TLS="false"
KEEPER_HOSTS=""
plain=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "init subcommand"          "init"                                   "$plain"
assert_contains "collector flag passed"    "-collector localhost:4317"              "$plain"
assert_contains "service flag is canonical click-dog-monitor" "-service click-dog-monitor" "$plain"
assert_contains "ch-user threads CLICKHOUSE_USER through" "-ch-user monitoring"     "$plain"
assert_contains "output path"              "-o /etc/click-dog/click-dog.yaml"       "$plain"
assert_contains "force overwrite"          "--force"                                "$plain"
assert_not_contains "no ch-secure when TLS off" "-ch-secure"                        "$plain"
assert_not_contains "no ha-keeper when keeper empty" "-ha-keeper"                   "$plain"

CLICKHOUSE_TLS="true"
KEEPER_HOSTS="keeper1:9181,keeper2:9181"
full=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "ch-secure added when CLICKHOUSE_TLS=true" "-ch-secure"             "$full"
assert_contains "ha-keeper threads KEEPER_HOSTS"          "-ha-keeper keeper1:9181,keeper2:9181" "$full"
assert_not_contains "no ch-port when CLICKHOUSE_PORT unset" "-ch-port"              "$full"

# Quickstart probe pins CLICKHOUSE_PORT to whichever port it actually
# checked (see "Detect ClickHouse TLS" in install.sh). When set, that
# value must flow through to `-ch-port` so the rendered config matches
# the probed endpoint — preventing the "TLS on 9000 → config for 9440"
# divergence flagged in PR #164 review.
saved_PORT="${CLICKHOUSE_PORT:-}"
CLICKHOUSE_PORT="9000"
pinned=$(render_click_dog_config /etc/click-dog/click-dog.yaml)
assert_contains "ch-port pinned when CLICKHOUSE_PORT=9000" "-ch-port 9000"          "$pinned"
CLICKHOUSE_PORT="$saved_PORT"

BINARY="$saved_BINARY"
CLICKHOUSE_TLS="$saved_TLS"
KEEPER_HOSTS="$saved_KEEPER"
COLLECTOR_ADDRESS="$saved_COLLECTOR"
CLICKHOUSE_USER="$saved_USER"

echo ""
echo "=== uninstall without systemctl (issue #383) ==="

# Run the real function under a private PATH containing an rm recorder but no
# systemctl. The child shell keeps errexit enabled, so an unguarded
# `systemctl daemon-reload` exits before the later removal calls are logged.
uninstall_sandbox=$(make_temp_dir)
TEST_TEMP_DIRS+=("$uninstall_sandbox")
uninstall_removal_log="$uninstall_sandbox/removals.log"
cat > "$uninstall_sandbox/rm" <<'STUB'
#!/bin/sh
printf '%s\n' "$*" >> "$UNINSTALL_REMOVAL_LOG"
STUB
chmod +x "$uninstall_sandbox/rm"

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

# Deliberately exclude systemctl and every other external command. The rm
# recorder proves execution continues beyond daemon-reload without touching
# the host filesystem; userdel/groupdel are already best-effort operations.
PATH="$UNINSTALL_BIN"
do_uninstall
HARNESS
chmod +x "$uninstall_sandbox/run-uninstall.sh"

uninstall_status=0
uninstall_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" \
    UNINSTALL_BIN="$uninstall_sandbox" \
    UNINSTALL_REMOVAL_LOG="$uninstall_removal_log" \
    bash "$uninstall_sandbox/run-uninstall.sh" <<<"y" 2>&1) || uninstall_status="$?"
assert_eq "uninstall exits 0 without systemctl" "0" "$uninstall_status"
assert_contains "uninstall reaches completion without systemctl" "Uninstall complete." "$uninstall_out"
removal_calls="$(<"$uninstall_removal_log")"
assert_contains "uninstall reaches binary removal" "/usr/local/bin/click-dog" "$removal_calls"
assert_contains "uninstall reaches config removal" "/etc/click-dog" "$removal_calls"
assert_contains "uninstall reaches log removal" "/var/log/click-dog" "$removal_calls"

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
cat > "$install_sandbox/click-dog" <<'STUB'
#!/bin/sh
case "$1" in
    -version|-validate) exit 0 ;;
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
eval "$installer_source"

CONFIG_FROM_FILE="$sandbox/input.yaml"
CLICKHOUSE_PASSWORD="testpass123"
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
systemctl() { return 0; }
sleep() { return 0; }

mkdir() {
    if [[ "$*" == *"/etc/click-dog"* || "$*" == *"/usr/local/bin"* ]]; then
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
    command mktemp "$@"
}
cp() {
    local target="${!#}"
    if [[ "$target" == /etc/* || "$target" == /usr/local/bin/* ]]; then
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
    update)  do_update ;;
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

update_status=0
update_out=$(INSTALL_SCRIPT="$SCRIPT_DIR/install.sh" INSTALL_SANDBOX="$install_sandbox" \
    HARNESS_ACTION=update \
    bash "$install_sandbox/run-install.sh" 2>&1) || update_status="$?"
assert_eq "successful do_update exits 0" "0" "$update_status"
assert_contains "successful do_update reaches completion" "Update complete." "$update_out"
assert_not_contains "successful do_update has no unbound trap variable" "unbound variable" "$update_out"

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

# Current stub: includes -ch-user in help text.
NEW_STUB=$(make_temp_dir)/click-dog-new
TEST_TEMP_DIRS+=("$(dirname "$NEW_STUB")")
cat > "$NEW_STUB" <<'STUB'
#!/bin/sh
echo "Usage: click-dog init [flags]"
echo "  -ch-user string"
echo "  -ch-secure"
echo "  -ha-keeper string"
STUB
chmod +x "$NEW_STUB"
BINARY="$NEW_STUB"
new_status="0"
require_unified_init_binary >/dev/null 2>&1 || new_status="$?"
assert_eq "current binary accepted (exit 0)" "0" "$new_status"

unset old_status new_status
BINARY="$saved_BINARY"

echo ""
echo "=== generate_systemd_unit ==="

unit=$(generate_systemd_unit)
assert_contains "has ExecStart" "ExecStart=/usr/local/bin/click-dog" "$unit"
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
echo "=== update is binary-swap only (no config merge) ==="

# Functions deleted in the merge-removal refactor must stay deleted —
# their presence would signal a partial revert.
assert_not_defined() {
    local name="$1"
    if declare -F "$name" >/dev/null 2>&1; then
        echo "  FAIL: function '$name' should not exist (merge removed)"
        FAIL=$((FAIL + 1))
    else
        echo "  PASS: $name is undefined"
        PASS=$((PASS + 1))
    fi
}
assert_not_defined generate_config_defaults
assert_not_defined generate_kubernetes_config_defaults
assert_not_defined generate_merge_script

# --no-merge stays in the flag parser as a noop alias for backward compat;
# regression-protect against an accidental re-introduction of MERGE_CONFIG.
if [[ -z "${MERGE_CONFIG+x}" ]]; then
    echo "  PASS: MERGE_CONFIG is unset (merge removed)"
    PASS=$((PASS + 1))
else
    echo "  FAIL: MERGE_CONFIG should be unset (merge removed)"
    FAIL=$((FAIL + 1))
fi

echo ""
echo "=== quickstart update keeps update safeguards ==="

quickstart_body=$(awk '/^do_quickstart\(\)/,/^# ── Dispatch/' deploy/install.sh)
assert_contains "quickstart update stages the binary before swap" \
    "click-dog-quickstart-update" "$quickstart_body"
assert_contains "quickstart update rejects downgrades" \
    "refusing to downgrade" "$quickstart_body"
assert_contains "quickstart update validates existing config" \
    "-validate -config /etc/click-dog/click-dog.yaml" "$quickstart_body"
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
    'cp ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}' "$quickstart_body"
assert_contains "manual users.d copy keeps loadable extension" \
    'cp ${user_xml} /etc/clickhouse-server/users.d/click-dog-${CLICKHOUSE_USER}.xml' "$quickstart_body"

echo ""
echo "=== do_kubernetes (delegates to click-dog deploy kubernetes) ==="

# do_kubernetes now runs the single Go generator via the published image rather
# than rendering bash/envsubst templates (the manifest content itself is covered
# by cmd_deploy_generate_test.go). Stub `docker` to capture the argv it would
# invoke — `command -v docker` finds the function and `docker run` calls it.
k8s_docker_args=""
docker() { k8s_docker_args="$*"; }

k8s_tmp=$(make_temp_dir)
TEST_TEMP_DIRS+=("$k8s_tmp")

# do_kubernetes reads these; it comes from the eval in source_functions, so the
# linter sees the writes and none of the reads.
# shellcheck disable=SC2034
OUTPUT_DIR="$k8s_tmp"
COLLECTOR_ADDRESS="otel-collector:4317"
CLICKHOUSE_PASSWORD="testpass123"
CLICKHOUSE_USER="click_dog_monitor"
CLICKHOUSE_HOST="clickhouse.clickhouse.svc.cluster.local"
# shellcheck disable=SC2034
CLICKHOUSE_CLUSTER="main"
VERSION="26.04.1"
# shellcheck disable=SC2034
SUBCOMMAND=""

do_kubernetes > /dev/null

# Must delegate to the single generator via the published image, threading the
# ClickHouse Service host + cluster (the #199 fix) — never the old DaemonSet.
assert_contains "runs the published image"        "ghcr.io/coltconsulting/click-dog:26.04.1"        "$k8s_docker_args"
assert_contains "invokes deploy kubernetes"        "deploy kubernetes"                                "$k8s_docker_args"
assert_contains "threads -ch-host Service DNS"     "-ch-host clickhouse.clickhouse.svc.cluster.local" "$k8s_docker_args"
assert_contains "threads -cluster"                 "-cluster main"                                    "$k8s_docker_args"
assert_contains "writes into the mounted /work"    "-o /work"                                         "$k8s_docker_args"
assert_contains "passes collector"                 "-c otel-collector:4317"                           "$k8s_docker_args"
assert_contains "passes username"                  "-u click_dog_monitor"                             "$k8s_docker_args"
assert_contains "password via env, not argv"       "-e CLICKHOUSE_PASSWORD"                            "$k8s_docker_args"
assert_not_contains "password not baked into argv" "testpass123"                                      "$k8s_docker_args"

# --ch-host is required: a standalone click-dog pod can't reach ClickHouse via
# localhost. Subshell so the function's `exit 1` doesn't kill the suite.
k8s_noch_rc=0
# shellcheck disable=SC2034  # read by do_kubernetes, which arrives via eval
( CLICKHOUSE_HOST=""; do_kubernetes >/dev/null 2>&1 ) || k8s_noch_rc=$?
assert_eq "kubernetes without --ch-host fails" "1" "$k8s_noch_rc"

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
echo "=== --no-merge backward-compat notice ==="

# --no-merge is a no-op now, but scripts may still pass it; it must print a
# one-line notice that the merge was removed. Drive the real script with -h so
# the flag loop runs and usage() exits before any filesystem writes.
nm_out=$(bash "$SCRIPT_DIR/install.sh" --no-merge -h 2>&1) || true
assert_contains "--no-merge prints a no-op notice"        "no-op"                   "$nm_out"
assert_contains "--no-merge notice explains merge removal" "config merge was removed" "$nm_out"

echo ""
echo "=== verify_archive_signed_checksums ==="

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
# verify_archive_signed_checksums needs (awk plus a SHA256 tool, and
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

# Run verify_archive_signed_checksums with PATH set to *only* <tool_dir>.
# Captures combined stdout+stderr in CAPTURED and exit status in RC.
run_verify() {
    local tool_dir="$1"
    local saved_path="$PATH"
    PATH="$tool_dir"
    set +e
    CAPTURED=$(verify_archive_signed_checksums "$VR_DIR" "$VR_ARCHIVE" "$VR_NAME" 2>&1)
    RC=$?
    set -e
    PATH="$saved_path"
}

# --- success: cosign exit 0 + matching hash ---
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 0
run_verify "$SANDBOX"
assert_eq "valid signature + hash returns 0" "0" "$RC"

# --- cosign verification failure (exit 1) → fail closed, no extraction ---
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
add_fake_cosign "$SANDBOX" 1
run_verify "$SANDBOX"
assert_eq "cosign failure returns nonzero" "1" "$RC"
assert_contains "cosign failure mentions verify-blob" "cosign verify-blob failed" "$CAPTURED"
assert_contains "cosign failure refuses install" "Refusing to install" "$CAPTURED"

# --- cosign not on PATH → fail closed before any download is trusted ---
# Sandbox built with no cosign symlink/script; PATH is set to that sandbox
# only, so a real cosign installed elsewhere on the host cannot satisfy
# `command -v cosign` and accidentally bypass the missing-cosign path.
stage_valid_release
SANDBOX=$(make_tool_sandbox); TEST_TEMP_DIRS+=("$SANDBOX")
run_verify "$SANDBOX"
assert_eq "missing cosign returns nonzero" "1" "$RC"
assert_contains "missing cosign error mentions cosign" "cosign not found" "$CAPTURED"

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
assert_contains "missing archive entry error explicit" "not found in signed checksums.txt" "$CAPTURED"

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

# Pure predicate: operator-supplied auth (via -C) must be detected so the
# runner doesn't append the monitoring user on top of it.
has_auth() { if _span_log_client_has_auth "$1"; then echo yes; else echo no; fi; }
assert_eq "-u + --password detected as auth"  "yes" "$(has_auth 'clickhouse-client -u admin --password secret')"
assert_eq "--user long flag detected as auth" "yes" "$(has_auth 'clickhouse-client --user admin')"
assert_eq "--password alone detected as auth" "yes" "$(has_auth 'clickhouse-client --password secret')"
assert_eq "plain client has no auth"          "no"  "$(has_auth 'clickhouse-client')"
assert_eq "host-only flag is not auth"        "no"  "$(has_auth 'clickhouse-client --host db.internal')"

# Runner: a client that already carries auth is used verbatim — the monitoring
# user (which may not exist yet at quickstart verification) is NOT appended, and
# the operator's -u is not duplicated.
record_args() { echo "$*"; }
_sv_chc="$CLICKHOUSE_CLIENT"; _sv_u="${CLICKHOUSE_USER:-}"; _sv_p="${CLICKHOUSE_PASSWORD:-}"

CLICKHOUSE_CLIENT="record_args -u admin --password secret"; CLICKHOUSE_USER="monitoring"; CLICKHOUSE_PASSWORD="monpass"
auth_out="$(_span_log_default_runner 'SELECT 1')"
assert_contains "operator auth used verbatim" "-u admin --password secret -q SELECT 1" "$auth_out"
assert_not_contains "monitoring user not appended over operator auth" "monitoring" "$auth_out"

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
docker_safe="$(_warn_clickhouse_client_password_forwarding 'docker exec -e CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_eq "docker exec by-name env forwarding is accepted" "" "$docker_safe"
docker_long_safe="$(_warn_clickhouse_client_password_forwarding 'docker exec --env=CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_eq "docker exec long env forwarding is accepted" "" "$docker_long_safe"
docker_invalid_short="$(_warn_clickhouse_client_password_forwarding 'docker exec -e=CLICKHOUSE_PASSWORD clickhouse clickhouse-client' 2>&1)"
assert_contains "invalid docker short env spelling still warns" "without forwarding CLICKHOUSE_PASSWORD" "$docker_invalid_short"
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
assert_contains "XML grants span log"          "GRANT SELECT ON system.opentelemetry_span_log TO click_dog_monitor" "$xml_out"
assert_contains "XML grants query log"         "GRANT SELECT ON system.query_log TO click_dog_monitor"              "$xml_out"

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
assert_contains "generated password uses child environment" 'CLICKHOUSE_PASSWORD="$mon_pass" $CLICKHOUSE_CLIENT' "$quickstart_body"
assert_contains "manual fallback models argv-safe password handling" 'Run with: CLICKHOUSE_PASSWORD=... clickhouse-client' "$quickstart_body"
assert_contains "embedded -C auth suppresses forwarding warning" 'if ! _span_log_client_has_auth "$CLICKHOUSE_CLIENT"; then' "$quickstart_body"
# The inaccurate "Nothing else is changed" reassurance must be gone.
assert_not_contains "no false 'Nothing else is changed' copy" "Nothing else is changed" "$quickstart_body"
# Copy must accurately describe the non-clobbering CREATE USER IF NOT EXISTS posture.
assert_contains "copy explains IF NOT EXISTS preserves existing user" "CREATE USER IF NOT EXISTS" "$quickstart_body"
# Manual SQL bundle is always surfaced for review.
assert_contains "manual SQL bundle is printed for review" "Manual SQL (review before running anywhere):" "$quickstart_body"
# Auth mutation is gated behind an explicit confirmation prompt.
assert_contains "auth reset is behind an explicit prompt" "Reset '\${CLICKHOUSE_USER}' password to match" "$quickstart_body"
assert_contains "existing-user path verifies real table access" 'generate_user_verify_sql' "$quickstart_body"
# Declining the reset must also drop the [x] XML path — its users.d overlay
# carries password_sha256_hex and would otherwise reset the existing user's
# credentials declaratively, undoing the operator's "no" (the clobber #181
# removes). Gate it on the same reset confirmation.
assert_contains "declined reset gates the XML path" 'offer_xml="false"' "$quickstart_body"
assert_contains "XML path keyed on declined auth-mismatch" '"$auth_decision" == "prompt" && "$AUTH_RESET_CONFIRMED" != "true"' "$quickstart_body"
assert_contains "menu omits [x] when reset declined" "Write XML config is omitted here" "$quickstart_body"
assert_contains "[x] is refused when not offered" "it would reset the password you" "$quickstart_body"

echo ""
echo "════════════════════════════════════════════"
echo "  Results: $PASS passed, $FAIL failed"
echo "════════════════════════════════════════════"

[[ $FAIL -eq 0 ]] || exit 1
