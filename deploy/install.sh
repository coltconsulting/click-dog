#!/usr/bin/env bash
#
# click-dog deployment script
#
# Quick start (single node, guided):
#   ./install.sh
#
# Custom install (single Linux node — for multi-node, use deploy/ansible/playbook.yaml):
#   ./install.sh install -c collector:4317 [--systemd]
#   ./install.sh update [-v VERSION]
#   ./install.sh status
#   ./install.sh uninstall [--yes]
#   ./install.sh kubernetes -c collector:4317 --ch-host ch.svc [--cluster name] [-o DIR]
#   ./install.sh kubernetes update -v VERSION [-o DIR]
#   ./install.sh docker -c collector:4317 [-o DIR]
#   ./install.sh docker update -v VERSION [-o DIR]
#
# Options:
#   -c ADDRESS       OTEL collector address (required for install/kubernetes/docker)
#   -u USER          ClickHouse username (default: click_dog_monitor).
#                    A dedicated, namespaced name avoids colliding with an
#                    existing "monitoring" user. The guide changes an existing
#                    user only when asked.
#   -v VERSION       Version to install/generate (default: latest release)
#   -b BINARY        Path to pre-downloaded binary (install/update only)
#   -k HOSTS         Keeper hosts for HA, comma-separated (install only)
#   -x PROXY         HTTPS proxy for binary download
#   -C CMD           clickhouse-client command/path (default: clickhouse-client)
#                    e.g. -C "clickhouse-client -u admin". A docker-exec
#                    wrapper must add `-i -e CLICKHOUSE_PASSWORD` (the variable
#                    by name, with no value): -i carries the SQL on stdin, and
#                    the name carries passwords into the container without
#                    putting them in host process arguments.
#   -f PATH          Use this pre-rendered click-dog.yaml instead of having
#                    install.sh render one (install only). Pair with the
#                    output of `click-dog init --wizard` from a laptop.
#   -o DIR           Output directory (kubernetes/docker modes)
#   --ch-host HOST   ClickHouse Service DNS for kubernetes (required; e.g.
#                    clickhouse.clickhouse.svc.cluster.local — not localhost)
#   --cluster NAME   ClickHouse cluster name for kubernetes; runs the
#                    Deployment in cluster mode (one replica per shard)
#   --prerelease     When -v is unset, resolve the newest PUBLIC pre-release
#                    (alpha/beta) instead of the latest GA release. When no GA
#                    is published at all, the default channel falls back to the
#                    newest pre-release automatically.
#   --dangerously-ignore-cosign
#                    Deliberately skip cosign even when it is installed. The
#                    archive SHA-256 is still required and verified.
#   --otel-secure[=true|false]
#                    OTLP transport to the receiver at -c: =true for TLS,
#                    verified against the host's system trust store; =false
#                    for plaintext. Bare --otel-secure means =true; the value
#                    takes '=' (never a separate word). Unset, the choice is
#                    automatic: plaintext only for a loopback collector
#                    (localhost, 127.0.0.0/8, ::1, unix:), TLS otherwise, as
#                    the click-dog being installed decides it
#                    (`click-dog init --print-otel-secure`). Whenever the
#                    result is TLS, install, docker and the guided quick start
#                    check the handshake first with that click-dog (`click-dog
#                    test tls`; docker runs the image with --network host) and
#                    stop on a mismatch; the guided quick start asks, with the
#                    automatic choice as its default. kubernetes passes the
#                    value to the generator, which refuses a collector off
#                    loopback without one (on a terminal, install.sh asks
#                    instead), and checks nothing: -c resolves inside the
#                    cluster. Not for -f; for mTLS, set client_cert /
#                    client_key in your own YAML (install -f).
#   --otel-ca-cert FILE
#                    Verify the OTLP receiver against this PEM CA (the
#                    collector's own certificate when self-signed) instead of
#                    the system store. Implies TLS, so it cannot combine with
#                    --otel-secure=false; installed as
#                    /etc/click-dog/tls/ca.pem and checked by handshake first.
#                    install and the guided quick start only. Unattended
#                    installs never trust a certificate on first use.
#   --systemd        Create + enable systemd unit (install only)
#   --yes            Skip confirmation for uninstall automation only
#   -h               Show this help
#
# Environment:
#   CLICKHOUSE_PASSWORD   ClickHouse password for non-interactive runs. When
#                         unset, the script prompts (TTY) — preferred over any
#                         flag, which would expose the secret in process
#                         listings and shell history.
#   GITHUB_TOKEN          Optional GitHub token (or GH_TOKEN), used to
#                         authenticate the release version probe and asset
#                         download. Useful when a host shares GitHub's
#                         unauthenticated API rate limit. Use -b
#                         /path/to/click-dog to skip the download entirely.
#
set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────
COMMAND=""
SUBCOMMAND=""
# Dedicated, namespaced default so re-running the installer can't collide with
# a pre-existing operator-managed "monitoring" account. Override with -u.
CLICKHOUSE_USER="click_dog_monitor"
# Kubernetes-only: the ClickHouse Service DNS click-dog connects to, and the
# cluster name for cluster() reads. No default — a standalone click-dog pod
# can't reach ClickHouse via localhost, so `install.sh kubernetes` requires
# --ch-host (see #199). --cluster enables cross-shard reads.
CLICKHOUSE_HOST=""
# clickhouse-client reads both from its environment, and an assignment keeps a
# variable exported. These values are this script's own.
export -n CLICKHOUSE_USER CLICKHOUSE_HOST
CLICKHOUSE_CLUSTER=""
BINARY=""
COLLECTOR_ADDRESS=""
# The operator's explicit OTLP transport: "true" or "false" from
# --otel-secure[=true|false], or a guided answer that differs from the
# default. Empty means automatic, and nothing is passed to `click-dog init`,
# which then renders by the loopback rule. --otel-ca-cert does not set it: the
# CA implies TLS on its own, and keeping the two apart is what lets an
# explicit false be refused with a CA in either order.
OTEL_SECURE=""
# The transport this run's inputs give, "true" (TLS) or "false", as the
# click-dog being installed reports it (resolve_otel_transport). It gates the
# handshake check and is never passed on.
OTEL_USES_TLS=""
# For docker, the image that decides and checks the transport in place of a
# host binary (see run_click_dog).
CLICK_DOG_RUN_IMAGE=""
# The collector CA the rendered config names (exporters.otel[].ca_cert): an
# operator's --otel-ca-cert FILE, or the certificate pinned on first use in
# the guided install, copied to OTEL_CA_CERT_PATH for the runtime user. The
# Ansible playbook uses the same path. OTEL_CA_PIN_TMP is set only for a pin.
OTEL_CA_CERT=""
OTEL_CA_CERT_PATH="/etc/click-dog/tls/ca.pem"
OTEL_CA_PIN_TMP=""
CLICKHOUSE_PASSWORD="${CLICKHOUSE_PASSWORD:-}"
KEEPER_HOSTS=""
CONFIG_FROM_FILE=""
HTTPS_PROXY_FLAG=""
VERSION=""
# When set (via --prerelease) and no -v is given, resolve_version picks the
# newest explicitly public prerelease instead of the latest GA. Private alpha
# releases in click-dog-internal are intentionally not resolved here.
PRERELEASE="false"
DANGEROUSLY_IGNORE_COSIGN="false"
SYSTEMD="false"
UNINSTALL_YES="false"
OUTPUT_DIR=""
CLICKHOUSE_TLS="true"
# CLICKHOUSE_PORT is left unset by default so the rendered config gets
# `click-dog init`'s built-in port defaulting (9000 plaintext, 9440 with
# --ch-secure when no --ch-port was given). Quickstart probes localhost and
# pins this to the detected port so the rendered config matches the
# endpoint that was actually checked — see "Detect ClickHouse TLS" below.
CLICKHOUSE_PORT=""
CLICKHOUSE_CLIENT="clickhouse-client"
CLICK_DOG_BINARY="/usr/local/bin/click-dog"
INSTALL_STATE_DIR="/var/lib/click-dog-installer"
INSTALLER_CREATED_USER_MARKER="${INSTALL_STATE_DIR}/installer-created-user"
INSTALLER_CREATED_GROUP_MARKER="${INSTALL_STATE_DIR}/installer-created-group"

usage() {
    # When piped via curl|bash, $0 is /dev/stdin — sed can't read the header.
    if [[ -f "$0" ]]; then
        sed -n '3,/^set /{
            /^set /d
            s/^# //
            t print
            s/^#$//
            t print
            b
            :print
            p
        }' "$0"
    else
        echo "Usage: install.sh {install|update|status|uninstall|kubernetes|docker} [options]"
        echo "Run './install.sh -h' from a local copy for full help."
    fi
    exit 1
}

# ── Parse command ───────────────────────────────────────────────
if [[ $# -gt 0 && ! "$1" =~ ^- ]]; then
    COMMAND="$1"
    shift
else
    # No command word → guided quickstart (flags pre-fill wizard prompts)
    COMMAND="quickstart"
fi

# Handle sub-commands for kubernetes/docker (e.g., "kubernetes update")
if [[ "$COMMAND" == "kubernetes" || "$COMMAND" == "docker" ]]; then
    if [[ $# -gt 0 && "$1" == "update" ]]; then
        SUBCOMMAND="update"
        shift
    fi
fi

case "$COMMAND" in
    quickstart|install|update|status|uninstall|kubernetes|docker) ;;
    *) echo "Error: unknown command '$COMMAND'"; usage ;;
esac

# Pre-process long options (getopts doesn't handle them)
ARGS=()
while [[ $# -gt 0 ]]; do
    case "$1" in
        --systemd)  SYSTEMD="true" ;;
        --yes)
            if [[ "$COMMAND" != "uninstall" ]]; then
                echo "Error: --yes is only valid with uninstall" >&2
                exit 1
            fi
            UNINSTALL_YES="true"
            ;;
        --ch-host)  CLICKHOUSE_HOST="${2:-}"; shift ;;
        --cluster)  CLICKHOUSE_CLUSTER="${2:-}"; shift ;;
        --prerelease) PRERELEASE="true" ;;
        --dangerously-ignore-cosign) DANGEROUSLY_IGNORE_COSIGN="true" ;;
        --otel-secure|--otel-secure=*)
            # Only the paths that render a config can carry it: `click-dog
            # init` for quickstart/install/docker, `click-dog deploy
            # kubernetes` for kubernetes. Elsewhere it would be silently
            # dropped.
            case "${COMMAND}${SUBCOMMAND:+ $SUBCOMMAND}" in
                quickstart|install|docker|kubernetes) ;;
                *)
                    echo "Error: --otel-secure is only valid with install, docker, kubernetes, or the guided quick start" >&2
                    exit 1
                    ;;
            esac
            case "$1" in
                --otel-secure)
                    # Go's flag package reads `--otel-secure false` as true
                    # plus a stray word; refuse the spelling here too.
                    case "${2:-}" in
                        [tT][rR][uU][eE]|[fF][aA][lL][sS][eE])
                            echo "Error: --otel-secure takes its value with '=': write --otel-secure=$(printf '%s' "$2" | tr '[:upper:]' '[:lower:]')" >&2
                            echo "       (--otel-secure followed by a separate word is not accepted)" >&2
                            exit 1
                            ;;
                    esac
                    OTEL_SECURE="true"
                    ;;
                --otel-secure=true)  OTEL_SECURE="true" ;;
                --otel-secure=false) OTEL_SECURE="false" ;;
                *)
                    echo "Error: ${1}: use --otel-secure=true or --otel-secure=false" >&2
                    exit 1
                    ;;
            esac
            ;;
        --otel-ca-cert)
            case "${COMMAND}${SUBCOMMAND:+ $SUBCOMMAND}" in
                quickstart|install) ;;
                docker)
                    echo "Error: --otel-ca-cert is not supported for docker: the generated compose file" >&2
                    echo "       mounts only click-dog.yaml, so the CA would not be visible in the" >&2
                    echo "       container. Mount it and set exporters.otel[].ca_cert." >&2
                    exit 1
                    ;;
                kubernetes)
                    echo "Error: --otel-ca-cert is not supported for kubernetes. Put the CA in the" >&2
                    echo "       click-dog-otel-ca ConfigMap the generated Deployment mounts, then set" >&2
                    echo "       ca_cert: /etc/click-dog/tls/ca.pem in configmap.yaml." >&2
                    exit 1
                    ;;
                *)
                    echo "Error: --otel-ca-cert is only valid with install or the guided quick start" >&2
                    exit 1
                    ;;
            esac
            if [[ -z "${2:-}" || "${2:-}" == -* ]]; then
                echo "Error: --otel-ca-cert needs a FILE" >&2
                exit 1
            fi
            OTEL_CA_CERT="$2"
            shift
            ;;
        *)          ARGS+=("$1") ;;
    esac
    shift
done
set -- "${ARGS[@]+"${ARGS[@]}"}"

while getopts "c:u:k:b:x:v:o:C:f:h" opt; do
    case $opt in
        c) COLLECTOR_ADDRESS="$OPTARG" ;;
        C) CLICKHOUSE_CLIENT="$OPTARG" ;;
        u) CLICKHOUSE_USER="$OPTARG"
           if [[ ! "$CLICKHOUSE_USER" =~ ^[a-zA-Z_][a-zA-Z0-9_]*$ ]]; then
               echo "Error: username must be alphanumeric/underscore (got '$CLICKHOUSE_USER')" >&2
               exit 1
           fi
           ;;
        k) KEEPER_HOSTS="$OPTARG" ;;
        b) BINARY="$OPTARG" ;;
        x) HTTPS_PROXY_FLAG="$OPTARG" ;;
        v) VERSION="${OPTARG#v}" ;;
        o) OUTPUT_DIR="$OPTARG" ;;
        f) CONFIG_FROM_FILE="$OPTARG" ;;
        h) usage ;;
        *) usage ;;
    esac
done
shift $((OPTIND - 1))
# getopts stops at the first word that is not an option, and every option
# after it would be dropped without a word: refuse the run instead.
if [[ $# -gt 0 ]]; then
    case "$1" in
        [tT][rR][uU][eE]|[fF][aA][lL][sS][eE])
            echo "Error: unexpected argument '$1'. Boolean options take their value with '=', e.g. --otel-secure=false" >&2
            ;;
        *)
            echo "Error: unexpected argument '$1'" >&2
            ;;
    esac
    exit 1
fi
# A CA implies TLS. Checked once every option is read, so the order the two
# were given in does not matter.
if [[ "$OTEL_SECURE" == "false" && -n "$OTEL_CA_CERT" ]]; then
    echo "Error: --otel-ca-cert implies TLS, so it cannot combine with --otel-secure=false" >&2
    exit 1
fi

# ── Password resolution ────────────────────────────────────────
resolve_password() {
    if [[ -n "$CLICKHOUSE_PASSWORD" ]]; then
        return
    fi
    if [[ -t 0 ]]; then
        read -rsp "ClickHouse password: " CLICKHOUSE_PASSWORD
        echo ""
        [[ -z "$CLICKHOUSE_PASSWORD" ]] && { echo "Error: password cannot be empty"; exit 1; }
    else
        echo "Error: ClickHouse password required. Set CLICKHOUSE_PASSWORD env var (no TTY to prompt)" >&2
        exit 1
    fi
}

# Record only identities this installer created. The root-owned numeric-ID
# markers let `click-dog deploy uninstall` remove those identities without
# deleting a pre-provisioned account that happens to use the same name.
record_installer_created_runtime_identity() {
    local group_existed_before="$1"
    local runtime_uid runtime_gid=""

    runtime_uid=$(id -u click-dog)
    mkdir -p "$INSTALL_STATE_DIR"
    chown root:root "$INSTALL_STATE_DIR"
    chmod 700 "$INSTALL_STATE_DIR"
    printf '%s\n' "$runtime_uid" > "$INSTALLER_CREATED_USER_MARKER"
    chown root:root "$INSTALLER_CREATED_USER_MARKER"
    chmod 600 "$INSTALLER_CREATED_USER_MARKER"

    # `useradd` may create a same-name private group. Record it only when that
    # group did not exist before, and only when its numeric ID can be resolved.
    rm -f "$INSTALLER_CREATED_GROUP_MARKER"
    if [[ "$group_existed_before" != "true" ]] && command -v getent >/dev/null 2>&1; then
        runtime_gid=$(getent group click-dog | awk -F: 'NR == 1 { print $3 }') || runtime_gid=""
        if [[ "$runtime_gid" =~ ^[0-9]+$ ]]; then
            printf '%s\n' "$runtime_gid" > "$INSTALLER_CREATED_GROUP_MARKER"
            chown root:root "$INSTALLER_CREATED_GROUP_MARKER"
            chmod 600 "$INSTALLER_CREATED_GROUP_MARKER"
        fi
    fi
}

# ── Arch detection ──────────────────────────────────────────────
detect_arch() {
    local raw_arch="$1"
    case "$raw_arch" in
        x86_64)          echo "amd64" ;;
        aarch64|arm64)   echo "arm64" ;;
        *) echo "Error: unsupported architecture $raw_arch" >&2; exit 1 ;;
    esac
}

resolve_target_arch() {
    detect_arch "$(uname -m)"
}

# ── Render the click-dog config via the binary ─────────────────
# This is the single rendering path for every install.sh sub-command that
# produces a /etc/click-dog/click-dog.yaml (or the docker-compose equivalent):
# we shell out to `click-dog init` so the wizard renderer is the only place
# YAML shape lives.
#
# `$1` is the output path. The binary must already be available at
# `$BINARY` (resolved by resolve_binary in each caller). Flags are built
# conditionally because Go's flag package rejects the empty-bool form
# `--ch-secure=`; concat-via-array lets us add or omit each flag cleanly.

# help_lists_flag FLAG CMD... — run CMD (a -h invocation) and succeed when its
# help names FLAG. Capture-then-grep (rather than `CMD | grep -q`) because the
# pipeline form races under `set -euo pipefail`: grep -q exits 0 after the
# first match, leaving the producer to SIGPIPE on subsequent writes; pipefail
# then promotes the pipeline status to nonzero, the `if !` negates true, and
# we falsely refuse a perfectly-good binary. Buffering via $() removes the
# pipe entirely. "-FLAG" is a substring match on purpose: early releases print
# `-flag` in their help, current ones `--flag`; both must pass.
help_lists_flag() {
    local flag="$1" help_out
    shift
    help_out="$("$@" 2>&1)" || true
    grep -q -- "-$flag" <<<"$help_out"
}

# Refuse a target binary that predates this install.sh's renderer
# unification. Pre-unified click-dog releases don't know `--ch-user` /
# `--ch-secure` / `--ha-keeper` and exit from Go's flag parser before
# writing a config — the worst possible failure mode (no diagnostic,
# half-finished install state). Detect by probing `init -h` for one of
# the new flags and fail-fast with a clear pointer to the version skew.
#
# Hand-installs at an older `-v` are still possible: the user just needs
# to check out a matching older install.sh from the tag they're pinning.
require_unified_init_binary() {
    if ! help_lists_flag ch-user "$BINARY" init -h; then
        cat >&2 <<EOF
Error: the selected click-dog binary predates this install.sh's
       unified config renderer (introduced in v26.06).

  Either:
    - omit -v to install the latest release, or
    - check out an install.sh that matches the target version:
        git checkout v\${VERSION} -- deploy/install.sh
EOF
        exit 1
    fi
}

# require_init_flag NAME — the same skew guard for an init flag added after
# unification: a release that lacks it would fail in Go's flag parser midway
# through the install. Run only when the flag is about to be passed.
require_init_flag() {
    if ! help_lists_flag "$1" "$BINARY" init -h; then
        cat >&2 <<EOF
Error: the selected click-dog binary predates 'click-dog init --$1'.

  Either:
    - omit -v to install the latest release, or
    - install without it and set the matching key by hand afterwards.
EOF
        exit 1
    fi
}

# require_otel_transport_image — the skew guard for the image `install.sh
# docker` and `install.sh kubernetes` run. Both ask the image for the OTLP
# transport (`init --print-otel-secure`) and pass `--otel-secure=true|false`
# to it; only images whose init lists --print-otel-secure accept those
# values, and every such image also carries the unified renderer. An older
# image would stop in Go's flag parser, or read `--otel-secure=false` the old
# way, instead of generating files. Run before anything else uses the image.
require_otel_transport_image() {
    if ! help_lists_flag print-otel-secure docker run --rm "ghcr.io/coltconsulting/click-dog:${VERSION}" init -h; then
        cat >&2 <<EOF
Error: ghcr.io/coltconsulting/click-dog:${VERSION} predates
       'click-dog init --print-otel-secure', which this install.sh uses to
       choose the OTLP transport.

  Either:
    - omit -v to use the latest release:
        https://github.com/coltconsulting/click-dog/releases/latest
    - or check out an install.sh that matches the target version:
        git checkout v${VERSION} -- deploy/install.sh
EOF
        exit 1
    fi
}

# SECRET_FILE is the path click-dog reads the ClickHouse password from. The
# config carries `clickhouse.password_file: $SECRET_FILE` (rendered via
# `init --ch-password-file`), so the secret stays in this 0600 file and never
# enters the service environment — the systemd unit needs no EnvironmentFile.
SECRET_FILE=/etc/click-dog/.secret
CLICKHOUSE_USERS_DIR=/etc/clickhouse-server/users.d

# clickhouse_server_user prints the user the clickhouse-server unit runs as, or
# nothing when systemd cannot say. A unit can name a numeric user, and su needs the
# name.
clickhouse_server_user() {
    local user
    user=$(systemctl show -p User --value clickhouse-server 2>/dev/null) || user=""
    if [[ "$user" =~ ^[0-9]+$ ]]; then
        user=$(getent passwd "$user" 2>/dev/null | cut -d: -f1) || user=""
    fi
    echo "$user"
}

# clickhouse_server_group prints the group the clickhouse-server unit runs as, or
# nothing when systemd cannot say or names a group this host does not have.
clickhouse_server_group() {
    local group unit_user
    group=$(systemctl show -p Group --value clickhouse-server 2>/dev/null) || group=""
    if [[ -z "$group" ]]; then
        unit_user=$(clickhouse_server_user)
        if [[ -n "$unit_user" ]]; then
            group=$(id -gn "$unit_user" 2>/dev/null) || group=""
        fi
    fi
    if [[ "$group" =~ ^[0-9]+$ ]]; then
        group=$(getent group "$group" 2>/dev/null | cut -d: -f1) || group=""
    fi
    if [[ -n "$group" ]] && ! getent group "$group" &>/dev/null; then
        group=""
    fi
    echo "$group"
}

