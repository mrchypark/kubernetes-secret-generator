#!/bin/sh
set -eu

KIND_VERSION=${KIND_VERSION:-v0.31.0}
KIND_NODE_IMAGE=${KIND_NODE_IMAGE:-kindest/node:v1.35.0@sha256:452d707d4862f52530247495d180205e029056831160e22870e37e3f6c1ac31f}
run_id=${RUN_ID:-"$(date -u +%Y%m%d%H%M%S)-$$"}
cluster_name=${KIND_CLUSTER_NAME:-"ksg-e2e-$run_id"}
test_namespace=${TEST_NAMESPACE:-"ksg-e2e-$run_id"}
owned_namespaces=$test_namespace
kubeconfig=$(mktemp "${TMPDIR:-/tmp}/ksg-kubeconfig.XXXXXX")
created=false

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

case "$run_id" in *[!a-zA-Z0-9._-]*|'') fail 'RUN_ID contains unsafe characters' ;; esac
case "$cluster_name" in ksg-e2e-*) ;; *) fail 'cluster name must start with ksg-e2e-' ;; esac
case "$test_namespace" in default|kube-*|'') fail 'refusing default, kube-*, or empty namespace' ;; esac
case "$test_namespace" in ksg-e2e-*) ;; *) fail 'test namespace must start with ksg-e2e-' ;; esac
[ "${#test_namespace}" -le 63 ] || fail 'test namespace must be at most 63 bytes'

command -v kind >/dev/null 2>&1 || fail 'kind is required'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
command -v openssl >/dev/null 2>&1 || fail 'openssl is required'
command -v helm >/dev/null 2>&1 || fail 'helm is required'
[ "$(kind version | awk '{print $2}')" = "$KIND_VERSION" ] || fail "kind $KIND_VERSION is required"
if kind get clusters 2>/dev/null | grep -F -x -q "$cluster_name"; then
	fail 'refusing pre-existing cluster'
fi

