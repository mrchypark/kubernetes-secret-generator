#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
fixture_dir=$repo_root/test/fixtures/v3.4.1/crds
source_file=$repo_root/test/fixtures/v3.4.1/SOURCE
release_lock=$repo_root/test/fixtures/v3.4.1/release-lock.json
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-n1-fixture.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }

command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v openssl >/dev/null 2>&1 || fail 'openssl is required'
command -v helm >/dev/null 2>&1 || fail 'helm is required'

commit=$(awk -F= '$1 == "crd.commit" { print $2 }' "$source_file")
case "$commit" in ????????????????????????????????????????) ;; *) fail 'N-1 fixture source commit is malformed' ;; esac
git -C "$repo_root" cat-file -e "$commit^{commit}" 2>/dev/null || fail "N-1 source commit $commit is unavailable; fetch pinned history before verification"
image_tag=$(awk -F= '$1 == "image.tag" { print $2 }' "$source_file")
image_tag_commit=$(awk -F= '$1 == "image.tagCommit" { print $2 }' "$source_file")
image_feature_commit=$(awk -F= '$1 == "image.sshFeatureMergeCommit" { print $2 }' "$source_file")
[ "$(git -C "$repo_root" rev-parse "refs/tags/$image_tag^{commit}")" = "$image_tag_commit" ] || fail 'compatibility image tag provenance is inconsistent'
git -C "$repo_root" merge-base --is-ancestor "$image_feature_commit" "$image_tag_commit" || fail 'compatibility image feature provenance is inconsistent'
jq -e --arg crd "$commit" --arg tag "$image_tag" --arg imageCommit "$image_tag_commit" --arg featureCommit "$image_feature_commit" \
	'.crdSourceCommit == $crd and .compatibilityImageTag == $tag and .compatibilityImageTagCommit == $imageCommit and .compatibilityImageSSHFeatureMergeCommit == $featureCommit' \
	"$release_lock" >/dev/null || fail 'release lock conflates CRD and compatibility image provenance'

for kind in basicauths sshkeypairs stringsecrets; do
	fixture=$fixture_dir/secretgenerator.mittwald.de_${kind}_crd.yaml
	[ -f "$fixture" ] || fail "missing N-1 CRD fixture: $kind"
	lock_key=crd.v3.4.1.$kind.spec-sha256
	expected=$(awk -F= -v key="$lock_key" '$1 == key { print $2; found=1 } END { if (!found) exit 1 }' "$repo_root/tools.lock")
	fixture_json=$(helm template yaml-normalizer "$repo_root/test/fixtures/yaml-normalizer" \
		--set-file input="$fixture" --show-only templates/normalize.yaml |
		sed -n '/^{/p')
	actual=$(printf '%s\n' "$fixture_json" |
		jq -Sc '.spec | if .conversion == {strategy:"None"} then del(.conversion) else . end | if .preserveUnknownFields == false then del(.preserveUnknownFields) else . end' |
		openssl dgst -sha256 -r | awk '{print $1}')
	[ "$actual" = "$expected" ] || fail "$kind fixture differs from the pinned canonical spec digest"
	webhook_hash=$(printf '%s\n' "$fixture_json" |
		jq -Sc '.spec | .conversion={strategy:"Webhook",webhook:{conversionReviewVersions:["v1"],clientConfig:{url:"https://invalid.example"}}} | if .conversion == {strategy:"None"} then del(.conversion) else . end | if .preserveUnknownFields == false then del(.preserveUnknownFields) else . end' |
		openssl dgst -sha256 -r | awk '{print $1}')
	[ "$webhook_hash" != "$expected" ] || fail "$kind normalization erased a non-default conversion strategy"
	expected_blob=$(awk -F= -v key="blob.$kind" '$1 == key { print $2; found=1 } END { if (!found) exit 1 }' "$source_file")
	[ "$(git hash-object "$fixture")" = "$expected_blob" ] || fail "$kind fixture differs from its independently pinned source blob"
	git -C "$repo_root" show "$commit:deploy/crds/secretgenerator.mittwald.de_${kind}_crd.yaml" | cmp -s - "$fixture" || fail "$kind fixture differs from source commit $commit"
done

git -C "$repo_root" archive "$commit" deploy | tar -x -C "$tmpdir"
n1_digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
helm template n1 "$tmpdir/deploy/helm-chart/kubernetes-secret-generator" --namespace ksg-system \
	--set image.registry=ghcr.io --set image.repository=mrchypark/kubernetes-secret-generator \
	--set-string "image.tag=v3.4.1@$n1_digest" >"$tmpdir/rendered.yaml"
grep -F -q "image: ghcr.io/mrchypark/kubernetes-secret-generator:v3.4.1@$n1_digest" "$tmpdir/rendered.yaml" || fail 'N-1 digest-pinned image does not render exactly'

extract_deployment() {
	awk '
		/^---$/ { if (deployment) printf "%s", document; document=""; deployment=0; next }
		{ document=document $0 "\n" }
		$0 == "kind: Deployment" { deployment=1 }
		END { if (deployment) printf "%s", document }
	' "$1" >"$2"
	[ -s "$2" ] || fail "rendered chart has no Deployment: $1"
}

current_chart=$repo_root/deploy/helm-chart/kubernetes-secret-generator
helm template n1 "$current_chart" --namespace ksg-system --set profile=dev \
	--set-string "image.digest=${n1_digest#*@}" >"$tmpdir/current-fresh.yaml"
helm template n1 "$current_chart" --namespace ksg-system --is-upgrade --set profile=dev \
	--set migration.confirmedScope=ownNamespace --set-string "image.digest=${n1_digest#*@}" >"$tmpdir/current-upgrade.yaml"
for render in rendered current-fresh current-upgrade; do
	extract_deployment "$tmpdir/$render.yaml" "$tmpdir/$render-deployment.yaml"
	helm template yaml-normalizer "$repo_root/test/fixtures/yaml-normalizer" \
		--set-file input="$tmpdir/$render-deployment.yaml" --show-only templates/normalize.yaml |
		sed -n '/^{/p' >"$tmpdir/$render-deployment.json"
done
jq -e '.spec.selector.matchLabels == {
	"name":"kubernetes-secret-generator",
	"app.kubernetes.io/name":"kubernetes-secret-generator",
	"app.kubernetes.io/instance":"n1"
}' "$tmpdir/rendered-deployment.json" >/dev/null || fail 'pinned v3.4.1 selector fixture is unexpected'
for render in current-fresh current-upgrade; do
	jq -e --slurpfile legacy "$tmpdir/rendered-deployment.json" '
		.spec.selector == $legacy[0].spec.selector and
		(. as $deployment | [$deployment.spec.selector.matchLabels | to_entries[] | . as $entry |
			$deployment.spec.template.metadata.labels[$entry.key] == $entry.value] | all)' \
		"$tmpdir/$render-deployment.json" >/dev/null || fail "$render Deployment selector is not v3.4.1-compatible"
done

printf 'N-1 CRD fixture verification passed\n'
