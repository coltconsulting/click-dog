#!/usr/bin/env bash
set -uo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Resolved from this script's repository root.
# shellcheck disable=SC1091
source "$ROOT/scripts/tool-versions.env"
agent_cache="$ROOT/.agent-cache"

WARNINGS=0
ERRORS=0

pass() { printf '  [ok]   %s\n' "$1"; }
warn() { printf '  [warn] %s\n' "$1"; WARNINGS=$((WARNINGS + 1)); }
fail() { printf '  [fail] %s\n' "$1"; ERRORS=$((ERRORS + 1)); }

prefer_local() {
    local name="$1" local_path="$2"
    if [[ -x "$local_path" ]]; then
        printf '%s\n' "$local_path"
    else
        command -v "$name" 2>/dev/null || true
    fi
}

version_contains() {
    local name="$1" command_path="$2" expected="$3"
    shift 3
    if [[ -z "$command_path" ]]; then
        warn "$name missing (run: make dev-setup)"
        return
    fi
    local output summary
    output="$("$command_path" "$@" 2>&1)"
    summary="$(printf '%s' "$output" | tr '\n' ' ' | sed 's/[[:space:]][[:space:]]*/ /g')"
    if [[ "$output" == *"${expected#v}"* ]]; then
        pass "$name $expected"
    else
        warn "$name expected $expected; found: $summary"
    fi
}

printf 'click-dog doctor\n'

for required in git go make bash; do
    if command -v "$required" >/dev/null 2>&1; then
        pass "$required available"
    else
        fail "$required missing"
    fi
done

if command -v go >/dev/null 2>&1; then
    toolchain_go="$(find "$agent_cache/go-mod/golang.org" -maxdepth 3 -type f -path "*/toolchain@v0.0.1-go${GO_VERSION}.*/*/go" -print -quit 2>/dev/null || true)"
    if [[ -n "$toolchain_go" ]]; then
        actual_go="$($toolchain_go env GOVERSION 2>/dev/null || true)"
    else
        actual_go="$(go env GOVERSION 2>/dev/null || true)"
    fi
    if [[ "$actual_go" == "go$GO_VERSION" ]]; then
        pass "Go $GO_VERSION"
    else
        fail "Go $GO_VERSION required; found ${actual_go:-unknown}"
    fi
fi

branch="$(git -C "$ROOT" symbolic-ref --quiet --short HEAD 2>/dev/null || true)"
if [[ -z "$branch" ]]; then
    warn "HEAD is detached; create a named feature branch before committing"
elif [[ "$branch" == "master" || "$branch" == "main" ]]; then
    warn "on protected branch $branch; use an agent-prefixed feature branch for changes"
else
    pass "feature branch $branch"
fi

origin="$(git -C "$ROOT" remote get-url origin 2>/dev/null || true)"
if [[ "$origin" == *"coltconsulting/click-dog-internal"* ]]; then
    pass "origin is the internal development repository"
elif [[ -z "$origin" ]]; then
    warn "origin is not configured"
else
    warn "origin is not the internal development repository: $origin"
fi

# Makefile.internal is export-ignored, so this policy applies only to the
# development repository. Public contributor forks must retain ordinary push
# remotes, while an internal worktree should never be able to push to the
# official public release target.
if [[ -f "$ROOT/Makefile.internal" ]]; then
    # shellcheck source=scripts/lib/development-remote-policy.sh
    source "$ROOT/scripts/lib/development-remote-policy.sh"
    unsafe_public_remotes="$(push_enabled_public_remotes "$ROOT")"
    if [[ -n "$unsafe_public_remotes" ]]; then
        unsafe_public_remotes="$(printf '%s' "$unsafe_public_remotes" | paste -sd, -)"
        fail "official public repository is push-enabled via remote(s): $unsafe_public_remotes"
    else
        pass "official public repository remotes are fetch-only"
    fi
fi

if [[ -n "$(git -C "$ROOT" status --porcelain 2>/dev/null)" ]]; then
    warn "working tree has changes; preserve unrelated work"
else
    pass "working tree clean"
fi

default_cache="$(GOTOOLCHAIN=local go env GOCACHE 2>/dev/null || true)"
if [[ -n "$default_cache" ]]; then
    cache_probe="$default_cache/.click-dog-write-probe.$$"
    if ( : > "$cache_probe" ) 2>/dev/null; then
        rm -f "$cache_probe"
        pass "default Go build cache is writable"
    else
        warn "default Go build cache is not writable; set GOCACHE or run in a normal environment"
    fi
fi

if mkdir -p "$agent_cache/go-build" 2>/dev/null; then
    pass "workspace-local agent cache is writable"
else
    fail "cannot create workspace-local agent cache"
fi

lint_bin="$(prefer_local golangci-lint "$agent_cache/bin/golangci-lint")"
vuln_bin="$(prefer_local govulncheck "$agent_cache/bin/govulncheck")"
actionlint_bin="$(prefer_local actionlint "$agent_cache/bin/actionlint")"
kubeconform_bin="$(prefer_local kubeconform "$agent_cache/bin/kubeconform")"
shellcheck_bin="$(prefer_local shellcheck "$agent_cache/bin/shellcheck")"
openspec_bin="$(prefer_local openspec "$agent_cache/npm/node_modules/.bin/openspec")"

version_contains golangci-lint "$lint_bin" "$GOLANGCI_LINT_VERSION" version
version_contains govulncheck "$vuln_bin" "$GOVULNCHECK_VERSION" -version
version_contains actionlint "$actionlint_bin" "$ACTIONLINT_VERSION" -version
if [[ "$kubeconform_bin" == "$agent_cache/bin/kubeconform" ]]; then
    pass "kubeconform $KUBECONFORM_VERSION (source-pinned local build)"
else
    version_contains kubeconform "$kubeconform_bin" "$KUBECONFORM_VERSION" -v
fi
version_contains shellcheck "$shellcheck_bin" "$SHELLCHECK_VERSION" --version
version_contains OpenSpec "$openspec_bin" "$OPENSPEC_VERSION" --version
if [[ -f "$ROOT/.beads/issues.jsonl" ]]; then
    bd_bin="$(prefer_local bd "$agent_cache/bin/bd")"
    version_contains bd "$bd_bin" "$BD_VERSION" --version
    if [[ -x "$ROOT/scripts/bd-local.sh" ]] && "$ROOT/scripts/bd-local.sh" show cd-9 --json >/dev/null 2>&1; then
        pass "repository-local bead tracker is readable and isolated"
    else
        warn "repository-local bead tracker unavailable (run: make dev-setup)"
    fi
fi

if [[ -x "$agent_cache/venv/bin/python" ]]; then
    material_version="$("$agent_cache/venv/bin/python" -c 'from importlib.metadata import version; print(version("mkdocs-material"))' 2>/dev/null || true)"
    if [[ "$material_version" == "$MKDOCS_MATERIAL_VERSION" ]]; then
        pass "MkDocs Material $MKDOCS_MATERIAL_VERSION"
    else
        warn "MkDocs Material expected $MKDOCS_MATERIAL_VERSION; found ${material_version:-unknown}"
    fi
elif [[ -f "$ROOT/requirements-docs.txt" ]]; then
    warn "pinned MkDocs environment missing (run: make dev-setup)"
fi

for optional in docker kubectl python3 npm; do
    if command -v "$optional" >/dev/null 2>&1; then
        pass "$optional available"
    else
        warn "$optional missing (needed only for its corresponding extended checks)"
    fi
done

printf '\nDoctor result: %d error(s), %d warning(s)\n' "$ERRORS" "$WARNINGS"
[[ "$ERRORS" -eq 0 ]]
