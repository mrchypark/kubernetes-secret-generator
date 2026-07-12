#!/bin/sh
set -eu

action=${1:-}
chart_dir=${CHART_DIR:-deploy/helm-chart/kubernetes-secret-generator}
repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

require() {
	eval "value=\${$1:-}"
	[ -n "$value" ] || fail "$1 is required"
}

case "$action" in
	install|upgrade|uninstall) ;;
	*) fail 'usage: helm-release.sh install|upgrade|uninstall' ;;
esac

require KUBE_CONTEXT
require CONFIRM_CONTEXT
require NAMESPACE
require RELEASE_NAME
[ "$KUBE_CONTEXT" = "$CONFIRM_CONTEXT" ] || fail 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT'

case "$RELEASE_NAME" in
	*[!a-z0-9-]*|-*|*-) fail 'RELEASE_NAME must be a lowercase DNS label' ;;
esac
[ "${#RELEASE_NAME}" -le 53 ] || fail 'RELEASE_NAME must be at most 53 bytes'

# shellcheck disable=SC2153 # NAMESPACE is an externally supplied deployment input.
case "$NAMESPACE" in
	default|kube-*|'') fail 'refusing default, kube-*, or empty namespace' ;;
	*[!a-z0-9-]*|-*|*-) fail 'NAMESPACE must be a DNS label' ;;
esac
[ "${#NAMESPACE}" -le 63 ] || fail 'NAMESPACE must be at most 63 bytes'

command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
command -v helm >/dev/null 2>&1 || fail 'helm is required'

helm_version=$(helm version --kube-context "$KUBE_CONTEXT" --template '{{.Version}}' | sed 's/^v//; s/+.*//; s/-.*//')
helm_major=$(printf '%s' "$helm_version" | awk -F. '{print $1}')
helm_minor=$(printf '%s' "$helm_version" | awk -F. '{print $2}')
[ "$helm_major" -gt 3 ] || { [ "$helm_major" -eq 3 ] && [ "$helm_minor" -ge 14 ]; } || fail 'Helm 3.14.0 or newer is required'

