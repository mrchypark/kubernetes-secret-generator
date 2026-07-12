#!/bin/sh
set -eu

action=${1:-}
case "$action" in object|manifests|verify) ;; *) echo 'usage: generate.sh object|manifests|verify' >&2; exit 2 ;; esac
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
controller_gen=${CONTROLLER_GEN:-controller-gen}
expected=$(awk -F= '$1 == "controller-gen.version" { print $2; count++ } END { if (count != 1) exit 1 }' "$repo_root/tools.lock")
[ "$($controller_gen --version)" = "Version: $expected" ] || { echo "controller-gen $expected is required" >&2; exit 2; }
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-generate.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15

generate_object() {
	mkdir -p "$tmp_dir/object"
	(
		cd "$repo_root"
		"$controller_gen" object:headerFile=/dev/null paths=./pkg/apis/secretgenerator/v1alpha1 output:dir="$tmp_dir/object"
	)
}

generate_manifests() {
	mkdir -p "$tmp_dir/crds"
	(
		cd "$repo_root"
		"$controller_gen" crd paths=./pkg/apis/secretgenerator/v1alpha1 output:crd:dir="$tmp_dir/crds"
	)
}

install_manifests() {
	cp "$tmp_dir/crds/secretgenerator.mittwald.de_basicauths.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_basicauths_crd.yaml"
	cp "$tmp_dir/crds/secretgenerator.mittwald.de_sshkeypairs.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_sshkeypairs_crd.yaml"
	cp "$tmp_dir/crds/secretgenerator.mittwald.de_stringsecrets.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_stringsecrets_crd.yaml"
}

compare_manifests() {
	cmp -s "$tmp_dir/crds/secretgenerator.mittwald.de_basicauths.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_basicauths_crd.yaml" &&
	cmp -s "$tmp_dir/crds/secretgenerator.mittwald.de_sshkeypairs.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_sshkeypairs_crd.yaml" &&
	cmp -s "$tmp_dir/crds/secretgenerator.mittwald.de_stringsecrets.yaml" "$repo_root/deploy/crds/secretgenerator.mittwald.de_stringsecrets_crd.yaml"
}

case "$action" in
	object)
		generate_object
		cp "$tmp_dir/object/zz_generated.deepcopy.go" "$repo_root/pkg/apis/secretgenerator/v1alpha1/zz_generated.deepcopy.go"
		;;
	manifests)
		generate_manifests
		install_manifests
		;;
	verify)
		generate_object
		generate_manifests
		cmp -s "$tmp_dir/object/zz_generated.deepcopy.go" "$repo_root/pkg/apis/secretgenerator/v1alpha1/zz_generated.deepcopy.go" || { echo 'generated deepcopy is stale; run make generate' >&2; exit 1; }
		compare_manifests || { echo 'generated CRDs are stale; run make manifests' >&2; exit 1; }
		printf 'generated artifacts match controller-gen %s\n' "$expected"
		;;
esac
