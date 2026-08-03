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
#   ./install.sh uninstall
#   ./install.sh kubernetes -c collector:4317 --ch-host ch.svc [--cluster name] [-o DIR]
#   ./install.sh kubernetes update -v VERSION [-o DIR]
#   ./install.sh docker -c collector:4317 [-o DIR]
#   ./install.sh docker update -v VERSION [-o DIR]
#
# Options:
#   -c ADDRESS       OTEL collector address (required for install/kubernetes/docker)
#   -u USER          ClickHouse username (default: click_dog_monitor).
#                    A dedicated, namespaced name avoids colliding with an
#                    existing "monitoring" user; setup never replaces a user
#                    that already exists.
#   -v VERSION       Version to install/generate (default: latest release)
#   -b BINARY        Path to pre-downloaded binary (install/update only)
#   -k HOSTS         Keeper hosts for HA, comma-separated (install only)
#   -x PROXY         HTTPS proxy for binary download
#   -C CMD           clickhouse-client command/path (default: clickhouse-client)
#                    e.g. -C "clickhouse-client -u admin --password secret"
#   -f PATH          Use this pre-rendered click-dog.yaml instead of having
#                    install.sh render one (install only). Pair with the
#                    output of `click-dog init --wizard` from a laptop.
#   -o DIR           Output directory (kubernetes/docker modes)
#   --ch-host HOST   ClickHouse Service DNS for kubernetes (required; e.g.
#                    clickhouse.clickhouse.svc.cluster.local — not localhost)
#   --cluster NAME   ClickHouse cluster name for kubernetes; enables cluster()
#                    reads across all shards
#   --prerelease     When -v is unset, resolve the newest pre-release
#                    (alpha/beta) instead of the latest GA release
#   --systemd        Create + enable systemd unit (install only)
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

# Templates dir is resolved when the script runs from a local checkout. When
# fetched via curl|bash there is no on-disk script and SCRIPT_DIR is empty;
# render_template will then refuse k8s/docker commands with a clear error.
if [[ -f "${BASH_SOURCE[0]:-}" ]]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
else
    SCRIPT_DIR=""
fi
TEMPLATES_DIR="${SCRIPT_DIR}/templates"

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
CLICKHOUSE_CLUSTER=""
BINARY=""
COLLECTOR_ADDRESS=""
CLICKHOUSE_PASSWORD="${CLICKHOUSE_PASSWORD:-}"
KEEPER_HOSTS=""
CONFIG_FROM_FILE=""
HTTPS_PROXY_FLAG=""
VERSION=""
# When set (via --prerelease) and no -v is given, resolve_version picks the
# newest release INCLUDING alpha/beta prereleases instead of the latest GA.
PRERELEASE="false"
SYSTEMD="false"
OUTPUT_DIR=""
CLICKHOUSE_TLS="true"
# CLICKHOUSE_PORT is left unset by default so the rendered config gets
# `click-dog init`'s built-in port defaulting (9000 plaintext, 9440 with
# -ch-secure when no -ch-port was given). Quickstart probes localhost and
# pins this to the detected port so the rendered config matches the
# endpoint that was actually checked — see "Detect ClickHouse TLS" below.
CLICKHOUSE_PORT=""
CLICKHOUSE_CLIENT="clickhouse-client"

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
        --no-merge) echo "Note: --no-merge is now a no-op; update-time config merge was removed (update is binary/image swap only)." >&2 ;;
        --systemd)  SYSTEMD="true" ;;
        --ch-host)  CLICKHOUSE_HOST="${2:-}"; shift ;;
        --cluster)  CLICKHOUSE_CLUSTER="${2:-}"; shift ;;
        --prerelease) PRERELEASE="true" ;;
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

# ── Template rendering ─────────────────────────────────────────
# Used by kubernetes and docker generators. These commands require running
# from a local checkout (curl|bash has no templates on disk); the curl|bash
# path only reaches install/quickstart which keep their config inline.
render_template() {
    # render_template <relative_path> [allowed_vars]
    # allowed_vars is the envsubst SHELL-FORMAT arg (e.g. '$COLLECTOR_ADDRESS $VERSION').
    # Vars not listed pass through literally, so runtime ${CLICKHOUSE_PASSWORD}
    # references in k8s manifests survive into the output.
    local tmpl="$TEMPLATES_DIR/$1"
    local allowed="${2:-}"
    if [[ -z "$SCRIPT_DIR" || ! -f "$tmpl" ]]; then
        echo "Error: template '$1' not found; this command requires running install.sh from a local checkout." >&2
        exit 1
    fi
    if [[ -n "$allowed" ]]; then
        envsubst "$allowed" < "$tmpl"
    else
        cat "$tmpl"
    fi
}

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
# YAML shape lives. Pre-unification install.sh emitted its own YAML inline
# (now removed); that path diverged from the wizard on schema, service_name,
# filters, and hardening (see docs/development/specs/unify-config-generators.md
# for the catalogue).
#
# `$1` is the output path. The binary must already be available at
# `$BINARY` (resolved by resolve_binary in each caller). Flags are built
# conditionally because Go's flag package rejects the empty-bool form
# `-ch-secure=`; concat-via-array lets us add or omit each flag cleanly.
# Refuse a target binary that predates this install.sh's renderer
# unification. Pre-unified click-dog releases don't know `-ch-user` /
# `-ch-secure` / `-ha-keeper` and exit from Go's flag parser before
# writing a config — the worst possible failure mode (no diagnostic,
# half-finished install state). Detect by probing `init -h` for one of
# the new flags and fail-fast with a clear pointer to the version skew.
#
# Hand-installs at an older `-v` are still possible: the user just needs
# to check out a matching older install.sh from the tag they're pinning.
require_unified_init_binary() {
    # Capture-then-grep (rather than `$BINARY init -h | grep -q`) because the
    # pipeline form races under `set -euo pipefail`: grep -q exits 0 after
    # the first match, leaving the producer to SIGPIPE on subsequent writes;
    # pipefail then promotes the pipeline status to nonzero, the `if !`
    # negates true, and we falsely refuse a perfectly-good binary. Buffering
    # via $() removes the pipe entirely.
    local help_out
    help_out="$("$BINARY" init -h 2>&1)" || true
    if ! grep -q -- "-ch-user" <<<"$help_out"; then
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

# Same skew check for the docker-mode path: the rendered config is
# produced by `click-dog init` inside the image, so the image must know
# the unified flags. This adds a `docker pull` + brief `docker run`
# before the real rendering, but the image is cached for the subsequent
# `compose up` so the wall-clock cost is one image pull total.
require_unified_init_docker_image() {
    # Same capture-then-grep pattern as require_unified_init_binary —
    # see commentary there for why piping into grep -q races under
    # `set -euo pipefail`.
    local help_out
    help_out="$(docker run --rm "ghcr.io/coltconsulting/click-dog:${VERSION}" init -h 2>&1)" || true
    if ! grep -q -- "-ch-user" <<<"$help_out"; then
        cat >&2 <<EOF
Error: ghcr.io/coltconsulting/click-dog:${VERSION} predates this
       install.sh's unified config renderer (introduced in v26.06).

  Either:
    - omit -v to use the latest image, or
    - check out an install.sh that matches the target version:
        git checkout v${VERSION} -- deploy/install.sh
EOF
        exit 1
    fi
}

# SECRET_FILE is the path click-dog reads the ClickHouse password from. The
# config carries `clickhouse.password_file: $SECRET_FILE` (rendered via
# `init -ch-password-file`), so the secret stays in this 0600 file and never
# enters the service environment — the systemd unit needs no EnvironmentFile.
SECRET_FILE=/etc/click-dog/.secret

render_click_dog_config() {
    local out_path="$1"
    local args=(
        -collector "$COLLECTOR_ADDRESS"
        -service click-dog-monitor
        -ch-user "$CLICKHOUSE_USER"
        -ch-password-file "$SECRET_FILE"
        -o "$out_path"
        --force
    )
    [[ "$CLICKHOUSE_TLS" == "true" ]]     && args+=( -ch-secure )
    [[ -n "${CLICKHOUSE_PORT:-}" ]]       && args+=( -ch-port "$CLICKHOUSE_PORT" )
    [[ -n "$KEEPER_HOSTS" ]]              && args+=( -ha-keeper "$KEEPER_HOSTS" )
    "$BINARY" init "${args[@]}"
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
        -collector "$COLLECTOR_ADDRESS"
        -service click-dog-monitor
        -ch-user "$CLICKHOUSE_USER"
        -o "/work/${out_file}"
        --force
    )
    [[ "$CLICKHOUSE_TLS" == "true" ]]     && args+=( -ch-secure )
    [[ -n "${CLICKHOUSE_PORT:-}" ]]       && args+=( -ch-port "$CLICKHOUSE_PORT" )
    [[ -n "$KEEPER_HOSTS" ]]              && args+=( -ha-keeper "$KEEPER_HOSTS" )
    docker run --rm \
        -v "${out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        init "${args[@]}"
}

