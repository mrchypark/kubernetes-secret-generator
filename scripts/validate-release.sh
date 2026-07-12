#!/bin/sh
set -eu

[ "$#" -eq 0 ] || { echo 'usage: validate-release.sh' >&2; exit 2; }

tag=${RELEASE_TAG:-}
source=${SOURCE_COMMIT:-}
printf '%s\n' "$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-rc\.(0|[1-9][0-9]*))?$' || {
	echo 'RELEASE_TAG must be vMAJOR.MINOR.PATCH or vMAJOR.MINOR.PATCH-rc.N' >&2
	exit 1
}
printf '%s\n' "$source" | grep -Eq '^[0-9a-f]{40}$' || { echo 'SOURCE_COMMIT must be 40 lowercase hex characters' >&2; exit 1; }
[ "$(git rev-list -n 1 "$tag")" = "$source" ] || { echo 'release tag does not resolve to SOURCE_COMMIT' >&2; exit 1; }
[ "$(git rev-parse HEAD)" = "$source" ] || { echo 'checked-out source does not match SOURCE_COMMIT' >&2; exit 1; }

version=${tag#v}
chart=deploy/helm-chart/kubernetes-secret-generator/Chart.yaml
values=deploy/helm-chart/kubernetes-secret-generator/values.yaml
[ "$(awk '$1 == "version:" { print $2; exit }' "$chart")" = "$version" ] || { echo 'Chart version must match RELEASE_TAG' >&2; exit 1; }
[ "$(awk '$1 == "appVersion:" { print $2; exit }' "$chart")" = "$tag" ] || { echo 'Chart appVersion must match RELEASE_TAG' >&2; exit 1; }
[ "$(awk '$1 == "image:" { image=1; next } image && $1 == "tag:" { print $2; exit } image && /^[^[:space:]]/ { exit }' "$values")" = "$tag" ] || {
	echo 'values image.tag must match RELEASE_TAG' >&2
	exit 1
}
