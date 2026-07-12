#!/bin/sh
set -eu

[ "$#" -eq 2 ] || { printf '%s\n' 'usage: merge-coverprofile.sh INPUT OUTPUT' >&2; exit 2; }
input=$1
output=$2
tmp=$(mktemp "${TMPDIR:-/tmp}/ksg-cover.XXXXXX")
trap 'rm -f "$tmp"' 0 1 2 15

mode=$(sed -n '1s/^mode: //p' "$input")
[ "$mode" = set ] || { printf '%s\n' 'coverage profile must use set mode' >&2; exit 2; }
awk 'NR > 1 {
	key = $1 " " $2
	if (!(key in covered) || $3 > covered[key]) covered[key] = $3
}
END {
	for (key in covered) print key, covered[key]
}' "$input" | LC_ALL=C sort >"$tmp"
{
	printf 'mode: set\n'
	cat "$tmp"
} >"$output"
