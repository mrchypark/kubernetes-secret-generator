#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
observer=$repo_root/test/e2e/recreate-observer.sh
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-recreate-observer.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15

run() {
	name=$1
	shift
	printf '%s\n' "$@" | OLD_UID=old READY_FILE="$tmpdir/$name.ready" SUMMARY_FILE="$tmpdir/$name.json" "$observer"
}

run valid '["old"]' '["old"]' '[]' '[]' '["new"]' '["new"]'
jq -e '.samples == 6 and .maxPods == 1 and .zeroObserved == true and .oldUID == "old" and .newUID == "new" and .order == ["old","zero","new"]' \
	"$tmpdir/valid.json" >/dev/null
[ -f "$tmpdir/valid.ready" ] || { printf '%s\n' 'error: valid observation did not signal readiness' >&2; exit 2; }

reject() {
	name=$1
	shift
	if run "$name" "$@" >"$tmpdir/$name.out" 2>"$tmpdir/$name.err"; then
		printf 'error: recreate observer accepted negative fixture: %s\n' "$name" >&2
		exit 2
	fi
	[ ! -e "$tmpdir/$name.json" ] || { printf 'error: rejected fixture produced a summary: %s\n' "$name" >&2; exit 2; }
}

reject missing-old '[]' '["new"]'
reject invalid-snapshot 'not-json'
reject overlap '["old"]' '["old","new"]'
reject new-before-zero '["old"]' '["new"]'
reject old-reappeared '["old"]' '[]' '["old"]'
reject replacement-changed '["old"]' '[]' '["new"]' '["other"]'
reject replacement-disappeared '["old"]' '[]' '["new"]' '[]'
reject no-replacement '["old"]' '[]'
