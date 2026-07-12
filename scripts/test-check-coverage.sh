#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ksg-coverage.XXXXXX")
trap 'rm -rf "$tmp"' 0 1 2 15

profile() {
	first_covered=$1 extra_count=$2
	output=$3
	{
		printf '%s\n' 'mode: set'
		printf 'example.test/repo/pkg/controller/crd/a.go:1.1,1.2 %s 1\n' "$first_covered"
		printf 'example.test/repo/pkg/controller/crd/a.go:2.1,2.2 %s 0\n' "$((10 - first_covered))"
		for package in controller/crd/stringsecret controller/crd/basicauth controller/crd/sshkeypair controller/secret apis/secretgenerator/v1alpha1; do
			printf 'example.test/repo/pkg/%s/a.go:1.1,1.2 9 1\n' "$package"
			printf 'example.test/repo/pkg/%s/a.go:2.1,2.2 1 0\n' "$package"
		done
		printf 'example.test/repo/cmd/manager/main.go:1.1,1.2 20 %s\n' "$extra_count"
	} >"$output"
}

profile 9 1 "$tmp/pass.out"
"$repo_root/scripts/check-coverage.sh" "$tmp/pass.out" >/dev/null
profile 8 1 "$tmp/package-fail.out"
if "$repo_root/scripts/check-coverage.sh" "$tmp/package-fail.out" >/dev/null; then
	printf '%s\n' 'coverage check accepted a critical package below 90%' >&2
	exit 1
fi
profile 9 0 "$tmp/overall-fail.out"
if "$repo_root/scripts/check-coverage.sh" "$tmp/overall-fail.out" >/dev/null; then
	printf '%s\n' 'coverage check accepted overall coverage below 80%' >&2
	exit 1
fi
sed '/pkg\/controller\/secret/d' "$tmp/pass.out" >"$tmp/missing.out"
if "$repo_root/scripts/check-coverage.sh" "$tmp/missing.out" >/dev/null; then
	printf '%s\n' 'coverage check accepted a missing critical package' >&2
	exit 1
fi

printf '%s\n' 'coverage threshold checks passed'
