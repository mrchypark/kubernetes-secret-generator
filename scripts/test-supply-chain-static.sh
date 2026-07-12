#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ksg-supply.XXXXXX")
trap 'rm -rf "$tmp"' 0 1 2 15
base=$tmp/base
mkdir -p "$base/.github/workflows" "$base/build" "$base/scripts" "$base/deploy/helm-chart/kubernetes-secret-generator"
cp "$root/.github/workflows/"*.yml "$base/.github/workflows/"
cp "$root/build/Dockerfile" "$base/build/"
cp "$root/scripts/helm-release.sh" "$root/scripts/verify-signing-identity.sh" "$base/scripts/"
cp "$root/deploy/helm-chart/kubernetes-secret-generator/Chart.yaml" "$root/deploy/helm-chart/kubernetes-secret-generator/values.yaml" "$base/deploy/helm-chart/kubernetes-secret-generator/"
cp "$root/Makefile" "$base/"
verify=$root/scripts/verify-supply-chain-static
"$verify" "$base" >/dev/null

reject() {
	name=$1
	shift
	fixture=$tmp/$name
	mkdir "$fixture"
	cp -R "$base/." "$fixture/"
	"$@" "$fixture"
	if "$verify" "$fixture" >/dev/null 2>&1; then
		echo "$name fixture was accepted" >&2
		exit 1
	fi
}

extra_workflow() { printf 'jobs: {}\n' >"$1/.github/workflows/extra.yml"; }
unpinned_action() { sed -i.bak 's/@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0/@v7/' "$1/.github/workflows/ci.yml"; }
ci_write() { sed -i.bak 's/contents: read/contents: write/' "$1/.github/workflows/ci.yml"; }
wrong_platforms() { sed -i.bak 's#platforms: linux/amd64,linux/arm64#platforms: linux/amd64#' "$1/.github/workflows/release-candidate.yml"; }
auto_promote() { sed -i.bak 's/workflow_dispatch:/push:/' "$1/.github/workflows/release-promote.yml"; }
missing_concurrency() { sed -i.bak '/group: promote-/d' "$1/.github/workflows/release-promote.yml"; }
promotion_rebuild() { awk '{print} /scripts\/validate-release.sh/{print "          helm package deploy/helm-chart/kubernetes-secret-generator"}' "$1/.github/workflows/release-promote.yml" >"$1/workflow.tmp" && mv "$1/workflow.tmp" "$1/.github/workflows/release-promote.yml"; }
force_conflicts() { printf '\nkubectl apply --server-side --force-conflicts\n' >>"$1/scripts/helm-release.sh"; }

reject extra-workflow extra_workflow
reject unpinned-action unpinned_action
reject ci-write ci_write
reject wrong-platforms wrong_platforms
reject auto-promote auto_promote
reject missing-concurrency missing_concurrency
reject promotion-rebuild promotion_rebuild
reject force-conflicts force_conflicts

echo 'supply-chain static negative fixtures passed'