cleanup() {
	status=$?
	trap - 0 1 2 15
	if [ "$created" = true ]; then
		owner=$(kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
		if [ "$owner" != "$run_id" ]; then
			printf '%s\n' 'error: cluster owner sentinel mismatch; refusing destructive cleanup' >&2
			status=1
		else
			cleanup_ok=true
			for namespace in $owned_namespaces; do
				existing=$(kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" get namespace "$namespace" --ignore-not-found -o name 2>/dev/null) || {
					printf 'error: could not verify namespace state for %s; refusing cluster deletion\n' "$namespace" >&2
					cleanup_ok=false
					continue
				}
				if [ -z "$existing" ]; then
					continue
				fi
				ns_owner=$(kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" get namespace "$namespace" -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
				if [ "$ns_owner" != "$run_id" ]; then
					printf 'error: namespace owner sentinel mismatch for %s; refusing deletion\n' "$namespace" >&2
					cleanup_ok=false
					continue
				fi
				kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" delete namespace "$namespace" --wait=true --timeout=60s >/dev/null
				existing=$(kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" get namespace "$namespace" --ignore-not-found -o name 2>/dev/null) || {
					printf 'error: could not verify namespace deletion for %s\n' "$namespace" >&2
					cleanup_ok=false
					continue
				}
				if [ -n "$existing" ]; then
					printf 'error: run-owned namespace still exists after cleanup: %s\n' "$namespace" >&2
					cleanup_ok=false
				fi
			done
			if [ "$cleanup_ok" = true ]; then
				owner=$(kubectl --kubeconfig "$kubeconfig" --context "kind-$cluster_name" get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
				[ "$owner" = "$run_id" ] || { printf '%s\n' 'error: cluster owner sentinel changed before deletion' >&2; exit 1; }
				kind delete cluster --name "$cluster_name" >/dev/null
				deleted=false
				for _ in 1 2 3 4 5 6 7 8 9 10; do
					clusters=$(kind get clusters 2>/dev/null) || { printf '%s\n' 'error: could not list kind clusters after deletion' >&2; status=1; break; }
					if ! printf '%s\n' "$clusters" | grep -F -x -q "$cluster_name"; then deleted=true; break; fi
					sleep 1
				done
				[ "$deleted" = true ] || { printf '%s\n' 'error: kind cluster deletion was not confirmed' >&2; status=1; }
			else
				status=1
			fi
		fi
	fi
	rm -f "$kubeconfig"
	exit "$status"
}
trap cleanup 0 1 2 15

kind create cluster --name "$cluster_name" --image "$KIND_NODE_IMAGE" --kubeconfig "$kubeconfig" --wait 120s
created=true
expected_context="kind-$cluster_name"
kube_context=${KUBE_CONTEXT:-$expected_context}
confirm_context=${CONFIRM_CONTEXT:-$expected_context}
[ "$kube_context" = "$expected_context" ] || fail 'KUBE_CONTEXT does not select the run-owned cluster'
[ "$confirm_context" = "$kube_context" ] || fail 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT'

server=$(kubectl --kubeconfig "$kubeconfig" --context "$kube_context" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
case "$server" in
	https://127.0.0.1:*|https://localhost:*|https://\[::1\]:*) ;;
	*) fail 'kind API server is not local' ;;
esac
ca_data=$(kubectl --kubeconfig "$kubeconfig" --context "$kube_context" config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[ -n "$ca_data" ] || fail 'kind kubeconfig has no embedded CA data'
ca_sha=$(printf '%s' "$ca_data" | openssl base64 -d -A | openssl dgst -sha256 -r | awk '{print $1}')
case "$ca_sha" in
	????????????????????????????????????????????????????????????????) ;;
	*) fail 'could not verify kind API CA fingerprint' ;;
esac

kubectl --kubeconfig "$kubeconfig" --context "$kube_context" label namespace kube-system "ksg-test-owner=$run_id" --overwrite >/dev/null
[ "$(kubectl --kubeconfig "$kubeconfig" --context "$kube_context" get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}')" = "$run_id" ] || fail 'failed to establish cluster owner sentinel'

kubectl --kubeconfig "$kubeconfig" --context "$kube_context" create namespace "$test_namespace" >/dev/null
kubectl --kubeconfig "$kubeconfig" --context "$kube_context" label namespace "$test_namespace" "ksg-test-owner=$run_id" >/dev/null
kubectl --kubeconfig "$kubeconfig" --context "$kube_context" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: ResourceQuota
metadata:
  name: ksg-test-quota
  namespace: $test_namespace
spec:
  hard:
    limits.cpu: "4"
    limits.memory: 4Gi
    requests.cpu: "2"
    requests.memory: 2Gi
    secrets: "300"
---
apiVersion: v1
kind: LimitRange
metadata:
  name: ksg-test-limits
  namespace: $test_namespace
spec:
  limits:
  - type: Container
    default:
      cpu: 500m
      memory: 512Mi
    defaultRequest:
      cpu: 50m
      memory: 64Mi
EOF

kubectl --kubeconfig "$kubeconfig" --context "$kube_context" apply -f deploy/crds >/dev/null
kubectl --kubeconfig "$kubeconfig" --context "$kube_context" wait --for=condition=Established crd --all --timeout=60s >/dev/null

extra_namespace="$test_namespace-extra"
outside_namespace="$test_namespace-outside"
for namespace in "$extra_namespace" "$outside_namespace"; do
	[ "${#namespace}" -le 63 ] || fail 'derived test namespace is too long'
	kubectl --kubeconfig "$kubeconfig" --context "$kube_context" create namespace "$namespace" >/dev/null
	kubectl --kubeconfig "$kubeconfig" --context "$kube_context" label namespace "$namespace" "ksg-test-owner=$run_id" >/dev/null
	owned_namespaces="$owned_namespaces $namespace"
done

assert_auth() {
	expected=$1
	service_account=$2
	namespace=$3
	verb=$4
	resource=$5
	actual=$(kubectl --kubeconfig "$kubeconfig" --context "$kube_context" auth can-i \
		--as "system:serviceaccount:$test_namespace:$service_account" --namespace "$namespace" "$verb" "$resource")
	[ "$actual" = "$expected" ] || fail "RBAC $service_account $namespace $verb $resource = $actual, want $expected"
}

verify_access_contract() {
	service_account=$1
	namespace=$2
	for verb in get list watch create update patch; do assert_auth yes "$service_account" "$namespace" "$verb" secrets; done
	assert_auth no "$service_account" "$namespace" delete secrets
	for verb in get list watch; do assert_auth yes "$service_account" "$namespace" "$verb" stringsecrets.secretgenerator.mittwald.de; done
	for verb in update patch; do assert_auth no "$service_account" "$namespace" "$verb" stringsecrets.secretgenerator.mittwald.de; done
	for verb in get update patch; do assert_auth yes "$service_account" "$namespace" "$verb" stringsecrets/status.secretgenerator.mittwald.de; done
	for verb in create patch; do assert_auth yes "$service_account" "$namespace" "$verb" events; done
	assert_auth no "$service_account" "$namespace" delete pods
	assert_auth no "$service_account" "$namespace" create servicemonitors.monitoring.coreos.com
}

render_and_apply_scope() {
	release=$1
	scope=$2
	shift 2
	rendered=$(mktemp "${TMPDIR:-/tmp}/ksg-scope.XXXXXX")
	helm template "$release" deploy/helm-chart/kubernetes-secret-generator --namespace "$test_namespace" \
		--kubeconfig "$kubeconfig" --kube-context "$kube_context" \
		--set profile=dev --set image.registry= --set image.repository=invalid.local/ksg --set-string image.tag=v4.0.0 \
		--set "scope.mode=$scope" "$@" >"$rendered"
	[ -s "$rendered" ] || fail "Helm rendered no manifests for $release"
	kubectl --kubeconfig "$kubeconfig" --context "$kube_context" apply -f "$rendered" >/dev/null
	rm -f "$rendered"
}

render_and_apply_scope rbac-own ownNamespace
own_sa=rbac-own-kubernetes-secret-generator
verify_access_contract "$own_sa" "$test_namespace"
assert_auth no "$own_sa" "$extra_namespace" get secrets

render_and_apply_scope rbac-namespaces namespaces --set "scope.namespaces={$test_namespace,$extra_namespace}"
namespaces_sa=rbac-namespaces-kubernetes-secret-generator
verify_access_contract "$namespaces_sa" "$test_namespace"
verify_access_contract "$namespaces_sa" "$extra_namespace"
assert_auth no "$namespaces_sa" "$outside_namespace" get secrets

render_and_apply_scope rbac-cluster cluster
cluster_sa=rbac-cluster-kubernetes-secret-generator
verify_access_contract "$cluster_sa" "$test_namespace"
verify_access_contract "$cluster_sa" "$outside_namespace"

printf 'kind safety foundation passed for run %s\n' "$run_id"
