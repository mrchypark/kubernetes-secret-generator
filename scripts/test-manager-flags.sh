#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-manager-flags.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15

(cd "$repo_root" && go build -trimpath -o "$tmpdir/manager" ./cmd/manager)
for removed in --leader-elect=true --leader-election-id=kubernetes-secret-generator-lock; do
	if "$tmpdir/manager" "$removed" >"$tmpdir/stdout" 2>"$tmpdir/stderr"; then
		printf 'error: removed manager flag was accepted: %s\n' "$removed" >&2
		exit 2
	fi
	grep -F -q 'unknown flag' "$tmpdir/stderr" || {
		printf 'error: removed manager flag did not fail as unknown: %s\n' "$removed" >&2
		exit 2
	}
done