# clickhouse_loaded_files DIR... prints each file ClickHouse loads from DIR. It goes
# by the extension, in any case, and skips dotfiles.
clickhouse_loaded_files() {
    local dir file
    for dir in "$@"; do
        for file in "$dir"/*; do
            case "$file" in
                *.[xX][mM][lL]|*.[cC][oO][nN][fF]|*.[yY][aA][mM][lL]|*.[yY][mM][lL])
                    if [[ -f "$file" ]]; then
                        echo "$file"
                    fi
                    ;;
            esac
        done
    done
}

# clickhouse_server_groups USER GROUP prints, comma-separated, the group IDs systemd
# starts the server with: GROUP's, those of the groups that list USER as a member,
# and the unit's SupplementaryGroups=. systemd adds the member groups with
# initgroups(USER, GROUP), which getent initgroups lists, and skips that for group 0.
# It fails when it cannot find a group. IDs keep a group name with a space whole.
clickhouse_server_groups() {
    local user="$1" gid g list members
    gid=$(getent group "$2" | cut -d: -f3) || return 1
    [[ -n "$gid" ]] || return 1
    list="$gid"
    if [[ "$gid" != "0" ]]; then
        members=$(getent initgroups "$user" | awk '{ for (i = 2; i <= NF; i++) print $i }') || return 1
        for g in $members; do
            if [[ ",${list}," != *",${g},"* ]]; then
                list+=",$g"
            fi
        done
    fi
    for g in $(systemctl show -p SupplementaryGroups --value clickhouse-server 2>/dev/null); do
        if [[ ! "$g" =~ ^[0-9]+$ ]]; then
            g=$(getent group "$g" | cut -d: -f3) || return 1
            [[ -n "$g" ]] || return 1
        fi
        if [[ ",${list}," != *",${g},"* ]]; then
            list+=",$g"
        fi
    done
    echo "$list"
}

# clickhouse_unreadable_files prints each file ClickHouse loads from config.d,
# conf.d, and users.d that its own user and groups cannot read. When it cannot test
# as them, it says so on stderr and prints nothing.
clickhouse_unreadable_files() {
    local server_user group group_list file dir="${CLICKHOUSE_USERS_DIR%/*}" as_server reason=""
    server_user=$(clickhouse_server_user)
    group=$(clickhouse_server_group)
    if [[ -z "$server_user" ]]; then
        reason="systemd names no user for clickhouse-server"
    elif [[ -z "$group" ]]; then
        reason="the guide cannot find the group clickhouse-server runs as"
    elif ! command -v setpriv &>/dev/null; then
        reason="setpriv is not installed"
    elif ! group_list=$(clickhouse_server_groups "$server_user" "$group"); then
        reason="the guide cannot find every group clickhouse-server starts with"
    else
        as_server=(setpriv --reuid="$server_user" --regid="$group" --groups="$group_list")
        if ! "${as_server[@]}" true &>/dev/null; then
            reason="setpriv cannot run as ${server_user}"
        fi
    fi
    if [[ -n "$reason" ]]; then
        echo "" >&2
        echo "  Note: the guide cannot check that ClickHouse can read its config files:" >&2
        echo "  ${reason}." >&2
        return 0
    fi
    while IFS= read -r file; do
        if ! "${as_server[@]}" test -r "$file" &>/dev/null </dev/null; then
            echo "$file"
        fi
    done < <(clickhouse_loaded_files "$dir/config.d" "$dir/conf.d" "$CLICKHOUSE_USERS_DIR")
}

# xml_without_comments FILE prints FILE with its <!-- --> comments removed.
xml_without_comments() {
    awk '{
        rest = $0
        out = ""
        while (rest != "") {
            if (in_comment) {
                i = index(rest, "-->")
                if (!i) { rest = ""; continue }
                rest = substr(rest, i + 3)
                in_comment = 0
            } else {
                i = index(rest, "<!--")
                if (!i) { out = out rest; rest = ""; continue }
                out = out substr(rest, 1, i - 1)
                rest = substr(rest, i + 4)
                in_comment = 1
            }
        }
        print out
    }' "$1"
}

# clickhouse_grantee_files prints each click-dog-*.xml file in users.d whose GRANT
# names a grantee. ClickHouse does not load such a file and does not start with one.
clickhouse_grantee_files() {
    local file text
    for file in "$CLICKHOUSE_USERS_DIR"/click-dog-*.xml; do
        [[ -f "$file" ]] || continue
        text=$(xml_without_comments "$file")
        if grep -Eqi 'GRANT[[:space:]].*[[:space:]]TO[[:space:]]' <<<"$text"; then
            echo "$file"
        fi
    done
}

# check_clickhouse_files prints the fix and returns 1 when a file stops ClickHouse
# at its next start.
check_clickhouse_files() {
    local files group
    files=$(clickhouse_grantee_files)
    if [[ -n "$files" ]]; then
        echo "" >&2
        echo "  Error: ClickHouse never loaded these files and does not start with them:" >&2
        sed 's/^/    /' <<<"$files" >&2
        echo "" >&2
        echo "  A GRANT in each one names a user, which ClickHouse rejects in users.d." >&2
        echo "  Delete each file, then run the guide again:" >&2
        echo "    rm FILE" >&2
        return 1
    fi
    files=$(clickhouse_unreadable_files)
    if [[ -n "$files" ]]; then
        group=$(clickhouse_server_group)
        echo "" >&2
        echo "  Error: ClickHouse does not start while its user cannot read these files:" >&2
        sed 's/^/    /' <<<"$files" >&2
        echo "" >&2
        echo "  Fix each file, then run the guide again:" >&2
        if [[ -n "$group" ]]; then
            echo "    chown root:${group} FILE && chmod 640 FILE" >&2
        else
            echo "    chmod 644 FILE" >&2
        fi
        return 1
    fi
}

# clickhouse_file_install_cmd prints the command that puts a file into ClickHouse's
# config.d or users.d. ClickHouse reads them as its own user, and it refuses to
# start, or to reload users, while one of them is unreadable. The guide's temp
# files are 0600, so a plain cp as root carries that over. With no group to give
# the file to, it has to be world-readable.
clickhouse_file_install_cmd() {
    local group
    group=$(clickhouse_server_group)
    if [[ -z "$group" ]] && getent group clickhouse &>/dev/null; then
        group="clickhouse"
    fi
    if [[ -n "$group" ]]; then
        echo "install -m 640 -o root -g ${group}"
    else
        echo "install -m 644 -o root -g root"
    fi
}

# install_secret_file SOURCE copies SOURCE to SECRET_FILE, for click-dog's user alone.
install_secret_file() {
    cp "$1" "$SECRET_FILE"
    chown click-dog:click-dog "$SECRET_FILE"
    chmod 600 "$SECRET_FILE"
}

# restart_click_dog returns 0 when the service is still up two seconds after a
# restart. A service that fails at startup can report active at first.
restart_click_dog() {
    systemctl restart click-dog || true
    sleep 2
    systemctl is-active click-dog >/dev/null 2>&1
}

render_click_dog_config() {
    local out_path="$1"
    local args=(
        --collector "$COLLECTOR_ADDRESS"
        --service click-dog-monitor
        --ch-user "$CLICKHOUSE_USER"
        --ch-password-file "$SECRET_FILE"
        -o "$out_path"
        --force
    )
    [[ "$CLICKHOUSE_TLS" == "true" ]]     && args+=( --ch-secure )
    [[ -n "${CLICKHOUSE_PORT:-}" ]]       && args+=( --ch-port "$CLICKHOUSE_PORT" )
    # Only an explicit choice: unset leaves init to apply the loopback rule.
    [[ -n "$OTEL_SECURE" ]]               && args+=( "--otel-secure=$OTEL_SECURE" )
    [[ -n "$OTEL_CA_CERT" ]]              && args+=( --otel-ca-cert "$OTEL_CA_CERT_PATH" )
    [[ -n "$KEEPER_HOSTS" ]]              && args+=( --ha-keeper "$KEEPER_HOSTS" )
    "$BINARY" init "${args[@]}"
}

# The Docker daemon's security options, read once per run by
# docker_security_options: the mount helper adapts to rootless, userns-remap
# and SELinux daemons.
DOCKER_SECURITY=""
DOCKER_SECURITY_PROBED=""

docker_security_options() {
    if [[ -z "$DOCKER_SECURITY_PROBED" ]]; then
        DOCKER_SECURITY=$(docker info --format '{{.SecurityOptions}}' 2>/dev/null) || DOCKER_SECURITY=""
        DOCKER_SECURITY_PROBED="true"
    fi
}

# docker_run_mounted read|write ARGS... — `docker run --rm` for a call that
# bind-mounts a host path, with what this daemon needs for the container to
# use it. Docker Engine on Linux keeps host ownership on a bind mount; Docker
# Desktop maps it, which hid all of this.
#
# write: the image's own user can neither create files in the 0755 directory
# `mkdir -p` just made nor replace the invoker's files, so run as the invoker,
# supplementary groups included. The generated files are then theirs, and a
# 0600 secret.yaml stays readable to the kubectl that applies it. Rootless
# Docker maps the invoker to root in the container and their own uid to a
# subordinate one, so there it runs as 0:0. userns-remap maps every container
# uid to a subordinate one, so it also leaves the user namespace.
#
# read: keep the image's own user, because that is who reads the file at run
# time, and mount just the file so the directory it sits in does not matter.
#
# An SELinux daemon confines the container away from the user's files. Lift
# that for this one container instead of relabeling (`:z`) a directory that
# `-o` may have pointed at $HOME.
docker_run_mounted() {
    local access="$1" gid primary
    local -a opts=()
    shift
    docker_security_options
    if [[ "$DOCKER_SECURITY" == *name=selinux* ]]; then
        opts+=(--security-opt label=disable)
    fi
    if [[ "$access" == "write" ]]; then
        if [[ "$DOCKER_SECURITY" == *name=rootless* ]]; then
            opts+=(--user 0:0)
        else
            primary=$(id -g)
            opts+=(--user "$(id -u):${primary}")
            for gid in $(id -G); do
                if [[ "$gid" != "$primary" ]]; then
                    opts+=(--group-add "$gid")
                fi
            done
            if [[ "$DOCKER_SECURITY" == *name=userns* ]]; then
                opts+=(--userns=host)
            fi
        fi
    fi
    docker run --rm ${opts[@]+"${opts[@]}"} "$@"
}

# Docker-mode equivalent of render_click_dog_config. The install/quickstart
# paths refuse to run on macOS, but `install.sh docker` is intended for it
# (Docker Desktop on macOS targeting a Linux container), so we cannot
# resolve_binary + invoke locally — the downloaded archive is always
# linux/<arch> and won't execute on a macOS host. Use `docker run` against
# the same image docker-compose will pull anyway. Resolves the output dir
# to an absolute path so the bind-mount works regardless of $PWD.
render_click_dog_config_via_docker() {
    local out_path="$1"
    local out_dir out_file
    out_dir=$(cd "$(dirname "$out_path")" && pwd)
    out_file=$(basename "$out_path")
    local args=(
        --collector "$COLLECTOR_ADDRESS"
        --service click-dog-monitor
        --ch-user "$CLICKHOUSE_USER"
        -o "/work/${out_file}"
        --force
    )
    [[ "$CLICKHOUSE_TLS" == "true" ]]     && args+=( --ch-secure )
    [[ -n "${CLICKHOUSE_PORT:-}" ]]       && args+=( --ch-port "$CLICKHOUSE_PORT" )
    # Only an explicit choice: unset leaves init to apply the loopback rule.
    [[ -n "$OTEL_SECURE" ]]               && args+=( "--otel-secure=$OTEL_SECURE" )
    [[ -n "$KEEPER_HOSTS" ]]              && args+=( --ha-keeper "$KEEPER_HOSTS" )
    # init's messages name the container path, so show them with the host's.
    local render_output render_rc=0
    render_output=$(secure_temp_file "click-dog-render")
    docker_run_mounted write \
        -v "${out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        init "${args[@]}" > "$render_output" || render_rc=$?
    # awk, because a # or & in the path means something to sed.
    OUT_DIR="${out_dir}/" awk '{
        line = $0
        out = ""
        while ((pos = index(line, "/work/")) > 0) {
            out = out substr(line, 1, pos - 1) ENVIRON["OUT_DIR"]
            line = substr(line, pos + 6)
        }
        print out line
    }' "$render_output"
    rm -f "$render_output"
    return "$render_rc"
}

# Validate an existing on-disk click-dog.yaml against the target image,
# mirroring do_update's pre-swap validation (the `validate` block above
# the local binary mv). Run as a pre-flight before the k8s/docker update
# paths rewrite any deployment file, so we never leave a manifest pointing
# at an image the config can't start under. Mounts the file read-only where
# docker-compose.yml mounts it, as the image's own user, so a config the
# service could not read fails here too. The image entrypoint is
# `click-dog`, so `validate --config` is passed directly.
#
# Validation reads every TLS file a secure connection names, but those files
# live where the deployment mounts them (a Kubernetes Secret or ConfigMap, a
# compose volume), not in this container. Each absolute ca_cert, client_cert,
# and client_key path gets an empty, readable stand-in, so the check is about
# the config and the image, as it is for a config without TLS files. Whether
# the real files exist stays the deployment's job: click-dog refuses to start
# without them.
validate_config_via_docker() {
    local cfg_path="$1"
    local cfg_dir cfg_file stub="" path rc=0
    local tls_mounts=()
    cfg_dir=$(cd "$(dirname "$cfg_path")" && pwd)
    cfg_file=$(basename "$cfg_path")
    while IFS= read -r path; do
        if [[ -z "$stub" ]]; then
            # Beside the config, which the daemon can already mount: a
            # VM-backed Docker (Colima, Rancher Desktop) may not share TMPDIR.
            stub=$(mktemp "${cfg_dir}/.click-dog-tls-stub.XXXXXX") || return 1
            chmod 644 "$stub"
        fi
        tls_mounts+=( -v "${stub}:${path}:ro" )
    done < <(config_tls_file_paths "$cfg_path")
    docker_run_mounted read \
        -v "${cfg_dir}/${cfg_file}:/etc/click-dog/click-dog.yaml:ro" \
        ${tls_mounts[@]+"${tls_mounts[@]}"} \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        validate --config /etc/click-dog/click-dog.yaml || rc=$?
    [[ -n "$stub" ]] && rm -f "$stub"
    return "$rc"
}

# config_tls_file_paths FILE — the absolute paths FILE names as ca_cert,
# client_cert, or client_key, once each. Commented-out keys, relative paths,
# and ${VAR} values are left out: validation reports those as it finds them.
config_tls_file_paths() {
    sed -n -E "s/^[[:space:]]*(- )?(ca_cert|client_cert|client_key):[[:space:]]*[\"']?(\/[^\"'#[:space:]]*)[\"']?[[:space:]]*(#.*)?$/\3/p" "$1" | sort -u
}

# Pre-flight wrapper shared by the k8s/docker update paths. Validation needs
# docker to run the target image; the docker workflow always has it, but the
# k8s workflow is kubectl-only, so when docker is absent we warn and skip
# rather than bolt a hard docker dependency onto kubernetes update. On a
# validation failure we surface the validator's own error and exit nonzero
# *before* the caller mutates the manifest. $2 is the re-render command shown
# in the hint.
preflight_validate_config() {
    local cfg_path="$1" regen_hint="$2"
    if ! command -v docker >/dev/null 2>&1; then
        echo "  Note: docker not found — skipping config validation against image v${VERSION}." >&2
        echo "        Verify the config loads under the new image before applying." >&2
        return 0
    fi
    if ! validate_config_via_docker "$cfg_path" >/dev/null 2>&1; then
        echo "ERROR: existing config does not validate against image v${VERSION}" >&2
        validate_config_via_docker "$cfg_path" >&2 || true
        echo "Re-render via '${regen_hint}' or fix the config, then re-run update." >&2
        echo "  'permission denied' means the image's own user cannot read the file, so" >&2
        echo "  the service could not either: make it mode 0644 (it holds no password)." >&2
        return 1
    fi
}

# Extract the embedded click-dog.yaml document from a generated configmap.yaml
# to stdout. The config lives under `  click-dog.yaml: |` indented four spaces;
# strip that indent so the result is a standalone, validatable config. Stops at
# the first non-indented, non-blank line so any trailing manifest keys added in
# future can't bleed into the extracted config.
extract_configmap_yaml() {
    local configmap="$1"
    awk '
        in_block && /^    / { sub(/^    /, ""); print; next }
        in_block && /^[[:space:]]*$/ { print ""; next }
        in_block { exit }
        /^  click-dog\.yaml: \|/ { in_block=1 }
    ' "$configmap"
}

# ── OTLP receiver TLS ──────────────────────────────────────────
# `secure: true` makes click-dog dial the receiver over TLS, verified against
# the host's system trust roots, or against `ca_cert` alone when one is set
# (Go's RootCAs: the system store is then not consulted). The transport for
# an address and flags is never decided here: the click-dog being installed
# reports it (`click-dog init --print-otel-secure`, see
# resolve_otel_transport). These helpers ask it, offer it as the guided
# default, and prove a TLS choice with one handshake before anything is
# installed. The handshake is made by the click-dog binary being installed
# (`click-dog test tls`), with the exporter's own TLS code, so its verdict is
# the one export will reach: the chain against those roots, the validity
# dates, a subjectAltName naming the dialed host (never the Common Name), and
# ALPN h2. A self-signed or private-CA collector can be pinned on first use in
# the guided install; unattended installs take --otel-ca-cert instead and
# never trust on first use. mTLS stays out of the installer.
OTEL_TLS_DOCS="https://click-dog.com/integrations/generic-otlp/#tls-configuration"
OTEL_SELF_SIGNED_DOCS="https://click-dog.com/integrations/generic-otlp/#self-signed-collector"

# Split COLLECTOR_ADDRESS as gRPC targets are written: [v6]:port for an IPv6
# literal, otherwise host before the first colon and port after the last;
# 443 when there is no port, as grpc's dns resolver defaults it, so the port
# checked is the one export dials. The host comes back without brackets, the
# form bash's /dev/tcp takes.
otel_collector_host() {
    if [[ "$COLLECTOR_ADDRESS" == \[*\]* ]]; then
        local host="${COLLECTOR_ADDRESS#\[}"
        echo "${host%%\]*}"
        return
    fi
    echo "${COLLECTOR_ADDRESS%%:*}"
}
otel_collector_port() {
    local port
    if [[ "$COLLECTOR_ADDRESS" == \[*\]* ]]; then
        port="${COLLECTOR_ADDRESS#*\]}"
        port="${port#:}"
        echo "${port:-443}"
        return
    fi
    port="${COLLECTOR_ADDRESS##*:}"
    [[ "$port" == "$COLLECTOR_ADDRESS" ]] && port="443"
    echo "$port"
}

is_ipv6() { [[ "$1" == *:* && "$1" =~ ^[0-9A-Fa-f:]+$ ]]; }

# otel_hostport HOST PORT — HOST:PORT, with an IPv6 literal bracketed as gRPC
# addresses and every message expect.
otel_hostport() {
    if is_ipv6 "$1"; then
        echo "[$1]:$2"
    else
        echo "$1:$2"
    fi
}

# run_click_dog ARGS... — the click-dog that decides and checks the OTLP
# transport: the resolved binary, or for docker the image it deploys
# (CLICK_DOG_RUN_IMAGE), run with the host network. That shares the network
# namespace the generated Compose service uses, including on Docker Desktop,
# where no host binary runs and the host network is the Docker VM's.
run_click_dog() {
    if [[ -n "$CLICK_DOG_RUN_IMAGE" ]]; then
        docker run --rm --network host "$CLICK_DOG_RUN_IMAGE" "$@"
    else
        "$BINARY" "$@"
    fi
}

# otel_print_secure_args — the `click-dog init --print-otel-secure` argv for
# this run's inputs, in OTEL_PRINT_SECURE_ARGS: the collector, plus
# --otel-secure and --otel-ca-cert only when given.
otel_print_secure_args() {
    OTEL_PRINT_SECURE_ARGS=( init --print-otel-secure --collector "$COLLECTOR_ADDRESS" )
    [[ -n "$OTEL_SECURE" ]] && OTEL_PRINT_SECURE_ARGS+=( "--otel-secure=$OTEL_SECURE" )
    [[ -n "$OTEL_CA_CERT" ]] && OTEL_PRINT_SECURE_ARGS+=( --otel-ca-cert "$OTEL_CA_CERT" )
    return 0
}

# resolve_otel_transport — set OTEL_USES_TLS to the transport the click-dog
# being installed gives this run's inputs. The installer keeps no copy of the
# loopback rule: the binary (or image) that will export decides, so the
# installer and the exporter cannot disagree. Returns 1, having said why,
# when that click-dog gives no answer (an older release, or one that cannot
# run here).
resolve_otel_transport() {
    local out
    otel_print_secure_args
    out=$(run_click_dog "${OTEL_PRINT_SECURE_ARGS[@]}" 2>/dev/null) || true
    case "$out" in
        true|false)
            OTEL_USES_TLS="$out"
            return 0
            ;;
    esac
    echo "Error: could not get the OTLP transport for ${COLLECTOR_ADDRESS} from" >&2
    echo "       'click-dog init --print-otel-secure'. Releases before it cannot be" >&2
    echo "       installed by this install.sh: omit -v for the latest release" >&2
    echo "       (https://github.com/coltconsulting/click-dog/releases/latest)." >&2
    run_click_dog "${OTEL_PRINT_SECURE_ARGS[@]}" 2>&1 | sed 's/^/  /' >&2 || true
    return 1
}

# otel_transport_label — the transport for status lines: TLS or plaintext,
# and whether it was chosen or followed the automatic rule.
otel_transport_label() {
    local transport="plaintext" source="automatic"
    [[ "$OTEL_USES_TLS" == "true" ]] && transport="TLS"
    if [[ -n "$OTEL_CA_CERT" ]]; then
        source="CA ${OTEL_CA_CERT}"
    elif [[ -n "$OTEL_SECURE" ]]; then
        source="--otel-secure=${OTEL_SECURE}"
    fi
    echo "${transport}, ${source}"
}

# validate_otel_ca_file FILE — reject what cannot be a collector CA before it
# reaches the config: a missing file, a private key (the usual copy mistake),
# DER or other non-PEM content, or PEM with no certificate in it.
validate_otel_ca_file() {
    local f="$1"
    if [[ ! -f "$f" || ! -r "$f" ]]; then
        echo "Error: --otel-ca-cert file not found or not readable: $f" >&2
        return 1
    fi
    if grep -q -- 'PRIVATE KEY-----' "$f"; then
        echo "Error: $f contains a PRIVATE KEY block. --otel-ca-cert takes the collector's" >&2
        echo "       CA certificate (its own certificate when self-signed), never a key." >&2
        return 1
    fi
    if ! grep -q -- '-----BEGIN ' "$f"; then
        echo "Error: $f is not PEM. If it is DER, convert it first:" >&2
        echo "       openssl x509 -inform der -in $f -out ca.pem" >&2
        return 1
    fi
    if ! grep -q -- '-----BEGIN CERTIFICATE-----' "$f"; then
        echo "Error: $f contains no certificate (no BEGIN CERTIFICATE block)." >&2
        return 1
    fi
    if command -v openssl >/dev/null 2>&1 && ! openssl x509 -noout -in "$f" >/dev/null 2>&1; then
        echo "Error: the first certificate in $f does not parse." >&2
        return 1
    fi
}

# otel_json_field NAME — the top-level string field NAME of the `click-dog
# test tls --json` object on stdin. click-dog writes one top-level field per
# line at a two-space indent and keeps verdict and message to one line with
# no quotes or backslashes (TestRunTestTLS_JSONLinesInstallShReads pins
# both), so no JSON parser is needed.
otel_json_field() {
    sed -n 's/^  "'"$1"'": "\([^"\\]*\)",\{0,1\}$/\1/p'
}

# otel_collector_has_scheme — COLLECTOR_ADDRESS is a gRPC target with a
# scheme (dns:, passthrough:, unix:, unix-abstract:, or any scheme://), not
# the host:port the installer's checks split it into.
otel_collector_has_scheme() {
    [[ "$COLLECTOR_ADDRESS" == *://* || "$COLLECTOR_ADDRESS" =~ ^(dns|passthrough|unix|unix-abstract): ]]
}

# probe_otel_tls HOST PORT [CAFILE [CHAIN]] — one handshake made by the
# click-dog binary's own exporter TLS code (`click-dog test tls`). Prints
# three fields: a verdict, its reason code ("-" when it has none), and a
# detail for people. Callers decide on the verdict and reason only; the
# detail is click-dog's wording, shown as is.
#   ok           TLS, certificate trusted and named for HOST, ALPN h2
#   plaintext    non-tls: the receiver answered in another protocol; closed:
#                it closed without a word, as a plaintext port does, and as
#                a TLS proxy that serves no certificate for HOST (SNI) does
#   no-san       TLS, but the certificate has no subjectAltName at all
#   untrusted    hostname-mismatch, expired, not-yet-valid,
#                unknown-authority, or invalid
#   client-cert  the receiver refused the handshake for want of a client
#                certificate (mTLS)
#   no-alpn      TLS, but the receiver does not select HTTP/2 (ALPN h2),
#                which the gRPC exporter requires
#   config       click-dog could not build the exporter's TLS settings (a
#                CA file it cannot parse, an address it refuses); the
#                exporter would fail at startup on the same input, so this
#                is a failure, never a skipped check
#   unknown      unreachable, timeout, handshake-ended (a TLS alert before
#                any certificate), other, or binary (this click-dog cannot
#                run here or gave no verdict); callers fall back to
#                reachability only
# With CAFILE, only that file is trusted, as for exporters.otel[].ca_cert.
# With CHAIN, the certificates the receiver sent are written there, leaf
# first, as PEM under "# Label: value" lines.
probe_otel_tls() {
    local host="$1" port="$2" ca="${3:-}" chain="${4:-}" out verdict reason message
    # HOST and PORT come from splitting COLLECTOR_ADDRESS at its first colon,
    # which is wrong for a gRPC target with a scheme; the exporter accepts
    # those, so a check aimed at the wrong place must not become a failure.
    if otel_collector_has_scheme; then
        echo "unknown target the installer checks host:port collector addresses only; 'click-dog check' tests ${COLLECTOR_ADDRESS} after install"
        return 0
    fi
    local args=( test tls --collector "$(otel_hostport "$host" "$port")" --json )
    [[ -n "$ca" ]] && args+=( --ca-cert "$ca" )
    [[ -n "$chain" ]] && args+=( --chain-out "$chain" )
    # Exit 1 is any verdict but ok; the JSON names it.
    out=$(run_click_dog "${args[@]}" 2>/dev/null) || true
    verdict=$(otel_json_field verdict <<<"$out")
    if [[ -z "$verdict" ]]; then
        # No verdict is never a guess: say why the check could not run.
        if ! run_click_dog --version >/dev/null 2>&1; then
            if [[ -n "$CLICK_DOG_RUN_IMAGE" ]]; then
                echo "unknown binary docker could not run ${CLICK_DOG_RUN_IMAGE} with the host network"
            else
                echo "unknown binary the click-dog binary cannot run on this host"
            fi
        elif [[ "$(run_click_dog test help 2>&1 || true)" != *"  tls  "* ]]; then
            echo "unknown binary this click-dog release predates 'click-dog test tls'"
        else
            echo "unknown binary 'click-dog test tls' gave no verdict"
        fi
        return 0
    fi
    reason=$(otel_json_field reason <<<"$out")
    message=$(otel_json_field message <<<"$out")
    # click-dog reports these as unknown, since nothing was dialed, but they
    # are not inconclusive: export would fail the same way.
    if [[ "$verdict:$reason" == "unknown:config" ]]; then
        verdict="config"
    fi
    echo "${verdict} ${reason:--}${message:+ $message}"
}

# otel_chain_block first|last — the first or last certificate of a
# `click-dog test tls --chain-out` file on stdin (the leaf, or the top-most
# certificate the receiver sent), with the "# Label: value" lines above it.
otel_chain_block() {
    awk -v want="$1" '
        /^-----BEGIN CERTIFICATE-----/ { inb = 1 }
        inb || /^# / { cur = cur $0 "\n" }
        /^-----END CERTIFICATE-----/ && inb { inb = 0; last = cur; if (first == "") first = cur; cur = "" }
        END { printf "%s", (want == "first" ? first : last) }
    '
}

# otel_cert_pem — the PEM certificate alone from a chain block on stdin.
otel_cert_pem() {
    sed -n '/^-----BEGIN CERTIFICATE-----/,/^-----END CERTIFICATE-----/p'
}

# otel_cert_label LABEL — the value on a chain block's "# LABEL:" line.
otel_cert_label() {
    sed -n "s/^# $1: *//p"
}