# Validate an existing on-disk click-dog.yaml against the target image,
# mirroring do_update's pre-swap validation (the `-validate` block above
# the local binary mv). Run as a pre-flight before the k8s/docker update
# paths rewrite any deployment file, so we never leave a manifest pointing
# at an image the config can't start under. Mounts read-only; the image
# entrypoint is `click-dog`, so `-validate -config` is passed directly.
validate_config_via_docker() {
    local cfg_path="$1"
    local cfg_dir cfg_file
    cfg_dir=$(cd "$(dirname "$cfg_path")" && pwd)
    cfg_file=$(basename "$cfg_path")
    docker run --rm \
        -v "${cfg_dir}:/work:ro" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        -validate -config "/work/${cfg_file}"
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
ExecStart=/usr/local/bin/click-dog -config /etc/click-dog/click-dog.yaml
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
# Security note: this knob cannot weaken the fail-closed guarantee. It
# only changes when verification gives up, not whether a missing-or-bad
# signature is accepted. cosign verify-blob's nonzero exit (signature
# mismatch, identity mismatch, unparseable cert, …) is treated the same
# whether or not `timeout` wraps it. Setting this very low can cause
# false-positive timeouts on slow networks (annoying, still fails
# closed); disabling it via 0/empty drops hang protection but cosign
# still runs and verifies. Future readers: do not remove this var
# thinking it's a bypass vector. It is not.
COSIGN_VERIFY_TIMEOUT_S="${COSIGN_VERIFY_TIMEOUT_S:-60}"

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

# verify_archive_signed_checksums <verify_dir> <archive_path> <archive_name>
#
# Verifies a release archive against the cosign-signed checksums.txt for
# the release. The verify_dir must already contain checksums.txt,
# checksums.txt.sig, and checksums.txt.pem (downloaded by the caller).
#
# Steps:
#   1. Verify cosign keyless signature on checksums.txt against the pinned
#      release-workflow identity and OIDC issuer. A stolen GITHUB_TOKEN
#      alone cannot forge this signature.
#   2. Look up the archive's SHA256 entry in the now-trusted checksums.txt.
#   3. Compute the archive's SHA256 and compare.
#
# Returns 0 only on full verification. Fails closed (nonzero, with a clear
# error to stderr) on:
#   - cosign or sha256sum/shasum not on PATH
#   - any of the three verification assets missing or empty
#   - cosign verify-blob failure
#   - archive name absent from checksums.txt
#   - archive hash mismatch
#
# Callers MUST treat any nonzero return as fatal and abort before
# extracting the archive.
verify_archive_signed_checksums() {
    local verify_dir="$1"
    local archive_path="$2"
    local archive_name="$3"

    local checksums="$verify_dir/checksums.txt"
    local sig="$verify_dir/checksums.txt.sig"
    local cert="$verify_dir/checksums.txt.pem"

    if ! command -v cosign &>/dev/null; then
        echo "Error: cosign not found on PATH." >&2
        echo "  Required to verify the signed release checksums." >&2
        echo "  Install: https://docs.sigstore.dev/cosign/installation/" >&2
        echo "  Or supply a pre-downloaded binary with -b /path/to/click-dog." >&2
        return 1
    fi
    if ! command -v sha256sum &>/dev/null && ! command -v shasum &>/dev/null; then
        echo "Error: neither sha256sum nor shasum found on PATH." >&2
        echo "  Required to verify the release archive checksum." >&2
        return 1
    fi

    local f
    for f in "$checksums" "$sig" "$cert"; do
        if [[ ! -s "$f" ]]; then
            echo "Error: missing or empty release verification asset: ${f##*/}" >&2
            echo "  The release must publish checksums.txt, checksums.txt.sig," >&2
            echo "  and checksums.txt.pem. Refusing to install an unverified release." >&2
            return 1
        fi
    done

    if [[ ! -f "$archive_path" ]]; then
        echo "Error: archive not found at $archive_path" >&2
        return 1
    fi

    # Step 1: cosign keyless signature on checksums.txt. The pinned identity
    # ties the signature to the click-dog release workflow on a release tag.
    # Wrapped with `timeout` so a slow/blocked Rekor or Fulcio fails closed
    # quickly instead of hanging the installer. `timeout` exits 124 on
    # deadline; we surface that with a Sigstore-egress-specific message so
    # operators behind a firewall know what to fix.
    local cosign_args=(verify-blob
        --certificate "$cert"
        --signature "$sig"
        --certificate-identity-regexp "$COSIGN_IDENTITY_REGEXP"
        --certificate-oidc-issuer "$COSIGN_OIDC_ISSUER"
        "$checksums")
    local cosign_output rc=0
    # Skip the wrapper if `timeout` is missing OR the cap was explicitly
    # disabled via 0/empty — see COSIGN_VERIFY_TIMEOUT_S declaration for
    # why we don't pass 0 through to `timeout`. install.sh's auto-download
    # path is Linux-only at the OS check above, so the no-`timeout`
    # branch should only be reached on hosts missing coreutils.
    if command -v timeout &>/dev/null \
            && [[ -n "$COSIGN_VERIFY_TIMEOUT_S" && "$COSIGN_VERIFY_TIMEOUT_S" != "0" ]]; then
        cosign_output=$(timeout "$COSIGN_VERIFY_TIMEOUT_S" cosign "${cosign_args[@]}" 2>&1) || rc=$?
    else
        cosign_output=$(cosign "${cosign_args[@]}" 2>&1) || rc=$?
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
        return 1
    fi

    # Step 2: archive entry must exist in the now-trusted checksums.txt.
    # awk match on exact filename (field 2) avoids any prefix/suffix tricks.
    local expected_hash
    expected_hash=$(awk -v name="$archive_name" '$2 == name {print $1; exit}' "$checksums")
    if [[ -z "$expected_hash" || ${#expected_hash} -ne 64 ]]; then
        echo "Error: $archive_name not found in signed checksums.txt." >&2
        echo "  Refusing to install an archive missing from trusted release metadata." >&2
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

    return 0
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
resolve_version() {
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
        # /releases/latest excludes prereleases, and ?per_page=1 would just
        # return the newest release of ANY kind (a GA if one is newer than the
        # latest beta). So list a page and pick the first prerelease below.
        endpoint="https://api.github.com/repos/coltconsulting/click-dog/releases?per_page=20"
        kind="pre-release"
    else
        endpoint="https://api.github.com/repos/coltconsulting/click-dog/releases/latest"
        kind="release"
    fi
    # Status-aware probe: build args explicitly WITHOUT -f (rather than mutating
    # CURL_ARGS, whose exact tokenisation we don't want to depend on). Without -f,
    # curl returns success on a 404 and the status lands in -w output, so we can
    # tell "no release yet" (404) from a network failure. Capture curl's exit
    # explicitly — under `set -e` a bare `resp=$(curl ...)` aborts the whole
    # script on a network/proxy error and, with stderr redirected, fails silently.
    # Auth via --config <(...) keeps the token out of curl's argv (see curl_auth_config).
    local -a probe_args=(-sSL --proto '=https' -w $'\n%{http_code}')
    [[ -n "$HTTPS_PROXY_FLAG" ]] && probe_args+=(--proxy "$HTTPS_PROXY_FLAG")
    local resp http_code
    if ! resp=$(curl "${probe_args[@]}" --config <(curl_auth_config) "$endpoint" 2>/dev/null); then
        echo "Error: could not reach the GitHub releases API (network or proxy failure)." >&2
        echo "If behind a proxy, use -x http://proxy:3128 or export HTTPS_PROXY" >&2
        exit 1
    fi
    http_code="${resp##*$'\n'}"
    if [[ "$PRERELEASE" == "true" ]]; then
        VERSION=$(printf '%s' "$resp" | extract_first_prerelease_tag)
    else
        VERSION=$(printf '%s' "$resp" | sed -n 's/.*"tag_name": *"v\([^"]*\)".*/\1/p' | sed -n '1p')
    fi
    if [[ "$http_code" == "401" || "$http_code" == "403" ]]; then
        echo "Error: GitHub rejected the release request (HTTP ${http_code})." >&2
        echo "  The request may be rate-limited, or GITHUB_TOKEN/GH_TOKEN may be invalid." >&2
        echo "  Retry with a valid token, or use -b /path/to/click-dog." >&2
        exit 1
    fi
    if [[ "$http_code" == "404" ]]; then
        echo "Error: no published click-dog ${kind} found (HTTP 404)." >&2
        echo "  The release and its binaries may not be published yet." >&2
        echo "  To install from a locally built binary instead:" >&2
        echo "    sudo $0 -b /path/to/click-dog" >&2
        exit 1
    fi
    if [[ -z "$VERSION" ]]; then
        echo "Error: could not resolve the latest ${kind} version from GitHub API (HTTP ${http_code:-?})" >&2
        if [[ "$PRERELEASE" == "true" ]]; then
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
extract_first_prerelease_tag() {
    tr ',' '\n' | awk '
        match($0, /"tag_name"[ \t]*:[ \t]*"v?[^"]+"/) {
            t = substr($0, RSTART, RLENGTH)
            sub(/.*:[ \t]*"v?/, "", t)
            sub(/".*/, "", t)
            last_tag = t
        }
        /"prerelease"[ \t]*:[ \t]*true/ { print last_tag; exit }
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
}

resolve_binary() {
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
            echo "  You may set GITHUB_TOKEN/GH_TOKEN or pass -b /path/to/click-dog." >&2
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

    # Fetch trusted release metadata: checksums.txt and its cosign keyless
    # signature pair (.sig + .pem). All three are required — the installer
    # verifies the signature on checksums.txt against the pinned release
    # workflow identity before trusting any hash inside it, and aborts
    # before extraction on any verification failure.
    local verify_dir
    if ! verify_dir=$(mktemp -d "$download_dir/verify.XXXXXX" 2>/dev/null); then
        echo "Error: failed to create temp dir for release verification" >&2
        rm -rf "$download_dir"
        exit 1
    fi
    _RESOLVE_BINARY_VERIFY_DIR="$verify_dir"
    local f
    for f in checksums.txt checksums.txt.sig checksums.txt.pem; do
        if ! download_release_asset "$release_json" "$f" "$verify_dir/$f"; then
            echo "Error: failed to download $f for v${VERSION}" >&2
            echo "  Required to verify the release signature." >&2
            rm -rf "$download_dir"
            exit 1
        fi
    done

    echo "Verifying signed release checksums..."
    if ! verify_archive_signed_checksums "$verify_dir" "$archive_tmp" "$archive_name"; then
        rm -f "$archive_tmp"
        rm -rf "$verify_dir"
        exit 1
    fi
    rm -rf "$verify_dir"
    _RESOLVE_BINARY_VERIFY_DIR=""
    echo "Release signature verified — archive matches signed checksums.txt"

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
    # All transient state cleaned up; release the trap so the caller is
    # free to install its own EXIT handler over BINARY/config tempfiles.
    trap - EXIT
    echo "Downloaded to $BINARY"
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
    fi
    resolve_password

    if [[ "$(uname -s)" == "Darwin" ]]; then
        echo "Error: Local install is not supported on macOS. Use docker mode." >&2
        exit 1
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

    if [[ -n "$CONFIG_FROM_FILE" ]]; then
        cp "$CONFIG_FROM_FILE" "$CONFIG_TMPFILE"
    else
        require_unified_init_binary
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
        echo "click-dog install: local, collector=$COLLECTOR_ADDRESS (systemd: $SYSTEMD)"
    fi

    # Step 1-3: create user + config dir
    id click-dog &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin click-dog
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
    if ! "$staged_binary" -version >/dev/null 2>&1; then
        echo "ERROR: binary smoke-test failed" >&2
        rm -rf "$install_stage"
        install_stage=""
        exit 1
    fi

    # Step 6: place config files
    cp "$CONFIG_TMPFILE" /etc/click-dog/click-dog.yaml
    cp "$ENV_TMPFILE"    /etc/click-dog/.secret
    chown click-dog:click-dog /etc/click-dog/click-dog.yaml /etc/click-dog/.secret
    chmod 640 /etc/click-dog/click-dog.yaml
    chmod 600 /etc/click-dog/.secret

    # Step 7: validate config against staged binary
    if ! "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "ERROR: config validation failed" >&2
        "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >&2 || true
        rm -f /etc/click-dog/click-dog.yaml /etc/click-dog/.secret
        rm -rf "$install_stage"
        install_stage=""
        exit 1
    fi

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
    if ! "$staged_binary" -version >/dev/null 2>&1; then
        echo "ERROR: new binary failed smoke test, aborting" >&2
        rm -rf "$update_stage"
        update_stage=""
        exit 1
    fi

    # Reject downgrades. `sort -V` puts the lower version first; if that's the new one, it's a downgrade.
    local new_ver cur_ver lowest
    new_ver=$("$staged_binary" -version 2>&1 | awk '{print $2}')
    cur_ver=$(/usr/local/bin/click-dog -version 2>&1 | awk '{print $2}') || cur_ver=""
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
    if ! "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "ERROR: existing config does not validate against new binary" >&2
        "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >&2 || true
        echo "Re-render via 'click-dog init' or fix the config, then re-run update." >&2
        rm -rf "$update_stage"
        update_stage=""
        exit 1
    fi

    # Swap binary, restart
    [[ -f /usr/local/bin/click-dog ]] && cp /usr/local/bin/click-dog /usr/local/bin/click-dog.prev
    mv "$staged_binary" /usr/local/bin/click-dog
    chmod 755 /usr/local/bin/click-dog
    rmdir "$update_stage"
    update_stage=""
    systemctl restart click-dog
    sleep 2
    systemctl is-active click-dog >/dev/null

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
        ver=$(click-dog -version 2>/dev/null || echo "unknown")
        echo "running ($ver)"
    else
        echo "STOPPED"
    fi
}

# ── UNINSTALL — stop and remove everything ─────────────────────
do_uninstall() {
    echo "This will stop click-dog and remove all files from this machine."

    while true; do
        read -rp "Continue? [y/n] " confirm
        case "$confirm" in
            [yY]) break ;;
            [nN]) echo "Aborted."; exit 0 ;;
            *)    echo "Please enter y or n." ;;
        esac
    done

    echo "uninstalling..."
    systemctl stop click-dog 2>/dev/null || true
    systemctl disable click-dog 2>/dev/null || true
    rm -f /etc/systemd/system/click-dog.service
    systemctl daemon-reload 2>/dev/null || true
    rm -f /usr/local/bin/click-dog
    rm -rf /etc/click-dog
    rm -rf /var/log/click-dog
    userdel click-dog 2>/dev/null || true
    groupdel click-dog 2>/dev/null || true
    echo "Uninstall complete."
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
        echo "    click-dog deploy kubernetes -c <collector> -ch-host <ch-service> [-cluster <name>]" >&2
        exit 1
    fi
    [[ -z "$COLLECTOR_ADDRESS" ]] && { echo "Error: -c collector address is required for kubernetes"; usage; }
    if [[ -z "$CLICKHOUSE_HOST" ]]; then
        echo "Error: --ch-host is required for kubernetes — the ClickHouse Service DNS" >&2
        echo "       (e.g. clickhouse.clickhouse.svc.cluster.local). A standalone click-dog" >&2
        echo "       pod can't reach ClickHouse via localhost. Add --cluster <name> for" >&2
        echo "       cluster() reads across shards." >&2
        exit 1
    fi
    resolve_password
    resolve_version

    mkdir -p "$out_dir"
    local abs_out_dir
    abs_out_dir=$(cd "$out_dir" && pwd)

    local args=(
        -c "$COLLECTOR_ADDRESS"
        -ch-host "$CLICKHOUSE_HOST"
        -u "$CLICKHOUSE_USER"
        -v "$VERSION"
        -o /work
    )
    [[ -n "$CLICKHOUSE_CLUSTER" ]] && args+=( -cluster "$CLICKHOUSE_CLUSTER" )

    # Password flows through the env (read by `deploy kubernetes`), never argv.
    export CLICKHOUSE_PASSWORD
    docker run --rm \
        -e CLICKHOUSE_PASSWORD \
        -v "${abs_out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        deploy kubernetes "${args[@]}"

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
        if ! preflight_validate_config "$tmp_dir/click-dog.yaml" "install.sh kubernetes"; then
            rm -rf "$tmp_dir"
            exit 1
        fi
        rm -rf "$tmp_dir"
    fi

    # Delegate the image-tag bump to the generator (backs up deployment.yaml.bak
    # and rewrites the single image line) so install.sh and `click-dog deploy
    # kubernetes` stay one implementation. --update only bumps the tag, so it
    # needs neither -c nor -ch-host (the config in the ConfigMap is untouched).
    local abs_out_dir
    abs_out_dir=$(cd "$out_dir" && pwd)
    docker run --rm \
        -v "${abs_out_dir}:/work" \
        "ghcr.io/coltconsulting/click-dog:${VERSION}" \
        deploy kubernetes --update -v "${VERSION}" -o /work

    echo "Kubernetes update complete. Review changes and apply with: kubectl apply -k $out_dir/"
    echo "  Note: update only bumps the Deployment image tag (deployment.yaml.bak is the"
    echo "  backup). If this directory predates dedicated health probes, regenerate"
    echo "  manifests to pick up the health port plus /healthz and /readyz probes."
}

# ── DOCKER — generate docker-compose.yml + config ──────────────
do_docker() {
    local out_dir="${OUTPUT_DIR:-./click-dog-docker}"

    if [[ "$SUBCOMMAND" == "update" ]]; then
        do_docker_update "$out_dir"
        return
    fi

    command -v envsubst >/dev/null || { echo "Error: envsubst (gettext) is required for docker mode" >&2; exit 1; }

    [[ -z "$COLLECTOR_ADDRESS" ]] && { echo "Error: -c collector address is required for docker"; usage; }
    resolve_password
    resolve_version

    mkdir -p "$out_dir"

    export VERSION
    render_template "docker/docker-compose.yml.tmpl" '$VERSION' > "$out_dir/docker-compose.yml"

    # click-dog.yaml — rendered by `click-dog init` inside the image (see
    # render_click_dog_config_via_docker for why this path can't use a
    # local binary). Probe the image first so a -v pointing at a
    # pre-unified release fails with a useful error instead of a
    # cryptic "flag provided but not defined" from inside docker run.
    require_unified_init_docker_image
    render_click_dog_config_via_docker "$out_dir/click-dog.yaml"

    # .env — Docker Compose format: escape \, $, and # per Compose v2 spec
    local safe_pw="$CLICKHOUSE_PASSWORD"
    safe_pw="${safe_pw//\\/\\\\}"
    safe_pw="${safe_pw//\$/\\\$}"
    safe_pw="${safe_pw//#/\\#}"
    printf "CLICKHOUSE_PASSWORD=%s\n" "$safe_pw" > "$out_dir/.env"
    chmod 600 "$out_dir/.env"

    render_template "docker/gitignore.tmpl" > "$out_dir/.gitignore"

    echo "Docker files generated in $out_dir/"
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
            echo "  WARNING: system.opentelemetry_span_log does NOT exist after setup."
            echo "  The filesystem config did not take effect — click-dog will have no"
            echo "  data to export. Verify manually with:"
            echo "    ${SPAN_LOG_EXISTS_SQL}"
            echo "    ${SPAN_LOG_RECENT_SQL}"
            echo ""
            echo "  Then re-run this installer or:"
            echo "    click-dog check"
            ;;
        no_client)
            echo "  Could not verify span logging via SQL (no clickhouse-client or"
            echo "  credentials available). Confirm it manually by running:"
            print_span_log_manual_sql
            ;;
    esac

    echo "$state"
}

# _span_log_client_has_auth CLIENT_CMD
#
# Pure predicate: does the client command already carry user/password auth
# flags? Kept separate so install_test.sh can exercise it without a client.
# Returns 0 (true) when -u / --user / --password is present.
_span_log_client_has_auth() {
    case " $1 " in
        *" -u "*|*" --user"*|*" --password"*) return 0 ;;
        *) return 1 ;;
    esac
}

# Default query runner: invoke $CLICKHOUSE_CLIENT for the SQL string. Word-split
# $CLICKHOUSE_CLIENT intentionally (it may carry host/port/auth flags).
#
# Auth is deliberately conservative because this verification runs in quickstart
# BEFORE the monitoring user is created and BEFORE a password is generated:
#   1. Operator supplied auth via -C ("clickhouse-client -u admin --password
#      ..."): use it verbatim — appending our own -u would duplicate the flag.
#   2. A password already exists (CLICKHOUSE_PASSWORD env, the
#      "use existing credentials" path): authenticate as that user — it exists.
#   3. Default quickstart (no -C auth, no password yet): run the client as-is so
#      it uses its own default auth. We must NOT append -u "$CLICKHOUSE_USER"
#      here — the monitoring user does not exist yet, so probing as it would
#      fail and drop a working single-node install to the no_client fallback,
#      skipping the SQL verification this path exists to provide.
_span_log_default_runner() {
    local sql="$1"
    if _span_log_client_has_auth "$CLICKHOUSE_CLIENT"; then
        # shellcheck disable=SC2086
        $CLICKHOUSE_CLIENT -q "$sql"
        return
    fi
    if [[ -n "${CLICKHOUSE_PASSWORD:-}" ]]; then
        # shellcheck disable=SC2086
        $CLICKHOUSE_CLIENT -u "$CLICKHOUSE_USER" --password "$CLICKHOUSE_PASSWORD" -q "$sql"
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
# is readonly=1 and does not match click-dog's session contract.
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
                <query>GRANT SELECT ON system.opentelemetry_span_log TO ${user}</query>
                <query>GRANT SELECT ON system.query_log TO ${user}</query>
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
    local clickhouse_setup_tempfiles=()

    # ── Pre-flight checks ───────────────────────────────────────
    echo ""
    echo "click-dog — ClickHouse OTEL span exporter"
    echo "==========================================="
    echo ""
    echo "Exports ClickHouse query traces to Datadog, Honeycomb, or any"
    echo "OTEL-compatible backend. Runs as a lightweight systemd sidecar."
    echo ""
    echo "The only ClickHouse change is creating a read-only user for"
    echo "reading span data. You review the SQL first and choose whether"
    echo "to run it yourself or let this script run it for you."
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
        existing_version=$(/usr/local/bin/click-dog -version 2>/dev/null | awk '{print $NF}') || true
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
        echo "  [r] Reinstall — overwrite config and service; keep existing credentials"
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
                    if ! "$staged_binary" -version >/dev/null 2>&1; then
                        echo "  Error: binary smoke-test failed (wrong architecture?)" >&2
                        echo "  Build with: make build-amd64  (or build-arm64)" >&2
                        rm -f "$staged_binary"
                        exit 1
                    fi
                    local new_version lowest
                    new_version=$("$staged_binary" -version 2>/dev/null | awk '{print $NF}') || true
                    if [[ -n "$existing_version" && -n "$new_version" && "$existing_version" != "dev" && "$new_version" != "dev" && "$existing_version" != "$new_version" ]]; then
                        lowest=$(printf '%s\n%s\n' "${existing_version#v}" "${new_version#v}" | sort -V | head -n1)
                        if [[ "$lowest" == "${new_version#v}" ]]; then
                            echo "  Error: refusing to downgrade from $existing_version to $new_version" >&2
                            rm -f "$staged_binary"
                            exit 1
                        fi
                    fi
                    if [[ -f /etc/click-dog/click-dog.yaml ]] && ! "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
                        echo "  Error: existing config does not validate against new binary" >&2
                        "$staged_binary" -validate -config /etc/click-dog/click-dog.yaml >&2 || true
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
                        echo "        Choose Reinstall if you want the latest default unit."
                    else
                        echo "  [3/3] Systemd unit unchanged."
                    fi
                    rm -f "$unit_tmp"

                    # Only restart if the service was previously running.
                    # Don't resurrect a stopped/disabled service.
                    echo ""
                    if [[ "$existing_was_active" == "true" ]]; then
                        echo "  Restarting service..."
                        systemctl restart click-dog
                        # Poll for up to 5 seconds instead of a fixed sleep
                        local waited=0
                        while (( waited < 10 )); do
                            if systemctl is-active click-dog >/dev/null 2>&1; then
                                break
                            fi
                            sleep 0.5
                            waited=$((waited + 1))
                        done
                        if systemctl is-active click-dog >/dev/null 2>&1; then
                            echo "  Updated: ${existing_version:-unknown} → ${new_version:-unknown}"
                            echo "  click-dog is running."
                        else
                            echo "  Binary updated but service did not start."
                            echo "  Check logs: journalctl -u click-dog -f"
                        fi
                    else
                        echo "  Updated: ${existing_version:-unknown} → ${new_version:-unknown}"
                        echo "  Service was not running before update — leaving stopped."
                        echo "  Start with: systemctl start click-dog"
                    fi
                    echo ""
                    return
                    ;;
                [rR])
                    echo ""
                    echo "  Proceeding with reinstall — config and service will be rewritten."
                    echo "  Existing credentials in /etc/click-dog/.secret are kept so the"
                    echo "  ClickHouse user keeps authenticating; delete that file first to rotate."
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
    echo "                       (SELECT only; you review the SQL), then verify"
    echo "                       spans are readable as that user"
    echo "  2. OTEL collector  — confirm where to send traces"
    echo "  3. Install         — download binary to /usr/local/bin/click-dog,"
    echo "                       configure in /etc/click-dog/, start systemd service"
    echo ""
    echo "Nothing is installed until step 3. All steps require confirmation."
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
                        echo "    cp ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}"
                        echo ""
                        read -rp "  Press Enter once done..."
                    else
                        echo ""
                        echo "  Writing to ${ch_config_dir}/opentelemetry.${otel_cfg_ext}..."
                        cp "$otel_cfg_file" "${ch_config_dir}/opentelemetry.${otel_cfg_ext}"
                        echo -n "  Restarting ClickHouse... "
                        if systemctl restart clickhouse-server 2>/dev/null; then
                            sleep 3
                            echo "done"
                        else
                            echo "FAILED"
                            echo "  Restart clickhouse-server manually."
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
                    echo "    cp ${otel_cfg_file} /etc/clickhouse-server/config.d/opentelemetry.${otel_cfg_ext}"
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
    # -ch-secure default), pointing the monitor at an endpoint nobody
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
    else
        local mon_pass=""
        local reusing_password="false"
        # Tracks whether the operator explicitly confirmed a password reset;
        # consumed only for messaging — the ALTER is gated at the prompt below.
        local AUTH_RESET_CONFIRMED="false"
        # Reuse existing password on re-runs so it stays in sync with ClickHouse.
        # The secret file holds the raw password (read by click-dog via
        # clickhouse.password_file); $(cat) strips any trailing newline.
        if [[ -f "$SECRET_FILE" ]]; then
            mon_pass=$(cat "$SECRET_FILE")
            if [[ -n "$mon_pass" ]]; then
                reusing_password="true"
            fi
        fi
        if [[ -z "$mon_pass" ]]; then
            mon_pass=$(generate_password)
            [[ ${#mon_pass} -ge 16 ]] || { echo "Error: failed to generate password"; exit 1; }
        fi
        CLICKHOUSE_PASSWORD="$mon_pass"

        # Generate SHA256 hash (used by both SQL and XML user config)
        local pass_hash=""
        if command -v sha256sum &>/dev/null; then
            pass_hash=$(printf '%s' "$mon_pass" | sha256sum | awk '{print $1}')
        elif command -v shasum &>/dev/null; then
            pass_hash=$(printf '%s' "$mon_pass" | shasum -a 256 | awk '{print $1}')
        fi

        # Idempotent setup SQL — CREATE USER IF NOT EXISTS, never OR REPLACE.
        # The password-mutating ALTER lives in a SEPARATE bundle so we never
        # reset an existing user's credentials without explicit confirmation.
        local setup_sql
        setup_sql=$(secure_temp_file "click-dog-setup")
        generate_user_setup_sql "$CLICKHOUSE_USER" "$pass_hash" > "$setup_sql"

        local alter_sql
        alter_sql=$(secure_temp_file "click-dog-alter-user")
        generate_user_alter_sql "$CLICKHOUSE_USER" "$pass_hash" > "$alter_sql"

        local verify_sql
        verify_sql=$(secure_temp_file "click-dog-verify")
        generate_user_verify_sql "$CLICKHOUSE_USER" > "$verify_sql"

        # Write XML user config file (declarative users.d overlay).
        local user_xml
        user_xml=$(secure_temp_file "click-dog-user")
        generate_user_setup_xml "$CLICKHOUSE_USER" "$pass_hash" > "$user_xml"
        clickhouse_setup_tempfiles+=("$setup_sql" "$alter_sql" "$verify_sql" "$user_xml")

        echo ""
        # ── Probe the existing user before touching anything ─────────────
        # We never assume access. When clickhouse-client is available we
        # discover three facts: does the user authenticate with our password,
        # and (if so) can it actually read the two system tables click-dog
        # needs. The auth-mismatch decision is then made by a pure helper so
        # we can never silently reset credentials.
        local user_can_auth="false"
        local probed="false"
        if command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
            probed="true"
            if $CLICKHOUSE_CLIENT -u "$CLICKHOUSE_USER" --password "$mon_pass" -q "SELECT 1" &>/dev/null; then
                user_can_auth="true"
            fi
        fi

        # We can't list users with our read-only probe, so treat a stored
        # password (re-run) as evidence the user already exists. Fresh installs
        # with no .secret are the "create" case. The pure decision helper turns
        # (exists, can_auth) into create | ok | prompt.
        local user_exists="$reusing_password"
        local auth_decision
        auth_decision=$(user_auth_mismatch_decision "$user_exists" "$user_can_auth")
        # Without a working client we can't probe at all — fall back to the
        # safe non-mutating create/grant flow rather than prompting blindly.
        if [[ "$probed" != "true" ]]; then
            auth_decision="create"
        fi

        if [[ "$auth_decision" == "ok" ]]; then
            # User authenticates. Verify it can read BOTH required tables
            # instead of assuming SELECT 1 implies access.
            if $CLICKHOUSE_CLIENT -u "$CLICKHOUSE_USER" --password "$mon_pass" --multiquery < "$verify_sql" &>/dev/null; then
                echo "  Existing ClickHouse user '${CLICKHOUSE_USER}' authenticates and can read"
                echo "  system.opentelemetry_span_log and system.query_log — nothing to do."
                echo "  Password: /etc/click-dog/.secret"
                sql_choice="s"
            else
                echo "  ClickHouse user '${CLICKHOUSE_USER}' authenticates but is MISSING SELECT on"
                echo "  one of the required system tables. The setup SQL only ADDS the two"
                echo "  GRANTs click-dog needs (it never replaces the user)."
            fi
        elif [[ "$auth_decision" == "prompt" ]]; then
            # We have a stored password but the user won't authenticate with
            # it — the user exists with DIFFERENT credentials (or doesn't
            # exist yet). Either way we must PROMPT before mutating auth.
            # CREATE USER IF NOT EXISTS won't fix a wrong password, so offer
            # an explicit, confirmed ALTER.
            echo "  ClickHouse user '${CLICKHOUSE_USER}' did NOT authenticate with the password"
            echo "  stored in /etc/click-dog/.secret. If the user already exists with a"
            echo "  different password, click-dog cannot connect until they match."
            echo ""
            echo "  Resetting the password ALTERs the existing user's auth method. This is the"
            echo "  ONLY change that mutates an existing account — it is not done automatically."
            echo ""
            local reset_choice=""
            while true; do
                read -rp "  Reset '${CLICKHOUSE_USER}' password to match /etc/click-dog/.secret? [y/n] " reset_choice
                case "$reset_choice" in
                    [yY])
                        echo ""
                        echo "  Will run (after you choose [r] below):"
                        echo "  ──────────────────────────────────────────────────────"
                        cat "$alter_sql"
                        echo "  ──────────────────────────────────────────────────────"
                        # Fold the confirmed ALTER into the setup bundle so the
                        # [r] (run-SQL) path resets the password. Without this
                        # explicit confirmation the ALTER is never emitted. (The
                        # [x] XML path sets password_sha256_hex declaratively, so
                        # it always reflects the saved secret on its own.)
                        cat "$alter_sql" >> "$setup_sql"
                        AUTH_RESET_CONFIRMED="true"
                        break
                        ;;
                    [nN])
                        echo "  Leaving '${CLICKHOUSE_USER}' credentials unchanged."
                        echo "  The setup SQL below only creates the user if absent and adds grants."
                        break
                        ;;
                    *) echo "  Please enter y or n." ;;
                esac
            done
            echo ""
        fi

        if [[ "${sql_choice:-}" != "s" ]]; then
        if [[ "$reusing_password" == "true" ]]; then
            echo "  Reusing password from /etc/click-dog/.secret"
            echo "  (the ClickHouse user must use the same password to connect)"
        else
            echo "  This sets up a read-only ClickHouse user '${CLICKHOUSE_USER}':"
            echo "    • CREATE USER IF NOT EXISTS — an existing user of this name is left"
            echo "      untouched; its auth, profile, quota, settings, and grants are preserved."
            echo "    • GRANT SELECT on system.opentelemetry_span_log and system.query_log"
            echo "      (additive — these are the only privileges click-dog requires)."
            echo ""
            echo "  The same password will be set in ClickHouse and saved to"
            echo "  /etc/click-dog/.secret — both sides must match for click-dog to connect."
            echo ""
            echo "  To change it later, update both: the ClickHouse user password and"
            echo "  /etc/click-dog/.secret, then restart click-dog."
        fi
        echo ""
        # Always print the full SQL bundle so security-conscious operators can
        # review or run it by hand instead of via clickhouse-client.
        echo "  Manual SQL (review before running anywhere):"
        echo "  ──────────────────────────────────────────────────────"
        cat "$setup_sql"
        echo "  ──────────────────────────────────────────────────────"
        echo "  Saved to: ${setup_sql}"
        echo "  Run with: ${CLICKHOUSE_CLIENT} --multiquery < ${setup_sql}"
        echo ""
        # The [x] XML overlay carries <password_sha256_hex>, so for an EXISTING
        # user it declaratively resets credentials. In the auth-mismatch case
        # where the operator DECLINED the reset, offering [x] would undo that
        # choice (the clobber this change removes) — so drop it, leaving [r]
        # (additive grants, no password change) and [s].
        local offer_xml="true" menu_choices="x/r/s"
        if [[ "$auth_decision" == "prompt" && "$AUTH_RESET_CONFIRMED" != "true" ]]; then
            offer_xml="false"
            menu_choices="r/s"
        fi
        if [[ "$offer_xml" == "true" ]]; then
            echo "  [x] Write XML config (no clickhouse-client needed)"
        fi
        echo "  [r] Run SQL via clickhouse-client (will prompt for command if needed)"
        echo "  [s] Skip — set up the '${CLICKHOUSE_USER}' user yourself"
        if [[ "$offer_xml" != "true" ]]; then
            echo ""
            echo "  ([x] Write XML config is omitted here: its users.d overlay would"
            echo "   reset '${CLICKHOUSE_USER}'s password, which you chose not to change.)"
        fi
        echo ""
        while true; do
            read -rp "  Choice [${menu_choices}]: " sql_choice
            case "$sql_choice" in
                [xX])
                    if [[ "$offer_xml" != "true" ]]; then
                        echo "  [x] is unavailable here — it would reset the password you"
                        echo "  declined to change. Choose [r] (adds grants only) or [s]."
                        continue
                    fi
                    echo ""
                    local ch_users_dir="/etc/clickhouse-server/users.d"
                    local ch_users_file="${ch_users_dir}/click-dog-${CLICKHOUSE_USER}.xml"
                    if [[ -d "$ch_users_dir" ]]; then
                        cp "$user_xml" "$ch_users_file"
                        echo "  Written to: ${ch_users_file}"
                        echo "  ClickHouse will pick this up automatically."
                    else
                        echo "  ${ch_users_dir} not found."
                        echo ""
                        echo "  Copy this file manually:"
                        echo "    cp ${user_xml} /etc/clickhouse-server/users.d/click-dog-${CLICKHOUSE_USER}.xml"
                        echo ""
                        read -rp "  Press Enter once done..."
                    fi
                    break
                    ;;
                [rR])
                    echo ""
                    if ! command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
                        echo "  ${CLICKHOUSE_CLIENT%% *} not found."
                        echo ""
                        read -rp "  clickhouse-client command [${CLICKHOUSE_CLIENT}]: " user_client
                        [[ -n "$user_client" ]] && CLICKHOUSE_CLIENT="$user_client"
                        if ! command -v ${CLICKHOUSE_CLIENT%% *} &>/dev/null; then
                            echo ""
                            echo "  Still not found. SQL saved to: ${setup_sql}"
                            echo "  ──────────────────────────────────────────────────────"
                            cat "$setup_sql"
                            echo "  ──────────────────────────────────────────────────────"
                            echo ""
                            echo "  Run with: ${CLICKHOUSE_CLIENT} --multiquery < ${setup_sql}"
                            echo ""
                            read -rp "  Press Enter once done..."
                            break
                        fi
                    fi
                    echo "  Running setup SQL..."
                    if $CLICKHOUSE_CLIENT --multiquery < "$setup_sql" >/dev/null 2>&1; then
                        echo "  Done."
                        break
                    fi
                    # The bare client failed — almost always because the admin
                    # user needs a host/port/password the installer doesn't have.
                    # Re-prompting for the client PATH (the old behavior) can't
                    # fix that, so collect admin connection details and retry with
                    # explicit flags. These are used only to run the setup SQL and
                    # are never written to disk.
                    echo "  Could not connect with '${CLICKHOUSE_CLIENT}' — the admin user"
                    echo "  likely needs a host/port/password. Enter connection details to"
                    echo "  retry (used only to run this SQL; not saved)."
                    echo ""
                    local run_ok="false" attempt
                    for attempt in 1 2; do
                        local ch_addr ch_admin ch_admin_pass
                        read -rp "  ClickHouse host[:port] [${CLICKHOUSE_HOST:-localhost}]: " ch_addr
                        ch_addr="${ch_addr:-${CLICKHOUSE_HOST:-localhost}}"
                        read -rp "  Admin user [default]: " ch_admin
                        ch_admin="${ch_admin:-default}"
                        read -rsp "  Admin password (blank if none): " ch_admin_pass
                        echo ""
                        # Base the retry on the FULL $CLICKHOUSE_CLIENT (word-split,
                        # like the rest of the script) so flags the operator passed
                        # via -C survive — "docker exec ... clickhouse-client",
                        # --secure, --config-file, etc. Using only the first word
                        # dropped them and stranded exactly the non-default installs
                        # this path exists to rescue. Appended --host/--port/-u
                        # override any stale values (clickhouse-client takes the last).
                        # NOTE: IPv6 literals like [::1]:9000 aren't parsed (the
                        # host:port split is naive) — use a hostname/IPv4, or put the
                        # full connection in -C.
                        # shellcheck disable=SC2206
                        local -a admin_cli=($CLICKHOUSE_CLIENT --host "${ch_addr%%:*}")
                        [[ "$ch_addr" == *:* ]] && admin_cli+=(--port "${ch_addr##*:}")
                        admin_cli+=(-u "$ch_admin")
                        echo "  Retrying as '${ch_admin}' on '${ch_addr}' (attempt ${attempt}/2)..."
                        # Password via CLICKHOUSE_PASSWORD env (command-scoped) so it
                        # never appears in ps / /proc/<pid>/cmdline. Empty = no
                        # password. (For a "docker exec" client this env won't cross
                        # into the container — put the auth in -C for that case.)
                        if CLICKHOUSE_PASSWORD="$ch_admin_pass" "${admin_cli[@]}" --multiquery < "$setup_sql" >/dev/null 2>&1; then
                            echo "  Done."
                            run_ok="true"
                            break
                        fi
                        echo "  Still could not connect."
                    done
                    [[ "$run_ok" == "true" ]] && break
                    echo ""
                    echo "  Run the SQL manually:"
                    echo "  ──────────────────────────────────────────────────────"
                    cat "$setup_sql"
                    echo "  ──────────────────────────────────────────────────────"
                    echo ""
                    echo "  Saved to: ${setup_sql}"
                    echo "  Run with: clickhouse-client --host HOST -u ADMIN --password ... --multiquery < ${setup_sql}"
                    echo ""
                    read -rp "  Press Enter once done..."
                    break
                    ;;
                [sS])
                    echo "  Skipped."
                    break
                    ;;
                *)
                    echo "  Please enter x, r, or s."
                    ;;
            esac
        done
        fi  # sql_choice != s (user not already verified)
    fi

    # Runtime verification — now that the monitoring user exists, confirm span
    # logging THROUGH ClickHouse as the user click-dog will actually connect as.
    # verify_span_log_via_sql authenticates with CLICKHOUSE_USER + the password
    # we just set (via _span_log_default_runner), so this proves the real export
    # path can read spans — not just that some config file mentions span logging.
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

    local col_host="${COLLECTOR_ADDRESS%%:*}"
    local col_port="${COLLECTOR_ADDRESS##*:}"
    [[ "$col_port" == "$col_host" ]] && col_port="4317"

    echo -n "  Checking ${col_host}:${col_port}... "
    if timeout 3 bash -c "echo >/dev/tcp/${col_host}/${col_port}" 2>/dev/null; then
        echo "ok"
    else
        echo "FAILED"
        echo ""
        echo "  Cannot reach ${COLLECTOR_ADDRESS}."
        echo "  Is your OTEL collector / Datadog Agent running?"
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
    echo "  To undo everything: ./deploy/install.sh uninstall"
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

    # Download
    echo "  [1/5] Downloading click-dog..."
    resolve_binary

    CONFIG_TMPFILE=""
    SECRET_TMPFILE=""
    UNIT_TMPFILE=""
    trap 'rm -f "$CONFIG_TMPFILE" "$SECRET_TMPFILE" "$UNIT_TMPFILE"; cleanup_auto_downloaded_binary' EXIT

    # System user + dirs
    if id click-dog &>/dev/null 2>&1; then
        echo "  [2/5] Using existing click-dog system account..."
    else
        echo "  [2/5] Creating click-dog system account (for running the service)..."
        useradd --system --no-create-home --shell /usr/sbin/nologin click-dog
    fi
    mkdir -p /etc/click-dog /var/log/click-dog
    chown click-dog:click-dog /etc/click-dog /var/log/click-dog
    chmod 750 /etc/click-dog /var/log/click-dog

    # Config + secret
    echo "  [3/5] Writing config..."
    SYSTEMD="true"
    CONFIG_TMPFILE=$(mktemp)
    SECRET_TMPFILE=$(mktemp)
    UNIT_TMPFILE=$(mktemp)

    require_unified_init_binary
    render_click_dog_config "$CONFIG_TMPFILE"

    # Raw password bytes (no shell quoting, no KEY= prefix) — click-dog reads
    # the file verbatim via clickhouse.password_file. See the install path.
    printf '%s' "$CLICKHOUSE_PASSWORD" > "$SECRET_TMPFILE"

    # Back up existing config files before overwriting so we can restore on failure
    local config_backed_up="false"
    if [[ -f /etc/click-dog/click-dog.yaml ]]; then
        cp /etc/click-dog/click-dog.yaml /etc/click-dog/click-dog.yaml.bak
        config_backed_up="true"
    fi
    if [[ -f /etc/click-dog/.secret ]]; then
        cp /etc/click-dog/.secret /etc/click-dog/.secret.bak
    fi

    cp "$CONFIG_TMPFILE" /etc/click-dog/click-dog.yaml
    cp "$SECRET_TMPFILE" /etc/click-dog/.secret
    chown click-dog:click-dog /etc/click-dog/click-dog.yaml /etc/click-dog/.secret
    chmod 640 /etc/click-dog/click-dog.yaml
    chmod 600 /etc/click-dog/.secret

    # Validate — restore backups on failure
    if ! "$BINARY" -validate -config /etc/click-dog/click-dog.yaml >/dev/null 2>&1; then
        echo "  Error: config validation failed" >&2
        "$BINARY" -validate -config /etc/click-dog/click-dog.yaml >&2 || true
        # Restore previous config if we had one, otherwise clean up
        if [[ "$config_backed_up" == "true" ]]; then
            echo "  Restoring previous config..." >&2
            mv /etc/click-dog/click-dog.yaml.bak /etc/click-dog/click-dog.yaml 2>/dev/null || true
            mv /etc/click-dog/.secret.bak /etc/click-dog/.secret 2>/dev/null || true
        else
            rm -f /etc/click-dog/click-dog.yaml /etc/click-dog/.secret
        fi
        exit 1
    fi

    # Validation passed — remove backups
    rm -f /etc/click-dog/click-dog.yaml.bak /etc/click-dog/.secret.bak

    # Binary + systemd
    echo "  [4/5] Installing binary and systemd unit..."
    chmod 755 "$BINARY"
    if ! "$BINARY" -version >/dev/null 2>&1; then
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
    echo "  [5/5] Starting click-dog..."
    systemctl restart click-dog
    sleep 2

    # ═══ Done ══════════════════════════════════════════════════
    echo ""
    echo "════════════════════════════════════════════════════════════"
    if systemctl is-active click-dog >/dev/null 2>&1; then
        if ((${#clickhouse_setup_tempfiles[@]} > 0)); then
            rm -f "${clickhouse_setup_tempfiles[@]}"
        fi
        echo "  click-dog is running."
        echo "════════════════════════════════════════════════════════════"
        echo ""
        local docs_url="https://click-dog.com"
        local installed_version
        installed_version=$(/usr/local/bin/click-dog -version 2>/dev/null | awk '{print $NF}') || true
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
        echo "    vi /etc/click-dog/click-dog.yaml && systemctl restart click-dog"
        echo "    Full reference: ${docs_url}/configuration/${v}"
        echo "    Datadog guide: ${docs_url}/integrations/datadog/${v}"
    else
        systemctl stop click-dog 2>/dev/null || true
        echo "  Installed, but not yet running."
        echo "════════════════════════════════════════════════════════════"
        echo ""
        echo "  We tried starting the service as a courtesy but it needs"
        echo "  a working ClickHouse user first. Most likely the"
        echo "  '${CLICKHOUSE_USER}' user hasn't been created yet."
        echo ""
        echo "  To finish setup:"
        echo "    1. Create the ClickHouse user (if not done already):"
        if [[ -n "${setup_sql:-}" ]]; then
            echo "       ${CLICKHOUSE_CLIENT} -u admin --multiquery < ${setup_sql}"
        else
            echo "       Create/grant the '${CLICKHOUSE_USER}' user in ClickHouse using your admin workflow."
        fi
        echo "    2. Start the service:"
        echo "       systemctl restart click-dog"
        echo ""
        echo "  Config:  /etc/click-dog/click-dog.yaml"
        echo "  Logs:    journalctl -u click-dog -f"
    fi
    echo ""
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
