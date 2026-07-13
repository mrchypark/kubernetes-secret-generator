#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ksg-release.XXXXXX")
trap 'rm -rf "$tmp"' 0 1 2 15
mkdir -p "$tmp/scripts" "$tmp/deploy/helm-chart/kubernetes-secret-generator"
cp "$root/scripts/validate-release.sh" "$tmp/scripts/"
cp "$root/scripts/verify-signing-identity.sh" "$tmp/scripts/"
cp "$root/deploy/helm-chart/kubernetes-secret-generator/Chart.yaml" "$root/deploy/helm-chart/kubernetes-secret-generator/values.yaml" "$tmp/deploy/helm-chart/kubernetes-secret-generator/"
git -C "$tmp" init -q
git -C "$tmp" config user.name test
git -C "$tmp" config user.email test@example.invalid
git -C "$tmp" add .
git -C "$tmp" commit -qm test
sha=$(git -C "$tmp" rev-parse HEAD)

run() { (cd "$tmp" && RELEASE_TAG=$1 SOURCE_COMMIT=$2 scripts/validate-release.sh); }

git -C "$tmp" tag v4.0.0-rc.13
run v4.0.0-rc.13 "$sha"
if run v4.0 "$sha" >/dev/null 2>&1; then echo 'invalid tag accepted' >&2; exit 1; fi
if run v4.0.0-rc.13 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null 2>&1; then echo 'wrong source accepted' >&2; exit 1; fi

sed -i.bak 's/^version: 4.0.0-rc.13$/version: 4.0.0/; s/^appVersion: v4.0.0-rc.13$/appVersion: v4.0.0/' "$tmp/deploy/helm-chart/kubernetes-secret-generator/Chart.yaml"
sed -i.bak 's/tag: v4.0.0-rc.13/tag: v4.0.0/' "$tmp/deploy/helm-chart/kubernetes-secret-generator/values.yaml"
git -C "$tmp" add .
git -C "$tmp" commit -qm stable
sha=$(git -C "$tmp" rev-parse HEAD)
git -C "$tmp" tag v4.0.0
run v4.0.0 "$sha"

sed -i.bak 's/^version: 4.0.0$/version: 4.0.1/' "$tmp/deploy/helm-chart/kubernetes-secret-generator/Chart.yaml"
if run v4.0.0 "$sha" >/dev/null 2>&1; then echo 'chart mismatch accepted' >&2; exit 1; fi

identity=https://github.com/mrchypark/kubernetes-secret-generator/.github/workflows/release-candidate.yml@refs/tags/v4.0.0-rc.13
sh scripts/verify-signing-identity.sh "$identity" https://token.actions.githubusercontent.com v4.0.0-rc.13
old_identity=https://github.com/mrchypark/kubernetes-secret-generator/.github/workflows/workflow.yml@refs/tags/v4.0.0-rc.13
if sh scripts/verify-signing-identity.sh "$old_identity" https://token.actions.githubusercontent.com v4.0.0-rc.13 >/dev/null 2>&1; then
	echo 'deleted workflow signing identity was accepted' >&2
	exit 1
fi

echo 'release contract checks passed'