# otel_tls_problem VERDICT REASON DETAIL HOST PORT [CAFILE] — what a failed
# probe means and where the fix lives, chosen by verdict and reason; DETAIL
# is only shown. Plain stdout lines; callers indent and pick the stream.
otel_tls_problem() {
    local verdict="$1" reason="$2" detail="$3" host="$4" port="$5" ca="${6:-}" at
    at=$(otel_hostport "$host" "$port")
    case "$verdict" in
        plaintext)
            if [[ "$reason" == "closed" ]]; then
                echo "${at} closed the connection without answering the TLS handshake."
                echo "Either it does not speak TLS on this port, or it is a TLS proxy or load"
                echo "balancer that serves no certificate for '${host}' (SNI). Find out which"
                echo "before choosing plaintext."
            else
                echo "The collector does not speak TLS on ${at}: it accepted the"
                echo "connection but answered the TLS handshake without a certificate."
            fi
            ;;
        no-san)
            echo "The certificate at ${at} has no subjectAltName. click-dog"
            echo "checks names against SANs only and ignores the Common Name, so it"
            echo "would reject this certificate. Reissue it with a SAN for '${host}'."
            echo "See ${OTEL_SELF_SIGNED_DOCS}"
            ;;
        untrusted)
            case "$reason" in
                hostname-mismatch)
                    echo "The certificate at ${at} is not issued for '${host}'"
                    echo "(${detail}). Use the name it was issued for, or reissue it."
                    ;;
                expired|not-yet-valid)
                    echo "The certificate at ${at} is not valid now (${detail})."
                    echo "Renew it, and check this host's clock."
                    ;;
                *)
                    if [[ -n "$ca" ]]; then
                        echo "Certificate not trusted: ${ca} does not verify the certificate at"
                        echo "${at} (${detail}). Use the CA that issued it, or the"
                        echo "collector's own certificate when it is self-signed."
                    else
                        echo "Certificate not trusted: ${at} speaks TLS, but this host's"
                        echo "system trust store rejects its certificate (${detail})."
                        echo "A self-signed or private-CA collector needs its CA as exporters.otel[].ca_cert."
                    fi
                    ;;
            esac
            echo "See ${OTEL_TLS_DOCS}"
            ;;
        client-cert)
            echo "${at} requires a client certificate (mTLS): it presented a"
            echo "trusted certificate, then refused the handshake. That needs"
            echo "exporters.otel[].client_cert and client_key."
            echo "See ${OTEL_TLS_DOCS}"
            ;;
        config)
            if [[ -n "$ca" ]]; then
                echo "click-dog cannot use ${ca} as the collector's CA (${detail})."
                echo "It must be a readable PEM CA certificate; the service would fail to"
                echo "start with it."
            else
                echo "click-dog cannot build TLS settings for ${at} (${detail})."
            fi
            echo "See ${OTEL_TLS_DOCS}"
            ;;
        no-alpn)
            echo "${at} speaks TLS but does not select HTTP/2 (ALPN h2), which"
            echo "click-dog's gRPC exporter requires. Enable HTTP/2 on the TLS listener"
            echo "(a load balancer or proxy in front of the collector usually needs it"
            echo "turned on), or point -c at the collector itself."
            echo "See ${OTEL_TLS_DOCS}"
            ;;
    esac
}

# preflight_otel_tls — the TLS gate for install and docker, run whenever the
# transport is TLS (automatic or explicit), once the click-dog that will
# export can run, and before anything is written. A definitive mismatch
# returns 1: the rendered config could not export. An inconclusive probe
# (receiver not up yet, a binary that cannot run on this host) is a note, the
# same best-effort stance as the ClickHouse TLS detection. Unattended
# installs never trust on first use: an unknown CA needs --otel-ca-cert.
preflight_otel_tls() {
    local host port at result verdict reason detail why plain_fix
    host=$(otel_collector_host)
    port=$(otel_collector_port)
    at=$(otel_hostport "$host" "$port")
    result=$(probe_otel_tls "$host" "$port" "$OTEL_CA_CERT")
    read -r verdict reason detail <<<"$result"
    case "$verdict" in
        ok)
            if [[ -n "$OTEL_CA_CERT" ]]; then
                echo "OTLP receiver ${at}: TLS handshake ok, certificate verified by ${OTEL_CA_CERT}"
            else
                echo "OTLP receiver ${at}: TLS handshake ok, certificate trusted"
            fi
            return 0
            ;;
        unknown)
            echo "Note: TLS to ${at} not verified (${detail}); 'click-dog check' tests it after install." >&2
            return 0
            ;;
    esac
    # Why the transport is TLS, and how plaintext would be asked for instead.
    # Dropping a flag is never the fix: unset, a collector off loopback still
    # gets TLS.
    if [[ -n "$OTEL_CA_CERT" ]]; then
        why="--otel-ca-cert was given"
        plain_fix="pass --otel-secure=false in place of --otel-ca-cert"
    elif [[ "$OTEL_SECURE" == "true" ]]; then
        why="--otel-secure was given"
        plain_fix="pass --otel-secure=false in place of --otel-secure"
    else
        why="TLS is the default for ${COLLECTOR_ADDRESS}, which is not loopback"
        plain_fix="pass --otel-secure=false"
    fi
    if [[ "$verdict" == "config" ]]; then
        echo "Error: ${why}, but click-dog cannot use its TLS settings for ${at}." >&2
    else
        echo "Error: ${why}, but the TLS handshake with ${at} failed." >&2
    fi
    otel_tls_problem "$verdict" "$reason" "$detail" "$host" "$port" "$OTEL_CA_CERT" | sed 's/^/  /' >&2
    case "$verdict:$reason" in
        plaintext:closed)
            echo "  If the receiver is a plaintext port, ${plain_fix};" >&2
            echo "  if it is a TLS proxy or load balancer, have it serve a certificate for '${host}'." >&2
            ;;
        plaintext:*)
            echo "  To export in plaintext, ${plain_fix}. Or enable TLS on the receiver." >&2
            ;;
        untrusted:unknown-authority)
            if [[ -z "$OTEL_CA_CERT" ]]; then
                echo "  Pass --otel-ca-cert FILE with that CA (the collector's own certificate" >&2
                echo "  when self-signed). Unattended installs never trust a certificate on first use." >&2
            fi
            ;;
        client-cert:*)
            echo "  The installer does not set client certificates. Render a config with" >&2
            echo "  'click-dog init --otel-secure=true ...', add them under exporters.otel, and" >&2
            echo "  install it with 'install.sh install -f click-dog.yaml'." >&2
            ;;
    esac
    return 1
}

# prompt_otel_tls — the guided quick start's TLS question. The answer on
# Enter is OTEL_USES_TLS, the automatic choice the binary gave. Accepting it
# leaves OTEL_SECURE empty, so `click-dog init` renders by the loopback rule
# (a loopback collector gets the rule comment, not a sticky `secure: false`);
# only a different answer becomes an explicit OTEL_SECURE.
prompt_otel_tls() {
    local default="$OTEL_USES_TLS" hint answer choice
    hint="y/N"
    [[ "$default" == "true" ]] && hint="Y/n"
    echo "  TLS verifies the collector against this host's system trust store; a"
    echo "  self-signed one can be trusted after a fingerprint check. A local Agent"
    echo "  or Collector on localhost usually listens in plaintext."
    while true; do
        read -rp "  Use TLS for the collector connection? [${hint}] " answer
        case "$answer" in
            "")                choice="$default" ;;
            [yY]|[yY][eE][sS]) choice="true" ;;
            [nN]|[nN][oO])     choice="false" ;;
            *)                 echo "  Please enter y or n."; continue ;;
        esac
        break
    done
    OTEL_USES_TLS="$choice"
    if [[ "$choice" == "$default" ]]; then
        OTEL_SECURE=""
    else
        OTEL_SECURE="$choice"
    fi
}

# guided_otel_transport — step 2's transport choice. The binary fetched
# before step 2 gives the transport for the address and any flags; a flag
# settles the question, otherwise the operator answers with that as the
# default.
guided_otel_transport() {
    if [[ -n "$OTEL_CA_CERT" ]]; then
        validate_otel_ca_file "$OTEL_CA_CERT" || exit 1
    fi
    resolve_otel_transport || exit 1
    if [[ -n "$OTEL_CA_CERT" ]]; then
        echo "  Using TLS with CA ${OTEL_CA_CERT} (from --otel-ca-cert)"
    elif [[ "$OTEL_SECURE" == "true" ]]; then
        echo "  Using TLS (from --otel-secure)"
    elif [[ "$OTEL_SECURE" == "false" ]]; then
        echo "  Using plaintext (from --otel-secure=false)"
    else
        prompt_otel_tls
    fi
}

# prompt_k8s_otel_transport — `install.sh kubernetes` on a terminal, with no
# --otel-secure: the generator cannot see the cluster's network and refuses a
# collector off loopback without a transport, so ask which one the in-cluster
# collector serves. There is no default: either guess can fail after rollout.
# Sets OTEL_SECURE.
prompt_k8s_otel_transport() {
    local answer
    echo "  ${COLLECTOR_ADDRESS} is resolved inside the cluster, so this host cannot"
    echo "  check it. In-cluster collectors (an OpenTelemetry Collector or Datadog"
    echo "  Agent on 4317) usually serve plaintext; a TLS receiver is verified"
    echo "  against the image's system trust store."
    while true; do
        read -rp "  Which transport does ${COLLECTOR_ADDRESS} serve? [t]ls or [p]laintext: " answer
        case "$answer" in
            [tT]|[tT][lL][sS])                      OTEL_SECURE="true"; break ;;
            [pP]|[pP][lL][aA][iI][nN][tT][eE][xX][tT]) OTEL_SECURE="false"; break ;;
            *) echo "  Please enter t or p." ;;
        esac
    done
    echo "  → Passing --otel-secure=${OTEL_SECURE} to the generator"
}

# Trust on first use needs a person reading the certificate. A seam so tests
# can drive the prompt from a pipe.
stdin_is_tty() { [[ -t 0 ]]; }

# otel_cert_summary — subject, issuer, SANs and validity of a chain block on
# stdin, one labeled line each, as click-dog described the certificate.
otel_cert_summary() {
    grep -E '^# (Subject|Issuer|SANs|Not before|Not after):' | sed 's/^# //' || true
}

# otel_cert_self_issued — succeeds when the chain block on stdin names itself
# as issuer: a self-signed collector certificate or a root CA.
otel_cert_self_issued() {
    local block subject
    block=$(cat)
    subject=$(otel_cert_label Subject <<<"$block")
    [[ -n "$subject" && "$subject" == "$(otel_cert_label Issuer <<<"$block")" ]]
}

# otel_cert_fingerprint — SHA-256 fingerprint of the chain block on stdin, in
# the colon form `openssl x509 -fingerprint -sha256` prints.
otel_cert_fingerprint() {
    otel_cert_label SHA-256
}

# otel_tofu_refusal VERDICT REASON DETAIL HOST — why a certificate is not
# offered, chosen by verdict and reason.
otel_tofu_refusal() {
    local verdict="$1" reason="$2" detail="$3" host="$4"
    case "$verdict:$reason" in
        no-san:*)
            echo "it has no subjectAltName for '${host}', and click-dog ignores the Common Name. Reissue it with a SAN: ${OTEL_SELF_SIGNED_DOCS}" ;;
        untrusted:hostname-mismatch)
            echo "it is not issued for '${host}' (${detail})." ;;
        untrusted:expired)
            echo "it has expired (${detail})." ;;
        untrusted:not-yet-valid)
            echo "it is not valid yet (${detail}); check this host's clock and the certificate's dates." ;;
        client-cert:*)
            echo "the collector also requires a client certificate (mTLS), which this installer does not set." ;;
        no-alpn:*)
            echo "the collector does not select HTTP/2 (ALPN h2), which the gRPC exporter requires." ;;
        unknown:*|config:*)
            echo "click-dog could not prove it would verify the collector (${detail})." ;;
        *)
            echo "trusting it would still not verify the collector (${detail:-$verdict})." ;;
    esac
}

# offer_otel_tofu HOST PORT — trust on first use, guided and TTY only, for a
# collector whose certificate the system store rejects. Pins the top-most
# certificate the collector sent: the certificate itself when self-signed,
# otherwise the highest CA in the chain, which survives leaf rotation. It is
# offered only when a handshake trusting that pin alone passes every check
# the exporter makes, so an expired or not-yet-valid certificate, or one
# without a SAN for HOST, is refused rather than pinned. The answer defaults
# to no. On yes, sets OTEL_CA_CERT (and OTEL_CA_PIN_TMP) to the pinned PEM,
# a temp file the install step copies to OTEL_CA_CERT_PATH. Returns 1 when
# the operator declines, 2 when the certificate is refused.
offer_otel_tofu() {
    local host="$1" port="$2" chain leaf top pin result verdict reason detail answer
    chain=$(mktemp "${TMPDIR:-/tmp}/click-dog-otel-chain.XXXXXX")
    probe_otel_tls "$host" "$port" "" "$chain" >/dev/null
    leaf=$(otel_chain_block first < "$chain")
    top=$(otel_chain_block last < "$chain")
    rm -f "$chain"
    [[ -n "$top" ]] || return 1
    pin=$(mktemp "${TMPDIR:-/tmp}/click-dog-otel-ca.XXXXXX")
    # The certificate alone: the summary lines are for this prompt.
    otel_cert_pem <<<"$top" > "$pin"
    result=$(probe_otel_tls "$host" "$port" "$pin")
    read -r verdict reason detail <<<"$result"

    echo "  The collector presented:"
    otel_cert_summary <<<"$leaf" | sed 's/^/    /'
    if [[ "$top" != "$leaf" ]]; then
        echo "  Certificate to trust, the top of the chain it sent:"
        otel_cert_summary <<<"$top" | sed 's/^/    /'
    fi
    echo "    SHA-256:     $(otel_cert_fingerprint <<<"$top")"
    echo ""
    if [[ "$verdict" != "ok" ]]; then
        echo "  Not offering to trust it: $(otel_tofu_refusal "$verdict" "$reason" "$detail" "$host")"
        rm -f "$pin"
        return 2
    fi
    # A leaf sent without its chain verifies as its own anchor, as it does for
    # Go, but the pin then names that one certificate: renewal breaks it.
    if [[ "$top" == "$leaf" ]] && ! otel_cert_self_issued <<<"$leaf"; then
        echo "  Note: the collector sent only its own certificate, issued by another CA."
        echo "  Trusting it pins this certificate, so export stops verifying when it is"
        echo "  renewed. To trust the issuing CA instead, answer no and rerun with"
        echo "  --otel-ca-cert FILE."
        echo ""
    fi
    echo "  Compare the SHA-256 fingerprint with the certificate on the collector"
    echo "  (its CA certificate when it sent a chain) before answering:"
    echo "    openssl x509 -in <cert> -noout -fingerprint -sha256"
    read -rp "  Trust this certificate? [y/N] " answer
    case "$answer" in
        [yY]|[yY][eE][sS])
            OTEL_CA_CERT="$pin"
            # quickstart_exit_cleanup removes it if the run stops before
            # the install step copies it into place.
            OTEL_CA_PIN_TMP="$pin"
            echo "  → Will install it as ${OTEL_CA_CERT_PATH} (exporters.otel[].ca_cert)"
            return 0
            ;;
    esac
    rm -f "$pin"
    return 1
}

# confirm_otel_tls_choice HOST PORT — the guided check for a TLS answer, run
# once the port is known to be reachable. A mismatch is explained, then the
# operator picks: plaintext when the receiver has no TLS; trusting the
# certificate on first use when only the CA is unknown; or carrying on (and
# fixing the config after install) otherwise.
confirm_otel_tls_choice() {
    local host="$1" port="$2" result verdict reason detail choice tofu=1 prompt
    if [[ -n "$OTEL_CA_CERT" ]]; then
        echo -n "  Checking TLS handshake (CA ${OTEL_CA_CERT})... "
    else
        echo -n "  Checking TLS handshake (system trust store)... "
    fi
    result=$(probe_otel_tls "$host" "$port" "$OTEL_CA_CERT")
    read -r verdict reason detail <<<"$result"
    case "$verdict" in
        ok)      echo "ok, certificate trusted"; return 0 ;;
        unknown) echo "skipped (${detail})"; return 0 ;;
    esac
    echo "FAILED"
    echo ""
    otel_tls_problem "$verdict" "$reason" "$detail" "$host" "$port" "$OTEL_CA_CERT" | sed 's/^/  /'
    echo ""
    # Continuing would install a config the service cannot start with.
    if [[ "$verdict" == "config" ]]; then
        echo "Aborted."
        exit 1
    fi
    if [[ "$verdict" == "plaintext" ]]; then
        prompt="  [p]laintext instead, [c]ontinue with TLS, or [q]uit? "
        # A silent close is as likely a TLS proxy with no certificate for
        # this name as a plaintext port: do not lead with plaintext.
        if [[ "$reason" == "closed" ]]; then
            echo "  Plaintext is right only if this port really is plaintext. For a TLS"
            echo "  proxy or load balancer, fix its certificate for '${host}' and continue."
            prompt="  [c]ontinue with TLS, [p]laintext instead, or [q]uit? "
        fi
        while true; do
            read -rp "$prompt" choice
            case "$choice" in
                [pP])
                    OTEL_SECURE="false"
                    OTEL_USES_TLS="false"
                    OTEL_CA_CERT=""
                    echo "  → Will configure click-dog for plaintext export (--otel-secure=false)"
                    return 0
                    ;;
                [cC]) return 0 ;;
                [qQ]) echo "Aborted."; exit 0 ;;
                *)    echo "  Please enter p, c, or q." ;;
            esac
        done
    fi
    if [[ "$verdict" == "untrusted" && "$reason" != "hostname-mismatch" && -z "$OTEL_CA_CERT" ]] && stdin_is_tty; then
        if offer_otel_tofu "$host" "$port"; then
            return 0
        else
            tofu=$?
        fi
        echo ""
    fi
    # Only a client certificate or the collector's CA would fix these; a
    # refused pin already said why a CA would not help.
    if [[ "$verdict" == "client-cert" || ( "$verdict:$reason" == "untrusted:unknown-authority" && -z "$OTEL_CA_CERT" && "$tofu" != 2 ) ]]; then
        echo "  To finish, add those files under exporters.otel in"
        echo "  /etc/click-dog/click-dog.yaml after install, then restart click-dog."
    fi
    while true; do
        read -rp "  Continue with TLS? [y/n] " choice
        case "$choice" in
            [yY]) return 0 ;;
            [nN]) echo "Aborted."; exit 0 ;;
            *)    echo "  Please enter y or n." ;;
        esac
    done
}

# install_otel_ca_cert — place OTEL_CA_CERT at OTEL_CA_CERT_PATH, where the
# rendered config's ca_cert points: owned by the runtime user, directory
# 0750, file 0640. Callers run it once the click-dog account exists.
install_otel_ca_cert() {
    local dir="${OTEL_CA_CERT_PATH%/*}"
    mkdir -p "$dir"
    chown click-dog:click-dog "$dir"
    chmod 750 "$dir"
    if [[ ! "$OTEL_CA_CERT" -ef "$OTEL_CA_CERT_PATH" ]]; then
        cp "$OTEL_CA_CERT" "$OTEL_CA_CERT_PATH"
    fi
    chown click-dog:click-dog "$OTEL_CA_CERT_PATH"
    chmod 640 "$OTEL_CA_CERT_PATH"
}

# backup_otel_ca — keep the CA a previous install's config names as
# OTEL_CA_CERT_PATH.bak before install_otel_ca_cert replaces it, so a failed
# reinstall can put it back. A stale backup from an earlier run is dropped.
backup_otel_ca() {
    if [[ -f "$OTEL_CA_CERT_PATH" ]]; then
        cp -p "$OTEL_CA_CERT_PATH" "${OTEL_CA_CERT_PATH}.bak"
    else
        rm -f "${OTEL_CA_CERT_PATH}.bak"
    fi
}

# restore_otel_ca_backup — put back the CA a failed reinstall replaced.
restore_otel_ca_backup() {
    if [[ -f "${OTEL_CA_CERT_PATH}.bak" ]]; then
        mv "${OTEL_CA_CERT_PATH}.bak" "$OTEL_CA_CERT_PATH"
    fi
}

# verify_installed_otel_ca HOST PORT — re-run the handshake against the file
# the config now names, so what is installed is what was proven. Returns 1 on
# a definitive failure, 2 when click-dog cannot use the file at all (the
# service could not start, whether or not the receiver is reachable); an
# unreachable receiver is only a note.
verify_installed_otel_ca() {
    local host="$1" port="$2" result verdict reason detail
    echo -n "  Checking TLS handshake against ${OTEL_CA_CERT_PATH}... "
    result=$(probe_otel_tls "$host" "$port" "$OTEL_CA_CERT_PATH")
    read -r verdict reason detail <<<"$result"
    case "$verdict" in
        ok)      echo "ok"; return 0 ;;
        unknown) echo "skipped (${detail})"; return 0 ;;
    esac
    echo "FAILED"
    otel_tls_problem "$verdict" "$reason" "$detail" "$host" "$port" "$OTEL_CA_CERT_PATH" | sed 's/^/    /'
    [[ "$verdict" == "config" ]] && return 2
    return 1
}

# ── Generate systemd unit ──────────────────────────────────────
generate_systemd_unit() {
    cat <<UNIT
[Unit]
Description=click-dog ClickHouse OTEL span exporter
Documentation=https://github.com/coltconsulting/click-dog
After=network-online.target clickhouse-server.service
Wants=network-online.target
StartLimitIntervalSec=60
StartLimitBurst=5

[Service]
Type=simple
User=click-dog
Group=click-dog
ExecStart=/usr/local/bin/click-dog --config /etc/click-dog/click-dog.yaml
Restart=on-failure
RestartSec=5

# Hardening
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/log/click-dog
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT
}

# ── Release verification: signed checksums (cosign keyless) ─────
#
# Pinned signer identity for the click-dog release workflow. Hardcoded —
# never read from config or release assets — so a compromised config or a
# malicious release cannot weaken it. Mirrors the constants in
# internal/updater/github.go and the manual-verification snippet in
# docs/install.md; keep all three in sync if they ever change.
COSIGN_IDENTITY_REGEXP='^https://github\.com/coltconsulting/click-dog/\.github/workflows/release\.yml@refs/tags/v.+$'
COSIGN_OIDC_ISSUER='https://token.actions.githubusercontent.com'

# Cap on cosign verify-blob, which makes outbound calls to Rekor (and
# potentially Fulcio). Without a bound a slow or blocked transparency-log
# endpoint would hang the installer indefinitely. Mirrors the 60s cap in
# internal/updater/github.go (cosignVerifyTimeout). Overridable via env
# only — primarily so tests can shorten it; the runtime install path
# always uses the default.
#
# Special values: empty string or "0" disables the timeout wrapper
# entirely (cosign runs unbounded). We handle this in the shell rather
# than relying on `timeout 0`, whose semantics differ between
# implementations: GNU coreutils treats 0 as "disable", but other
# `timeout` implementations send SIGTERM immediately. Skipping the
# wrapper in shell makes the behavior deterministic regardless of
# which `timeout` is on PATH.
#
# Security note: cosign is optional for installation, but when it is present
# verification remains fail closed. This knob only changes when that
# verification gives up; it never turns a bad signature into a checksum-only
# success. Setting it very low can cause false-positive timeouts on slow
# networks; disabling it via 0/empty drops hang protection but cosign still
# runs and verifies.
COSIGN_VERIFY_TIMEOUT_S="${COSIGN_VERIFY_TIMEOUT_S:-60}"

# Set by verify_release_archive after all selected verification succeeds.
# Values: signed, checksum-no-cosign, checksum-cosign-ignored, or provided.
# Callers use this single result for completion messages; a failed verifier
# leaves it empty so no success claim can be printed accidentally.
RELEASE_VERIFICATION_MODE=""

# sha256_file <path>
# Print the hex SHA256 of <path> on stdout. Returns nonzero if no SHA256
# tool is on PATH; callers must check the exit status (don't pipe directly
# into command substitution and ignore failure).
sha256_file() {
    local path="$1"
    if command -v sha256sum &>/dev/null; then
        sha256sum "$path" | awk '{print $1}'
    elif command -v shasum &>/dev/null; then
        shasum -a 256 "$path" | awk '{print $1}'
    else
        return 1
    fi
}

