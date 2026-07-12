#!/bin/sh
set -eu

run_id=${RUN_ID:-"$(date -u +%Y%m%d%H%M%S)-$$"}
namespace=ksg-benchmark-$run_id
release=ksg-benchmark
created=false

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }
require() { eval "value=\${$1:-}"; [ -n "$value" ] || fail "$1 is required"; }
k() { kubectl --kubeconfig "$KUBECONFIG" --context "$KUBE_CONTEXT" "$@"; }

# shellcheck disable=SC2329 # Invoked by the trap below.
cleanup() {
	status=$?
	trap - 0 1 2 15
	if [ "$created" = true ]; then
		owner=$(k get namespace "$namespace" --ignore-not-found -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
		if [ -n "$owner" ] && [ "$owner" != "$RUN_OWNER_ID" ]; then
			printf '%s\n' 'error: benchmark namespace owner sentinel mismatch; refusing cleanup' >&2
			status=1
		elif [ -n "$owner" ]; then
			k delete namespace "$namespace" --wait=true --timeout=120s >/dev/null || status=1
		fi
	fi
	exit "$status"
}
trap cleanup 0 1 2 15

for name in KUBECONFIG KUBE_CONTEXT CONFIRM_CONTEXT RUN_OWNER_ID CHART_TGZ CANDIDATE_IMAGE; do require "$name"; done
[ "$KUBE_CONTEXT" = "$CONFIRM_CONTEXT" ] || fail 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT'
case "$KUBE_CONTEXT" in kind-*) cluster=${KUBE_CONTEXT#kind-} ;; *) fail 'benchmark requires a kind context' ;; esac
case "$run_id" in ''|*[!a-zA-Z0-9._-]*) fail 'RUN_ID contains unsafe characters' ;; esac
case "$RUN_OWNER_ID" in ''|*[!a-zA-Z0-9._-]*) fail 'RUN_OWNER_ID contains unsafe characters' ;; esac
[ "${#namespace}" -le 63 ] || fail 'derived namespace exceeds 63 bytes'
case "$KUBECONFIG" in /*) ;; *) fail 'KUBECONFIG must be absolute' ;; esac
[ -f "$KUBECONFIG" ] && [ ! -L "$KUBECONFIG" ] || fail 'KUBECONFIG must be a regular non-symlink file'
case "$CHART_TGZ" in /*) ;; *) fail 'CHART_TGZ must be absolute' ;; esac
[ -f "$CHART_TGZ" ] && [ ! -L "$CHART_TGZ" ] || fail 'CHART_TGZ must be a regular non-symlink file'
case "$CANDIDATE_IMAGE" in
	ghcr.io/mrchypark/kubernetes-secret-generator@sha256:????????????????????????????????????????????????????????????????|ghcr.io/mrchypark/kubernetes-secret-generator:*@sha256:????????????????????????????????????????????????????????????????) ;;
	*) fail 'CANDIDATE_IMAGE must be an exact digest reference' ;;
esac
candidate_digest=${CANDIDATE_IMAGE##*@}
case "${candidate_digest#sha256:}" in ''|*[!0-9a-f]*) fail 'candidate digest must be lowercase hexadecimal' ;; esac
for tool in docker kind kubectl helm jq; do command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"; done
kind get clusters 2>/dev/null | grep -F -x -q "$cluster" || fail 'selected kind cluster does not exist'
server=$(k config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
case "$server" in https://127.0.0.1:*|https://localhost:*|https://\[::1\]:*) ;; *) fail 'kind API server is not local' ;; esac
[ -n "$(k config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')" ] || fail 'kind kubeconfig has no embedded CA'
[ "$(k get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}')" = "$RUN_OWNER_ID" ] || fail 'cluster owner sentinel mismatch'
[ "$(k get nodes -o json | jq -r '[.items[].status.nodeInfo.architecture]|unique|if length==1 then .[0] else "mixed" end')" = amd64 ] || fail 'benchmark requires an amd64 kind cluster'
[ -z "$(k get namespace "$namespace" --ignore-not-found -o name)" ] || fail 'benchmark namespace already exists'
docker image inspect "$CANDIDATE_IMAGE" >/dev/null 2>&1 || fail 'CANDIDATE_IMAGE must exist locally'
[ "$(docker image inspect --format '{{.Architecture}}' "$CANDIDATE_IMAGE")" = amd64 ] || fail 'CANDIDATE_IMAGE is not amd64'
[ -n "$(helm show crds "$CHART_TGZ")" ] || fail 'CHART_TGZ must contain CRDs'

k create namespace "$namespace" >/dev/null
created=true
k label namespace "$namespace" "ksg-test-owner=$RUN_OWNER_ID" pod-security.kubernetes.io/enforce=restricted >/dev/null
kind load docker-image "$CANDIDATE_IMAGE" --name "$cluster" >/dev/null
helm show crds "$CHART_TGZ" | k apply --server-side --field-manager=kubernetes-secret-generator-crd-manager -f - >/dev/null
k wait --for=condition=Established --timeout=60s \
	crd/basicauths.secretgenerator.mittwald.de \
	crd/sshkeypairs.secretgenerator.mittwald.de \
	crd/stringsecrets.secretgenerator.mittwald.de >/dev/null
helm install "$release" "$CHART_TGZ" --kubeconfig "$KUBECONFIG" --kube-context "$KUBE_CONTEXT" \
	--namespace "$namespace" --skip-crds --set profile=dev --set replicaCount=1 \
	--set scope.mode=ownNamespace --set migration.confirmedScope=ownNamespace --set crdLifecycle.manager=direct \
	--set image.registry=ghcr.io --set image.repository=mrchypark/kubernetes-secret-generator \
	--set-string image.digest="$candidate_digest" --wait --timeout 180s >/dev/null

emit_string() {
	prefix=$1 count=$2 encoding=$3
	i=1
	while [ "$i" -le "$count" ]; do
		name=$(printf '%s-%03d' "$prefix" "$i")
		cat <<EOF
---
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: StringSecret
metadata:
  name: $name
  labels: {ksg-benchmark: "$run_id"}
spec:
  fields: [{fieldName: value, encoding: $encoding, length: "32"}]
EOF
		i=$((i + 1))
	done
}

emit_basic() {
	prefix=$1 count=$2 encoding=$3
	i=1
	while [ "$i" -le "$count" ]; do
		name=$(printf '%s-%03d' "$prefix" "$i")
		cat <<EOF
---
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: BasicAuth
metadata:
  name: $name
  labels: {ksg-benchmark: "$run_id"}
spec: {username: benchmark, encoding: $encoding, length: "24"}
EOF
		i=$((i + 1))
	done
}

emit_ssh() {
	prefix=$1 count=$2 algorithm=$3 length=${4:-}
	i=1
	while [ "$i" -le "$count" ]; do
		name=$(printf '%s-%03d' "$prefix" "$i")
		printf '%s\n' '---' 'apiVersion: secretgenerator.mittwald.de/v1alpha1' 'kind: SSHKeyPair' 'metadata:' \
			"  name: $name" "  labels: {ksg-benchmark: \"$run_id\"}" 'spec:' "  algorithm: $algorithm"
		[ -z "$length" ] || printf '  length: "%s"\n' "$length"
		i=$((i + 1))
	done
}

started=$(date +%s)
{
	emit_string string-raw 40 raw
	emit_string string-url 20 base64url
	emit_basic basic-raw 10 raw
	emit_basic basic-url 10 base64url
	emit_ssh ssh-ed25519 10 ed25519
	emit_ssh ssh-ecdsa 5 ecdsa 256
	emit_ssh ssh-rsa 3 rsa 2048
	for i in 1 2; do
		cat <<EOF
---
apiVersion: v1
kind: Secret
metadata:
  name: annotation-$i
  labels: {ksg-benchmark: "$run_id"}
  annotations:
    secret-generator.v1.mittwald.de/type: string
    secret-generator.v1.mittwald.de/autogenerate: value
type: Opaque
EOF
	done
} | k -n "$namespace" apply -f - >/dev/null

ready=false
while [ "$(( $(date +%s) - started ))" -le 180 ]; do
	secret_count=$(k -n "$namespace" get secrets -l "ksg-benchmark=$run_id" -o json | jq '.items|length')
	ready_count=$(k -n "$namespace" get stringsecrets,basicauths,sshkeypairs -l "ksg-benchmark=$run_id" -o json | jq '[.items[]|select(any(.status.conditions[]?; .type=="Ready" and .status=="True"))]|length')
	if [ "$secret_count" -eq 100 ] && [ "$ready_count" -eq 98 ]; then ready=true; break; fi
	sleep 2
done
[ "$ready" = true ] || fail "100-object benchmark exceeded 180s (Secrets=$secret_count Ready=$ready_count)"
elapsed=$(( $(date +%s) - started ))
restarts=$(k -n "$namespace" get pods -l app.kubernetes.io/instance="$release" -o json | jq '[.items[].status.containerStatuses[]?.restartCount]|add//0')
[ "$restarts" -eq 0 ] || fail 'controller restarted during benchmark'
if k -n "$namespace" logs -l app.kubernetes.io/instance="$release" --all-containers --prefix 2>&1 | grep -E 'panic:|fatal error:|OOMKilled' >/dev/null; then fail 'controller logs contain panic/fatal/OOM'; fi
jq -cn --argjson elapsed "$elapsed" '{status:"passed",objects:100,readyCustomResources:98,durationSeconds:$elapsed}'
