#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-safety.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15

printf '%s\n' 'ordinary test output' >"$tmp_dir/safe.log"
cache_dir="$tmp_dir/cache"
CACHED_GO_DEBUG=1 CODEX_GO_SCRIPT_CACHE="$cache_dir" "$repo_root/scripts/check-test-artifacts.sh" "$tmp_dir/safe.log" >/dev/null 2>"$tmp_dir/scanner-first"
CACHED_GO_DEBUG=1 CODEX_GO_SCRIPT_CACHE="$cache_dir" "$repo_root/scripts/check-test-artifacts.sh" "$tmp_dir/safe.log" >/dev/null 2>"$tmp_dir/scanner-second"
grep -F 'building ' "$tmp_dir/scanner-first" >/dev/null || { printf '%s\n' 'artifact scanner did not build on first cache use' >&2; exit 1; }
if grep -F 'building ' "$tmp_dir/scanner-second" >/dev/null; then
	printf '%s\n' 'artifact scanner rebuilt instead of reusing its cache' >&2
	exit 1
fi
"$repo_root/scripts/check-test-artifacts.sh" "$tmp_dir/safe.log"
mkdir "$tmp_dir/safe-dir"
printf '\000ordinary binary output\n' >"$tmp_dir/safe-dir/safe.bin"
"$repo_root/scripts/check-test-artifacts.sh" --stage "$tmp_dir/staged" "$tmp_dir/safe-dir"
cmp "$tmp_dir/safe-dir/safe.bin" "$tmp_dir/staged/safe-dir/safe.bin"

reject_artifact() {
	file=$1
	if "$repo_root/scripts/check-test-artifacts.sh" "$file" >"$tmp_dir/scanner.stdout" 2>"$tmp_dir/scanner.stderr"; then
		printf 'artifact scanner accepted unsafe fixture %s\n' "$(basename "$file")" >&2
		exit 1
	fi
	[ ! -s "$tmp_dir/scanner.stdout" ] || { printf '%s\n' 'artifact scanner wrote unsafe data to stdout' >&2; exit 1; }
}

printf '%s\n' 'KSG_TEST_SECRET_must-not-upload' >"$tmp_dir/sentinel.log"
reject_artifact "$tmp_dir/sentinel.log"
printf '\000\001KSG_RUN_SENTINEL_binary\002' >"$tmp_dir/binary.log"
reject_artifact "$tmp_dir/binary.log"
printf '%s\n' '-----BEGIN PRIVATE KEY-----' >"$tmp_dir/pem.log"
reject_artifact "$tmp_dir/pem.log"
dollar='$'
printf '%s\n' "${dollar}2a${dollar}10${dollar}N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy" >"$tmp_dir/bcrypt.log"
reject_artifact "$tmp_dir/bcrypt.log"
printf '%s\n' 'secretgenerator.mittwald.de/managed-data-checksums: redacted' >"$tmp_dir/checksum.log"
reject_artifact "$tmp_dir/checksum.log"
printf '%s\n' 'password: plaintext-fixture' >"$tmp_dir/password.log"
reject_artifact "$tmp_dir/password.log"

ln -s "$tmp_dir/safe.log" "$tmp_dir/escape-link"
reject_artifact "$tmp_dir/escape-link"
mkfifo "$tmp_dir/non-regular"
reject_artifact "$tmp_dir/non-regular"
printf '%s\n' 'unreadable' >"$tmp_dir/unreadable"
chmod 000 "$tmp_dir/unreadable"
reject_artifact "$tmp_dir/unreadable"

if "$repo_root/scripts/check-test-artifacts.sh" --stage "$tmp_dir/blocked-stage" "$tmp_dir/sentinel.log" >/dev/null 2>&1; then
	printf '%s\n' 'artifact staging accepted unsafe input' >&2
	exit 1
fi
[ ! -e "$tmp_dir/blocked-stage" ] || { printf '%s\n' 'unsafe artifact stage was retained' >&2; exit 1; }

if KUBE_CONTEXT=one CONFIRM_CONTEXT=two NAMESPACE=ksg-system RELEASE_NAME=ksg \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	printf '%s\n' 'deployment guard accepted mismatched context confirmation' >&2
	exit 1
fi

printf '%s\n' 'safety checks passed'