# verify_release_archive <verify_dir> <archive_path> <archive_name> <cosign_path> <checksum_reason>
#
# Always verifies a release archive against checksums.txt. A non-empty
# cosign_path authenticates that manifest against the pinned release-workflow
# identity; the verify_dir must then contain checksums.txt.sig and
# checksums.txt.pem as well. When cosign_path is empty, checksum_reason must be
# either "missing" or "ignored" so the warning states why authentication was
# skipped.
#
# Steps:
#   1. If available, verify the cosign keyless signature on checksums.txt
#      against the pinned release-workflow identity and OIDC issuer.
#   2. Look up the archive's exact SHA256 entry in checksums.txt.
#   3. Compute the archive's SHA256 and compare it.
#
# Returns 0 only when the checksum matches and, if cosign is installed, the
# manifest signature is valid. Fails closed (nonzero, with a clear error to
# stderr) on:
#   - sha256sum/shasum not on PATH
#   - checksums.txt missing or empty
#   - cosign present but its signature assets are missing or verification fails
#   - archive name absent from checksums.txt
#   - archive hash mismatch
#
# Callers MUST treat any nonzero return as fatal and abort before
# extracting the archive.
verify_release_archive() {
    local verify_dir="$1"
    local archive_path="$2"
    local archive_name="$3"
    local cosign_path="$4"
    local checksum_reason="$5"
    local verification_mode=""

    RELEASE_VERIFICATION_MODE=""

    local checksums="$verify_dir/checksums.txt"
    local sig="$verify_dir/checksums.txt.sig"
    local cert="$verify_dir/checksums.txt.pem"

    if ! command -v sha256sum &>/dev/null && ! command -v shasum &>/dev/null; then
        echo "Error: neither sha256sum nor shasum found on PATH." >&2
        echo "  Required to verify the release archive checksum." >&2
        return 1
    fi

    if [[ ! -s "$checksums" ]]; then
        echo "Error: missing or empty release verification asset: checksums.txt" >&2
        echo "  Refusing to install an archive without a release checksum." >&2
        return 1
    fi

    if [[ ! -f "$archive_path" ]]; then
        echo "Error: archive not found at $archive_path" >&2
        return 1
    fi

    # Step 1 (recommended): authenticate checksums.txt when cosign is
    # available. Missing cosign is not an installation blocker: the exact
    # archive hash is still checked below, with an explicit warning that a
    # same-origin checksum does not authenticate the publisher.
    if [[ -n "$cosign_path" ]]; then
        local f
        for f in "$sig" "$cert"; do
            if [[ ! -s "$f" ]]; then
                echo "Error: missing or empty release verification asset: ${f##*/}" >&2
                echo "  cosign is installed, so signed verification must succeed." >&2
                echo "  Refusing to silently downgrade to checksum-only verification." >&2
                return 1
            fi
        done

        # The pinned identity ties the signature to the click-dog release
        # workflow on a release tag. Bound the call so blocked Sigstore egress
        # does not hang the installer.
        local cosign_args=(verify-blob
            --certificate "$cert"
            --signature "$sig"
            --certificate-identity-regexp "$COSIGN_IDENTITY_REGEXP"
            --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER"
            "$checksums")
        local cosign_output rc=0
        if command -v timeout &>/dev/null \
                && [[ -n "$COSIGN_VERIFY_TIMEOUT_S" && "$COSIGN_VERIFY_TIMEOUT_S" != "0" ]]; then
            cosign_output=$(timeout "$COSIGN_VERIFY_TIMEOUT_S" "$cosign_path" "${cosign_args[@]}" 2>&1) || rc=$?
        else
            cosign_output=$("$cosign_path" "${cosign_args[@]}" 2>&1) || rc=$?
        fi
        if [[ $rc -ne 0 ]]; then
            if [[ $rc -eq 124 ]]; then
                echo "Error: cosign verify-blob timed out after ${COSIGN_VERIFY_TIMEOUT_S}s" >&2
                echo "  Sigstore Rekor/Fulcio appears unreachable from this host." >&2
                echo "  Open egress to rekor.sigstore.dev and fulcio.sigstore.dev," >&2
                echo "  or pre-verify on a host with sigstore access and use -b." >&2
            else
                echo "Error: cosign verify-blob failed for checksums.txt" >&2
                echo "  Signature does not match the pinned click-dog release workflow." >&2
                echo "  Refusing to install an unverified release." >&2
            fi
            if [[ -n "$cosign_output" ]]; then
                echo "  cosign output: $cosign_output" >&2
            fi
            echo "  To retry with checksum-only verification, rerun with" >&2
            echo "  --dangerously-ignore-cosign." >&2
            return 1
        fi
        verification_mode="signed"
    else
        case "$checksum_reason" in
            ignored)
                echo "DANGER: --dangerously-ignore-cosign set; publisher authentication is disabled." >&2
                echo "  Continuing only after the release archive SHA-256 matches." >&2
                verification_mode="checksum-cosign-ignored"
                ;;
            missing)
                echo "Warning: cosign not found; continuing with checksum verification only." >&2
                echo "  This detects download corruption but does not authenticate the publisher." >&2
                echo "  Official installation docs: https://docs.sigstore.dev/cosign/system_config/installation/" >&2
                verification_mode="checksum-no-cosign"
                ;;
            *)
                echo "Error: internal verification mode is invalid: $checksum_reason" >&2
                return 1
                ;;
        esac
    fi

    # Step 2: archive entry must exist in checksums.txt.
    # awk match on exact filename (field 2) avoids any prefix/suffix tricks.
    local expected_hash
    expected_hash=$(awk -v name="$archive_name" '$2 == name {print $1; exit}' "$checksums")
    if [[ -z "$expected_hash" || ${#expected_hash} -ne 64 ]]; then
        echo "Error: $archive_name not found in release checksums.txt." >&2
        echo "  Refusing to install an archive missing from release metadata." >&2
        return 1
    fi

    # Step 3: hash the local archive and compare.
    local actual_hash
    if ! actual_hash=$(sha256_file "$archive_path"); then
        echo "Error: failed to compute SHA256 of $archive_path" >&2
        return 1
    fi
    if [[ "$actual_hash" != "$expected_hash" ]]; then
        echo "Error: archive checksum mismatch for $archive_name" >&2
        echo "  Expected: $expected_hash" >&2
        echo "  Got:      $actual_hash" >&2
        echo "  This indicates a corrupted download, MITM, or tampered release asset." >&2
        return 1
    fi

    RELEASE_VERIFICATION_MODE="$verification_mode"
    return 0
}

print_release_verification_summary() {
    local prefix="${1:-}"
    case "$RELEASE_VERIFICATION_MODE" in
        signed)
            echo "${prefix}Publisher signature: verified with cosign; archive SHA-256 matched."
            ;;
        checksum-no-cosign)
            echo "${prefix}Publisher signature: not verified (cosign unavailable); archive SHA-256 matched." >&2
            ;;
        checksum-cosign-ignored)
            echo "${prefix}DANGER: publisher signature deliberately skipped; archive SHA-256 matched." >&2
            ;;
        provided)
            echo "${prefix}Binary source: operator-provided (-b); automatic release verification skipped."
            ;;
    esac
}

# ── GitHub auth (env only, never in any process's argv) ─────────
# Resolve an optional token from the standard env vars (empty if none). Public
# releases work without one; authentication raises the GitHub API rate limit.
github_token() {
    printf '%s' "${GITHUB_TOKEN:-${GH_TOKEN:-}}"
}

# Emit a curl config snippet carrying the auth header when a token is set, else
# nothing. Consumed at each call site via `curl --config <(curl_auth_config)`,
# so the token lives in the config FD's *content* — only `/dev/fd/NN` reaches
# any process's argv, never the secret. Passing it as `-H "Authorization: …"`
# instead would expose it in curl's argv (`ps -ww`, /proc/<pid>/cmdline) for the
# duration of every request, the same leak the script avoids for the ClickHouse
# password (#230). An empty snippet (no token) is a harmless no-op for curl.
curl_auth_config() {
    local token
    token=$(github_token)
    [[ -n "$token" ]] && printf 'header = "Authorization: Bearer %s"\n' "$token"
    return 0
}

