#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-helm-guards.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15
mkdir "$tmpdir/bin"
log=$tmpdir/calls.log
digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
real_openssl=$(command -v openssl)

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

cat >"$tmpdir/bin/helm" <<'EOF'
#!/bin/sh
printf 'helm %s\n' "$*" >>"$CALL_LOG"
case "$1" in
	version) printf 'v3.21.3\n' ;;
	list)
		[ "${MOCK_LIST_ERROR:-false}" != true ] || exit 72
		[ "${MOCK_RELEASE_EXISTS:-false}" != true ] || printf 'ksg\n'
		;;
	template) printf 'image: "example.invalid/ksg@%s"\n' "$IMAGE_DIGEST" ;;
	package)
		while [ "$#" -gt 0 ]; do
			if [ "$1" = --destination ]; then shift; touch "$1/ksg-4.0.0-rc.12.tgz"; break; fi
			shift
		done
		;;
	show)
		for crd in "$MOCK_CHART_DIR"/crds/*_crd.yaml; do cat "$crd"; printf '\n'; done
		;;
	install|upgrade|uninstall) ;;
	*) exit 71 ;;
esac
EOF
cat >"$tmpdir/bin/kubectl" <<'EOF'
#!/bin/sh
printf 'kubectl %s\n' "$*" >>"$CALL_LOG"
case "$*" in
	*current-context*) exit 70 ;;
	*certificate-authority-data*) printf '%s' 'dGVzdC1jYQ==' ;;
	*config\ view*) printf '%s' 'https://target.example.invalid' ;;
	*get\ configmaps*) printf '%b' "${MOCK_OWNER_RECORD:-}" ;;
	*--request-timeout=20s\ get\ customresourcedefinitions.apiextensions.k8s.io*)
		if [ "${MOCK_ACTIVE_FLUX_API:-false}" = true ]; then
			printf '%s' '{"apiVersion":"v1","kind":"List","items":[{"metadata":{"name":"kustomizations.kustomize.toolkit.fluxcd.io"}}]}'
		else
			printf '%s' '{"apiVersion":"v1","kind":"List","items":[]}'
		fi
		;;
	*--request-timeout=20s\ get\ deployments.apps*)
		if [ "${MOCK_ACTIVE_FLUX_CONTROLLER:-false}" = true ]; then
			printf '%s' '{"apiVersion":"v1","kind":"List","items":[{"metadata":{"name":"kustomize-controller","namespace":"flux-system"}}]}'
		else
			printf '%s' '{"apiVersion":"v1","kind":"List","items":[]}'
		fi
		;;
	*schema-release*)
		if [ "${MOCK_CRD_EXISTS:-false}" = true ] || [ -n "${MOCK_CRD_VERSION:-}" ]; then
			printf 'uid-1|%s' "${MOCK_CRD_VERSION:-}"
		fi
		;;
	*get\ nodes*)
		if [ "${MOCK_DUPLICATE_NODES:-false}" = true ]; then
			printf 'false\tnode-a\tTrue\t\nfalse\tnode-a\tTrue\t\n'
		else
			printf 'false\tnode-a\tTrue\t\nfalse\tnode-b\tTrue\t\n'
		fi
		;;
	*get\ deployment*) printf '%s' '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"ksg","namespace":"ksg-system","uid":"deployment-uid","resourceVersion":"1"},"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"WATCH_NAMESPACE","value":"ksg-system"}]}]}}}}' ;;
	*get\ stringsecrets.secretgenerator.mittwald.de,basicauths.secretgenerator.mittwald.de,sshkeypairs.secretgenerator.mittwald.de,secrets*) printf '%s' '{"apiVersion":"v1","kind":"List","items":[]}' ;;
	*get\ customresourcedefinitions.apiextensions.k8s.io*) cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"basicauths.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}},
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"sshkeypairs.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"algorithm":{},"privateKey":{},"privateKeyField":{},"publicKeyField":{}}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}},
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"stringsecrets.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}}
]}
JSON
		;;
	*get\ stringsecrets.secretgenerator.mittwald.de\ -A*|*get\ basicauths.secretgenerator.mittwald.de\ -A*|*get\ sshkeypairs.secretgenerator.mittwald.de\ -A*|*get\ secrets\ -A*) printf '%s' '{"apiVersion":"v1","kind":"List","items":[]}' ;;
	*' -o json')
		case "$*" in
			*customresourcedefinition/basicauths.*) hash=6d90f44c610fe07c51aa729946c8368296d1bfe8bea2bb098cbd85c3a36c59f5 ;;
			*customresourcedefinition/sshkeypairs.*) hash=fd44204099ac052e2518b63cddfb9d677edbb539860463898ec28c267f12eaed ;;
			*customresourcedefinition/stringsecrets.*) hash=569acb9a3ff0dac64254fc72dd276b3ae29c7947da92c08120e18ccba8827871 ;;
			*) exit 71 ;;
		esac
		printf '%s' "$hash" >"$MOCK_SPEC_HASH"
		extra=
		[ "${MOCK_EXTRA_MANAGER:-false}" != true ] || extra=',{"manager":"unknown-reconciler","operation":"Apply","apiVersion":"apiextensions.k8s.io/v1","fieldsV1":{"f:spec":{}}}'
		printf '{"metadata":{"uid":"uid-1","resourceVersion":"10","labels":{"kustomize.toolkit.fluxcd.io/name":"%s","kustomize.toolkit.fluxcd.io/namespace":"%s"},"managedFields":[{"manager":"kube-apiserver","operation":"Update","apiVersion":"apiextensions.k8s.io/v1","subresource":"status","fieldsV1":{"%s":{}}},{"manager":"%s","operation":"%s","apiVersion":"apiextensions.k8s.io/v1","fieldsV1":{"f:metadata":{},"f:spec":{}}}%s]},"spec":{}}' \
			"${MOCK_OWNER_NAME:-dev-infra-stable}" "${MOCK_OWNER_NAMESPACE:-gitops}" "${MOCK_STATUS_FIELD:-f:status}" \
			"${MOCK_FIELD_MANAGER:-kubectl-client-side-apply}" "${MOCK_FIELD_OPERATION:-Update}" "$extra"
		;;
	*create\ --dry-run=client*) printf '%s' '{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"mock"},"spec":{}}' ;;
	*replace\ --dry-run=server*) ;;
	*replace\ --field-manager*) [ "${MOCK_REPLACE_CONFLICT:-false}" != true ] ;;
	*apply\ --server-side*) [ "${MOCK_SSA_CONFLICT:-false}" != true ] ;;
	*wait\ --for=condition=Established*) ;;
	*get\ customresourcedefinition/basicauths.*) printf string ;;
	*get\ customresourcedefinition/sshkeypairs.*) printf stringstringstring ;;
	*get\ customresourcedefinition/stringsecrets.*) printf array ;;
	*) exit 71 ;;
esac
EOF
cat >"$tmpdir/bin/openssl" <<'EOF'
#!/bin/sh
if [ "${MOCK_LEGACY_MATCH:-false}" = true ] && [ "$#" -eq 3 ] && [ "$1" = dgst ] && [ "$3" = -r ]; then
	payload=$(cat)
	if [ ! -s "$MOCK_SPEC_HASH" ] || [ "$payload" = test-ca ]; then
		printf '%s' "$payload" | "$REAL_OPENSSL" "$@"
		exit $?
	fi
	printf '%s *stdin\n' "$(cat "$MOCK_SPEC_HASH")"
	exit 0
fi
exec "$REAL_OPENSSL" "$@"
EOF
chmod +x "$tmpdir/bin/helm" "$tmpdir/bin/kubectl" "$tmpdir/bin/openssl"

common_env() {
	env PATH="$tmpdir/bin:$PATH" CALL_LOG="$log" REAL_OPENSSL="$real_openssl" MOCK_SPEC_HASH="$tmpdir/spec-hash" \
		MOCK_CHART_DIR="$repo_root/deploy/helm-chart/kubernetes-secret-generator" \
		KUBE_CONTEXT=explicit-target CONFIRM_CONTEXT=explicit-target \
		NAMESPACE=ksg-system RELEASE_NAME=ksg CHART_VERSION=4.0.0-rc.12 \
		IMAGE_DIGEST="$digest" CRD_LIFECYCLE_MANAGER=direct PROFILE=dev "$@"
}

assert_no_mutation() {
	if grep -E -q 'kubectl .* (apply|create|delete|patch|replace)|helm (upgrade|install|uninstall)' "$log"; then
		fail 'API mutation occurred before a guard failed'
	fi
}

: >"$log"
if common_env EXPECTED_SERVER_URL=https://other.example.invalid \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'mismatched API server guard succeeded'
fi
grep -F -q 'kubectl --context explicit-target config view' "$log" || fail 'kubectl did not use the explicit context'
assert_no_mutation

: >"$log"
if common_env EXPECTED_CA_SHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'mismatched CA guard succeeded'
fi
assert_no_mutation

: >"$log"
if env PATH="$tmpdir/bin:$PATH" CALL_LOG="$log" \
	KUBE_CONTEXT=explicit-target CONFIRM_CONTEXT=different \
	NAMESPACE=ksg-system RELEASE_NAME=ksg \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'mismatched context confirmation succeeded'
fi
[ ! -s "$log" ] || fail 'context confirmation failure invoked a cluster tool'

: >"$log"
common_env "$repo_root/scripts/helm-release.sh" install >/dev/null
grep -F -q 'helm list --all --short --filter ^ksg$ --kube-context explicit-target --namespace ksg-system' "$log" || fail 'install did not check release absence'
grep -F -q 'helm install ksg ' "$log" || fail 'fresh install did not use helm install'
grep -F -q -- '--skip-crds' "$log" || fail 'fresh install did not skip Helm CRD ownership'
if grep -F 'helm template ' "$log" | grep -F -q -- '--is-upgrade'; then fail 'fresh install rendered as an upgrade'; fi
if grep -F -q 'helm upgrade ' "$log"; then fail 'fresh install used helm upgrade'; fi
grep -F -q 'kubectl --context explicit-target apply --server-side --field-manager kubernetes-secret-generator-crd-manager' "$log" || fail 'direct install did not apply CRDs with the fixed field manager'
if grep -F -q -- '--force-conflicts' "$log"; then fail 'direct CRD apply used force-conflicts'; fi
grep -F 'kubectl --context explicit-target apply --server-side' "$log" | grep -F -q -- '--dry-run=server' || fail 'CRD bundle did not pass a full server-side dry-run before mutation'
apply_line=$(grep -n -F 'kubectl --context explicit-target apply --server-side' "$log" | cut -d: -f1)
apply_line=$(printf '%s\n' "$apply_line" | tail -n 1)
install_line=$(grep -n -F 'helm install ksg ' "$log" | cut -d: -f1)
[ "$apply_line" -lt "$install_line" ] || fail 'controller install started before CRD SSA'

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true "$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'install accepted an existing release'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_LIST_ERROR=true "$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'release lookup transport error was treated as release absence'
fi
assert_no_mutation

for bad_namespaces in 'alpha,' ',alpha' 'alpha,,beta'; do
	: >"$log"
	if common_env MOCK_RELEASE_EXISTS=true SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces \
		SCOPE_NAMESPACES="$bad_namespaces" CONFIRMED_NAMESPACES_SHA256="$digest" \
		"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
		fail "upgrade accepted empty namespace entry: $bad_namespaces"
	fi
	assert_no_mutation
done

: >"$log"
if env PATH="$tmpdir/bin:$PATH" CALL_LOG="$log" KUBE_CONTEXT=explicit-target CONFIRM_CONTEXT=explicit-target \
	NAMESPACE=ksg-system RELEASE_NAME=-unsafe \
	"$repo_root/scripts/helm-release.sh" uninstall >/dev/null 2>&1; then
	fail 'unsafe release name crossed the Helm argument boundary'
fi
[ ! -s "$log" ] || fail 'release-name validation invoked a cluster tool'

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_OWNER_RECORD='ksg-system\towner\tflux\tksg-system\tksg\t4.0.0-rc.12\townNamespace\t\n' \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'direct upgrade accepted Flux CRD lifecycle ownership'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_OWNER_RECORD='ksg-system\towner\tdirect\tksg-system\tksg\t4.1.0\townNamespace\t\n' \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'older chart accepted a persisted newer CRD lifecycle version'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_OWNER_RECORD='ksg-system\tbroken\t\t\t\t\t\t\n' \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'malformed lifecycle owner evidence was treated as absent'
fi
assert_no_mutation

: >"$log"
common_env MOCK_CRD_VERSION=4.0.0 MOCK_OWNER_RECORD='ksg-system\towner\tdirect\tksg-system\tksg\t4.0.0-rc.12\townNamespace\t\n' \
	REINSTALL=true SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	CONFIRM_REINSTALL=explicit-target/ksg-system/ksg \
	"$repo_root/scripts/helm-release.sh" install >/dev/null
grep -F -q 'helm install ksg ' "$log" || fail 'confirmed retained-CRD reinstall did not use helm install'

: >"$log"
if common_env MOCK_CRD_VERSION=4.0.0 REINSTALL=true SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	CONFIRM_REINSTALL=explicit-target/ksg-system/ksg \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'reinstall accepted marked CRDs without retained lifecycle owner evidence'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_CRD_EXISTS=true "$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'fresh install accepted an existing unmarked product CRD set'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_CRD_VERSION=4.1.0 "$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'older chart accepted a newer installed CRD schema annotation'
fi
assert_no_mutation

: >"$log"
if common_env SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces SCOPE_NAMESPACES=beta,alpha \
	CONFIRMED_NAMESPACES_SHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa \
	MOCK_RELEASE_EXISTS=true "$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'upgrade accepted an incorrect namespace confirmation digest'
fi
assert_no_mutation

: >"$log"
if common_env SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'upgrade accepted a missing release'
fi
assert_no_mutation

: >"$log"
namespace_digest=$(printf 'alpha\nbeta' | openssl dgst -sha256 -r | awk '{print $1}')
common_env MOCK_RELEASE_EXISTS=true SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces \
	SCOPE_NAMESPACES=beta,alpha CONFIRMED_NAMESPACES_SHA256="$namespace_digest" \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null
grep -F 'helm template ' "$log" | grep -F -q -- '--is-upgrade' || fail 'upgrade render omitted --is-upgrade'
grep -F -q 'helm upgrade ksg ' "$log" || fail 'upgrade did not use exact helm upgrade'
if grep -F 'helm upgrade ksg ' "$log" | grep -F -q -- '--install'; then fail 'upgrade retained install fallback'; fi
if grep -F -q 'helm install ' "$log"; then fail 'upgrade invoked helm install'; fi

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_SSA_CONFLICT=true \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'CRD SSA conflict did not abort upgrade'
fi
grep -F -q 'kubectl --context explicit-target apply --server-side' "$log" || fail 'conflict fixture did not reach CRD SSA'
if grep -F 'kubectl --context explicit-target apply --server-side' "$log" | grep -v -F -q -- '--dry-run=server'; then fail 'CRD write occurred after server-side dry-run conflict'; fi
if grep -F -q 'helm upgrade ksg ' "$log"; then fail 'controller rollout started after CRD SSA conflict'; fi

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'legacy CRD ownership takeover accepted a missing target-bound preflight report'
fi
assert_no_mutation

preflight=$tmpdir/preflight.json
ca_sha=$(printf '%s' 'test-ca' | "$real_openssl" dgst -sha256 -r | awk '{print $1}')
printf '{"schemaVersion":1,"generatedAt":"%s","blockerCount":0,"target":{"context":"explicit-target","server":"https://target.example.invalid","caSHA256":"%s","releaseNamespace":"ksg-system","releaseName":"ksg"}}\n' \
	"$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$ca_sha" >"$preflight"
preflight_sha=$("$real_openssl" dgst -sha256 -r "$preflight" | awk '{print $1}')
rm -f "$tmpdir/spec-hash"
: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true MOCK_FIELD_MANAGER=flux-client-side-apply \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'direct legacy adoption accepted Flux managedFields ownership'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
	MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'orphaned Flux adoption accepted missing owner confirmation'
fi
assert_no_mutation

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
	MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	CONFIRM_ORPHANED_FLUX_OWNER=dev-infra-stable/gitops \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'orphaned Flux adoption accepted missing organizational decommission confirmation'
fi
assert_no_mutation

for fixture in wrong-owner wrong-manager unknown-spec; do
	rm -f "$tmpdir/spec-hash"
	: >"$log"
	case "$fixture" in
		wrong-owner) set -- MOCK_OWNER_NAME=other ;;
		wrong-manager) set -- MOCK_FIELD_MANAGER=other-controller ;;
		unknown-spec) set -- MOCK_EXTRA_MANAGER=true ;;
	esac
	if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
		MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply "$@" \
		EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
		RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
		SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
		CONFIRM_ORPHANED_FLUX_OWNER=dev-infra-stable/gitops \
		CONFIRM_ORPHANED_FLUX_DECOMMISSIONED=dev-infra-stable/gitops \
		"$repo_root/scripts/helm-release.sh" upgrade >"$tmpdir/orphaned-$fixture.out" 2>&1; then
		fail "orphaned Flux adoption accepted $fixture ownership"
	fi
	grep -F -q 'managedFields or orphaned Flux owner labels are not the exact allowed set' "$tmpdir/orphaned-$fixture.out" || fail "$fixture omitted the safe ownership diagnostic"
	grep -F -q '"fieldPaths"' "$tmpdir/orphaned-$fixture.out" || fail "$fixture diagnostic omitted redacted field paths"
	assert_no_mutation
done

for fixture in unknown-manager status-spec-owner; do
	rm -f "$tmpdir/spec-hash"
	: >"$log"
	case "$fixture" in
		unknown-manager) set -- MOCK_EXTRA_MANAGER=true ;;
		status-spec-owner) set -- MOCK_STATUS_FIELD=f:spec ;;
	esac
	if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true "$@" \
		EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
		RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
		SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
		"$repo_root/scripts/helm-release.sh" upgrade >"$tmpdir/$fixture.out" 2>&1; then
		fail "direct legacy adoption accepted $fixture managedFields ownership"
	fi
	grep -F -q 'managedFields tuples are not the exact v3 client-apply plus Kubernetes status set' "$tmpdir/$fixture.out" || fail "$fixture did not emit the safe managedFields diagnostic"
	grep -F -q '"fieldPaths"' "$tmpdir/$fixture.out" || fail "$fixture diagnostic omitted redacted field paths"
	assert_no_mutation
done
rm -f "$tmpdir/spec-hash"
: >"$log"
common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null
replace_lines=$(grep -F 'kubectl --context explicit-target replace ' "$log")
[ "$(printf '%s\n' "$replace_lines" | grep -F -c -- '--dry-run=server')" -eq 3 ] || fail 'exact legacy CRD adoption did not server-dry-run every replacement'
[ "$(printf '%s\n' "$replace_lines" | grep -F -v -c -- '--dry-run=server')" -eq 3 ] || fail 'exact legacy CRD adoption did not replace every CRD with a resourceVersion precondition'
grep -F -q 'kubectl --context explicit-target apply --server-side --field-manager kubernetes-secret-generator-crd-manager' "$log" || fail 'legacy CRD replacement did not establish normal SSA ownership'
if grep -F -q -- '--force-conflicts' "$log"; then fail 'legacy CRD replacement used force-conflicts'; fi
grep -F -q 'helm upgrade ksg ' "$log" || fail 'validated legacy CRD takeover did not continue to manager upgrade'

for fixture in active-api active-controller; do
	rm -f "$tmpdir/spec-hash"
	: >"$log"
	case "$fixture" in
		active-api) set -- MOCK_ACTIVE_FLUX_API=true ;;
		active-controller) set -- MOCK_ACTIVE_FLUX_CONTROLLER=true ;;
	esac
	if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
		MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply "$@" \
		EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
		RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
		SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
		CONFIRM_ORPHANED_FLUX_OWNER=dev-infra-stable/gitops \
		CONFIRM_ORPHANED_FLUX_DECOMMISSIONED=dev-infra-stable/gitops \
		"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
		fail "orphaned Flux adoption accepted $fixture"
	fi
	grep -F -q -- '--request-timeout=20s get customresourcedefinitions.apiextensions.k8s.io' "$log" || fail "$fixture did not immediately recheck Flux APIs"
	assert_no_mutation
done

rm -f "$tmpdir/spec-hash"
: >"$log"
common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true \
	MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	CONFIRM_ORPHANED_FLUX_OWNER=dev-infra-stable/gitops \
	CONFIRM_ORPHANED_FLUX_DECOMMISSIONED=dev-infra-stable/gitops \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null
grep -F -q -- '--request-timeout=20s get deployments.apps --all-namespaces' "$log" || fail 'orphaned Flux adoption did not recheck controller deployments'
grep -F -q 'helm upgrade ksg ' "$log" || fail 'validated orphaned Flux adoption did not continue to manager upgrade'

rm -f "$tmpdir/spec-hash"
: >"$log"
if common_env MOCK_RELEASE_EXISTS=true MOCK_CRD_EXISTS=true MOCK_LEGACY_MATCH=true MOCK_REPLACE_CONFLICT=true \
	MOCK_FIELD_MANAGER=kustomize-controller MOCK_FIELD_OPERATION=Apply \
	EXPECTED_SERVER_URL=https://target.example.invalid EXPECTED_CA_SHA256="$ca_sha" \
	RAW_V3_PREFLIGHT_REPORT="$preflight" RAW_V3_PREFLIGHT_SHA256="$preflight_sha" \
	SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1 \
	CONFIRM_ORPHANED_FLUX_OWNER=dev-infra-stable/gitops \
	CONFIRM_ORPHANED_FLUX_DECOMMISSIONED=dev-infra-stable/gitops \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'orphaned Flux CRD replacement ignored a concurrent resourceVersion conflict'
fi
grep -F -q 'kubectl --context explicit-target replace --field-manager kubernetes-secret-generator-crd-manager' "$log" || fail 'concurrency fixture did not reach guarded CRD replacement'
if grep -F -q 'helm upgrade ksg ' "$log"; then fail 'manager rollout continued after a CRD resourceVersion conflict'; fi

: >"$log"
common_env "$repo_root/scripts/helm-release.sh" uninstall >/dev/null
grep -F -q 'helm uninstall ksg --kube-context explicit-target --namespace ksg-system' "$log" || fail 'uninstall did not use the exact release context and namespace'
if grep -F -q 'kubectl --context explicit-target config current-context' "$log"; then
	fail 'deployment guard depends on current-context'
fi

: >"$log"
if common_env MOCK_DUPLICATE_NODES=true PROFILE=production \
	"$repo_root/scripts/helm-release.sh" install >/dev/null 2>&1; then
	fail 'production preflight accepted duplicate hostname topology domains'
fi
grep -F -q 'kubectl --context explicit-target get nodes' "$log" || fail 'production preflight did not inspect schedulable node topology'
assert_no_mutation

: >"$log"
if common_env MOCK_RELEASE_EXISTS=true SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	COMPATIBILITY_PROFILE=v3.4.1 LEADER_ELECTION_ID=custom-v4-lock \
	"$repo_root/scripts/helm-release.sh" upgrade >/dev/null 2>&1; then
	fail 'v3.4.1 rolling rollback accepted a custom leader-election ID'
fi
assert_no_mutation

printf 'Helm release guard verification passed\n'