server=$(kubectl --context "$KUBE_CONTEXT" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
[ -n "$server" ] || fail 'selected context has no API server'
if [ -n "${EXPECTED_SERVER_URL:-}" ] && [ "$server" != "$EXPECTED_SERVER_URL" ]; then
	fail 'EXPECTED_SERVER_URL does not match selected context'
fi

if [ -n "${EXPECTED_CA_SHA256:-}" ]; then
	command -v openssl >/dev/null 2>&1 || fail 'openssl is required for EXPECTED_CA_SHA256'
	ca_data=$(kubectl --context "$KUBE_CONTEXT" config view --minify --raw --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
	[ -n "$ca_data" ] || fail 'selected context has no embedded CA data'
	ca_sha=$(printf '%s' "$ca_data" | openssl base64 -d -A | openssl dgst -sha256 -r | awk '{print $1}')
	[ "$ca_sha" = "$EXPECTED_CA_SHA256" ] || fail 'EXPECTED_CA_SHA256 does not match selected context'
fi

validate_legacy_preflight() {
	require EXPECTED_SERVER_URL
	require EXPECTED_CA_SHA256
	require RAW_V3_PREFLIGHT_SHA256
	case "$RAW_V3_PREFLIGHT_SHA256" in
		????????????????????????????????????????????????????????????????) ;;
		*) fail 'RAW_V3_PREFLIGHT_SHA256 must be a 64-character lowercase SHA-256' ;;
	esac
	case "$RAW_V3_PREFLIGHT_SHA256" in *[!0-9a-f]*) fail 'RAW_V3_PREFLIGHT_SHA256 must be lowercase hexadecimal' ;; esac
	require RAW_V3_PREFLIGHT_REPORT
	case "$RAW_V3_PREFLIGHT_REPORT" in /*) ;; *) fail 'RAW_V3_PREFLIGHT_REPORT must be an absolute path' ;; esac
	[ -f "$RAW_V3_PREFLIGHT_REPORT" ] || fail 'RAW_V3_PREFLIGHT_REPORT does not exist'
	command -v jq >/dev/null 2>&1 || fail 'jq is required to validate RAW_V3_PREFLIGHT_REPORT'
	command -v openssl >/dev/null 2>&1 || fail 'openssl is required to validate RAW_V3_PREFLIGHT_REPORT'
	report_sha=$(openssl dgst -sha256 -r "$RAW_V3_PREFLIGHT_REPORT" | awk '{print $1}')
	[ "$report_sha" = "$RAW_V3_PREFLIGHT_SHA256" ] || fail 'RAW_V3_PREFLIGHT_SHA256 does not match the report'
	jq -e --arg context "$KUBE_CONTEXT" --arg server "$server" --arg ca "$ca_sha" --arg namespace "$NAMESPACE" --arg release "$RELEASE_NAME" \
		'.schemaVersion == 1 and .blockerCount == 0 and .target.context == $context and .target.server == $server and .target.caSHA256 == $ca and .target.releaseNamespace == $namespace and .target.releaseName == $release and ((.generatedAt | fromdateiso8601) <= now) and (now - (.generatedAt | fromdateiso8601) <= 86400)' \
		"$RAW_V3_PREFLIGHT_REPORT" >/dev/null || fail 'RAW_V3_PREFLIGHT_REPORT is not a zero-blocker report for this exact target'
}

if [ "$action" = uninstall ]; then
	exec helm uninstall "$RELEASE_NAME" --kube-context "$KUBE_CONTEXT" --namespace "$NAMESPACE"
fi

require CHART_VERSION
require IMAGE_DIGEST
require CRD_LIFECYCLE_MANAGER
[ "$CRD_LIFECYCLE_MANAGER" = direct ] || fail 'helm-release.sh is only for crdLifecycle.manager=direct; Flux owns flux lifecycle releases'
if [ "$action" = install ]; then
	RAW_V3_MIGRATION=${RAW_V3_MIGRATION:-false}
	REINSTALL=${REINSTALL:-false}
	case "$RAW_V3_MIGRATION:$REINSTALL" in false:false|true:false|false:true) ;; *) fail 'select at most one of RAW_V3_MIGRATION=true or REINSTALL=true' ;; esac
	if [ "$RAW_V3_MIGRATION" = true ] || [ "$REINSTALL" = true ]; then
		require SCOPE_MODE
		require CONFIRMED_SCOPE
		[ "$SCOPE_MODE" = "$CONFIRMED_SCOPE" ] || fail 'CONFIRMED_SCOPE must exactly match SCOPE_MODE'
	fi
	if [ "$RAW_V3_MIGRATION" = true ]; then
		:
	elif [ "$REINSTALL" = true ]; then
		require CONFIRM_REINSTALL
		[ "$CONFIRM_REINSTALL" = "$KUBE_CONTEXT/$NAMESPACE/$RELEASE_NAME" ] || fail 'CONFIRM_REINSTALL must exactly equal KUBE_CONTEXT/NAMESPACE/RELEASE_NAME'
	else
		SCOPE_MODE=${SCOPE_MODE:-ownNamespace}
		[ -z "${CONFIRMED_SCOPE:-}" ] || fail 'CONFIRMED_SCOPE is only valid for upgrade or raw v3 migration'
		[ -z "${CONFIRMED_NAMESPACES_SHA256:-}" ] || fail 'CONFIRMED_NAMESPACES_SHA256 is only valid for upgrade or raw v3 migration'
	fi
else
	RAW_V3_MIGRATION=false
	REINSTALL=false
	require SCOPE_MODE
	require CONFIRMED_SCOPE
	[ "$SCOPE_MODE" = "$CONFIRMED_SCOPE" ] || fail 'CONFIRMED_SCOPE must exactly match SCOPE_MODE'
fi
case "$SCOPE_MODE" in
	ownNamespace|namespaces|cluster) ;;
	*) fail 'SCOPE_MODE must be ownNamespace, namespaces, or cluster' ;;
esac
case "$IMAGE_DIGEST" in
	sha256:????????????????????????????????????????????????????????????????) ;;
	*) fail 'IMAGE_DIGEST must be sha256 followed by 64 lowercase hexadecimal characters' ;;
esac
case "${IMAGE_DIGEST#sha256:}" in
	*[!0-9a-f]*) fail 'IMAGE_DIGEST must contain lowercase hexadecimal characters only' ;;
esac

[ -f "$chart_dir/Chart.yaml" ] || fail "chart not found at $chart_dir"
actual_chart_version=$(awk '$1 == "version:" { print $2; exit }' "$chart_dir/Chart.yaml")
[ "$actual_chart_version" = "$CHART_VERSION" ] || fail "CHART_VERSION does not match local chart ($actual_chart_version)"

tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-release.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15
scope_values=$tmpdir/scope-values.yaml
compatibility_profile=${COMPATIBILITY_PROFILE:-v4}
leader_election_enabled=${LEADER_ELECTION_ENABLED:-true}
leader_election_id=${LEADER_ELECTION_ID:-kubernetes-secret-generator-lock}
case "$compatibility_profile" in
	v4|v3.4.1) ;;
	*) fail 'COMPATIBILITY_PROFILE must be v4 or v3.4.1' ;;
esac
case "$leader_election_enabled" in
	true|false) ;;
	*) fail 'LEADER_ELECTION_ENABLED must be true or false' ;;
esac
case "$leader_election_id" in
	''|*[!a-z0-9.-]*|[!a-z0-9]*|*[!a-z0-9]) fail 'LEADER_ELECTION_ID must be a DNS-compatible Lease name' ;;
esac
[ "${#leader_election_id}" -le 63 ] || fail 'LEADER_ELECTION_ID must be at most 63 bytes'
if [ "$compatibility_profile" = v3.4.1 ] && { [ "$leader_election_enabled" != true ] || [ "$leader_election_id" != kubernetes-secret-generator-lock ]; }; then
	fail 'COMPATIBILITY_PROFILE=v3.4.1 rolling rollback requires the enabled default kubernetes-secret-generator-lock lease; use the approved downtime runbook for a custom lease'
fi
{
	printf 'scope:\n  mode: %s\n  namespaces:\n' "$SCOPE_MODE"
	if [ "$SCOPE_MODE" = namespaces ]; then
		require SCOPE_NAMESPACES
		case "$SCOPE_NAMESPACES" in
			,*|*,|*,,*) fail 'SCOPE_NAMESPACES must not contain empty entries' ;;
			*'
'*) fail 'SCOPE_NAMESPACES must be a comma-separated single line' ;;
		esac
		canonical=$(printf '%s' "$SCOPE_NAMESPACES" | tr ',' '\n' | LC_ALL=C sort)
		[ "$(printf '%s\n' "$canonical" | uniq -d | wc -l | tr -d ' ')" -eq 0 ] || fail 'SCOPE_NAMESPACES must not contain duplicates'
		printf '%s\n' "$canonical" | while IFS= read -r namespace; do
			case "$namespace" in
				''|*[!a-z0-9-]*|-*|*-) fail "invalid namespace in SCOPE_NAMESPACES: $namespace" ;;
			esac
			[ "${#namespace}" -le 63 ] || fail "namespace exceeds 63 bytes: $namespace"
			printf '    - %s\n' "$namespace"
		done
		command -v openssl >/dev/null 2>&1 || fail 'openssl is required for namespace confirmation'
		canonical_sha=$(printf '%s' "$canonical" | openssl dgst -sha256 -r | awk '{print $1}')
		if [ "$action" = upgrade ] || [ "$RAW_V3_MIGRATION" = true ]; then
			require CONFIRMED_NAMESPACES_SHA256
			[ "$canonical_sha" = "$CONFIRMED_NAMESPACES_SHA256" ] || fail "CONFIRMED_NAMESPACES_SHA256 must equal $canonical_sha"
		fi
	else
		[ -z "${SCOPE_NAMESPACES:-}" ] || fail 'SCOPE_NAMESPACES is only valid for namespaces mode'
		canonical_sha=
		printf '    []\n'
	fi
	if [ "$action" = upgrade ] || [ "$RAW_V3_MIGRATION" = true ]; then
		printf 'migration:\n  confirmedScope: %s\n  confirmedNamespacesSHA256: %s\n' "$CONFIRMED_SCOPE" "$canonical_sha"
	else
		printf 'migration:\n  confirmedScope: ""\n  confirmedNamespacesSHA256: ""\n'
	fi
	printf 'crdLifecycle:\n  manager: direct\n'
	printf 'compatibilityProfile: %s\n' "$compatibility_profile"
	printf 'leaderElection:\n  enabled: %s\n  id: %s\n' "$leader_election_enabled" "$leader_election_id"
} >"$scope_values"

release_matches=$(helm list --all --short --filter "^${RELEASE_NAME}$" --kube-context "$KUBE_CONTEXT" --namespace "$NAMESPACE")
case "$release_matches" in
	'') release_exists=false ;;
	"$RELEASE_NAME") release_exists=true ;;
	*) fail 'release lookup returned an ambiguous result' ;;
esac
[ "$action" = upgrade ] || [ "$release_exists" = false ] || fail 'release already exists; use upgrade'
[ "$action" = install ] || [ "$release_exists" = true ] || fail 'release does not exist; use install'

lifecycle_records=$(kubectl --context "$KUBE_CONTEXT" get configmaps --all-namespaces \
	--selector secretgenerator.mittwald.de/crd-lifecycle-owner=true \
	-o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.data.manager}{"\t"}{.data.releaseNamespace}{"\t"}{.data.releaseName}{"\t"}{.data.chartVersion}{"\t"}{.data.scopeMode}{"\t"}{.data.namespacesSHA256}{"\n"}{end}')
lifecycle_count=$(printf '%s\n' "$lifecycle_records" | awk 'NF { count++ } END { print count + 0 }')
[ "$lifecycle_count" -le 1 ] || fail 'multiple CRD lifecycle owners exist; manual review is required'
[ "$REINSTALL" = false ] || [ "$lifecycle_count" -eq 1 ] || fail 'reinstall requires exactly one retained CRD lifecycle owner record'
if [ "$lifecycle_count" -eq 1 ]; then
	IFS="$(printf '\t')" read -r owner_namespace owner_name lifecycle_manager lifecycle_namespace lifecycle_release lifecycle_version lifecycle_scope lifecycle_namespaces_sha <<EOF
$lifecycle_records
EOF
	[ -n "$owner_namespace" ] && [ -n "$owner_name" ] && [ -n "$lifecycle_manager" ] && [ -n "$lifecycle_namespace" ] && [ -n "$lifecycle_release" ] && [ -n "$lifecycle_version" ] && [ -n "$lifecycle_scope" ] || fail 'CRD lifecycle owner evidence is malformed'
	[ "$lifecycle_manager" = direct ] || fail 'CRDs are owned by a non-direct lifecycle manager'
	[ "$lifecycle_namespace" = "$NAMESPACE" ] && [ "$lifecycle_release" = "$RELEASE_NAME" ] || fail 'CRD lifecycle owner does not match this release'
	[ "$action" = upgrade ] || [ "$REINSTALL" = true ] || fail 'retained CRD lifecycle evidence exists; use the confirmed reinstall path'
	if [ "$REINSTALL" = true ]; then
		[ "$SCOPE_MODE" = "$lifecycle_scope" ] || fail 'reinstall scope differs from retained lifecycle evidence'
		[ "$SCOPE_MODE" = namespaces ] || [ -z "$lifecycle_namespaces_sha" ] || fail 'retained namespace digest is invalid for this scope'
		[ "$SCOPE_MODE" != namespaces ] || [ "$canonical_sha" = "$lifecycle_namespaces_sha" ] || fail 'reinstall namespace set differs from retained lifecycle evidence'
	fi
	[ -n "$lifecycle_version" ] || fail 'CRD lifecycle owner has no chart version'
fi

version_is_newer() {
	installed=${1#v}; installed=${installed%%-*}
	target=${2#v}; target=${target%%-*}
	awk -v installed="$installed" -v target="$target" 'BEGIN {
		if (installed !~ /^[0-9]+\.[0-9]+\.[0-9]+$/ || target !~ /^[0-9]+\.[0-9]+\.[0-9]+$/) exit 2
		split(installed, a, "."); split(target, b, ".")
		for (i=1; i<=3; i++) { if ((a[i]+0) > (b[i]+0)) exit 0; if ((a[i]+0) < (b[i]+0)) exit 1 }
		exit 1
	}'
}

if [ "$lifecycle_count" -eq 1 ]; then
	if version_is_newer "$lifecycle_version" "$CHART_VERSION"; then
		fail "refusing CRD downgrade from chart $lifecycle_version to $CHART_VERSION"
	else
		status=$?
		[ "$status" -ne 2 ] || fail 'CRD lifecycle owner has a malformed chart version'
	fi
fi
crd_count=0
legacy_crd_count=0
adopt_legacy_crds=false
for crd in basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de; do
	crd_state=$(kubectl --context "$KUBE_CONTEXT" get "customresourcedefinition/$crd" --ignore-not-found \
		-o jsonpath='{.metadata.uid}{"|"}{.metadata.annotations.secretgenerator\.mittwald\.de/schema-release}')
	[ -n "$crd_state" ] || continue
	crd_count=$((crd_count + 1))
	crd_uid=${crd_state%%|*}
	installed_schema_version=${crd_state#*|}
	[ -n "$crd_uid" ] || fail "$crd returned malformed identity evidence"
	if [ "$installed_schema_version" = "$crd_state" ] || [ -z "$installed_schema_version" ]; then
		legacy_crd_count=$((legacy_crd_count + 1))
		continue
	fi
	if version_is_newer "$installed_schema_version" "$CHART_VERSION"; then
		fail "refusing $crd schema downgrade from $installed_schema_version to $CHART_VERSION"
	else
		status=$?
		[ "$status" -ne 2 ] || fail "$crd has a malformed schema-release annotation"
	fi
done
[ "$crd_count" -eq 0 ] || [ "$crd_count" -eq 3 ] || fail 'partial target CRD set exists; manual review is required'
if [ "$action" = install ] && [ "$RAW_V3_MIGRATION" = false ] && [ "$REINSTALL" = false ]; then
	[ "$crd_count" -eq 0 ] || fail 'fresh install requires all target CRDs to be absent; use the reviewed raw v3 migration path'
fi
if [ "$REINSTALL" = true ]; then
	[ "$crd_count" -eq 3 ] && [ "$legacy_crd_count" -eq 0 ] || fail 'reinstall requires all three marked product CRDs'
fi
if [ "$RAW_V3_MIGRATION" = true ]; then
	[ "$legacy_crd_count" -eq 3 ] || fail 'raw v3 migration requires all three exact unmarked legacy CRDs'
fi
if [ "$legacy_crd_count" -gt 0 ]; then
	[ "$legacy_crd_count" -eq 3 ] || fail 'mixed marked and unmarked CRDs require manual review'
	[ "$action" = upgrade ] || [ "$RAW_V3_MIGRATION" = true ] || fail 'unmarked CRDs cannot be adopted by a fresh install'
	require CONFIRM_LEGACY_CRD_ADOPTION
	[ "$CONFIRM_LEGACY_CRD_ADOPTION" = v3.4.1 ] || fail 'CONFIRM_LEGACY_CRD_ADOPTION must exactly equal v3.4.1'
	command -v jq >/dev/null 2>&1 || fail 'jq is required to verify legacy CRD identity'
	command -v openssl >/dev/null 2>&1 || fail 'openssl is required to verify legacy CRD identity'
	validate_legacy_preflight
	legacy_live_dir=$tmpdir/legacy-live
	mkdir "$legacy_live_dir"
	for crd in basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de; do
		case "$crd" in
			basicauths.*) lock_key=crd.v3.4.1.basicauths.spec-sha256 ;;
			sshkeypairs.*) lock_key=crd.v3.4.1.sshkeypairs.spec-sha256 ;;
			stringsecrets.*) lock_key=crd.v3.4.1.stringsecrets.spec-sha256 ;;
		esac
		expected_spec_sha=$(awk -F= -v key="$lock_key" '$1 == key { print $2; found=1 } END { if (!found) exit 1 }' "$repo_root/tools.lock")
		live_crd=$legacy_live_dir/$crd.json
		kubectl --context "$KUBE_CONTEXT" get "customresourcedefinition/$crd" --show-managed-fields=true -o json >"$live_crd"
		if ! jq -e '(.metadata.managedFields // []) as $fields |
			($fields | length) == 2 and
			any($fields[]; .manager == "kubectl-client-side-apply" and .operation == "Update" and
				.apiVersion == "apiextensions.k8s.io/v1" and (.subresource // "") == "" and
				(.fieldsV1 | has("f:spec")) and ((.fieldsV1 | keys) - ["f:metadata","f:spec"] | length) == 0) and
			any($fields[]; .manager == "kube-apiserver" and .operation == "Update" and
				.apiVersion == "apiextensions.k8s.io/v1" and .subresource == "status" and
				(.fieldsV1 | keys) == ["f:status"])' "$live_crd" >/dev/null; then
			printf 'error: %s managedFields tuples are not the exact v3 client-apply plus Kubernetes status set:\n' "$crd" >&2
			jq -c '[.metadata.managedFields[]? | {manager,operation,apiVersion,subresource:(.subresource // ""),fieldPaths:([(.fieldsV1 // {}) | paths | map(tostring) | join(".")] | sort)}]' "$live_crd" >&2
			fail 'refusing direct CRD ownership takeover'
		fi
		jq -e '.metadata.uid != null and .metadata.uid != "" and .metadata.resourceVersion != null and .metadata.resourceVersion != ""' \
			"$live_crd" >/dev/null || fail "$crd lacks UID/resourceVersion concurrency evidence"
		actual_spec_sha=$(jq -Sc '.spec | if .conversion == {strategy:"None"} then del(.conversion) else . end | if .preserveUnknownFields == false then del(.preserveUnknownFields) else . end' "$live_crd" |
			openssl dgst -sha256 -r | awk '{print $1}')
		[ "$actual_spec_sha" = "$expected_spec_sha" ] || fail "$crd is unmarked but does not match the pinned v3.4.1 CRD spec"
	done
	adopt_legacy_crds=true
fi

profile=${PROFILE:-production}
case "$profile" in
	production|dev) ;;
	*) fail 'PROFILE must be production or dev' ;;
esac
if [ "$profile" = production ]; then
	ready_hostnames=$(kubectl --context "$KUBE_CONTEXT" get nodes -o jsonpath='{range .items[*]}{.spec.unschedulable}{"\t"}{.metadata.labels.kubernetes\.io/hostname}{"\t"}{range .status.conditions[?(@.type=="Ready")]}{.status}{end}{"\t"}{range .spec.taints[*]}{.effect}{","}{end}{"\n"}{end}' |
		awk -F '\t' '($1 == "" || $1 == "false") && $2 != "" && $3 == "True" && $4 !~ /(NoSchedule|NoExecute)/ { print $2 }' |
		LC_ALL=C sort -u)
	ready_hostname_count=$(printf '%s\n' "$ready_hostnames" | awk 'NF { count++ } END { print count + 0 }')
	[ "$ready_hostname_count" -ge 2 ] || fail 'production profile requires at least two Ready schedulable nodes with distinct kubernetes.io/hostname labels'
fi

rendered=$tmpdir/rendered.yaml
set -- helm template "$RELEASE_NAME" "$chart_dir" \
	--kube-context "$KUBE_CONTEXT" \
	--kube-version 1.35.0 \
	--namespace "$NAMESPACE"
[ "$action" = install ] || set -- "$@" --is-upgrade
"$@" --values "$scope_values" \
	--set-string "image.digest=$IMAGE_DIGEST" \
	--set-string "profile=$profile" >"$rendered"
grep -F -q "@$IMAGE_DIGEST" "$rendered" || fail 'rendered chart does not use the requested immutable image digest'

package_dir=$tmpdir/package
mkdir "$package_dir"
helm package "$chart_dir" --kube-context "$KUBE_CONTEXT" --destination "$package_dir" >/dev/null
package=$(find "$package_dir" -type f -name '*.tgz' -print | head -n 1)
[ -n "$package" ] || fail 'Helm chart package was not created'
crd_bundle=$tmpdir/crds.yaml
helm show crds "$package" --kube-context "$KUBE_CONTEXT" >"$crd_bundle"
[ -s "$crd_bundle" ] || fail 'packaged chart does not contain CRDs'
canonical_crds=$tmpdir/canonical-crds.yaml
LC_ALL=C
export LC_ALL
for crd_file in "$chart_dir"/crds/*_crd.yaml; do
	[ -f "$crd_file" ] || fail 'canonical CRD files are missing'
	cat "$crd_file"
	printf '\n'
done >"$canonical_crds"
cmp -s "$canonical_crds" "$crd_bundle" || fail 'packaged chart CRDs differ from the canonical generated CRDs'

if [ "$adopt_legacy_crds" = true ]; then
	current_preflight=$tmpdir/current-preflight.json
	CONFIRM_CONTEXT="$KUBE_CONTEXT" EXPECTED_SERVER_URL="$server" EXPECTED_CA_SHA256="$ca_sha" \
		DEPLOYMENT_NAME="${DEPLOYMENT_NAME:-$RELEASE_NAME}" \
		SCOPE_MODE="$SCOPE_MODE" CONFIRMED_SCOPE="$CONFIRMED_SCOPE" \
		SCOPE_NAMESPACES="${SCOPE_NAMESPACES:-}" CONFIRMED_NAMESPACES_SHA256="${CONFIRMED_NAMESPACES_SHA256:-}" \
		"$repo_root/scripts/preflight-v4.sh" >"$current_preflight" || fail 'immediate legacy-adoption preflight reported blockers or an unstable snapshot'
	[ "$(jq -r '.blockerCount' "$current_preflight")" -eq 0 ] || fail 'immediate legacy-adoption preflight reported blockers'
	for crd_file in "$chart_dir"/crds/*_crd.yaml; do
		crd=$(awk '$1 == "name:" { print $2; exit }' "$crd_file")
		live_crd=$legacy_live_dir/$crd.json
		[ -f "$live_crd" ] || fail "packaged target contains an unexpected CRD: $crd"
		uid=$(jq -er '.metadata.uid' "$live_crd")
		resource_version=$(jq -er '.metadata.resourceVersion' "$live_crd")
		target_crd=$tmpdir/target-$crd.json
		kubectl --context "$KUBE_CONTEXT" create --dry-run=client --filename "$crd_file" --output json |
			jq --arg uid "$uid" --arg rv "$resource_version" '.metadata.uid=$uid | .metadata.resourceVersion=$rv' >"$target_crd"
		kubectl --context "$KUBE_CONTEXT" replace --dry-run=server --field-manager kubernetes-secret-generator-crd-manager --filename "$target_crd" >/dev/null
		kubectl --context "$KUBE_CONTEXT" replace --field-manager kubernetes-secret-generator-crd-manager --filename "$target_crd"
		kubectl --context "$KUBE_CONTEXT" apply --server-side --field-manager kubernetes-secret-generator-crd-manager --filename "$crd_file" >/dev/null
	done
else
	kubectl --context "$KUBE_CONTEXT" apply --server-side --dry-run=server \
		--field-manager kubernetes-secret-generator-crd-manager --filename "$crd_bundle" >/dev/null
	kubectl --context "$KUBE_CONTEXT" apply --server-side \
		--field-manager kubernetes-secret-generator-crd-manager --filename "$crd_bundle"
fi
for crd in basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de; do
	kubectl --context "$KUBE_CONTEXT" wait --for=condition=Established --timeout=60s "customresourcedefinition/$crd"
	case "$crd" in
		basicauths.*)
			required_schema='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.username.type}'
			expected_schema=string
			;;
		sshkeypairs.*)
			required_schema='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.algorithm.type}{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.privateKeyField.type}{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.publicKeyField.type}'
			expected_schema=stringstringstring
			;;
		stringsecrets.*)
			required_schema='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.fields.type}'
			expected_schema=array
			;;
	esac
	schema=$(kubectl --context "$KUBE_CONTEXT" get "customresourcedefinition/$crd" -o "jsonpath=$required_schema")
	[ "$schema" = "$expected_schema" ] || fail "$crd does not expose the required v4 OpenAPI fields"
done

if [ "$action" = install ]; then
	helm install "$RELEASE_NAME" "$chart_dir" \
		--kube-context "$KUBE_CONTEXT" \
		--namespace "$NAMESPACE" \
		--create-namespace \
		--atomic \
		--skip-crds \
		--values "$scope_values" \
		--set-string "image.digest=$IMAGE_DIGEST" \
		--set-string "profile=$profile"
else
	helm upgrade "$RELEASE_NAME" "$chart_dir" \
		--kube-context "$KUBE_CONTEXT" \
		--namespace "$NAMESPACE" \
		--atomic \
		--skip-crds \
		--values "$scope_values" \
		--set-string "image.digest=$IMAGE_DIGEST" \
		--set-string "profile=$profile"
fi