# Reject a version string that isn't X.Y.Z (optionally -prerelease) before it
# flows into download URLs and /tmp paths. Mirrors the Go ParseVersion contract
# and fails fast on a malformed/forged API response or a fat-fingered -v.
validate_version_format() {
    local v="$1"
    if [[ ! "$v" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
        echo "Error: version '$v' is not valid (expected X.Y.Z or X.Y.Z-prerelease)." >&2
        echo "  Refusing to build download URLs from an unexpected version string." >&2
        exit 1
    fi
}

# ── Build curl args (proxy + https-only; auth is added per-call via
# --config <(curl_auth_config), see above) ──────────────────────
build_curl_args() {
    # https-only, including across the asset-API -> signed-URL redirect, so a
    # token-bearing request can never be downgraded to http.
    CURL_ARGS=(-fsSL --proto '=https' --proto-redir '=https')
    if [[ -n "$HTTPS_PROXY_FLAG" ]]; then
        if [[ ! "$HTTPS_PROXY_FLAG" =~ ^https?:// ]]; then
            echo "Error: proxy URL must start with http:// or https:// (got '$HTTPS_PROXY_FLAG')" >&2
            exit 1
        fi
        CURL_ARGS+=(--proxy "$HTTPS_PROXY_FLAG")
        echo "Using proxy: $HTTPS_PROXY_FLAG"
    elif [[ -n "${HTTPS_PROXY:-}" || -n "${https_proxy:-}" || -n "${ALL_PROXY:-}" ]]; then
        echo "Using proxy from environment: ${HTTPS_PROXY:-${https_proxy:-${ALL_PROXY:-}}}"
    fi
}

# ── Resolve version (latest from GitHub if not pinned) ──────────
# The prerelease channel lists a page and picks the newest prerelease from it:
# /releases/latest excludes prereleases, and ?per_page=1 would just return the
# newest release of ANY kind (a GA if one is newer than the latest beta).
PRERELEASE_ENDPOINT="https://api.github.com/repos/coltconsulting/click-dog/releases?per_page=20"
GA_ENDPOINT="https://api.github.com/repos/coltconsulting/click-dog/releases/latest"

# Status-aware probe of a releases endpoint. Sets PROBE_RESP (body) and
# PROBE_CODE (HTTP status). Builds args explicitly WITHOUT -f (rather than
# mutating CURL_ARGS, whose exact tokenization we don't want to depend on).
# Without -f, curl returns success on a 404 and the status lands in -w output,
# so we can tell "no release yet" (404) from a network failure. Capture curl's
# exit explicitly — under `set -e` a bare `resp=$(curl ...)` aborts the whole
# script on a network/proxy error and, with stderr redirected, fails silently.
# Auth via --config <(...) keeps the token out of curl's argv (see curl_auth_config).
PROBE_RESP=""
PROBE_CODE=""
probe_release_endpoint() {
    local endpoint="$1"
    local -a probe_args=(-sSL --proto '=https' -w $'\n%{http_code}')
    [[ -n "$HTTPS_PROXY_FLAG" ]] && probe_args+=(--proxy "$HTTPS_PROXY_FLAG")
    if ! PROBE_RESP=$(curl "${probe_args[@]}" --config <(curl_auth_config) "$endpoint" 2>/dev/null); then
        echo "Error: could not reach the GitHub releases API (network or proxy failure)." >&2
        echo "If behind a proxy, use -x http://proxy:3128 or export HTTPS_PROXY" >&2
        exit 1
    fi
    PROBE_CODE="${PROBE_RESP##*$'\n'}"
}

# resolve_version [no-fallback]
#   no-fallback  Do not switch to the prerelease channel when no GA exists.
resolve_version() {
    local allow_fallback="true" fell_back="false"
    [ "${1:-}" = "no-fallback" ] && allow_fallback="false"
    # Initialize CURL_ARGS (proxy + https-only) unconditionally — the download
    # path (resolve_binary) calls resolve_version and then curls with CURL_ARGS
    # even when the version is pinned with -v, so this MUST run before the early
    # return or that curl dereferences an unset array under `set -u`.
    build_curl_args
    if [[ -n "$VERSION" ]]; then
        validate_version_format "$VERSION"
        return
    fi
    local endpoint kind
    if [[ "$PRERELEASE" == "true" ]]; then
        endpoint="$PRERELEASE_ENDPOINT"
        kind="pre-release"
    else
        endpoint="$GA_ENDPOINT"
        kind="release"
    fi
    probe_release_endpoint "$endpoint"
    local resp="$PROBE_RESP" http_code="$PROBE_CODE"
    if [[ "$PRERELEASE" == "true" ]]; then
        VERSION=$(printf '%s' "$resp" | extract_first_prerelease_tag)
    else
        VERSION=$(printf '%s' "$resp" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | sed -n '1p')
    fi
    if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
        echo "Error: GitHub rejected the release request (HTTP ${http_code})." >&2
        echo "  The request may be rate-limited, or GITHUB_TOKEN/GH_TOKEN may be invalid." >&2
        if [[ "$allow_fallback" != "true" ]]; then
            # A manifest generator has no use for -b: it needs an image tag, not
            # a local binary, so that advice just repeats the failing probe.
            echo "  Retry with a valid token, or re-run with -v VERSION." >&2
        else
            echo "  Retry with a valid token, or use -b /path/to/click-dog." >&2
        fi
        exit 1
    fi
    # No GA published yet. While Click-Dog is in public beta that is the normal
    # state, not an error: /releases/latest 404s whenever every published release
    # is a prerelease. The caller asked for "the latest", and the newest beta is
    # the latest thing there is — so fall back to it rather than dead-ending.
    # Announced on stderr so an unattended install still records the channel it
    # actually resolved.
    #
    # Gated on 404 ALONE, deliberately. An empty VERSION also results from a 5xx
    # or a malformed 200, and those mean "we could not tell", not "no GA exists":
    # switching channels there would quietly hand a beta to someone who asked for
    # GA while a GA may well be published. Every other status falls through to the
    # error paths below with its HTTP code.
    #
    # Callers that write a long-lived artifact rather than installing a binary
    # here pass `no-fallback`: baking a prerelease image tag into a Kubernetes
    # manifest or a compose file on the strength of one stderr line would break
    # the opt-in channel contract in .goreleaser.yaml, and the operator has to
    # live with that file afterwards. They get the plain "no release" error and
    # can re-run with -v or --prerelease deliberately.
    if [[ "$allow_fallback" == "true" && "$PRERELEASE" != "true" && "$http_code" == "404" ]]; then
        echo "No GA release published yet — falling back to the newest pre-release." >&2
        fell_back="true"
        kind="pre-release"
        probe_release_endpoint "$PRERELEASE_ENDPOINT"
        resp="$PROBE_RESP"
        http_code="$PROBE_CODE"
        VERSION=$(printf '%s' "$resp" | extract_first_prerelease_tag)
        # The re-probe is a second unauthenticated call against a 60/hr budget, so
        # it can be rate-limited even when the first was not. Without this the
        # 401/403 handler above has already been passed and the operator is told
        # "no release found" for what is really an auth or rate-limit failure.
        if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
            echo "Error: GitHub rejected the pre-release request (HTTP ${http_code})." >&2
            echo "  The request may be rate-limited, or GITHUB_TOKEN/GH_TOKEN may be invalid." >&2
            echo "  Retry with a valid token, or use -b /path/to/click-dog." >&2
            exit 1
        fi
    fi
    if [[ "$http_code" == "404" ]]; then
        # A no-fallback caller is a manifest generator, and it reached here
        # because no GA exists — the expected state during public beta. The
        # generic advice below is wrong for it twice over: -b supplies a local
        # binary, which a generator cannot bake into an image tag, and it omits
        # the two flags that actually work.
        if [[ "$allow_fallback" != "true" ]]; then
            # Flags to add, not a reconstructed command line: $* here is this
            # FUNCTION's arguments ("no-fallback"), not the caller's, so printing
            # "$0 $*" produced an invalid `install.sh no-fallback --prerelease`.
            echo "Error: no published click-dog stable release found (HTTP 404)." >&2
            echo "  A generated manifest is long-lived, so it is never switched to the" >&2
            echo "  pre-release channel implicitly. Re-run the command with one of:" >&2
            echo "    --prerelease   use the newest public beta" >&2
            echo "    -v VERSION     pin an exact version" >&2
            exit 1
        fi
        echo "Error: no published click-dog ${kind} found (HTTP 404)." >&2
        echo "  The release and its binaries may not be published yet." >&2
        echo "  To install from a locally built binary instead:" >&2
        echo "    sudo $0 -b /path/to/click-dog" >&2
        exit 1
    fi
    if [[ -z "$VERSION" ]]; then
        echo "Error: could not resolve the latest ${kind} version from GitHub API (HTTP ${http_code:-?})" >&2
        if [[ "$PRERELEASE" == "true" || "$fell_back" == "true" ]]; then
            # Only these paths actually read a page of releases, so only they can
            # meaningfully report an empty window.
            echo "  No pre-release found among the most recent releases; drop --prerelease for the latest GA." >&2
        fi
        echo "If behind a proxy, use -x http://proxy:3128 or export HTTPS_PROXY" >&2
        exit 1
    fi
    validate_version_format "$VERSION"
    echo "Latest ${kind}: v${VERSION}"
}

# ── Release asset download (optional GitHub authentication) ─────
# Extract the numeric GitHub asset id for a given asset name from a release JSON
# document supplied on stdin. Within an asset object the API lists "id" before
# "name", so we remember the most recent "id" and emit it when the matching
# "name" line appears. `tr ',' '\n'` splits compact JSON onto separate lines so
# this works whether the API returns pretty-printed or minified JSON. Emits
# nothing if the asset is absent. ("node_id" can't match the /"id"/ regex — the
# char before its `id` is `_`, not a quote.)
extract_asset_id() {
    local name="$1"
    tr ',' '\n' | awk -v name="$name" '
        match($0, /"id"[ \t]*:[ \t]*[0-9]+/) {
            last_id = substr($0, RSTART, RLENGTH)
            sub(/[^0-9]*/, "", last_id)
        }
        index($0, "\"name\"") && index($0, "\"" name "\"") { print last_id; exit }
    '
}

# Emit the v-stripped tag_name of the FIRST (newest) release whose "prerelease"
# is true, from a /releases list document on stdin. Within a release object
# "tag_name" precedes "prerelease", so we remember each tag and print it when
# that object's `"prerelease": true` appears. `tr ',' '\n'` handles minified
# JSON. Emits nothing if no prerelease is present in the page.
#
# awk reads to the end instead of exiting at the match, for the reason
# help_lists_flag buffers: the newest prerelease is near the top of a page far
# larger than a pipe buffer, so exiting early leaves tr and the caller's printf
# to SIGPIPE, pipefail makes the substitution 141, and set -e ends the install
# silently on the VERSION=$(...) line.
extract_first_prerelease_tag() {
    tr ',' '\n' | awk '
        found { next }
        match($0, /"tag_name"[ \t]*:[ \t]*"v?[^"]+"/) {
            t = substr($0, RSTART, RLENGTH)
            sub(/.*:[ \t]*"v?/, "", t)
            sub(/".*/, "", t)
            last_tag = t
        }
        /"prerelease"[ \t]*:[ \t]*true/ { print last_tag; found = 1 }
    '
}

# Download a single named release asset to a path. The asset API URL plus
# `Accept: application/octet-stream` returns a 302 to the signed asset URL, which
# curl follows via -L (already in CURL_ARGS' -fsSL). curl >= 7.58 drops the
# Authorization header on the cross-host redirect, so the token never reaches the
# signed URL (which carries its own query-string auth); --proto-redir '=https'
# (in CURL_ARGS) keeps that redirect https-only. Auth is supplied via
# --config <(curl_auth_config) so the token stays out of curl's argv. This is the
# path supports the same optional authentication as metadata requests and keeps
# one download flow for authenticated and anonymous installs. $1=JSON,
# $2=asset, $3=output.
download_release_asset() {
    local release_json="$1" asset_name="$2" out="$3"
    local id
    id=$(printf '%s' "$release_json" | extract_asset_id "$asset_name")
    if [[ -z "$id" ]]; then
        echo "Error: release asset '$asset_name' not found for v${VERSION}." >&2
        echo "  The release exists but is missing this artifact (or the name scheme changed)." >&2
        return 1
    fi
    curl "${CURL_ARGS[@]}" --config <(curl_auth_config) -H "Accept: application/octet-stream" -o "$out" \
        "https://api.github.com/repos/coltconsulting/click-dog/releases/assets/${id}"
}

# ── Find or download binary ────────────────────────────────────
BINARY_AUTO_DOWNLOADED="false"
BINARY_AUTO_DOWNLOAD_DIR=""

# Script-level state for the resolve_binary cleanup trap. Local variables
# aren't visible to an EXIT trap once `exit` has unwound the function's
# stack, so we keep these at script scope for the trap to reach.
_RESOLVE_BINARY_ARCHIVE_TMP=""
_RESOLVE_BINARY_VERIFY_DIR=""
_RESOLVE_BINARY_DOWNLOAD_DIR=""
# The caller's EXIT handler (a function name), which resolve_binary's own
# trap also runs and hands the trap back to, so a failed download still
# removes what the caller created. Empty when the caller has none.
OUTER_EXIT_CLEANUP=""

cleanup_auto_downloaded_binary() {
    if [[ "$BINARY_AUTO_DOWNLOADED" == "true" ]]; then
        if [[ -n "$BINARY_AUTO_DOWNLOAD_DIR" ]]; then
            rm -rf "$BINARY_AUTO_DOWNLOAD_DIR"
        elif [[ -n "$BINARY" ]]; then
            rm -f "$BINARY"
        fi
    fi
}

secure_temp_file() {
    local prefix="$1"
    local tmp
    if ! tmp=$(mktemp "${TMPDIR:-/tmp}/${prefix}.XXXXXX" 2>/dev/null); then
        echo "Error: failed to create secure temp file for ${prefix}" >&2
        exit 1
    fi
    chmod 600 "$tmp"
    printf '%s\n' "$tmp"
}

# Cleanup invoked from a single EXIT trap installed inside resolve_binary
# while a partial download / verify dir / extracted binary exists.
# Belt-and-braces: every error path inside the function still does its
# own explicit rm, so this only matters on signals (SIGINT/SIGTERM) that
# bypass the explicit paths.
_resolve_binary_cleanup() {
    if [[ -n "$_RESOLVE_BINARY_ARCHIVE_TMP" ]]; then
        rm -f "$_RESOLVE_BINARY_ARCHIVE_TMP"
    fi
    if [[ -n "$_RESOLVE_BINARY_VERIFY_DIR" ]]; then
        rm -rf "$_RESOLVE_BINARY_VERIFY_DIR"
    fi
    if [[ -n "$_RESOLVE_BINARY_DOWNLOAD_DIR" ]]; then
        rm -rf "$_RESOLVE_BINARY_DOWNLOAD_DIR"
    fi
    if [[ -n "$OUTER_EXIT_CLEANUP" ]]; then
        "$OUTER_EXIT_CLEANUP"
    fi
}

resolve_binary() {
    RELEASE_VERIFICATION_MODE=""
    if [[ -n "$BINARY" ]]; then
        if [[ ! -f "$BINARY" ]]; then
            echo "Error: binary not found at $BINARY"
            exit 1
        fi
        # Verify the binary is a Linux ELF executable
        if command -v file &>/dev/null; then
            local filetype
            filetype=$(file -b "$BINARY")
            if [[ "$filetype" != *ELF* ]]; then
                echo "Error: $BINARY is not a Linux binary" >&2
                echo "  Detected: $filetype" >&2
                echo "  Build with: make build-amd64  (or build-arm64)" >&2
                exit 1
            fi
        fi
        RELEASE_VERIFICATION_MODE="provided"
        return 0
    fi

    resolve_version
    # build_curl_args ran inside resolve_version (before its early return too),
    # so CURL_ARGS is initialized even on the pinned -v path.

    # Always download linux_<arch> regardless of host OS
    local arch
    arch=$(resolve_target_arch)

    local download_dir
    if ! download_dir=$(mktemp -d "${TMPDIR:-/tmp}/click-dog-binary.XXXXXX" 2>/dev/null); then
        echo "Error: failed to create temp dir for binary download" >&2
        exit 1
    fi
    chmod 700 "$download_dir"
    _RESOLVE_BINARY_DOWNLOAD_DIR="$download_dir"
    BINARY="$download_dir/click-dog"
    BINARY_AUTO_DOWNLOAD_DIR="$download_dir"
    BINARY_AUTO_DOWNLOADED="true"
    echo "Downloading v${VERSION} (linux/${arch})..."

    local archive_name="click-dog_${VERSION}_linux_${arch}.tar.gz"

    # Fetch the tagged release's metadata once (authenticated via the config FD
    # when a token is set) so every asset can be pulled through its API URL.
    # CURL_ARGS carries the proxy + https-only flags (set by resolve_version on
    # every path).
    local release_json
    if ! release_json=$(curl "${CURL_ARGS[@]}" --config <(curl_auth_config) -H "Accept: application/vnd.github+json" \
        "https://api.github.com/repos/coltconsulting/click-dog/releases/tags/v${VERSION}" 2>/dev/null); then
        echo "Error: could not fetch release metadata for v${VERSION}." >&2
        if [[ -z "$(github_token)" ]]; then
            echo "  Check network access, GitHub API rate limits, and that the tag exists." >&2
            echo "  Set GITHUB_TOKEN/GH_TOKEN, or pass -b /path/to/click-dog." >&2
        else
            echo "  Check the token has read access and that the tag v${VERSION} exists." >&2
        fi
        exit 1
    fi

    # Download archive
    local archive_tmp="$download_dir/$archive_name"
    _RESOLVE_BINARY_ARCHIVE_TMP="$archive_tmp"
    # Trap signals/exit while transient download + verify state exists,
    # so SIGINT/SIGTERM during the cosign call doesn't leave artifacts in
    # /tmp. Cleared on the success path below; explicit rm calls at error
    # paths are kept so behavior is identical with or without the trap.
    trap _resolve_binary_cleanup EXIT
    if ! download_release_asset "$release_json" "$archive_name" "$archive_tmp"; then
        echo "Error: download failed for $archive_name (v${VERSION})" >&2
        echo "If behind a proxy, use -x http://proxy:3128 or export HTTPS_PROXY" >&2
        rm -f "$archive_tmp"
        exit 1
    fi

    # Always fetch the checksum manifest. If cosign is available, also fetch
    # its keyless signature pair and authenticate the manifest before trusting
    # the archive hash. Missing cosign is allowed but clearly reported.
    local verify_dir
    if ! verify_dir=$(mktemp -d "$download_dir/verify.XXXXXX" 2>/dev/null); then
        echo "Error: failed to create temp dir for release verification" >&2
        rm -rf "$download_dir"
        exit 1
    fi
    _RESOLVE_BINARY_VERIFY_DIR="$verify_dir"
    local f
    if ! download_release_asset "$release_json" "checksums.txt" "$verify_dir/checksums.txt"; then
        echo "Error: failed to download checksums.txt for v${VERSION}" >&2
        echo "  Required to verify the release archive checksum." >&2
        rm -rf "$download_dir"
        exit 1
    fi

    local cosign_path=""
    local checksum_reason="missing"
    if [[ "$DANGEROUSLY_IGNORE_COSIGN" == "true" ]]; then
        checksum_reason="ignored"
    elif cosign_path=$(command -v cosign 2>/dev/null); then
        checksum_reason=""
    fi

    if [[ -n "$cosign_path" ]]; then
        for f in checksums.txt.sig checksums.txt.pem; do
            if ! download_release_asset "$release_json" "$f" "$verify_dir/$f"; then
                echo "Error: failed to download $f for v${VERSION}" >&2
                echo "  cosign is installed, so signed verification must succeed." >&2
                rm -rf "$download_dir"
                exit 1
            fi
        done
        echo "Verifying signed release checksums..."
    else
        echo "Verifying release archive checksum..."
    fi

    if ! verify_release_archive "$verify_dir" "$archive_tmp" "$archive_name" "$cosign_path" "$checksum_reason"; then
        rm -f "$archive_tmp"
        rm -rf "$verify_dir"
        exit 1
    fi
    rm -rf "$verify_dir"
    _RESOLVE_BINARY_VERIFY_DIR=""
    print_release_verification_summary

    # Extract binary
    if ! tar xzf "$archive_tmp" -C "$download_dir" click-dog; then
        echo "Error: failed to extract archive" >&2
        rm -rf "$download_dir"
        exit 1
    fi
    rm -f "$archive_tmp"
    _RESOLVE_BINARY_ARCHIVE_TMP=""
    chmod 700 "$BINARY"
    _RESOLVE_BINARY_DOWNLOAD_DIR=""
    # All transient state cleaned up; hand the trap back to the caller's
    # handler, if any, so it can install its own over BINARY/config tempfiles.
    if [[ -n "$OUTER_EXIT_CLEANUP" ]]; then
        # shellcheck disable=SC2064  # bind the handler's name now, on purpose
        trap "$OUTER_EXIT_CLEANUP" EXIT
    else
        trap - EXIT
    fi
    echo "Downloaded to $BINARY"
}

# quickstart_exit_cleanup — the guided install's EXIT handler: its config,
# secret and unit temp files, a pinned CA's temp copy, the ClickHouse setup
# files (which hold the monitoring user's password hash), and an
# auto-downloaded binary. A run that ends by telling the operator to run the setup
# SQL keeps that file and removes the rest first.
quickstart_exit_cleanup() {
    rm -f "${CONFIG_TMPFILE:-}" "${SECRET_TMPFILE:-}" "${UNIT_TMPFILE:-}" "${OTEL_CA_PIN_TMP:-}"
    if [[ -n "${clickhouse_setup_tempfiles[*]:-}" ]]; then
        rm -f "${clickhouse_setup_tempfiles[@]}"
    fi
    cleanup_auto_downloaded_binary
}

# ── INSTALL — binary + config (+ optional systemd) ─────────────
do_install() {
    # -f PATH brings your own pre-rendered YAML (typically produced by
    # `click-dog init --wizard` on a laptop). In that mode the -c collector
    # is already in the YAML, so skip the require-collector check; everything
    # else (password, systemd, binary install) is unchanged.
    if [[ -z "$CONFIG_FROM_FILE" ]]; then
        [[ -z "$COLLECTOR_ADDRESS" ]] && { echo "Error: -c collector address is required for install"; usage; }
    else
        [[ ! -f "$CONFIG_FROM_FILE" ]] && { echo "Error: -f file not found: $CONFIG_FROM_FILE" >&2; exit 1; }
        if [[ -n "$OTEL_SECURE" || -n "$OTEL_CA_CERT" ]]; then
            echo "Error: --otel-secure and --otel-ca-cert render the config, so they cannot combine" >&2
            echo "       with -f. Set 'secure' (and ca_cert) under exporters.otel in" >&2
            echo "       $CONFIG_FROM_FILE instead." >&2
            exit 1
        fi
    fi
    resolve_password

    if [[ "$(uname -s)" == "Darwin" ]]; then
        echo "Error: Local install is not supported on macOS. Use docker mode." >&2
        exit 1
    fi

    if [[ -n "$OTEL_CA_CERT" ]]; then
        validate_otel_ca_file "$OTEL_CA_CERT" || exit 1
    fi

    resolve_binary

    CONFIG_TMPFILE=$(mktemp)
    ENV_TMPFILE=$(mktemp)
    UNIT_TMPFILE=$(mktemp)
    local install_stage=""
    local staged_binary=""
    # The EXIT trap also runs after do_install returns successfully, when its
    # locals are out of scope. Default expansion keeps nounset from turning a
    # completed install into exit 1; failure paths still see the live value.
    trap 'rm -f "$CONFIG_TMPFILE" "$ENV_TMPFILE" "$UNIT_TMPFILE"; [[ -n "${install_stage:-}" ]] && rm -rf "$install_stage"; cleanup_auto_downloaded_binary' EXIT

    # Before anything is written: a receiver that cannot take this TLS choice
    # makes every later step wasted work. The binary just resolved decides
    # the transport and makes the handshake, so both are its exporter's; the
    # trap above removes a download when the gate stops the install.
    if [[ -z "$CONFIG_FROM_FILE" ]]; then
        resolve_otel_transport || exit 1
        if [[ "$OTEL_USES_TLS" == "true" ]]; then
            preflight_otel_tls || exit 1
        fi
    fi

    if [[ -n "$CONFIG_FROM_FILE" ]]; then
        cp "$CONFIG_FROM_FILE" "$CONFIG_TMPFILE"
    else
        require_unified_init_binary
        [[ -n "$OTEL_CA_CERT" ]] && require_init_flag otel-ca-cert
        render_click_dog_config "$CONFIG_TMPFILE"
    fi

    # The secret file holds the password as raw bytes (no shell quoting, no
    # KEY= prefix) — click-dog reads it verbatim via clickhouse.password_file
    # and trims one trailing newline. printf '%s' adds no newline, so the file
    # is exactly the password. It never enters the service environment.
    # If you edit this file by hand, the whole file content IS the password
    # (minus one optional trailing newline) — don't add extra lines.
    printf '%s' "$CLICKHOUSE_PASSWORD" > "$ENV_TMPFILE"

    if [[ "$SYSTEMD" == "true" ]]; then
        generate_systemd_unit > "$UNIT_TMPFILE"
    fi

    if [[ -n "$CONFIG_FROM_FILE" ]]; then
        echo "click-dog install: local, config=$CONFIG_FROM_FILE (systemd: $SYSTEMD)"
    else
        echo "click-dog install: local, collector=$COLLECTOR_ADDRESS (OTLP: $(otel_transport_label); systemd: $SYSTEMD)"
    fi

    # Step 1-3: create user + config dir
    local click_dog_group_existed="false"
    if command -v getent >/dev/null 2>&1 && getent group click-dog >/dev/null 2>&1; then
        click_dog_group_existed="true"
    fi
    if ! id click-dog &>/dev/null; then
        useradd --system --no-create-home --shell /usr/sbin/nologin click-dog
        record_installer_created_runtime_identity "$click_dog_group_existed"
    fi
    mkdir -p /etc/click-dog /var/log/click-dog
    chown click-dog:click-dog /etc/click-dog /var/log/click-dog
    chmod 750 /etc/click-dog /var/log/click-dog

    # Step 4-5: stage binary in the destination directory, smoke-test
    mkdir -p /usr/local/bin
    install_stage=$(mktemp -d /usr/local/bin/.click-dog-install.XXXXXX)
    chmod 700 "$install_stage"
    staged_binary="$install_stage/click-dog"
    cp "$BINARY" "$staged_binary"
    chmod 700 "$staged_binary"
    if ! "$staged_binary" --version >/dev/null 2>&1; then
        echo "ERROR: binary smoke-test failed" >&2
        rm -rf "$install_stage"
        install_stage=""
        exit 1
    fi

    # Step 6: place config files (and the collector CA the config names, so
    # validation below can read it; the CA it replaces is kept for step 7)
    if [[ -n "$OTEL_CA_CERT" ]]; then
        backup_otel_ca
        install_otel_ca_cert
    fi
    cp "$CONFIG_TMPFILE" /etc/click-dog/click-dog.yaml
    chown click-dog:click-dog /etc/click-dog/click-dog.yaml
    chmod 640 /etc/click-dog/click-dog.yaml
    install_secret_file "$ENV_TMPFILE"

    # Step 7: validate config against staged binary
    if ! "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "ERROR: config validation failed" >&2
        "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >&2 || true
        rm -f /etc/click-dog/click-dog.yaml /etc/click-dog/.secret
        [[ -n "$OTEL_CA_CERT" ]] && restore_otel_ca_backup
        rm -rf "$install_stage"
        install_stage=""
        exit 1
    fi
    rm -f "${OTEL_CA_CERT_PATH}.bak"

    # Step 8: place binary
    mv "$staged_binary" /usr/local/bin/click-dog
    chmod 755 /usr/local/bin/click-dog
    rmdir "$install_stage"
    install_stage=""

    # Step 9: systemd setup (if --systemd)
    if [[ "$SYSTEMD" == "true" ]]; then
        cp "$UNIT_TMPFILE" /etc/systemd/system/click-dog.service
        systemctl daemon-reload
        systemctl enable click-dog
        systemctl restart click-dog
        sleep 2
        systemctl is-active click-dog >/dev/null
    fi

    print_release_verification_summary
    echo "Install complete."
}

# ── UPDATE — binary swap (systemd installs only) ──────────────
# Config is never modified by update. New config keys ship with sane
# in-binary defaults (internal/config), so an existing YAML keeps working
# across versions without rewriting. If the user wants new defaults
# materialised into their YAML, they re-render via `click-dog init`.
do_update() {
    resolve_binary

    local update_stage=""
    local staged_binary=""
    # As with do_install, this trap can outlive the function-local staging
    # variable on a successful return.
    trap '[[ -n "${update_stage:-}" ]] && rm -rf "$update_stage"; cleanup_auto_downloaded_binary' EXIT

    echo "click-dog update: local (binary swap only — config unchanged)"

    if [[ ! -f /etc/systemd/system/click-dog.service ]]; then
        echo "ERROR: No systemd unit found. For non-systemd installs, re-run install to update." >&2
        exit 1
    fi

    # Stage new binary in the destination directory, smoke-test
    mkdir -p /usr/local/bin
    update_stage=$(mktemp -d /usr/local/bin/.click-dog-update.XXXXXX)
    chmod 700 "$update_stage"
    staged_binary="$update_stage/click-dog"
    cp "$BINARY" "$staged_binary"
    chmod 700 "$staged_binary"
    if ! "$staged_binary" --version >/dev/null 2>&1; then
        echo "ERROR: new binary failed smoke test, aborting" >&2
        rm -rf "$update_stage"
        update_stage=""
        exit 1
    fi

    # Reject downgrades. `sort -V` puts the lower version first; if that's the new one, it's a downgrade.
    local new_ver cur_ver lowest
    new_ver=$("$staged_binary" --version 2>&1 | awk '{print $2}')
    cur_ver=$(/usr/local/bin/click-dog --version 2>&1 | awk '{print $2}') || cur_ver=""
    if [[ -n "$cur_ver" && -n "$new_ver" && "$cur_ver" != "dev" && "$new_ver" != "dev" && "$cur_ver" != "$new_ver" ]]; then
        lowest=$(printf '%s\n%s\n' "${cur_ver#v}" "${new_ver#v}" | sort -V | head -n1)
        if [[ "$lowest" == "${new_ver#v}" ]]; then
            echo "ERROR: refusing to downgrade from $cur_ver to $new_ver" >&2
            rm -rf "$update_stage"
            update_stage=""
            exit 1
        fi
    fi

    # Validate existing config against staged binary before swapping.
    # Catches the case where the new binary tightens validation and the
    # on-disk config would no longer load.
    if ! "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "ERROR: existing config does not validate against new binary" >&2
        "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >&2 || true
        echo "Re-render via 'click-dog init' or fix the config, then re-run update." >&2
        rm -rf "$update_stage"
        update_stage=""
        exit 1
    fi

    # Swap binary, restart
    local was_active="false"
    systemctl is-active click-dog >/dev/null 2>&1 && was_active="true"
    [[ -f /usr/local/bin/click-dog ]] && cp /usr/local/bin/click-dog /usr/local/bin/click-dog.prev
    mv "$staged_binary" /usr/local/bin/click-dog
    chmod 755 /usr/local/bin/click-dog
    rmdir "$update_stage"
    update_stage=""
    if [[ "$was_active" != "true" ]]; then
        print_release_verification_summary
        echo "Update complete. click-dog was not running, so it stays stopped."
        echo "Start it with: systemctl start click-dog"
        return 0
    fi
    if ! restart_click_dog; then
        echo "Error: the update installed the new binary, but click-dog is not running." >&2
        echo "  Check its log: journalctl -u click-dog -n 50" >&2
        echo "  The prior binary is /usr/local/bin/click-dog.prev." >&2
        exit 1
    fi

    print_release_verification_summary
    echo "Update complete."
}

# ── STATUS — check service (systemd only) ──────────────────────
do_status() {
    echo "click-dog status: local"
    echo ""

    if [[ ! -f /etc/systemd/system/click-dog.service ]]; then
        echo "No systemd unit found. Use 'docker compose ps' or 'kubectl get pods' for non-systemd deployments."
        return
    fi
    if ! command -v click-dog &>/dev/null; then
        echo "NOT INSTALLED"
        return
    fi
    if systemctl is-active click-dog >/dev/null 2>&1; then
        local ver
        ver=$(click-dog --version 2>/dev/null || echo "unknown")
        echo "running ($ver)"
    else
        echo "STOPPED"
    fi
}

# ── UNINSTALL — compatibility delegate ──────────────────────────
do_uninstall() {
    if [[ ! -x "$CLICK_DOG_BINARY" ]]; then
        echo "Error: installed click-dog binary not found at $CLICK_DOG_BINARY." >&2
        echo "Use the deployment's orchestrator, or install a current binary before retrying." >&2
        return 1
    fi
    if ! "$CLICK_DOG_BINARY" deploy uninstall --help >/dev/null 2>&1; then
        echo "Error: the installed click-dog binary does not support 'deploy uninstall'." >&2
        echo "Update it first with: sudo bash install.sh update" >&2
        echo "Then retry with: sudo bash install.sh uninstall" >&2
        return 1
    fi

    local -a uninstall_args=()
    if [[ "$UNINSTALL_YES" == "true" ]]; then
        uninstall_args+=(--yes)
    fi
    "$CLICK_DOG_BINARY" deploy uninstall "${uninstall_args[@]+"${uninstall_args[@]}"}"
}

# ── KUBERNETES — generate templated manifests ──────────────────
do_kubernetes() {
    local out_dir="${OUTPUT_DIR:-./click-dog-k8s}"

    if [[ "$SUBCOMMAND" == "update" ]]; then
        do_kubernetes_update "$out_dir"
        return
    fi

    # `click-dog deploy kubernetes` is the single manifest generator (the old
    # parallel bash/envsubst templates are gone). It runs from the published
    # image so this works on a laptop, where a downloaded linux/<arch> binary
    # wouldn't execute — same reason `install.sh docker` renders config via
    # `docker run` (see render_click_dog_config_via_docker).
    if ! command -v docker >/dev/null 2>&1; then
        echo "Error: 'install.sh kubernetes' needs Docker — it runs 'click-dog deploy" >&2
        echo "       kubernetes' from the published image to generate the manifests." >&2
        echo "  Install Docker, or run the generator directly on a Linux host:" >&2
        echo "    click-dog deploy kubernetes -c <collector> --ch-host <ch-service> [--cluster <name>]" >&2
        exit 1
    fi
    [[ -z "$COLLECTOR_ADDRESS" ]] && { echo "Error: -c collector address is required for kubernetes"; usage; }
    if [[ -z "$CLICKHOUSE_HOST" ]]; then
        echo "Error: --ch-host is required for kubernetes — the ClickHouse Service DNS" >&2
        echo "       (e.g. clickhouse.clickhouse.svc.cluster.local). A standalone click-dog" >&2
        echo "       pod can't reach ClickHouse via localhost. Add --cluster <name> to" >&2
        echo "       run it in cluster mode." >&2
        exit 1
    fi
    # A manifest outlives this run: never switch it onto the prerelease
    # channel implicitly. See resolve_version. Resolved before the password
    # prompt so an image that cannot take --otel-secure= is refused first.
    resolve_version no-fallback
    require_otel_transport_image
    # No --otel-secure: the generator refuses a collector off loopback, in
    # its own words. On a terminal, ask instead, but only when the image's
    # automatic choice is TLS: a loopback collector (a container in the same
    # pod) is settled, and the generator renders the rule comment for it.
    if [[ -z "$OTEL_SECURE" ]] && stdin_is_tty; then
        CLICK_DOG_RUN_IMAGE="ghcr.io/coltconsulting/click-dog:${VERSION}"
        resolve_otel_transport || exit 1
        if [[ "$OTEL_USES_TLS" == "true" ]]; then
            prompt_k8s_otel_transport
        fi
    fi
    resolve_password

    mkdir -p "$out_dir"
    local abs_out_dir
    abs_out_dir=$(cd "$out_dir" && pwd)

    local args=(
        -c "$COLLECTOR_ADDRESS"
        --ch-host "$CLICKHOUSE_HOST"
        -u "$CLICKHOUSE_USER"
        -v "$VERSION"
        -o /work
    )
    [[ -n "$CLICKHOUSE_CLUSTER" ]] && args+=( --cluster "$CLICKHOUSE_CLUSTER" )
    # No handshake check, unlike install: -c names the receiver as the
    # cluster resolves it, which this host usually cannot reach. Passed only
    # when given, so the generator's refusal applies otherwise.
    [[ -n "$OTEL_SECURE" ]] && args+=( "--otel-secure=$OTEL_SECURE" )

    # Password flows through the env (read by `deploy kubernetes`), never argv.
    # A refusal (a collector off loopback with no transport) is the
    # generator's own message on stderr; stop on it explicitly, whatever the
    # caller's errexit state.
    export CLICKHOUSE_PASSWORD
    local gen_rc=0
    docker_run_mounted write \
        -e CLICKHOUSE_PASSWORD \
        -v "${abs_out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        deploy kubernetes "${args[@]}" || gen_rc=$?
    if [[ "$gen_rc" -ne 0 ]]; then
        exit "$gen_rc"
    fi

    echo "Kubernetes manifests generated in $out_dir/"
    echo "  WARNING: secret.yaml contains credentials — do not commit to version control"
    echo "  Apply with: kubectl apply -k $out_dir/"
}

do_kubernetes_update() {
    local out_dir="$1"

    [[ -z "$VERSION" ]] && { echo "Error: -v VERSION is required for kubernetes update"; usage; }
    [[ ! -d "$out_dir" ]] && { echo "Error: output directory '$out_dir' not found. Run 'kubernetes' first."; exit 1; }

    if ! command -v docker >/dev/null 2>&1; then
        echo "Error: 'install.sh kubernetes update' needs Docker — it runs" >&2
        echo "       'click-dog deploy kubernetes --update' from the published image." >&2
        exit 1
    fi

    # Validate the embedded config against the target image before bumping the
    # image tag (do_update parity). The config lives inside the configmap, so
    # extract it to a temp file first.
    if [[ -f "$out_dir/configmap.yaml" ]]; then
        local tmp_dir
        tmp_dir=$(mktemp -d)
        extract_configmap_yaml "$out_dir/configmap.yaml" > "$tmp_dir/click-dog.yaml"
        # The image's own user validates it, as the pod reads it from a
        # ConfigMap. Nothing in it is secret: configmap.yaml is 0644.
        chmod 644 "$tmp_dir/click-dog.yaml"
        if ! preflight_validate_config "$tmp_dir/click-dog.yaml" "install.sh kubernetes"; then
            rm -rf "$tmp_dir"
            exit 1
        fi
        rm -rf "$tmp_dir"
    fi

    # Delegate the image-tag bump to the generator (backs up deployment.yaml.bak
    # and rewrites the single image line) so install.sh and `click-dog deploy
    # kubernetes` stay one implementation. --update only bumps the tag, so it
    # needs neither -c nor --ch-host (the config in the ConfigMap is untouched).
    local abs_out_dir
    abs_out_dir=$(cd "$out_dir" && pwd)
    docker_run_mounted write \
        -v "${abs_out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        deploy kubernetes --update -v "${VERSION}" -o /work

    echo "Kubernetes update complete. Review changes and apply with: kubectl apply -k $out_dir/"
    echo "  Note: update only bumps the Deployment image tag (deployment.yaml.bak is the"
    echo "  backup)."
}

# ── DOCKER — generate docker-compose.yml + config ──────────────
do_docker() {
    local out_dir="${OUTPUT_DIR:-./click-dog-docker}"

    if [[ "$SUBCOMMAND" == "update" ]]; then
        do_docker_update "$out_dir"
        return
    fi

    [[ -z "$COLLECTOR_ADDRESS" ]] && { echo "Error: -c collector address is required for docker"; usage; }
    # A manifest outlives this run: never switch it onto the prerelease
    # channel implicitly. See resolve_version.
    resolve_version no-fallback

    # The image decides the transport and makes the handshake, not a host
    # binary: none runs on Docker Desktop, and the Compose service dials from
    # the image's network. An image that predates --print-otel-secure is
    # refused first, so the render below never meets a flag it lacks. All of
    # it runs before the password prompt and before anything is written.
    require_otel_transport_image
    CLICK_DOG_RUN_IMAGE="ghcr.io/coltconsulting/click-dog:${VERSION}"
    resolve_otel_transport || exit 1
    if [[ "$OTEL_USES_TLS" == "true" ]]; then
        preflight_otel_tls || exit 1
    fi
    echo "click-dog docker: collector=$COLLECTOR_ADDRESS (OTLP: $(otel_transport_label))"
    resolve_password

    mkdir -p "$out_dir"
    local abs_out_dir
    abs_out_dir=$(cd "$out_dir" && pwd)

    # docker-compose.yml, .env, and .gitignore come from `click-dog deploy docker`
    # in the image, the same generator do_kubernetes runs, so a downloaded
    # install.sh needs no files beside it. Its own messages name /work. It also
    # requires a transport for a collector off loopback and writes a
    # click-dog.yaml, which `click-dog init` replaces below. A first run removes
    # that draft, so init does not report replacing an existing file.
    #
    # Password flows through the env (read by `deploy docker`), never argv.
    export CLICKHOUSE_PASSWORD
    local had_config="false"
    if [[ -e "$out_dir/click-dog.yaml" ]]; then
        had_config="true"
    fi
    local gen_rc=0
    docker_run_mounted write \
        -e CLICKHOUSE_PASSWORD \
        -v "${abs_out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        deploy docker \
        -c "$COLLECTOR_ADDRESS" \
        -u "$CLICKHOUSE_USER" \
        -v "$VERSION" \
        -o /work \
        "--otel-secure=$OTEL_USES_TLS" > /dev/null || gen_rc=$?
    if [[ "$gen_rc" -ne 0 ]]; then
        exit "$gen_rc"
    fi
    if [[ "$had_config" != "true" ]]; then
        rm -f "$out_dir/click-dog.yaml"
    fi

    # click-dog.yaml — rendered by `click-dog init` inside the image (see
    # render_click_dog_config_via_docker for why this path can't use a
    # local binary). require_otel_transport_image above already refused an
    # image whose init would fail with "flag provided but not defined".
    render_click_dog_config_via_docker "$out_dir/click-dog.yaml"
    # `init` writes 0600, but the compose service reads this file as the
    # image's own user, not the invoker who now owns it. It holds no secret
    # (the password comes from .env).
    chmod 644 "$out_dir/click-dog.yaml"

    echo "Docker files generated in $out_dir/"
    echo "  click-dog.yaml connects to ClickHouse at localhost:9440 over TLS. Edit its"
    echo "  clickhouse section for another server or a plaintext port."
    echo "  WARNING: .env contains credentials — do not commit to version control"
    echo "  Start with: cd $out_dir && docker compose up -d"
}

do_docker_update() {
    local out_dir="$1"

    [[ -z "$VERSION" ]] && { echo "Error: -v VERSION is required for docker update"; usage; }
    [[ ! -d "$out_dir" ]] && { echo "Error: output directory '$out_dir' not found. Run 'docker' first."; exit 1; }

    # Validate the existing config against the target image before rewriting
    # the compose tag (do_update parity).
    if [[ -f "$out_dir/click-dog.yaml" ]]; then
        preflight_validate_config "$out_dir/click-dog.yaml" "install.sh docker" || exit 1
    fi

    # Backup + update image tag in docker-compose.yml
    if [[ -f "$out_dir/docker-compose.yml" ]]; then
        cp "$out_dir/docker-compose.yml" "$out_dir/docker-compose.yml.bak"
        sed -i.tmp "s|image: ghcr.io/coltconsulting/click-dog:.*|image: ghcr.io/coltconsulting/click-dog:${VERSION}|" "$out_dir/docker-compose.yml"
        rm -f "$out_dir/docker-compose.yml.tmp"
        echo "Updated image tag to ${VERSION} in docker-compose.yml"
    fi

    # click-dog.yaml in the output dir is left untouched. New keys ship with
    # sane in-binary defaults; users who want them materialised should
    # regenerate via `install.sh docker`.

    echo "Docker update complete. Apply with: cd $out_dir && docker compose up -d"
}

# ── QUICKSTART — guided single-node install ────────────────────
generate_password() {
    LC_ALL=C tr -dc 'A-Za-z0-9' < /dev/urandom 2>/dev/null | head -c 24 || true
}

# ── Span-log SQL verification ──────────────────────────────────
# The filesystem grep for "opentelemetry_span_log" under
# /etc/clickhouse-server/ is a cheap first signal, but it lies in both
# directions: the string can live in a comment, a backup, or an unused
# include (false positive), or the setting can be supplied via an include
# the grep never visits (false negative). And "restart succeeded" is not
# proof the table now exists and is being written to. So after any
# span-log setup decision we verify through ClickHouse itself when a
# client + auth are available.
#
# classify_span_log_state is the pure decision core: given (a) whether a
# usable client is available, (b) the `EXISTS TABLE` result, and (c) the
# recent-row count, it emits one status token. Keeping it argument-driven
# lets install_test.sh exercise every branch with synthetic inputs — no
# ClickHouse required.
#
# Args:
#   $1 client_available  "true" | "false"
#   $2 exists_result      ClickHouse `EXISTS TABLE ...` output: "1" | "0" | ""
#   $3 recent_count       integer row count (only meaningful when exists)
# Echoes one of: no_client | missing | empty | ok
classify_span_log_state() {
    local client_available="$1" exists_result="$2" recent_count="$3"
    if [[ "$client_available" != "true" ]]; then
        echo "no_client"
        return
    fi
    if [[ "$exists_result" != "1" ]]; then
        echo "missing"
        return
    fi
    # Table exists. Treat a non-numeric/empty count as 0 so a malformed
    # response degrades to the (safe) "empty" warning rather than a false
    # "ok".
    if [[ "$recent_count" =~ ^[0-9]+$ ]] && [[ "$recent_count" -gt 0 ]]; then
        echo "ok"
    else
        echo "empty"
    fi
}

# The two queries we ask the operator to run by hand when we can't reach
# ClickHouse ourselves. Defined once so the runtime path and the
# manual-fallback path can't drift apart.
SPAN_LOG_EXISTS_SQL="EXISTS TABLE system.opentelemetry_span_log;"
SPAN_LOG_RECENT_SQL="SELECT count() FROM system.opentelemetry_span_log WHERE finish_date >= today();"

# Print the exact SQL the operator should run by hand, plus the deferred
# verification hook. Used by the no-client branch and as a tail on the
# missing/empty branches so the guidance is always copy-pasteable.
print_span_log_manual_sql() {
    echo "    ${SPAN_LOG_EXISTS_SQL}"
    echo "    ${SPAN_LOG_RECENT_SQL}"
    echo ""
    echo "  Or let click-dog verify once config is written:"
    echo "    click-dog check"
}

# verify_span_log_via_sql runs the real queries and reports.
#
# The query runner is injected via SPAN_LOG_QUERY_RUNNER so the harness can
# stub ClickHouse: the runner is a command that takes a single SQL string
# as its last argument, prints the result on stdout, and exits nonzero if
# it cannot connect. When unset, we build one from $CLICKHOUSE_CLIENT (and
# only if that client is actually on PATH and able to answer `SELECT 1`).
#
# Returns 0 when the operator may proceed (ok, empty, or no_client — all of
# which are non-fatal here; the menu/skip already committed to a setup
# path), nonzero only on the hard "table missing" failure if the caller
# wants to gate on it. The textual report is the primary product.
#
# Echoes the classification token as its last line so callers/tests can
# branch on the outcome without re-parsing the human-readable output.
verify_span_log_via_sql() {
    local runner="${SPAN_LOG_QUERY_RUNNER:-}"
    local client_available="false"

    if [[ -n "$runner" ]]; then
        client_available="true"
    elif command -v "${CLICKHOUSE_CLIENT%% *}" &>/dev/null; then
        # Probe connectivity with the credentials we have. If SELECT 1
        # fails (e.g. default user needs a password we don't hold yet) we
        # treat the client as unavailable and fall back to manual SQL
        # rather than emitting misleading "missing/empty" verdicts from a
        # failed connection.
        if _span_log_default_runner "SELECT 1" &>/dev/null; then
            client_available="true"
            runner="_span_log_default_runner"
        fi
    fi

    local exists_result="" recent_count=""
    if [[ "$client_available" == "true" ]]; then
        # SELECT 1 succeeding doesn't guarantee these queries do — now that
        # verification runs as the monitoring user, an existing user can
        # authenticate yet lack SELECT on a system table, so the query exits
        # nonzero. Under `set -euo pipefail` a failing command substitution
        # would abort the installer before classify/report runs. Tolerate it
        # (|| ...="") and let classify_span_log_state treat the empty result as
        # missing/unknown — surfaced loudly and deferred to `click-dog check`.
        exists_result="$("$runner" "$SPAN_LOG_EXISTS_SQL" 2>/dev/null | tr -d '[:space:]')" || exists_result=""
        if [[ "$exists_result" == "1" ]]; then
            recent_count="$("$runner" "$SPAN_LOG_RECENT_SQL" 2>/dev/null | tr -d '[:space:]')" || recent_count=""
        fi
    fi

    local state
    state="$(classify_span_log_state "$client_available" "$exists_result" "$recent_count")"

    case "$state" in
        ok)
            echo "  Verified: system.opentelemetry_span_log exists and has recent spans."
            ;;
        empty)
            echo "  WARNING: system.opentelemetry_span_log exists but has no spans"
            echo "  recorded today. The table is configured, but ClickHouse has not"
            echo "  written any spans yet — click-dog will export nothing until it does."
            echo ""
            echo "  Spans are only recorded for traced queries. Run a query that takes"
            echo "  long enough to be sampled (or set the trace context), then re-check:"
            echo "    ${SPAN_LOG_RECENT_SQL}"
            ;;
        missing)
            echo "  WARNING: system.opentelemetry_span_log does NOT exist yet, or this user cannot"
            echo "  see it. ClickHouse creates it when it records its first span, so this is normal"
            echo "  until a traced query has run. If it stays missing, check the span-log config and"
            echo "  this user's GRANT: click-dog has no data to export until then. Verify with:"
            echo "    ${SPAN_LOG_EXISTS_SQL}"
            echo "    ${SPAN_LOG_RECENT_SQL}"
            echo ""
            echo "  Then re-run this installer or:"
            echo "    click-dog check"
            ;;
        no_client)
            echo "  Could not verify span logging via SQL: clickhouse-client is missing, or it"
            echo "  could not connect with these credentials. Confirm it manually by running:"
            print_span_log_manual_sql
            ;;
    esac

    echo "$state"
}

# Warn when a wrapper in -C would drop what the guide sends through it: the
# password in CLICKHOUSE_PASSWORD, forwarded by name so it stays out of host process
# arguments, and the SQL on stdin.
# _wrapper_drops_password CLIENT returns 0 when CLIENT starts with a wrapper that
# does not forward CLICKHOUSE_PASSWORD.
_wrapper_drops_password() {
    local wrapper="${1%% *}"
    wrapper="${wrapper##*/}"
    case "$wrapper" in
        docker|podman|nerdctl|kubectl|oc|sudo|doas|ssh) ;;
        *) return 1 ;;
    esac
    case " $1 " in
        *" -e CLICKHOUSE_PASSWORD "*|*" --env CLICKHOUSE_PASSWORD "*|*" --env=CLICKHOUSE_PASSWORD "*|*" -E "*|*" --preserve-env"*) return 1 ;;
    esac
}

