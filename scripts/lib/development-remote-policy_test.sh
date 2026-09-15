#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
# shellcheck source=scripts/lib/development-remote-policy.sh
. "$ROOT/scripts/lib/development-remote-policy.sh"

TMP_DIR="$(mktemp -d "${TMPDIR:-/tmp}/click-dog-remote-policy-test.XXXXXX")"
trap 'rm -rf "$TMP_DIR"' EXIT

repo="$TMP_DIR/repo"
git init -q "$repo"
git -C "$repo" remote add origin https://github.com/coltconsulting/click-dog-internal.git
git -C "$repo" remote add public https://github.com/coltconsulting/click-dog.git
git -C "$repo" remote set-url --push public DISABLED_BY_POLICY

assert_public_url() {
	local label="$1" url="$2"
	if ! is_official_public_repository_url "$url"; then
		echo "FAIL: official public URL was not detected ($label)" >&2
		exit 1
	fi
}

assert_not_public_url() {
	local label="$1" url="$2"
	if is_official_public_repository_url "$url"; then
		echo "FAIL: non-public URL was reported as official ($label)" >&2
		exit 1
	fi
}

assert_public_url "HTTPS" "https://github.com/coltconsulting/click-dog.git"
assert_public_url "HTTPS userinfo" "https://TOKEN_PLACEHOLDER:x-oauth-basic@github.com/coltconsulting/click-dog.git"
assert_public_url "mixed case" "HTTPS://GitHub.Com/ColtConsulting/Click-Dog.GIT"
assert_public_url "HTTP port" "http://github.com:80/coltconsulting/click-dog"
assert_public_url "FQDN trailing dot" "https://github.com./coltconsulting/click-dog.git"
assert_public_url "www host" "https://www.github.com/coltconsulting/click-dog.git"
assert_public_url "empty port" "https://github.com:/coltconsulting/click-dog.git"
assert_public_url "repeated leading separators" "https://github.com//coltconsulting/click-dog.git"
assert_public_url "repeated path separators" "https://github.com/coltconsulting//click-dog.git"
assert_public_url "userinfo mixed-case www and empty port" "https://GIT@WWW.GITHUB.COM:/coltconsulting/click-dog.git"
assert_public_url "userinfo FQDN and leading separators" "https://git@www.github.com.//coltconsulting/click-dog.git"
assert_public_url "userinfo empty port and path separators" "https://git@www.github.com:/coltconsulting//click-dog.git"
assert_public_url "scp-like SSH" "git@github.com:coltconsulting/click-dog.git"
assert_public_url "scp-like SSH mixed case" "GIT@GITHUB.COM:COLTCONSULTING/CLICK-DOG"
assert_public_url "scp-like SSH FQDN trailing dot" "git@github.com.:coltconsulting/click-dog.git"
assert_public_url "scp-like SSH www host" "git@www.github.com:coltconsulting/click-dog.git"
assert_public_url "scp-like SSH mixed-case combined alias" "GIT@WWW.GITHUB.COM.:COLTCONSULTING/CLICK-DOG.GIT"
assert_public_url "scp-like SSH leading separator" "git@github.com:/coltconsulting/click-dog.git"
assert_public_url "scp-like SSH path separators" "git@github.com:coltconsulting//click-dog.git"
assert_public_url "SSH URL" "ssh://git@github.com/coltconsulting/click-dog.git"
assert_public_url "SSH URL explicit port" "ssh://git@github.com:22/coltconsulting/click-dog.git"
assert_public_url "Git protocol" "git://github.com/coltconsulting/click-dog/"

assert_not_public_url "disabled sentinel" "DISABLED_BY_POLICY"
assert_not_public_url "internal repository" "https://github.com/coltconsulting/click-dog-internal.git"
assert_not_public_url "sibling repository" "https://github.com/coltconsulting/click-dog-tools.git"
assert_not_public_url "sibling owner" "https://github.com/other/click-dog.git"
assert_not_public_url "lookalike HTTPS host" "https://github.com.evil.test/coltconsulting/click-dog.git"
assert_not_public_url "userinfo before lookalike host" "https://github.com@evil.test/coltconsulting/click-dog.git"
assert_not_public_url "lookalike scp host" "git@github.com.evil.test:coltconsulting/click-dog.git"
assert_not_public_url "lookalike scp www host" "git@www.github.com.evil.test:coltconsulting/click-dog.git"
assert_not_public_url "lookalike scp combined alias" "GIT@WWW.GITHUB.COM.EVIL.TEST.:COLTCONSULTING/CLICK-DOG.GIT"
assert_not_public_url "scp alias sibling repository" "git@www.github.com.:coltconsulting/click-dog-internal.git"
assert_not_public_url "trailing-dot lookalike host" "https://github.com.evil.test./coltconsulting/click-dog.git"
assert_not_public_url "www lookalike host" "https://www.github.com.evil.test/coltconsulting/click-dog.git"
assert_not_public_url "empty-port lookalike host" "https://github.com.evil.test:/coltconsulting/click-dog.git"
assert_not_public_url "combined lookalike host" "HTTPS://GitHub.Com@WWW.GITHUB.COM.EVIL.TEST.://COLTCONSULTING///CLICK-DOG.GIT"
assert_not_public_url "multiple port separators" "https://github.com::/coltconsulting/click-dog.git"
assert_not_public_url "repository suffix" "https://github.com/coltconsulting/click-dog.git.evil"
assert_not_public_url "nested repository path" "https://github.com/coltconsulting/click-dog/extra"
assert_not_public_url "repeated separators sibling repository" "https://github.com///coltconsulting//click-dog-internal.git"
assert_not_public_url "repeated separators nested repository" "https://github.com/coltconsulting//nested//click-dog.git"
assert_not_public_url "repeated separators on lookalike host" "https://evil.test//github.com//coltconsulting/click-dog.git"
assert_not_public_url "scp-like SSH double leading separator" "git@github.com://coltconsulting/click-dog.git"
assert_not_public_url "non-numeric port" "ssh://git@github.com:ssh/coltconsulting/click-dog.git"
assert_not_public_url "unknown scheme" "ftp://github.com/coltconsulting/click-dog.git"

if [ -n "$(push_enabled_public_remotes "$repo")" ]; then
	echo "FAIL: fetch-only public remote was reported as push-enabled" >&2
	exit 1
fi

git -C "$repo" remote set-url --push public https://TOKEN_PLACEHOLDER:x-oauth-basic@GitHub.Com/ColtConsulting/Click-Dog.git
if [ "$(push_enabled_public_remotes "$repo")" != "public" ]; then
	echo "FAIL: credential-bearing public push target was not detected" >&2
	exit 1
fi

git -C "$repo" remote set-url --push public DISABLED_BY_POLICY
git -C "$repo" remote set-url --add --push origin https://github.com/coltconsulting/click-dog.git
if [ "$(push_enabled_public_remotes "$repo")" != "origin" ]; then
	echo "FAIL: public pushurl behind an internal fetch URL was not detected" >&2
	exit 1
fi

echo "development remote policy tests: PASS"