_warn_clickhouse_client_password_forwarding() {
    local wrapper="${1%% *}"
    wrapper="${wrapper##*/}"
    if _wrapper_drops_password "$1"; then
        echo "  WARNING: -C uses ${wrapper} without forwarding CLICKHOUSE_PASSWORD, so password" >&2
        echo "           checks fail. For Docker: docker exec -i -e CLICKHOUSE_PASSWORD ... clickhouse-client" >&2
    fi
    case "$wrapper" in
        docker|podman|nerdctl)
            case " $1 " in
                *" -i "*|*" -it "*|*" -ti "*|*" --interactive "*) ;;
                *) echo "  WARNING: -C uses ${wrapper} without -i, so SQL cannot reach the client." >&2 ;;
            esac
            ;;
    esac
}

# Default query runner for the span-log check. With a password in hand it connects
# as the monitoring user, so the check proves what click-dog itself can read. With
# none, it uses the client's own auth.
_span_log_default_runner() {
    local sql="$1"
    if [[ -n "${CLICKHOUSE_PASSWORD:-}" ]]; then
        # clickhouse-client reads CLICKHOUSE_PASSWORD natively. Supplying it
        # only in the child environment keeps the secret out of /proc/*/cmdline.
        set_client_connection
        CLICKHOUSE_PASSWORD="$CLICKHOUSE_PASSWORD" "${CLIENT_CONNECTION[@]}" -u "$CLICKHOUSE_USER" -q "$sql"
        return
    fi
    # shellcheck disable=SC2086
    $CLICKHOUSE_CLIENT -q "$sql"
}

# generate_user_setup_sql USER HASH
#
# Emit the idempotent monitoring-user setup SQL on stdout. Uses
# CREATE USER IF NOT EXISTS (NOT "OR REPLACE") so re-running the installer
# never clobbers an operator-managed user's auth, profile, quota, or grants.
# GRANTs are additive and only cover the two system tables click-dog reads.
generate_user_setup_sql() {
    local user="$1" hash="$2"
    cat <<EOSQL
-- click-dog monitoring user — safe to re-run.
-- CREATE USER IF NOT EXISTS does NOT alter an existing user's auth, profile,
-- quota, settings, or grants. To (re)set this user's password, run the
-- ALTER USER statement printed separately by the installer.
-- Password hash is the SHA256 of the plaintext stored in /etc/click-dog/.secret.
CREATE USER IF NOT EXISTS ${user} IDENTIFIED WITH sha256_hash BY '${hash}';
GRANT SELECT ON system.opentelemetry_span_log TO ${user};
GRANT SELECT ON system.query_log TO ${user};
EOSQL
}

# generate_user_alter_sql USER HASH
#
# Emit ONLY the password-mutating statement. Kept separate from the setup SQL
# so the installer never resets credentials without explicit confirmation.
generate_user_alter_sql() {
    local user="$1" hash="$2"
    cat <<EOSQL
-- Resets ${user}'s password. This mutates the existing user's auth method.
ALTER USER ${user} IDENTIFIED WITH sha256_hash BY '${hash}';
EOSQL
}

# generate_user_verify_sql USER
#
# Emit probe SQL that proves the user can actually read both system tables
# click-dog needs, rather than assuming SELECT 1 connectivity implies access.
generate_user_verify_sql() {
    local user="$1"
    cat <<EOSQL
SELECT count() FROM system.opentelemetry_span_log LIMIT 1;
SELECT count() FROM system.query_log LIMIT 1;
EOSQL
}

# generate_user_setup_xml USER HASH
#
# Emit the users.d XML overlay on stdout. ClickHouse treats users.d entries
# declaratively; this defines an explicit readonly=2 profile, assigns it to the
# user, and grants only the two required tables. The built-in `readonly` profile
# is readonly=1 and does not match click-dog's session contract. A grant here
# names no grantee: ClickHouse refuses `TO user` in a config file, and the refusal
# fails the whole users config, so the server will not start with it.
generate_user_setup_xml() {
    local user="$1" hash="$2"
    cat <<USERXML
<clickhouse>
    <profiles>
        <click_dog_readonly>
            <readonly>2</readonly>
        </click_dog_readonly>
    </profiles>
    <users>
        <${user}>
            <password_sha256_hex>${hash}</password_sha256_hex>
            <profile>click_dog_readonly</profile>
            <quota>default</quota>
            <grants>
                <query>GRANT SELECT ON system.opentelemetry_span_log</query>
                <query>GRANT SELECT ON system.query_log</query>
            </grants>
        </${user}>
    </users>
</clickhouse>
USERXML
}

# user_auth_mismatch_decision USER_EXISTS CAN_AUTH
#
# Pure decision helper for the existing-user path. Prints one of:
#   create  — user does not exist; safe to CREATE USER IF NOT EXISTS
#   ok      — user exists and authenticates with the desired password
#   prompt  — user exists but cannot authenticate; auth differs, so the
#             installer must PROMPT before any ALTER USER (never silent)
# Inputs are the strings "true"/"false".
user_auth_mismatch_decision() {
    local user_exists="$1" can_auth="$2"
    if [[ "$user_exists" != "true" ]]; then
        echo "create"
    elif [[ "$can_auth" == "true" ]]; then
        echo "ok"
    else
        echo "prompt"
    fi
}

require_quickstart_tty() {
    if [[ -t 0 ]]; then
        return 0
    fi

    echo "Error: Guided install requires an interactive terminal." >&2
    echo "For non-interactive install, use:" >&2
    echo "  ./deploy/install.sh install -c collector:4317 --systemd" >&2
    return 1
}

do_quickstart() {
    # Global, not local: the EXIT handler must still see it after an exit
    # deep in this function. Every abort from here on removes these files.
    clickhouse_setup_tempfiles=()
    OUTER_EXIT_CLEANUP=quickstart_exit_cleanup
    trap quickstart_exit_cleanup EXIT

    # ── Pre-flight checks ───────────────────────────────────────
    echo ""
    echo "click-dog — ClickHouse OTEL span exporter"
    echo "==========================================="
    echo ""
    echo "Exports ClickHouse query traces to Datadog, Honeycomb, or any"
    echo "OTEL-compatible backend. Runs as a lightweight systemd sidecar."
    echo ""
    echo "The guide asks before it changes ClickHouse. It adds a read-only"
    echo "user and can turn on span logging, which restarts ClickHouse."
    echo "It shows the SQL first, and can run the SQL or leave it for later."
    echo ""
    echo "For manual install or multi-node deploy, see:"
    echo "  https://click-dog.com/install/"
    echo ""

    if [[ $EUID -ne 0 ]]; then
        echo "Error: must be run as root (sudo ./deploy/install.sh)" >&2
        exit 1
    fi

    if [[ "$(uname -s)" != "Linux" ]]; then
        echo "Error: Guided install requires Linux (for systemd)." >&2
        echo "" >&2
        echo "Alternatives:" >&2
        echo "  Docker:     ./deploy/install.sh docker -c collector:4317" >&2
        echo "  Kubernetes: ./deploy/install.sh kubernetes -c collector:4317" >&2
        echo "  Multi-node: deploy/ansible/playbook.yaml" >&2
        exit 1
    fi

    if ! require_quickstart_tty; then
        exit 1
    fi

    # ── Detect existing installation ──────────────────────────
    # Note: detection only checks /usr/local/bin/click-dog; custom paths
    # from a prior -b install won't be detected here.
    local existing_version=""
    local existing_was_active="false"
    local existing_was_enabled="false"
    if [[ -f /usr/local/bin/click-dog ]]; then
        existing_version=$(/usr/local/bin/click-dog --version 2>/dev/null | awk '{print $NF}') || true
        systemctl is-active click-dog >/dev/null 2>&1 && existing_was_active="true"
        systemctl is-enabled click-dog >/dev/null 2>&1 && existing_was_enabled="true"

        echo ""
        echo "════════════════════════════════════════════════════════════"
        echo "  Existing installation detected"
        echo "════════════════════════════════════════════════════════════"
        echo ""
        echo "  Binary:  /usr/local/bin/click-dog (${existing_version:-unknown version})"
        [[ -f /etc/click-dog/click-dog.yaml ]] && echo "  Config:  /etc/click-dog/click-dog.yaml"
        [[ -f /etc/click-dog/.secret ]]        && echo "  Secret:  /etc/click-dog/.secret"
        if [[ "$existing_was_active" == "true" ]]; then
            echo "  Service: running"
        elif [[ "$existing_was_enabled" == "true" ]]; then
            echo "  Service: stopped (enabled)"
        else
            echo "  Service: stopped (disabled)"
        fi
        echo ""
        echo "  [u] Update — replace binary, keep existing config and credentials"
        if [[ -n "$CLICKHOUSE_PASSWORD" ]]; then
            echo "  [r] Reinstall — overwrite config and service; CLICKHOUSE_PASSWORD replaces the saved password"
        else
            echo "  [r] Reinstall — overwrite config and service; keep existing credentials"
        fi
        echo "  [q] Quit"
        echo ""
        while true; do
            read -rp "  Choice [u/r/q]: " existing_choice
            case "$existing_choice" in
                [uU])
                    echo ""
                    echo "  Updating binary. Config and credentials will not change."
                    resolve_binary
                    trap 'cleanup_auto_downloaded_binary' EXIT
                    echo "  [1/3] Validating binary..."
                    local staged_binary
                    staged_binary=$(mktemp /tmp/click-dog-quickstart-update.XXXXXX)
                    cp "$BINARY" "$staged_binary"
                    chmod 755 "$staged_binary"
                    if ! "$staged_binary" --version >/dev/null 2>&1; then
                        echo "  Error: binary smoke-test failed (wrong architecture?)" >&2
                        echo "  Build with: make build-amd64  (or build-arm64)" >&2
                        rm -f "$staged_binary"
                        exit 1
                    fi
                    local new_version lowest
                    new_version=$("$staged_binary" --version 2>/dev/null | awk '{print $NF}') || true
                    if [[ -n "$existing_version" && -n "$new_version" && "$existing_version" != "dev" && "$new_version" != "dev" && "$existing_version" != "$new_version" ]]; then
                        lowest=$(printf '%s\n%s\n' "${existing_version#v}" "${new_version#v}" | sort -V | head -n1)
                        if [[ "$lowest" == "${new_version#v}" ]]; then
                            echo "  Error: refusing to downgrade from $existing_version to $new_version" >&2
                            rm -f "$staged_binary"
                            exit 1
                        fi
                    fi
                    if [[ -f /etc/click-dog/click-dog.yaml ]] && ! "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
                        echo "  Error: existing config does not validate against new binary" >&2
                        "$staged_binary" validate --config /etc/click-dog/click-dog.yaml >&2 || true
                        echo "  Fix /etc/click-dog/click-dog.yaml, then re-run update." >&2
                        rm -f "$staged_binary"
                        exit 1
                    fi
                    echo "  [2/3] Installing binary..."
                    [[ -f /usr/local/bin/click-dog ]] && cp /usr/local/bin/click-dog /usr/local/bin/click-dog.prev
                    systemctl stop click-dog 2>/dev/null || true
                    mv "$staged_binary" /usr/local/bin/click-dog
                    chmod 755 /usr/local/bin/click-dog

                    # Only update the systemd unit if it differs from what we'd generate.
                    # This preserves any user customizations (ExecStartPre, LimitNOFILE, etc.)
                    local unit_tmp
                    unit_tmp=$(mktemp)
                    generate_systemd_unit > "$unit_tmp"
                    if [[ ! -f /etc/systemd/system/click-dog.service ]]; then
                        echo "  [3/3] Installing systemd unit..."
                        cp "$unit_tmp" /etc/systemd/system/click-dog.service
                        systemctl daemon-reload
                    elif ! diff -q "$unit_tmp" /etc/systemd/system/click-dog.service >/dev/null 2>&1; then
                        echo "  [3/3] Systemd unit differs — keeping existing (customizations preserved)."
                        echo "        Reinstall writes the latest default unit."
                    else
                        echo "  [3/3] Systemd unit unchanged."
                    fi
                    rm -f "$unit_tmp"

                    # Only restart if the service was previously running.
                    # Don't resurrect a stopped/disabled service.
                    echo ""
                    local update_ok="true"
                    if [[ "$existing_was_active" == "true" ]]; then
                        echo "  Restarting service..."
                        if restart_click_dog; then
                            echo "  Updated: ${existing_version:-unknown} → ${new_version:-unknown}"
                            echo "  click-dog is running."
                        else
                            update_ok="false"
                            echo "  Binary updated but service did not start."
                            echo "  Check logs: journalctl -u click-dog -f"
                            echo "  The prior binary is /usr/local/bin/click-dog.prev."
                        fi
                    else
                        echo "  Updated: ${existing_version:-unknown} → ${new_version:-unknown}"
                        echo "  Service was not running before update — leaving stopped."
                        echo "  Start with: systemctl start click-dog"
                    fi
                    print_release_verification_summary "  "
                    echo ""
                    [[ "$update_ok" == "true" ]] || exit 1
                    return
                    ;;
                [rR])
                    echo ""
                    echo "  Proceeding with reinstall — config and service will be rewritten."
                    if [[ -n "$CLICKHOUSE_PASSWORD" ]]; then
                        echo "  CLICKHOUSE_PASSWORD replaces the password in /etc/click-dog/.secret."
                    else
                        echo "  Existing credentials in /etc/click-dog/.secret are kept so the"
                        echo "  ClickHouse user keeps authenticating; delete that file first to rotate."
                    fi
                    break
                    ;;
                [qQ])
                    echo "  Aborted."
                    exit 0
                    ;;
                *)
                    echo "  Please enter u, r, or q."
                    ;;
            esac
        done
    fi

    # ── Overview ────────────────────────────────────────────────
    echo "Steps:"
    echo ""
    echo "  1. ClickHouse      — enable span logging, create a read-only user"
    echo "                       (SELECT only; the SQL shows first), then verify"
    echo "                       spans are readable as that user"
    echo "  2. OTEL collector  — confirm where to send traces, and whether"
    echo "                       over TLS"
    echo "  3. Install         — download binary to /usr/local/bin/click-dog,"
    echo "                       configure in /etc/click-dog/, start systemd service"
    echo ""
    echo "Step 1 can change ClickHouse. Step 3 installs click-dog. Each change asks first."
    echo ""
    read -rp "Press Enter to begin..."

    # ═══ Step 1: ClickHouse ═════════════════════════════════════
    # Gather and verify everything ClickHouse-side first — span logging, TLS,
    # and the monitoring user — so the span-log check that closes this step can
    # run as the user click-dog will actually connect as. The collector (Step 2)
    # has no bearing on these checks, so it no longer leads the flow.
    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo "  Step 1/3 — ClickHouse"
    echo "════════════════════════════════════════════════════════════"

    # A later restart of ClickHouse depends on these files.
    check_clickhouse_files || exit 1
    # Step 3 writes CLICKHOUSE_PASSWORD to .secret, so check it before [w] can
    # restart ClickHouse.
    if [[ -n "$CLICKHOUSE_PASSWORD" ]]; then
        check_monitor_password_from_env || exit 1
    fi

    # Check OTEL span logging — config file check, no auth needed
    echo ""
    echo -n "  Checking ClickHouse OTEL span logging... "
    local otel_enabled="false"
    if grep -rql "opentelemetry_span_log" /etc/clickhouse-server/ 2>/dev/null; then
        echo "enabled"
        otel_enabled="true"
    else
        echo "not configured"
    fi
    if [[ "$otel_enabled" == "false" ]]; then
        echo ""
        echo "  Click-dog needs system.opentelemetry_span_log to exist."
        echo "  This requires a config file and a ClickHouse restart."
        echo ""

        # Detect config format: YAML if any .yaml/.yml exist, otherwise XML
        local otel_cfg_file otel_cfg_ext
        if ls /etc/clickhouse-server/config.d/*.yaml /etc/clickhouse-server/config.d/*.yml 2>/dev/null | head -1 &>/dev/null; then
            otel_cfg_ext="yaml"
            otel_cfg_file=$(secure_temp_file "click-dog-opentelemetry")
            clickhouse_setup_tempfiles+=("$otel_cfg_file")
            cat > "$otel_cfg_file" <<'OTELYAML'
opentelemetry_span_log:
    engine: |
        ENGINE = MergeTree
        PARTITION BY toYYYYMM(finish_date)
        ORDER BY (finish_date, finish_time_us, trace_id)
    database: system
    table: opentelemetry_span_log
    flush_interval_milliseconds: 7500
OTELYAML
        else
            otel_cfg_ext="xml"
            otel_cfg_file=$(secure_temp_file "click-dog-opentelemetry")
            clickhouse_setup_tempfiles+=("$otel_cfg_file")
            cat > "$otel_cfg_file" <<'OTELXML'
<clickhouse>
    <opentelemetry_span_log>
        <engine>
            ENGINE = MergeTree
            PARTITION BY toYYYYMM(finish_date)
            ORDER BY (finish_date, finish_time_us, trace_id)
        </engine>
        <database>system</database>
        <table>opentelemetry_span_log</table>
        <flush_interval_milliseconds>7500</flush_interval_milliseconds>
    </opentelemetry_span_log>
</clickhouse>
OTELXML
        fi

        echo "  [w] Write config + restart ClickHouse now"
        echo "  [m] I'll do it myself"
        echo "  [s] Skip — it's already enabled elsewhere"
        echo ""
        while true; do
            read -rp "  Choice [w/m/s]: " otel_choice
            case "$otel_choice" in
                [wW])
                    local ch_config_dir="/etc/clickhouse-server/config.d"
                    if [[ ! -d "$ch_config_dir" ]]; then
                        echo "  ${ch_config_dir} not found."
                        echo "  Copy manually:"
                        echo "    $(clickhouse_file_install_cmd) ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}"
                        echo ""
                        read -rp "  Press Enter once done..."
                    else
                        echo ""
                        echo "  Writing to ${ch_config_dir}/opentelemetry.${otel_cfg_ext}..."
                        # shellcheck disable=SC2046 # the command's words
                        $(clickhouse_file_install_cmd) "$otel_cfg_file" "${ch_config_dir}/opentelemetry.${otel_cfg_ext}"
                        echo -n "  Restarting ClickHouse... "
                        if systemctl restart clickhouse-server 2>/dev/null; then
                            sleep 3
                            echo "done"
                        else
                            echo "FAILED"
                            echo "  Restart clickhouse-server manually. If it will not start, remove"
                            echo "  ${ch_config_dir}/opentelemetry.${otel_cfg_ext} and check its log."
                            echo ""
                            read -rp "  Press Enter once done..."
                        fi
                    fi
                    otel_enabled="true"
                    break
                    ;;
                [mM])
                    echo ""
                    echo "  Config saved to: ${otel_cfg_file}"
                    echo "    $(clickhouse_file_install_cmd) ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}"
                    echo "    systemctl restart clickhouse-server"
                    echo ""
                    read -rp "  Press Enter once done..."
                    otel_enabled="true"
                    break
                    ;;
                [sS])
                    echo "  Skipped."
                    otel_enabled="true"
                    break
                    ;;
                *)
                    echo "  Please enter w, m, or s."
                    ;;
            esac
        done
    fi

    # ── Detect ClickHouse TLS ──────────────────────────────────
    # Pin CLICKHOUSE_PORT to whichever port the probe actually checked.
    # Without this, the openssl branch confirms TLS on 9000 but the
    # rendered config silently flips to 9440 (click-dog init's
    # --ch-secure default), pointing the monitor at an endpoint nobody
    # tested. Pin per branch so the rendered config matches the proof.
    echo ""
    echo -n "  Checking ClickHouse TLS on localhost:9000... "
    if command -v openssl &>/dev/null; then
        if echo | openssl s_client -connect localhost:9000 -servername localhost 2>&1 | grep -q "BEGIN CERTIFICATE"; then
            echo "TLS enabled"
            CLICKHOUSE_TLS="true"
            CLICKHOUSE_PORT="9000"
        else
            echo "plaintext"
            CLICKHOUSE_TLS="false"
            CLICKHOUSE_PORT="9000"
        fi
    elif echo | timeout 2 bash -c "cat < /dev/tcp/localhost/9440" 2>/dev/null; then
        echo "TLS (port 9440 open)"
        CLICKHOUSE_TLS="true"
        CLICKHOUSE_PORT="9440"
    else
        echo "plaintext (assuming no TLS)"
        CLICKHOUSE_TLS="false"
        CLICKHOUSE_PORT="9000"
    fi

    if [[ "$CLICKHOUSE_TLS" == "false" ]]; then
        echo "  → Will configure click-dog for plaintext connections"
    fi

    # ── ClickHouse monitoring user ─────────────────────────────
    # A sub-section of Step 1 (not its own step): we set up the read-only
    # monitoring user here, then verify span logging as that user below.
    echo ""
    echo "  ── Monitoring user ──"

    if [[ -n "$CLICKHOUSE_PASSWORD" ]]; then
        # Password provided via CLICKHOUSE_PASSWORD env — skip user creation
        echo ""
        echo "  Using existing credentials (user: ${CLICKHOUSE_USER}, password from env)"
        echo "  Skipping ClickHouse user setup."
        _warn_clickhouse_client_password_forwarding "$CLICKHOUSE_CLIENT"
    else
        setup_monitoring_user
    fi

    # Confirm span logging through ClickHouse as the user click-dog connects as.
    # verify_span_log_via_sql authenticates with CLICKHOUSE_USER and the password
    # (via _span_log_default_runner), so this proves the export path can read spans.
    # We don't hard-fail (the operator may be enabling it elsewhere, or set the
    # user up by hand); a missing/empty table is surfaced loudly and deferred to
    # `click-dog check`. If the user couldn't be created, the probe falls back to
    # printing the manual SQL.
    echo ""
    echo "  Verifying span logging as ${CLICKHOUSE_USER}..."
    # verify_span_log_via_sql prints the human-readable report and emits the
    # classification token as its last line. Capture once, drop the trailing
    # token from the displayed report (it's for machine/test consumption).
    local span_log_report
    span_log_report="$(verify_span_log_via_sql)"
    echo "${span_log_report%$'\n'*}"

    # The collector check below runs click-dog's own exporter TLS code, so
    # fetch (or check) the release now. It stays in a private temp directory,
    # removed by quickstart_exit_cleanup, until step 3 installs it.
    if [[ -z "$BINARY" ]]; then
        echo ""
        echo "  Fetching click-dog now: the collector check in step 2 runs it."
    fi
    resolve_binary

    # ═══ Step 2: OTEL collector ════════════════════════════════
    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo "  Step 2/3 — OTEL collector"
    echo "════════════════════════════════════════════════════════════"
    echo ""
    if [[ -n "$COLLECTOR_ADDRESS" ]]; then
        echo "  Using collector: ${COLLECTOR_ADDRESS} (from -c flag)"
    else
        read -rp "  Collector address [localhost:4317]: " COLLECTOR_ADDRESS
        COLLECTOR_ADDRESS="${COLLECTOR_ADDRESS:-localhost:4317}"
    fi

    local col_host col_port
    col_host=$(otel_collector_host)
    col_port=$(otel_collector_port)

    guided_otel_transport

    echo -n "  Checking $(otel_hostport "$col_host" "$col_port")... "
    if timeout 3 bash -c "echo >/dev/tcp/${col_host}/${col_port}" 2>/dev/null; then
        echo "ok"
        # Prove a TLS answer, automatic or explicit, with one handshake, so a
        # wrong choice surfaces here rather than as export errors after
        # install. A plaintext answer stays a reachability check: a
        # ClientHello sent to a plaintext gRPC port only adds a "bogus
        # greeting" error to the receiver's log.
        if [[ "$OTEL_USES_TLS" == "true" ]]; then
            confirm_otel_tls_choice "$col_host" "$col_port"
        fi
    else
        echo "FAILED"
        echo ""
        echo "  Cannot reach ${COLLECTOR_ADDRESS}."
        echo "  Check that the OTEL collector or Datadog Agent is running."
        echo ""
        while true; do
            read -rp "  Continue anyway? [y/n] " col_confirm
            case "$col_confirm" in
                [yY]) break ;;
                [nN]) echo "Aborted."; exit 0 ;;
                *)    echo "  Please enter y or n." ;;
            esac
        done
    fi

    # ═══ Step 3: Install ═══════════════════════════════════════
    echo ""
    echo "════════════════════════════════════════════════════════════"
    echo "  Step 3/3 — Install"
    echo "════════════════════════════════════════════════════════════"
    echo ""
    echo "  This will create:"
    echo "    /usr/local/bin/click-dog          binary"
    echo "    /etc/click-dog/click-dog.yaml     config"
    echo "    /etc/click-dog/.secret            credentials (600)"
    echo "    click-dog.service                 systemd unit"
    echo ""
    echo "  All settings can be changed later by editing the config."
    echo "  To remove Click-Dog later: sudo click-dog deploy uninstall"
    echo ""
    while true; do
        read -rp "  Continue? [y/n] " confirm
        case "$confirm" in
            [yY]) break ;;
            [nN]) echo "Aborted."; exit 0 ;;
            *)    echo "  Please enter y or n." ;;
        esac
    done
    echo ""

    # The binary was fetched before step 2; quickstart_exit_cleanup still
    # owns the EXIT trap and now covers these temp files too.
    CONFIG_TMPFILE=""
    SECRET_TMPFILE=""
    UNIT_TMPFILE=""

    # System user + dirs
    local click_dog_group_existed="false"
    if command -v getent >/dev/null 2>&1 && getent group click-dog >/dev/null 2>&1; then
        click_dog_group_existed="true"
    fi
    if id click-dog &>/dev/null 2>&1; then
        echo "  [1/4] Using existing click-dog system account..."
    else
        echo "  [1/4] Creating click-dog system account (for running the service)..."
        useradd --system --no-create-home --shell /usr/sbin/nologin click-dog
        record_installer_created_runtime_identity "$click_dog_group_existed"
    fi
    mkdir -p /etc/click-dog /var/log/click-dog
    chown click-dog:click-dog /etc/click-dog /var/log/click-dog
    chmod 750 /etc/click-dog /var/log/click-dog

    # Config + secret
    echo "  [2/4] Writing config..."
    SYSTEMD="true"
    CONFIG_TMPFILE=$(mktemp)
    SECRET_TMPFILE=$(mktemp)
    UNIT_TMPFILE=$(mktemp)

    require_unified_init_binary
    if [[ -n "$OTEL_CA_CERT" ]]; then
        require_init_flag otel-ca-cert
        # A reinstall replaces the CA the previous config names; keep it so a
        # failure below restores a config that still verifies its collector.
        backup_otel_ca
        install_otel_ca_cert
        # A pin was proven minutes ago against the same bytes, so a failure
        # here is real; an operator's CA the check already failed, and the
        # operator chose to keep, only gets the report.
        local verify_rc=0
        verify_installed_otel_ca "$col_host" "$col_port" || verify_rc=$?
        # A CA click-dog cannot use stops the install even when the receiver
        # was unreachable and the operator chose to go on: the service could
        # not start with it.
        if [[ "$verify_rc" -eq 2 ]]; then
            echo "  Error: click-dog cannot use ${OTEL_CA_CERT_PATH}; the configuration was not written." >&2
            restore_otel_ca_backup
            exit 1
        fi
        if [[ "$verify_rc" -ne 0 && -n "$OTEL_CA_PIN_TMP" ]]; then
            echo "  Error: the pinned certificate no longer verifies the collector." >&2
            restore_otel_ca_backup
            exit 1
        fi
    fi
    render_click_dog_config "$CONFIG_TMPFILE"

    # Raw password bytes (no shell quoting, no KEY= prefix) — click-dog reads
    # the file verbatim via clickhouse.password_file. See the install path.
    printf '%s' "$CLICKHOUSE_PASSWORD" > "$SECRET_TMPFILE"

    # Back up existing config files before overwriting so we can restore on failure
    local config_backed_up="false"
    if [[ -f /etc/click-dog/click-dog.yaml ]]; then
        cp -p /etc/click-dog/click-dog.yaml /etc/click-dog/click-dog.yaml.bak
        config_backed_up="true"
    fi
    if [[ -f /etc/click-dog/.secret ]]; then
        cp -p /etc/click-dog/.secret /etc/click-dog/.secret.bak
    fi

    cp "$CONFIG_TMPFILE" /etc/click-dog/click-dog.yaml
    chown click-dog:click-dog /etc/click-dog/click-dog.yaml
    chmod 640 /etc/click-dog/click-dog.yaml
    install_secret_file "$SECRET_TMPFILE"

    # Validate — restore backups on failure
    if ! "$BINARY" validate --config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "  Error: config validation failed" >&2
        "$BINARY" validate --config /etc/click-dog/click-dog.yaml >&2 || true
        # Restore previous config if we had one, otherwise clean up
        if [[ "$config_backed_up" == "true" ]]; then
            echo "  Restoring previous config..." >&2
            mv /etc/click-dog/click-dog.yaml.bak /etc/click-dog/click-dog.yaml 2>/dev/null || true
        else
            rm -f /etc/click-dog/click-dog.yaml
        fi
        # Step 1 may have saved a password ClickHouse now holds.
        if [[ -f /etc/click-dog/.secret.bak ]]; then
            mv /etc/click-dog/.secret.bak /etc/click-dog/.secret
        else
            rm -f /etc/click-dog/.secret
        fi
        restore_otel_ca_backup
        exit 1
    fi

    # Validation passed — remove backups
    rm -f /etc/click-dog/click-dog.yaml.bak /etc/click-dog/.secret.bak "${OTEL_CA_CERT_PATH}.bak"

    # Binary + systemd
    echo "  [3/4] Installing binary and systemd unit..."
    chmod 755 "$BINARY"
    if ! "$BINARY" --version >/dev/null 2>&1; then
        echo "  Error: binary smoke-test failed (wrong architecture?)" >&2
        echo "  Build with: make build-amd64  (or build-arm64)" >&2
        exit 1
    fi
    systemctl stop click-dog 2>/dev/null || true
    cp "$BINARY" /usr/local/bin/click-dog
    chmod 755 /usr/local/bin/click-dog

    generate_systemd_unit > "$UNIT_TMPFILE"
    cp "$UNIT_TMPFILE" /etc/systemd/system/click-dog.service
    systemctl daemon-reload
    systemctl enable click-dog >/dev/null 2>&1

    # Start
    echo "  [4/4] Starting click-dog..."
    local service_up="false"
    restart_click_dog && service_up="true"

    # ═══ Done ══════════════════════════════════════════════════
    echo ""
    echo "════════════════════════════════════════════════════════════"
    if [[ "$service_up" == "true" ]]; then
        if ((${#clickhouse_setup_tempfiles[@]} > 0)); then
            rm -f "${clickhouse_setup_tempfiles[@]}"
        fi
        echo "  click-dog is running."
        print_release_verification_summary "  "
        echo "════════════════════════════════════════════════════════════"
        echo ""
        local docs_url="https://click-dog.com"
        local installed_version
        installed_version=$(/usr/local/bin/click-dog --version 2>/dev/null | awk '{print $NF}') || true
        local v="${installed_version:+"?v=${installed_version}"}"
        echo "  Next steps:"
        echo ""
        echo "  1. Create click-dog dashboards in Datadog"
        echo "     (creates three: Application Query Analysis,"
        echo "      Exported User Activity, and Health)"
        echo ""
        echo "     DD_API_KEY=... DD_APP_KEY=... click-dog create-dashboards"
        echo ""
        echo "  2. Watch click-dog export spans"
        echo ""
        echo "     journalctl -u click-dog -f"
        echo ""
        echo "  3. View traces in Datadog APM > Traces (service: click-dog-monitor)"
        echo ""
        echo "  Customize:"
        echo "    sudo vi /etc/click-dog/click-dog.yaml && sudo systemctl restart click-dog"
        echo "    Full reference: ${docs_url}/configuration/${v}"
        echo "    Datadog guide: ${docs_url}/integrations/datadog/${v}"
    else
        # The steps below name the setup SQL. Keep it past exit, and only it: these
        # files hold the password hash.
        local temp_file
        if ((${#clickhouse_setup_tempfiles[@]} > 0)); then
            for temp_file in "${clickhouse_setup_tempfiles[@]}"; do
                if [[ "$temp_file" != "${MONITOR_SETUP_SQL:-}" ]]; then
                    rm -f "$temp_file"
                fi
            done
        fi
        clickhouse_setup_tempfiles=()
        systemctl stop click-dog 2>/dev/null || true
        echo "  Installed, but not yet running."
        echo "════════════════════════════════════════════════════════════"
        echo ""
        echo "  click-dog did not start. It needs a working ClickHouse user, most"
        echo "  likely '${CLICKHOUSE_USER}'."
        echo ""
        echo "  To finish setup:"
        if [[ "${MONITOR_PASSWORD_KEPT:-}" == "true" ]]; then
            echo "    1. Write the password of '${CLICKHOUSE_USER}' to /etc/click-dog/.secret."
        elif [[ -n "${MONITOR_SETUP_SQL:-}" ]]; then
            echo "    1. Create the ClickHouse user, if it does not exist, as an administrator:"
            echo "       clickhouse-client --multiquery < ${MONITOR_SETUP_SQL}"
        else
            echo "    1. Check that '${CLICKHOUSE_USER}' logs in with the password in /etc/click-dog/.secret"
            echo "       and can read system.opentelemetry_span_log and system.query_log."
        fi
        echo "    2. Start the service:"
        echo "       systemctl restart click-dog"
        echo ""
        echo "  Config:  /etc/click-dog/click-dog.yaml"
        echo "  Logs:    journalctl -u click-dog -f"
    fi
    echo ""
}

# ── Monitoring user ────────────────────────────────────────────
# The guided install's ClickHouse user setup. A password saved in .secret shows
# the user exists, but its absence shows nothing: `click-dog deploy uninstall`
# deletes the file and leaves the user.

# guide_read VARIABLE PROMPT reads one answer. A closed stdin stops the guide,
# because a menu that retried it would never end.
guide_read() {
    if ! read -rp "$2" "$1"; then
        echo "" >&2
        echo "  Error: stdin closed before an answer." >&2
        exit 1
    fi
}

# guide_read_secret VARIABLE PROMPT reads a password exactly as typed, unechoed.
guide_read_secret() {
    if ! IFS= read -rsp "$2" "$1"; then
        echo "" >&2
        echo "  Error: stdin closed before an answer." >&2
        exit 1
    fi
    echo ""
}

guide_pause() {
    # shellcheck disable=SC2034 # guide_read sets it, and nothing reads it
    local pause_answer
    guide_read pause_answer "  Press Enter once done..."
}

# add_client_words WORD... adds to both client connections.
add_client_words() {
    CLIENT_CONNECTION+=("$@")
    CLIENT_CONNECTION_NO_CONFIG+=("$@")
}

# add_client_host REPLACEMENT HOST WORD... adds -C's host flag, unless a replacement
# host takes its place, and notes when it names another server.
add_client_host() {
    local replacement="$1" host="$2"
    shift 2
    if [[ -n "$replacement" ]]; then
        return 0
    fi
    case "$host" in
        localhost|127.0.0.1|::1) ;;
        *) CLIENT_IS_LOCAL="false" ;;
    esac
    add_client_words "$@"
}

# set_client_connection [HOST [PORT]] sets CLIENT_CONNECTION to $CLICKHOUSE_CLIENT
# with no user or password of its own, for connecting as a chosen user:
# clickhouse-client refuses a repeated --user. HOST and PORT replace any in -C,
# because the client uses the first --host it sees. Words before the client belong
# to a wrapper and stay.
#
# CLIENT_CONNECTION_NO_CONFIG is the same connection without -C's config flags. A
# password in the client's config beats CLICKHOUSE_PASSWORD, so a refused login
# tries once more that way: see monitor_user_authenticates.
set_client_connection() {
    local host="${1:-}" port="${2:-}" words word base i start=0
    # shellcheck disable=SC2206 # split like every other use of the variable
    words=($CLICKHOUSE_CLIENT)
    for ((i = 0; i < ${#words[@]}; i++)); do
        base="${words[i]##*/}"
        if [[ "$base" == "clickhouse-client" || ("$base" == "clickhouse" && "${words[i + 1]:-}" == "client") ]]; then
            start=$i
            break
        fi
    done
    CLIENT_CONNECTION=("${words[@]:0:$((start + 1))}")
    CLIENT_CONNECTION_NO_CONFIG=("${CLIENT_CONNECTION[@]}")
    CLIENT_IS_LOCAL="true"
    if ((start > 0)); then
        CLIENT_IS_LOCAL="false"
    fi
    i=$((start + 1))
    while ((i < ${#words[@]})); do
        word="${words[i]}"
        i=$((i + 1))
        case "$word" in
            -u|--user) i=$((i + 1)) ;;
            --password)
                # With no value, --password makes the client prompt.
                if [[ "${words[i]:-}" != -* ]]; then
                    i=$((i + 1))
                fi
                ;;
            -u*|--user=*|--password=*|--ask-password) ;;
            -c|-C|--config|--config-file)
                CLIENT_CONNECTION+=("$word" "${words[i]:-}")
                i=$((i + 1))
                ;;
            -c*|-C*|--config=*|--config-file=*) CLIENT_CONNECTION+=("$word") ;;
            --connection)
                CLIENT_IS_LOCAL="false"
                CLIENT_CONNECTION+=("$word" "${words[i]:-}")
                i=$((i + 1))
                ;;
            --connection=*)
                CLIENT_IS_LOCAL="false"
                CLIENT_CONNECTION+=("$word")
                ;;
            -h|--host)
                add_client_host "$host" "${words[i]:-}" "$word" "${words[i]:-}"
                i=$((i + 1))
                ;;
            --host=*) add_client_host "$host" "${word#--host=}" "$word" ;;
            -h*) add_client_host "$host" "${word#-h}" "$word" ;;
            --port)
                if [[ -z "$port" ]]; then
                    add_client_words "$word" "${words[i]:-}"
                fi
                i=$((i + 1))
                ;;
            --port=*)
                if [[ -z "$port" ]]; then
                    add_client_words "$word"
                fi
                ;;
            *) add_client_words "$word" ;;
        esac
    done
    if [[ -n "$host" ]]; then
        add_client_host "" "$host" --host "$host"
    fi
    if [[ -n "$port" ]]; then
        add_client_words --port "$port"
    fi
    if [[ "${CLIENT_USES_EMPTY_CONFIG:-}" == "true" && "$CLIENT_IS_LOCAL" == "true" ]]; then
        CLIENT_CONNECTION=("${CLIENT_CONNECTION_NO_CONFIG[@]}" --config-file "$CLIENT_EMPTY_CONFIG")
    fi
}

# make_client_empty_config sets CLIENT_EMPTY_CONFIG to a private config file with
# nothing in it.
make_client_empty_config() {
    if [[ -n "${CLIENT_EMPTY_CONFIG:-}" ]]; then
        return 0
    fi
    local file
    file=$(secure_temp_file "click-dog-client-config")
    # clickhouse-client picks the parser from the extension.
    mv "$file" "${file}.xml"
    echo "<config></config>" > "${file}.xml"
    clickhouse_setup_tempfiles+=("${file}.xml")
    CLIENT_EMPTY_CONFIG="${file}.xml"
}

# monitor_user_login PASSWORD CONNECTION_WORD...
#
# The password stays in the child's environment, out of the process list. A
# wrapper in -C must forward it: `docker exec -i -e CLICKHOUSE_PASSWORD`.
monitor_user_login() {
    local password="$1"
    shift
    CLICKHOUSE_PASSWORD="$password" "$@" -u "$CLICKHOUSE_USER" -q "SELECT 1" &>/dev/null
}

# monitor_user_authenticates PASSWORD returns 0 when the monitoring user logs in on
# CLIENT_CONNECTION, 1 when ClickHouse refuses the password, and 2 when the client
# gets no answer. After a refusal, a client that reaches this host tries once with
# an empty config, and keeps using it if that works. That login can drop the
# config's TLS, so it stays on this host.
monitor_user_authenticates() {
    local rc=0
    monitor_user_login "$1" "${CLIENT_CONNECTION[@]}" || rc=$?
    case "$rc" in
        0) return 0 ;;
        "$CLICKHOUSE_CLIENT_AUTH_FAILED") ;;
        *) return 2 ;;
    esac
    if [[ "$CLIENT_IS_LOCAL" == "true" && "${CLIENT_USES_EMPTY_CONFIG:-}" != "true" ]]; then
        make_client_empty_config
        if monitor_user_login "$1" "${CLIENT_CONNECTION_NO_CONFIG[@]}" --config-file "$CLIENT_EMPTY_CONFIG"; then
            CLIENT_USES_EMPTY_CONFIG="true"
            CLIENT_CONNECTION=("${CLIENT_CONNECTION_NO_CONFIG[@]}" --config-file "$CLIENT_EMPTY_CONFIG")
            return 0
        fi
    fi
    return 1
}

# monitor_user_xml_file prints the first config file, other than the guide's own,
# that names the monitoring user. A profile or quota can share the name, so this
# counts only when ClickHouse cannot answer, or to say where a user it reports is.
monitor_user_xml_file() {
    local file dir="${CLICKHOUSE_USERS_DIR%/*}"
    while IFS= read -r file; do
        if [[ "$file" != "${CLICKHOUSE_USERS_DIR}/click-dog-${CLICKHOUSE_USER}.xml" ]] \
            && grep -Eqs "<${CLICKHOUSE_USER}[[:space:]>]|^[[:space:]]*[\"']?${CLICKHOUSE_USER}[\"']?:" "$file"; then
            echo "$file"
            return 0
        fi
    done < <(
        printf '%s\n' "$dir/users.xml" "$dir/users.yaml" "$dir/users.yml"
        clickhouse_loaded_files "$CLICKHOUSE_USERS_DIR" "$dir/conf.d"
    )
}

# monitoring_user_server_state prints where the user is defined, ignoring the
# guide's own users.d file:
#   xml      XML config
#   sql      SQL-managed: ALTER USER works
#   absent   no such user
#   unknown  the client could not read system.users
monitoring_user_server_state() {
    local storages
    # The type, because an operator can rename the storage itself.
    # shellcheck disable=SC2086
    if storages=$($CLICKHOUSE_CLIENT --param_monitor_user="$CLICKHOUSE_USER" \
        -q "SELECT d.type FROM system.users AS u INNER JOIN system.user_directories AS d ON u.storage = d.name WHERE u.name = {monitor_user:String}" 2>/dev/null); then
        # The same name can be in both storages. The XML definition wins.
        case "$storages" in
            "") echo "absent" ;;
            *users_xml*) echo "xml" ;;
            *) echo "sql" ;;
        esac
    elif [[ -n "$(monitor_user_xml_file)" ]]; then
        echo "xml"
    else
        echo "unknown"
    fi
}

# monitoring_user_state adds the guide's own users.d file to the server's answer:
#   overlay     the file defines the user. ClickHouse loaded it, or cannot say
#   not_loaded  the file exists on the server this client reaches, and ClickHouse
#               still does not report the user from XML after polling users.d
#   xml         another file defines the user. The guide cannot change it: a
#               second definition stops ClickHouse at its next start
monitoring_user_state() {
    local state polls=0
    state=$(monitoring_user_server_state)
    if [[ ! -f "${CLICKHOUSE_USERS_DIR}/click-dog-${CLICKHOUSE_USER}.xml" || "$CLIENT_IS_LOCAL" != "true" ]]; then
        echo "$state"
        return 0
    fi
    while [[ "$state" != "xml" && "$state" != "unknown" ]] && ((polls < 8)); do
        sleep 1
        state=$(monitoring_user_server_state)
        polls=$((polls + 1))
    done
    case "$state" in
        xml|unknown) echo "overlay" ;;
        *) echo "not_loaded" ;;
    esac
}

# offer_to_delete_monitor_user_file FILE asks about a file [x] just wrote.
offer_to_delete_monitor_user_file() {
    local answer
    echo "  ClickHouse does not start with a users.d file it cannot load."
    while true; do
        guide_read answer "  Delete $1? [y/n]: "
        case "$answer" in
            [yY]*)
                rm -f "$1"
                echo "  Deleted."
                return 0
                ;;
            [nN]*)
                echo "  Kept. Fix or delete it before ClickHouse next restarts."
                return 0
                ;;
            *) echo "  Please enter y or n." ;;
        esac
    done
}

# print_xml_user_note says what to do about a user another file defines.
print_xml_user_note() {
    local file
    file=$(monitor_user_xml_file)
    echo "  ${file:-The ClickHouse XML config} defines '${CLICKHOUSE_USER}'."
    echo "  The guide leaves that file alone: a second definition of this user stops"
    echo "  ClickHouse at its next start. To use this user, add these lines to its"
    echo "  definition there:"
    echo "    <grants>"
    echo "        <query>GRANT SELECT ON system.opentelemetry_span_log</query>"
    echo "        <query>GRANT SELECT ON system.query_log</query>"
    echo "    </grants>"
}

# monitor_password_hash PASSWORD — the SHA-256 both the SQL and the XML carry.
monitor_password_hash() {
    if command -v sha256sum &>/dev/null; then
        printf '%s' "$1" | sha256sum | awk '{print $1}'
    elif command -v shasum &>/dev/null; then
        printf '%s' "$1" | shasum -a 256 | awk '{print $1}'
    fi
}

# write_monitor_user_files PASSWORD SETUP_SQL ALTER_SQL USER_XML
write_monitor_user_files() {
    local hash
    hash=$(monitor_password_hash "$1")
    generate_user_setup_sql "$CLICKHOUSE_USER" "$hash" > "$2"
    generate_user_alter_sql "$CLICKHOUSE_USER" "$hash" > "$3"
    generate_user_setup_xml "$CLICKHOUSE_USER" "$hash" > "$4"
}

# save_monitor_password PASSWORD keeps the password the guide just set in
# ClickHouse, so stopping before step 3 does not lose it. On a first install the
# service user does not exist yet, and step 3 sets the owner.
save_monitor_password() {
    mkdir -p "${SECRET_FILE%/*}"
    (umask 077 && printf '%s' "$1" > "$SECRET_FILE")
    chown click-dog:click-dog "$SECRET_FILE" 2>/dev/null || true
    chmod 600 "$SECRET_FILE"
    echo "  Password saved to ${SECRET_FILE}."
}

# prompt_existing_monitor_user ALLOW_NEW
#
# The user exists and does not authenticate with the password in hand, which
# CREATE USER IF NOT EXISTS cannot fix. Sets MONITOR_USER_CHOICE to entered, new,
# or keep; "entered" leaves a password that authenticated in
# MONITOR_USER_PASSWORD. ALLOW_NEW is false when the guide cannot change the user.
prompt_existing_monitor_user() {
    local allow_new="$1" choices="e/k" choice entered
    MONITOR_USER_CHOICE="keep"
    MONITOR_USER_PASSWORD=""
    echo "  [e] Enter its current password"
    if [[ "$allow_new" == "true" ]]; then
        choices="e/n/k"
        echo "  [n] Set a new password and save it in /etc/click-dog/.secret. Anything else"
        echo "      using the current password stops working."
    fi
    echo "  [k] Keep it unchanged and fix the password by hand"
    echo ""
    while true; do
        guide_read choice "  Choice [${choices}]: "
        case "$choice" in
            [eE])
                guide_read_secret entered "  Current password for '${CLICKHOUSE_USER}': "
                if [[ -n "$entered" ]] && monitor_user_authenticates "$entered"; then
                    echo "  It authenticates."
                    MONITOR_USER_CHOICE="entered"
                    MONITOR_USER_PASSWORD="$entered"
                    return 0
                fi
                echo "  That password did not authenticate."
                ;;
            [nN])
                if [[ "$allow_new" == "true" ]]; then
                    MONITOR_USER_CHOICE="new"
                    return 0
                fi
                echo "  Please enter one of ${choices}."
                ;;
            [kK]) return 0 ;;
            *) echo "  Please enter one of ${choices}." ;;
        esac
    done
}

print_monitor_password_kept_note() {
    echo "  Leaving '${CLICKHOUSE_USER}' unchanged. click-dog cannot connect until"
    echo "  /etc/click-dog/.secret holds this user's password: after the install, write it"
    echo "  there and restart click-dog."
}

# check_monitor_password_from_env settles the connection the span-log check uses,
# and returns 1 when ClickHouse refuses CLICKHOUSE_PASSWORD: step 3 would replace a
# working password with it.
check_monitor_password_from_env() {
    local rc=0
    set_client_connection
    monitor_user_authenticates "$CLICKHOUSE_PASSWORD" || rc=$?
    if ((rc == 1)) && ! _wrapper_drops_password "$CLICKHOUSE_CLIENT"; then
        echo "" >&2
        echo "  Error: ClickHouse refused user '${CLICKHOUSE_USER}' with CLICKHOUSE_PASSWORD." >&2
        echo "  A password in the client's config beats CLICKHOUSE_PASSWORD, so check that" >&2
        echo "  config too." >&2
        echo "  /etc/click-dog/.secret is unchanged. Set the correct password, or unset" >&2
        echo "  CLICKHOUSE_PASSWORD so the guide sets up the user, then run the guide again." >&2
        return 1
    fi
}

# run_sql_file FILE CONNECTION_WORD... sends FILE on stdin and checks that a last
# statement came back. A wrapper without stdin (docker exec with no -i) runs
# nothing and exits 0.
SQL_NOT_RUN_MESSAGE="The SQL did not reach the client. A wrapper must pass stdin, as docker exec -i does."
# clickhouse-client's exit status when the server refuses a login.
CLICKHOUSE_CLIENT_AUTH_FAILED=4
run_sql_file() {
    local file="$1" output
    shift
    output=$("$@" --multiquery < <(cat "$file"; echo "SELECT 'click-dog-sql-ran';")) || return
    if [[ "$output" != *click-dog-sql-ran* ]]; then
        echo "$SQL_NOT_RUN_MESSAGE" >&2
        return 1
    fi
}

# run_setup_sql FILE runs FILE on the connection in SQL_ADMIN_CLIENT, as the
# admin. SQL_ADMIN_PASSWORD is unset when the client carries its own auth.
run_setup_sql() {
    if [[ -n "${SQL_ADMIN_PASSWORD+set}" ]]; then
        CLICKHOUSE_PASSWORD="$SQL_ADMIN_PASSWORD" run_sql_file "$1" "${SQL_ADMIN_CLIENT[@]}"
    else
        run_sql_file "$1" "${SQL_ADMIN_CLIENT[@]}"
    fi
}

# wait_for_monitor_user PASSWORD
#
# ClickHouse polls users.d, so an overlay takes effect a few seconds after it is
# written.
wait_for_monitor_user() {
    local attempt
    for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
        if monitor_user_authenticates "$1"; then
            return 0
        fi
        sleep 1
    done
    return 1
}

# generate_monitor_password prints a new password, or fails when it cannot make one.
generate_monitor_password() {
    local password
    password=$(generate_password)
    if ((${#password} < 16)); then
        echo "Error: failed to generate password" >&2
        return 1
    fi
    printf '%s' "$password"
}

# setup_monitoring_user creates or adopts the read-only ClickHouse user and
# leaves its password in CLICKHOUSE_PASSWORD for the rest of the guide.
setup_monitoring_user() {
    local mon_pass="" password_source="generated" sql_choice="" kept_pass="" kept_source=""
    # Whether the operator confirmed a new password for an existing user.
    local AUTH_RESET_CONFIRMED="false"
    # Whether ClickHouse now holds, or the operator is told to give it, mon_pass.
    local password_applied="false"
    # Reuse existing password on re-runs so it stays in sync with ClickHouse.
    # The secret file holds the raw password (read by click-dog via
    # clickhouse.password_file); $(cat) strips any trailing newline.
    if [[ -f "$SECRET_FILE" ]]; then
        mon_pass=$(cat "$SECRET_FILE")
        if [[ -n "$mon_pass" ]]; then
            password_source="secret"
        fi
    fi
    if [[ -z "$mon_pass" ]]; then
        mon_pass=$(generate_monitor_password) || exit 1
    fi
    CLICKHOUSE_PASSWORD="$mon_pass"
    _warn_clickhouse_client_password_forwarding "$CLICKHOUSE_CLIENT"

    # Idempotent setup SQL: CREATE USER IF NOT EXISTS, never OR REPLACE. The
    # ALTER that changes a password is a separate file, used only after the
    # operator confirms it.
    local setup_sql alter_sql verify_sql user_xml
    setup_sql=$(secure_temp_file "click-dog-setup")
    alter_sql=$(secure_temp_file "click-dog-alter-user")
    verify_sql=$(secure_temp_file "click-dog-verify")
    user_xml=$(secure_temp_file "click-dog-user")
    clickhouse_setup_tempfiles+=("$setup_sql" "$alter_sql" "$verify_sql" "$user_xml")
    MONITOR_SETUP_SQL="$setup_sql"
    write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
    generate_user_verify_sql "$CLICKHOUSE_USER" > "$verify_sql"

    echo ""
    # ── Probe the existing user before touching anything ─────────────
    # With clickhouse-client the guide learns three things: whether the user
    # exists, whether it authenticates with this password, and whether it can
    # read the two system tables. user_auth_mismatch_decision then decides.
    set_client_connection
    local user_can_auth="false" probed="false"
    # shellcheck disable=SC2086
    if command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
        local auth_rc=0
        monitor_user_authenticates "$mon_pass" || auth_rc=$?
        case "$auth_rc" in
            0)
                probed="true"
                user_can_auth="true"
                ;;
            1) probed="true" ;;
            *)
                echo "  The ClickHouse client could not connect, so the guide cannot check '${CLICKHOUSE_USER}'."
                echo "  Check that ClickHouse is running and that -C reaches it."
                echo ""
                ;;
        esac
    fi

    local ch_users_file="${CLICKHOUSE_USERS_DIR}/click-dog-${CLICKHOUSE_USER}.xml"
    local user_state user_exists="false"
    user_state=$(monitoring_user_state)
    if [[ "$user_state" == "not_loaded" ]]; then
        echo "  WARNING: ClickHouse does not report '${CLICKHOUSE_USER}' from ${ch_users_file}."
        echo "  ClickHouse does not start with a users.d file it cannot load. Check that file and"
        echo "  the ClickHouse server log before ClickHouse next restarts."
        echo ""
        user_state=$(monitoring_user_server_state)
    fi
    # SQL cannot change a user defined in XML, so no later step may offer it.
    if [[ "$user_state" == "overlay" || "$user_state" == "xml" ]]; then
        MONITOR_SETUP_SQL=""
    fi

    # ClickHouse answers when this client may read system.users. Otherwise a
    # stored password is the only evidence, and a user may exist that nothing
    # here knows about: [r] checks after its SQL runs, and [x] asks first.
    if [[ "$password_source" == "secret" ]]; then
        user_exists="true"
    fi
    case "$user_state" in
        overlay|xml|sql) user_exists="true" ;;
        absent) user_exists="false" ;;
    esac
    local auth_decision
    auth_decision=$(user_auth_mismatch_decision "$user_exists" "$user_can_auth")
    # With no client there is nothing to ask: offer the create/grant flow.
    if [[ "$probed" != "true" ]]; then
        auth_decision="create"
    fi

    if [[ "$auth_decision" == "prompt" ]]; then
        if [[ "$password_source" == "secret" ]]; then
            echo "  ClickHouse user '${CLICKHOUSE_USER}' did NOT authenticate with the password"
            echo "  stored in /etc/click-dog/.secret."
            if [[ "$CLIENT_IS_LOCAL" != "true" ]]; then
                echo "  A password in the client's config beats the saved one, so the saved"
                echo "  password can be right. Check that config before choosing [n]."
            fi
        else
            echo "  ClickHouse user '${CLICKHOUSE_USER}' already exists, and this host has no"
            echo "  saved password for it. 'click-dog deploy uninstall' removes"
            echo "  /etc/click-dog/.secret and leaves the ClickHouse user."
        fi
        local allow_new="true"
        if [[ "$user_state" == "xml" ]]; then
            allow_new="false"
            local xml_file
            xml_file=$(monitor_user_xml_file)
            echo "  ${xml_file:-The ClickHouse XML config} defines it, so the guide cannot set a new password."
        fi
        echo ""
        prompt_existing_monitor_user "$allow_new"
        case "$MONITOR_USER_CHOICE" in
            entered)
                mon_pass="$MONITOR_USER_PASSWORD"
                CLICKHOUSE_PASSWORD="$mon_pass"
                password_source="entered"
                write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
                auth_decision="ok"
                ;;
            new)
                # [s] puts these back: the new password reaches ClickHouse only
                # through [x] or [r].
                kept_pass="$mon_pass"
                kept_source="$password_source"
                mon_pass=$(generate_monitor_password) || exit 1
                CLICKHOUSE_PASSWORD="$mon_pass"
                password_source="generated"
                write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
                echo ""
                if [[ "$user_state" == "overlay" ]]; then
                    echo "  [x] below writes the new password to ${ch_users_file}."
                else
                    echo "  [r] below runs:"
                    echo "  ──────────────────────────────────────────────────────"
                    cat "$alter_sql"
                    echo "  ──────────────────────────────────────────────────────"
                    cat "$alter_sql" >> "$setup_sql"
                fi
                AUTH_RESET_CONFIRMED="true"
                ;;
            keep)
                print_monitor_password_kept_note
                MONITOR_PASSWORD_KEPT="true"
                MONITOR_SETUP_SQL=""
                # Nothing is left to run: SQL cannot change an XML user, and the
                # overlay would replace the password just kept.
                if [[ "$user_state" == "overlay" || "$user_state" == "xml" ]]; then
                    sql_choice="s"
                fi
                ;;
        esac
        echo ""
    fi

    if [[ "$auth_decision" == "ok" ]]; then
        # SELECT 1 says nothing about the two tables, so read them.
        local verify_err=""
        if verify_err=$(CLICKHOUSE_PASSWORD="$mon_pass" run_sql_file "$verify_sql" "${CLIENT_CONNECTION[@]}" -u "$CLICKHOUSE_USER" 2>&1 >/dev/null); then
            echo "  ClickHouse user '${CLICKHOUSE_USER}' authenticates and can read"
            echo "  system.opentelemetry_span_log and system.query_log — nothing to do."
            sql_choice="s"
        elif [[ "$verify_err" == *UNKNOWN_TABLE* ]]; then
            # ClickHouse creates these tables when it first writes to them, and a
            # missing table hides whether the user may read it.
            local missing_table="system.opentelemetry_span_log" created_when="records its first span"
            if [[ "$verify_err" == *query_log* ]]; then
                missing_table="system.query_log"
                created_when="logs its first query"
            fi
            echo "  ClickHouse user '${CLICKHOUSE_USER}' authenticates. The guide cannot check its"
            echo "  access to ${missing_table} yet."
            echo "  ClickHouse creates that table when it ${created_when}."
            echo "  Running the setup again adds the GRANTs if they are missing."
        elif [[ "$verify_err" == *"$SQL_NOT_RUN_MESSAGE"* ]]; then
            echo "  ClickHouse user '${CLICKHOUSE_USER}' authenticates, but the guide could not"
            echo "  check its access. ${SQL_NOT_RUN_MESSAGE}"
        else
            echo "  ClickHouse user '${CLICKHOUSE_USER}' authenticates but is MISSING SELECT on"
            echo "  one of the required system tables."
            case "$user_state" in
                overlay) echo "  [x] rewrites ${ch_users_file} with the two GRANTs click-dog needs." ;;
                xml) ;;
                *) echo "  The setup SQL adds the two GRANTs click-dog needs and changes nothing else." ;;
            esac
        fi
    fi

    if [[ "$sql_choice" == "s" ]]; then
        return 0
    fi

    if [[ "$user_state" == "xml" ]]; then
        print_xml_user_note
        return 0
    fi

    if [[ "$password_source" == "secret" ]]; then
        echo "  Reusing password from /etc/click-dog/.secret"
        echo "  (the ClickHouse user must use the same password to connect)"
    elif [[ "$auth_decision" == "create" ]]; then
        echo "  This sets up a read-only ClickHouse user '${CLICKHOUSE_USER}':"
        echo "    • CREATE USER IF NOT EXISTS leaves an existing user of this name as it is:"
        echo "      its password, profile, quota, settings, and grants stay."
        echo "    • GRANT SELECT on system.opentelemetry_span_log and system.query_log."
        echo "      click-dog needs nothing else."
        echo ""
        if [[ "$user_state" == "absent" ]]; then
            echo "  The guide sets the same password in ClickHouse and saves it to"
            echo "  /etc/click-dog/.secret. click-dog connects only when both match."
        else
            echo "  The guide saves the generated password to /etc/click-dog/.secret. This client"
            echo "  cannot read system.users, so the user may already exist with another password."
            echo "  [r] checks that after its SQL runs. [x] asks first, because it replaces a user."
        fi
        echo ""
        echo "  To change it later, update both: the ClickHouse user password and"
        echo "  /etc/click-dog/.secret, then restart click-dog."
    fi
    echo ""

    local offer_sql="true" offer_xml="true"
    case "$user_state" in
        # Every statement fails on a user defined in XML.
        overlay) offer_sql="false" ;;
        # An overlay hides a SQL user, taking its settings and grants with it.
        sql) offer_xml="false" ;;
    esac
    # The overlay sets the password, which the operator just declined to change.
    if [[ "$auth_decision" == "prompt" && "$AUTH_RESET_CONFIRMED" != "true" ]]; then
        offer_xml="false"
    fi
    if [[ "$offer_sql" == "true" ]]; then
        echo "  Manual SQL (review before running anywhere):"
        echo "  ──────────────────────────────────────────────────────"
        cat "$setup_sql"
        echo "  ──────────────────────────────────────────────────────"
        echo "  Saved to: ${setup_sql}"
        echo "  Run with: clickhouse-client --multiquery < ${setup_sql}"
        echo ""
    fi
    local menu_choices="s"
    [[ "$offer_sql" == "true" ]] && menu_choices="r/${menu_choices}"
    [[ "$offer_xml" == "true" ]] && menu_choices="x/${menu_choices}"
    if [[ "$offer_xml" == "true" ]]; then
        echo "  [x] Write XML config (no clickhouse-client needed)"
        if [[ -f "$ch_users_file" ]]; then
            echo "      This replaces the whole of ${ch_users_file}: its password and anything else in it."
        fi
    fi
    if [[ "$offer_sql" == "true" ]]; then
        echo "  [r] Run SQL via clickhouse-client (asks for the command if needed)"
    fi
    echo "  [s] Skip: set up '${CLICKHOUSE_USER}' by hand"
    echo ""
    while true; do
        guide_read sql_choice "  Choice [${menu_choices}]: "
        case "$sql_choice" in
            [xX])
                if [[ "$offer_xml" != "true" ]]; then
                    echo "  [x] is not available for this user. Choose one of ${menu_choices}."
                    continue
                fi
                if [[ "$user_state" == "unknown" ]]; then
                    local replace_answer
                    echo ""
                    echo "  This client cannot read system.users, so '${CLICKHOUSE_USER}' may already exist."
                    echo "  [x] replaces that user's password and settings."
                    guide_read replace_answer "  Write the file anyway? [y/n]: "
                    case "$replace_answer" in
                        [yY]*) ;;
                        *)
                            echo "  Choose one of ${menu_choices}."
                            continue
                            ;;
                    esac
                fi
                echo ""
                # SQL cannot change a user this file defines.
                MONITOR_SETUP_SQL=""
                if [[ -d "$CLICKHOUSE_USERS_DIR" ]]; then
                    # shellcheck disable=SC2046 # the command's words
                    $(clickhouse_file_install_cmd) "$user_xml" "$ch_users_file"
                    echo "  Written to: ${ch_users_file}"
                    # Saved before the wait, so stopping during it keeps the password.
                    save_monitor_password "$mon_pass"
                else
                    echo "  ${CLICKHOUSE_USERS_DIR} not found."
                    echo ""
                    echo "  Copy this file manually:"
                    echo "    $(clickhouse_file_install_cmd) ${user_xml} /etc/clickhouse-server/users.d/click-dog-${CLICKHOUSE_USER}.xml"
                    echo ""
                    save_monitor_password "$mon_pass"
                    guide_pause
                fi
                if [[ "$probed" == "true" ]]; then
                    echo -n "  Waiting for ClickHouse to load it... "
                    if wait_for_monitor_user "$mon_pass"; then
                        echo "ok"
                    else
                        echo "FAILED"
                        echo "  '${CLICKHOUSE_USER}' does not authenticate with the new password."
                        echo "  Check the ClickHouse server log: click-dog cannot connect until it does."
                        if [[ -f "$ch_users_file" ]]; then
                            offer_to_delete_monitor_user_file "$ch_users_file"
                        fi
                    fi
                fi
                break
                ;;
            [rR])
                if [[ "$offer_sql" != "true" ]]; then
                    echo "  [r] is not available: SQL cannot change a user defined in XML."
                    continue
                fi
                echo ""
                # shellcheck disable=SC2086
                if ! command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
                    echo "  The clickhouse-client command was not found."
                    echo ""
                    local user_client
                    guide_read user_client "  clickhouse-client command: "
                    [[ -n "$user_client" ]] && CLICKHOUSE_CLIENT="$user_client"
                    # shellcheck disable=SC2086
                    if ! command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
                        echo ""
                        echo "  Still not found. SQL saved to: ${setup_sql}"
                        echo "  ──────────────────────────────────────────────────────"
                        cat "$setup_sql"
                        echo "  ──────────────────────────────────────────────────────"
                        echo ""
                        echo "  Run with: clickhouse-client --multiquery < ${setup_sql}"
                        echo ""
                        guide_pause
                        password_applied="true"
                        break
                    fi
                    set_client_connection
                fi
                echo "  Running setup SQL..."
                # shellcheck disable=SC2206 # split like every other use
                SQL_ADMIN_CLIENT=($CLICKHOUSE_CLIENT)
                unset SQL_ADMIN_PASSWORD
                local run_ok="false" run_err=""
                if run_err=$(run_setup_sql "$setup_sql" 2>&1 >/dev/null); then
                    echo "  Done."
                    run_ok="true"
                elif [[ "$run_err" == *"$SQL_NOT_RUN_MESSAGE"* ]]; then
                    # Connection details cannot help a wrapper with no stdin.
                    echo "  ${SQL_NOT_RUN_MESSAGE}"
                else
                    # The bare client failed, usually because the admin needs a
                    # host, port, or password. Ask for those and retry. They are
                    # used only for this SQL and never written to disk.
                    echo "  Could not run it with the client command:"
                    head -n 2 <<<"$run_err" | sed 's/^/    /'
                    echo ""
                    echo "  The admin user likely needs a host, port, or password. Enter connection"
                    echo "  details to retry (used only to run this SQL; not saved)."
                    echo ""
                    local attempt
                    for attempt in 1 2; do
                        local ch_addr ch_admin ch_admin_pass
                        guide_read ch_addr "  ClickHouse host[:port] [${CLICKHOUSE_HOST:-localhost}]: "
                        ch_addr="${ch_addr:-${CLICKHOUSE_HOST:-localhost}}"
                        guide_read ch_admin "  Admin user [default]: "
                        ch_admin="${ch_admin:-default}"
                        guide_read_secret ch_admin_pass "  Admin password (blank if none): "
                        # Flags from -C survive: "docker exec ... clickhouse-client",
                        # --secure, and so on. The typed host and port replace -C's.
                        # IPv6 literals like [::1]:9000 aren't parsed (the host:port
                        # split is naive): use a hostname/IPv4, or put the full
                        # connection in -C.
                        local ch_port=""
                        if [[ "$ch_addr" == *:* ]]; then
                            ch_port="${ch_addr##*:}"
                        fi
                        set_client_connection "${ch_addr%%:*}" "$ch_port"
                        SQL_ADMIN_CLIENT=("${CLIENT_CONNECTION[@]}" -u "$ch_admin")
                        echo "  Retrying as '${ch_admin}' on '${ch_addr}' (attempt ${attempt}/2)..."
                        # The password goes in CLICKHOUSE_PASSWORD for this command
                        # only, so it stays out of the process list.
                        SQL_ADMIN_PASSWORD="$ch_admin_pass"
                        local retry_rc=0
                        run_err=$(run_setup_sql "$setup_sql" 2>&1 >/dev/null) || retry_rc=$?
                        if [[ "$retry_rc" == "0" ]]; then
                            echo "  Done."
                            run_ok="true"
                            break
                        fi
                        echo "  That did not work:"
                        head -n 2 <<<"$run_err" | sed 's/^/    /'
                        if [[ "$retry_rc" == "$CLICKHOUSE_CLIENT_AUTH_FAILED" ]]; then
                            echo "  If the client's config holds a password, it beats the one typed here."
                        fi
                    done
                fi
                if [[ "$run_ok" != "true" ]]; then
                    echo ""
                    echo "  Run the SQL manually:"
                    echo "  ──────────────────────────────────────────────────────"
                    cat "$setup_sql"
                    echo "  ──────────────────────────────────────────────────────"
                    echo ""
                    echo "  Saved to: ${setup_sql}"
                    echo "  Run with: CLICKHOUSE_PASSWORD=... clickhouse-client --host HOST -u ADMIN --multiquery < ${setup_sql}"
                    echo ""
                    guide_pause
                    password_applied="true"
                    break
                fi
                # The SQL succeeding proves little: CREATE USER IF NOT EXISTS
                # leaves an existing user's password alone. Declining a new
                # password above already accepted that outcome.
                if [[ "$auth_decision" == "prompt" && "$AUTH_RESET_CONFIRMED" != "true" ]]; then
                    break
                fi
                if monitor_user_authenticates "$mon_pass"; then
                    password_applied="true"
                else
                    echo ""
                    echo "  The SQL ran, but '${CLICKHOUSE_USER}' does not authenticate with the password"
                    echo "  this install holds. Most likely the user already existed: CREATE USER IF NOT"
                    echo "  EXISTS leaves its password alone."
                    echo ""
                    prompt_existing_monitor_user "true"
                    case "$MONITOR_USER_CHOICE" in
                        entered)
                            mon_pass="$MONITOR_USER_PASSWORD"
                            CLICKHOUSE_PASSWORD="$mon_pass"
                            write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
                            ;;
                        new)
                            mon_pass=$(generate_monitor_password) || exit 1
                            CLICKHOUSE_PASSWORD="$mon_pass"
                            write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
                            password_applied="true"
                            if run_err=$(run_setup_sql "$alter_sql" 2>&1 >/dev/null); then
                                echo "  Password set."
                            else
                                echo "  Could not set it:"
                                head -n 2 <<<"$run_err" | sed 's/^/    /'
                                echo "  Run this as a ClickHouse administrator:"
                                cat "$alter_sql"
                            fi
                            ;;
                        keep)
                            print_monitor_password_kept_note
                            MONITOR_PASSWORD_KEPT="true"
                            MONITOR_SETUP_SQL=""
                            ;;
                    esac
                fi
                break
                ;;
            [sS])
                echo "  Skipped."
                if [[ "$AUTH_RESET_CONFIRMED" == "true" ]]; then
                    mon_pass="$kept_pass"
                    CLICKHOUSE_PASSWORD="$mon_pass"
                    password_source="$kept_source"
                    write_monitor_user_files "$mon_pass" "$setup_sql" "$alter_sql" "$user_xml"
                    if [[ "$password_source" == "secret" ]]; then
                        echo "  The password of '${CLICKHOUSE_USER}' did not change, and /etc/click-dog/.secret"
                        echo "  keeps the password it had."
                    else
                        echo "  The password of '${CLICKHOUSE_USER}' did not change, so click-dog cannot connect"
                        echo "  until /etc/click-dog/.secret and ClickHouse match."
                    fi
                fi
                break
                ;;
            *)
                echo "  Please enter one of ${menu_choices}."
                ;;
        esac
    done

    if [[ "$password_applied" == "true" ]]; then
        save_monitor_password "$mon_pass"
    fi
}

# ── Dispatch ─────────────────────────────────────────────────────
case "$COMMAND" in
    quickstart) do_quickstart ;;
    install)    do_install ;;
    update)     do_update ;;
    status)     do_status ;;
    uninstall)  do_uninstall ;;
    kubernetes) do_kubernetes ;;
    docker)     do_docker ;;
esac
