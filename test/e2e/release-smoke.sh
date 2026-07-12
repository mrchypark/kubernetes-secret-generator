#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/../.." && pwd)
run_id=${RUN_ID:-"$(date -u +%Y%m%d%H%M%S)-$$"}
cluster=ksg-release-$run_id
context=kind-$cluster
namespace=ksg-release-$run_id
release=ksg-release
deployment=$release-kubernetes-secret-generator
kubeconfig=$(mktemp "${TMPDIR:-/tmp}/ksg-release-kubeconfig.XXXXXX")
workdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-release.XXXXXX")
created=false
watchdog=
started=$(date +%s)

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }
require() { eval "value=\${$1:-}"; [ -n "$value" ] || fail "$1 is required"; }
k() { kubectl --kubeconfig "$kubeconfig" --context "$context" "$@"; }
digest() { openssl dgst -sha256 -r | awk '{print $1}'; }
file_digest() { openssl dgst -sha256 -r "$1" | awk '{print "sha256:"$1}'; }

v3_install_diagnostics() {
	printf '%s\n' 'v3 Helm install failed; collecting bounded redacted diagnostics' >&2
	k -n "$namespace" get pods -o wide >&2 || true
	k -n "$namespace" get events --sort-by=.lastTimestamp 2>&1 | tail -n 80 >&2 || true
	for pod in $(k -n "$namespace" get pods -l app.kubernetes.io/instance="$release" -o name 2>/dev/null || true); do
		k -n "$namespace" describe "$pod" 2>&1 | tail -n 200 >&2 || true
		k -n "$namespace" logs "$pod" --all-containers --tail=200 2>&1 | tail -n 200 >&2 || true
	done
}

# shellcheck disable=SC2329 # Invoked by the trap below.
cleanup() {
	status=$?
	trap - 0 1 2 15
	[ -z "$watchdog" ] || kill "$watchdog" 2>/dev/null || true
	if [ "$created" = true ]; then
		owner=$(k get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
		if [ "$owner" != "$run_id" ]; then
			printf '%s\n' 'error: cluster owner sentinel mismatch; refusing cleanup' >&2
			status=1
		else
			ns_owner=$(k get namespace "$namespace" --ignore-not-found -o jsonpath='{.metadata.labels.ksg-test-owner}' 2>/dev/null || true)
			if [ -n "$ns_owner" ] && [ "$ns_owner" != "$run_id" ]; then
				printf '%s\n' 'error: namespace owner sentinel mismatch; refusing cleanup' >&2
				status=1
			else
				[ -z "$ns_owner" ] || k delete namespace "$namespace" --wait=true --timeout=120s >/dev/null || status=1
				if [ "$status" -eq 0 ]; then kind delete cluster --name "$cluster" >/dev/null || status=1; fi
			fi
		fi
	fi
	rm -rf "$workdir"
	rm -f "$kubeconfig"
	exit "$status"
}
trap cleanup 0 1 2 15

case "$run_id" in ''|*[!a-zA-Z0-9._-]*) fail 'RUN_ID contains unsafe characters' ;; esac
[ "${#namespace}" -le 63 ] || fail 'derived namespace exceeds 63 bytes'
locked_v3=$(awk -F= '$1=="image.v3.4.1.linux-amd64" {print $2}' "$repo_root/tools.lock")
V3_COMPAT_IMAGE=${V3_COMPAT_IMAGE:-$locked_v3}
for name in CHART_TGZ CANDIDATE_IMAGE RELEASE_TAG V3_COMPAT_IMAGE; do require "$name"; done
case "$CHART_TGZ" in /*) ;; *) fail 'CHART_TGZ must be absolute' ;; esac
[ -f "$CHART_TGZ" ] && [ ! -L "$CHART_TGZ" ] || fail 'CHART_TGZ must be a regular non-symlink file'
case "$CANDIDATE_IMAGE" in
	ghcr.io/mrchypark/kubernetes-secret-generator@sha256:????????????????????????????????????????????????????????????????|ghcr.io/mrchypark/kubernetes-secret-generator:*@sha256:????????????????????????????????????????????????????????????????) ;;
	*) fail 'CANDIDATE_IMAGE must be an exact digest reference' ;;
esac
case "$V3_COMPAT_IMAGE" in ghcr.io/mrchypark/kubernetes-secret-generator:v3.4.1@sha256:????????????????????????????????????????????????????????????????) ;; *) fail 'V3_COMPAT_IMAGE must pin v3.4.1 by tag and digest' ;; esac
[ "$V3_COMPAT_IMAGE" = "$locked_v3" ] || fail 'V3_COMPAT_IMAGE differs from the locked amd64 v3.4.1 image'
printf '%s\n' "$RELEASE_TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[1-9][0-9]*)?$' || fail 'RELEASE_TAG is invalid'
candidate_digest=${CANDIDATE_IMAGE##*@}
v3_digest=${V3_COMPAT_IMAGE##*@}
case "${candidate_digest#sha256:}${v3_digest#sha256:}" in *[!0-9a-f]*) fail 'image digests must be lowercase hexadecimal' ;; esac
case "$(uname -m)" in x86_64|amd64) ;; *) fail 'release smoke requires an amd64 runner' ;; esac
for tool in docker kind kubectl helm jq openssl git tar go; do command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"; done
locked_kind=$(awk -F= '$1=="kind.version" {print $2}' "$repo_root/tools.lock")
[ "$(kind version | awk '{print $2}')" = "$locked_kind" ] || fail "kind $locked_kind is required"
node_image=$(awk -F= '$1=="kind.node.v1.35.0" {print $2}' "$repo_root/tools.lock")
case "$node_image" in kindest/node:*@sha256:????????????????????????????????????????????????????????????????) ;; *) fail 'locked amd64 kind node image is malformed' ;; esac
kind get clusters 2>/dev/null | grep -F -x -q "$cluster" && fail 'refusing pre-existing cluster'
for image in "$CANDIDATE_IMAGE" "$V3_COMPAT_IMAGE"; do
	docker pull --platform linux/amd64 "$image" >/dev/null
	[ "$(docker image inspect --format '{{.Architecture}}' "$image")" = amd64 ] || fail "image is not amd64: $image"
done
candidate_image_id=$(docker image inspect --format '{{.Id}}' "$CANDIDATE_IMAGE")
v3_image_id=$(docker image inspect --format '{{.Id}}' "$V3_COMPAT_IMAGE")
candidate_local_image=ksg-release-candidate:$run_id
v3_local_image=ksg-release-v3:$run_id
docker image tag "$CANDIDATE_IMAGE" "$candidate_local_image"
docker image tag "$V3_COMPAT_IMAGE" "$v3_local_image"
[ "$(docker image inspect --format '{{.Id}}' "$candidate_local_image")" = "$candidate_image_id" ] || fail 'candidate local tag does not match exact candidate image ID'
[ "$(docker image inspect --format '{{.Id}}' "$v3_local_image")" = "$v3_image_id" ] || fail 'v3 local tag does not match exact v3 image ID'
chart_digest=$(file_digest "$CHART_TGZ")
[ "$(helm show chart "$CHART_TGZ" | awk '$1=="version:" {print $2}')" = "${RELEASE_TAG#v}" ] || fail 'CHART_TGZ version differs from RELEASE_TAG'
[ -n "$(helm show crds "$CHART_TGZ")" ] || fail 'CHART_TGZ must contain the v4 CRDs'

(
	sleep 900
	kill -TERM "$$" 2>/dev/null || true
) &
watchdog=$!

kind create cluster --name "$cluster" --image "$node_image" --kubeconfig "$kubeconfig" --wait 180s --config - >&2 <<'EOF'
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
EOF
created=true
server=$(k config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
case "$server" in https://127.0.0.1:*|https://localhost:*|https://\[::1\]:*) ;; *) fail 'kind API server is not local' ;; esac
ca_data=$(k config view --minify --raw -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[ -n "$ca_data" ] || fail 'kind kubeconfig has no embedded CA'
ca_sha=$(printf '%s' "$ca_data" | openssl base64 -d -A | openssl dgst -sha256 -r | awk '{print $1}')
case "$ca_sha" in ????????????????????????????????????????????????????????????????) ;; *) fail 'kind CA fingerprint is malformed' ;; esac
k label namespace kube-system "ksg-test-owner=$run_id" --overwrite >/dev/null
[ "$(k get namespace kube-system -o jsonpath='{.metadata.labels.ksg-test-owner}')" = "$run_id" ] || fail 'cluster owner sentinel was not established'
k create namespace "$namespace" >/dev/null
k label namespace "$namespace" "ksg-test-owner=$run_id" pod-security.kubernetes.io/warn=restricted >/dev/null
[ "$(k get nodes -o jsonpath='{.items[0].status.nodeInfo.architecture}')" = amd64 ] || fail 'kind node is not amd64'
kind load docker-image "$v3_local_image" --name "$cluster" >/dev/null
kind load docker-image "$candidate_local_image" --name "$cluster" >/dev/null

v3_tree=$workdir/v3
mkdir "$v3_tree"
git -C "$repo_root" archive b01e37dce377e5e4296392b7e4d823b6830b763e deploy | tar -x -C "$v3_tree"
k apply -f "$repo_root/test/fixtures/v3.4.1/crds" >/dev/null
k wait --for=condition=Established --timeout=60s \
	crd/basicauths.secretgenerator.mittwald.de \
	crd/sshkeypairs.secretgenerator.mittwald.de \
	crd/stringsecrets.secretgenerator.mittwald.de >/dev/null
if ! helm install "$release" "$v3_tree/deploy/helm-chart/kubernetes-secret-generator" \
	--kubeconfig "$kubeconfig" --kube-context "$context" --namespace "$namespace" \
	--set installCRDs=true --set rbac.clusterRole=false --set-string watchNamespace="$namespace" \
	--set image.registry= --set image.repository=ksg-release-v3 \
	--set image.pullPolicy=IfNotPresent \
	--set-string "image.tag=$run_id" \
	--wait --timeout 180s >/dev/null; then
	v3_install_diagnostics
	fail 'v3 Helm install failed'
fi

k -n "$namespace" apply -f - >/dev/null <<'EOF'
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: StringSecret
metadata: {name: smoke-string}
spec:
  fields: [{fieldName: password, encoding: raw, length: "32"}]
---
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: BasicAuth
metadata: {name: smoke-basic}
spec: {username: admin, length: "32", encoding: base64url}
---
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: SSHKeyPair
metadata: {name: smoke-ssh}
spec: {algorithm: ed25519, privateKeyField: ssh-privatekey, publicKeyField: ssh-publickey}
---
apiVersion: v1
kind: Secret
metadata:
  name: smoke-annotation
  annotations:
    secret-generator.v1.mittwald.de/type: string
    secret-generator.v1.mittwald.de/autogenerate: token
    secret-generator.v1.mittwald.de/secure: "yes"
type: Opaque
EOF

wait_secret() {
	name=$1
	i=0
	while [ "$i" -lt 60 ]; do
		k -n "$namespace" get secret "$name" >/dev/null 2>&1 && return 0
		sleep 1
		i=$((i + 1))
	done
	return 1
}
for name in smoke-string smoke-basic smoke-ssh smoke-annotation; do wait_secret "$name" || fail "v3 did not reconcile $name"; done
# The v3 CRD controller generated StringSecret values with crypto/rand but did
# not persist the marker used by the v4 migration preflight. Record that known
# fixture provenance explicitly; real clusters require the same human review.
k -n "$namespace" annotate secret smoke-string secret-generator.v1.mittwald.de/secure=yes --overwrite >/dev/null
[ "$(k -n "$namespace" get secret smoke-string -o jsonpath='{.metadata.annotations.secret-generator\.v1\.mittwald\.de/secure}')" = yes ] || fail 'v3 StringSecret secure provenance marker was not established'
secret_hash() { k -n "$namespace" get secret "$1" -o json | jq -cS .data | digest; }
string_hash=$(secret_hash smoke-string)
basic_hash=$(secret_hash smoke-basic)
ssh_hash=$(secret_hash smoke-ssh)
annotation_hash=$(secret_hash smoke-annotation)

assert_owner() {
	resource=$1 name=$2 kind=$3
	uid=$(k -n "$namespace" get "$resource" "$name" -o jsonpath='{.metadata.uid}')
	k -n "$namespace" get secret "$name" -o json | jq -e --arg uid "$uid" --arg kind "$kind" '
		(.metadata.ownerReferences | length) == 1 and
		.metadata.ownerReferences[0].uid == $uid and
		.metadata.ownerReferences[0].kind == $kind and
		.metadata.ownerReferences[0].controller == true' >/dev/null || fail "exact owner mismatch for $name"
}
assert_owners() {
	assert_owner stringsecret smoke-string StringSecret
	assert_owner basicauth smoke-basic BasicAuth
	assert_owner sshkeypair smoke-ssh SSHKeyPair
}
assert_owners

preflight_report=${PREFLIGHT_REPORT_OUT:-$workdir/preflight-report.md}
case "$preflight_report" in /*) ;; *) fail 'PREFLIGHT_REPORT_OUT must be absolute' ;; esac
[ ! -e "$preflight_report" ] || fail 'PREFLIGHT_REPORT_OUT already exists'
if KUBECONFIG="$kubeconfig" KUBE_CONTEXT="$context" CONFIRM_CONTEXT="$context" \
	EXPECTED_SERVER_URL="$server" EXPECTED_CA_SHA256="$ca_sha" NAMESPACE="$namespace" \
	RELEASE_NAME="$release" DEPLOYMENT_NAME="$deployment" SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	REPORT_FORMAT=markdown REPORT_FILE="$preflight_report" \
	"$repo_root/scripts/preflight-v4.sh" >/dev/null; then
	:
else
	status=$?
	if [ -s "$preflight_report" ]; then
		printf '%s\n' 'read-only v4 preflight failed; sanitized report follows' >&2
		cat "$preflight_report" >&2
	else
		printf '%s\n' 'read-only v4 preflight failed before producing a sanitized report' >&2
	fi
	exit "$status"
fi
grep -F -x -q -- '- Blockers: **0**' "$preflight_report" || fail 'read-only v4 preflight reported blockers'

# A v3.4.1 client-side apply owns the CRD version list. Take over only after
# the zero-blocker preflight and exact normalized v3 spec verification.
legacy_live_dir=$workdir/legacy-live
mkdir "$legacy_live_dir"
for crd in basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de; do
	case "$crd" in
		basicauths.*) lock_key=crd.v3.4.1.basicauths.spec-sha256 ;;
		sshkeypairs.*) lock_key=crd.v3.4.1.sshkeypairs.spec-sha256 ;;
		stringsecrets.*) lock_key=crd.v3.4.1.stringsecrets.spec-sha256 ;;
	esac
	expected_spec_sha=$(awk -F= -v key="$lock_key" '$1 == key { print $2; found=1 } END { if (!found) exit 1 }' "$repo_root/tools.lock")
	live_crd=$legacy_live_dir/$crd.json
	k get "customresourcedefinition/$crd" -o json >"$live_crd"
	jq -e '(.metadata.managedFields // []) as $fields |
		($fields | length) > 0 and all($fields[]; .manager == "kubectl-client-side-apply" and .operation == "Update")' \
		"$live_crd" >/dev/null || fail "$crd has an unexpected legacy field manager"
	jq -e '.metadata.uid != null and .metadata.uid != "" and .metadata.resourceVersion != null and .metadata.resourceVersion != ""' \
		"$live_crd" >/dev/null || fail "$crd lacks UID/resourceVersion concurrency evidence"
	actual_spec_sha=$(jq -Sc '.spec | if .conversion == {strategy:"None"} then del(.conversion) else . end | if .preserveUnknownFields == false then del(.preserveUnknownFields) else . end' "$live_crd" |
		openssl dgst -sha256 -r | awk '{print $1}')
	[ "$actual_spec_sha" = "$expected_spec_sha" ] || fail "$crd does not match the pinned v3.4.1 CRD spec before ownership takeover"
done

crds=$workdir/v4-crds.yaml
helm show crds "$CHART_TGZ" >"$crds"
adoption_preflight=$workdir/adoption-preflight.json
KUBECONFIG="$kubeconfig" KUBE_CONTEXT="$context" CONFIRM_CONTEXT="$context" \
	EXPECTED_SERVER_URL="$server" EXPECTED_CA_SHA256="$ca_sha" NAMESPACE="$namespace" \
	RELEASE_NAME="$release" DEPLOYMENT_NAME="$deployment" SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	"$repo_root/scripts/preflight-v4.sh" >"$adoption_preflight" || fail 'immediate legacy-adoption preflight reported blockers or an unstable snapshot'
[ "$(jq -r '.blockerCount' "$adoption_preflight")" -eq 0 ] || fail 'immediate legacy-adoption preflight reported blockers'
for crd_file in "$repo_root"/deploy/helm-chart/kubernetes-secret-generator/crds/*_crd.yaml; do
	crd=$(awk '$1 == "name:" { print $2; exit }' "$crd_file")
	live_crd=$legacy_live_dir/$crd.json
	uid=$(jq -er '.metadata.uid' "$live_crd")
	resource_version=$(jq -er '.metadata.resourceVersion' "$live_crd")
	target_crd=$workdir/target-$crd.json
	k create --dry-run=client -f "$crd_file" -o json |
		jq --arg uid "$uid" --arg rv "$resource_version" '.metadata.uid=$uid | .metadata.resourceVersion=$rv' >"$target_crd"
	k replace --dry-run=server --field-manager=kubernetes-secret-generator-crd-manager -f "$target_crd" >/dev/null
	k replace --field-manager=kubernetes-secret-generator-crd-manager -f "$target_crd" >/dev/null
	k apply --server-side --field-manager=kubernetes-secret-generator-crd-manager -f "$crd_file" >/dev/null
done
k wait --for=condition=Established --timeout=60s \
	crd/basicauths.secretgenerator.mittwald.de \
	crd/sshkeypairs.secretgenerator.mittwald.de \
	crd/stringsecrets.secretgenerator.mittwald.de >/dev/null

upgrade_manager() {
	profile=$1 local_image=$2 reuse=${3:-false}
	image_repository=${local_image%:*}
	image_tag=${local_image##*:}
	reuse_flag=
	[ "$reuse" = false ] || reuse_flag=--reuse-values
	# shellcheck disable=SC2086 # Empty or the single constant --reuse-values.
	helm upgrade "$release" "$CHART_TGZ" --kubeconfig "$kubeconfig" --kube-context "$context" \
		--namespace "$namespace" --skip-crds $reuse_flag \
		--set profile=dev --set replicaCount=1 --set scope.mode=ownNamespace \
		--set migration.confirmedScope=ownNamespace --set crdLifecycle.manager=direct \
		--set leaderElection.enabled=true --set leaderElection.id=kubernetes-secret-generator-lock \
		--set "compatibilityProfile=$profile" --set image.registry= \
		--set-string image.repository="$image_repository" --set-string image.tag="$image_tag" \
		--set-string image.digest= --set image.pullPolicy=IfNotPresent \
		--wait --timeout 180s >/dev/null
	k -n "$namespace" rollout status deployment/"$deployment" --timeout=180s >/dev/null
	[ "$(k -n "$namespace" get deployment "$deployment" -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="WATCH_NAMESPACE")].value}')" = "$namespace" ] || fail 'manager scope changed'
	i=0
	while [ "$i" -lt 60 ]; do
		[ -n "$(k -n "$namespace" get lease kubernetes-secret-generator-lock -o jsonpath='{.spec.holderIdentity}' 2>/dev/null || true)" ] && return 0
		sleep 1
		i=$((i + 1))
	done
	fail 'default leader lease has no holder'
}

assert_controller_health() {
	k -n "$namespace" wait --for=condition=Available deployment/"$deployment" --timeout=10s >/dev/null
	desired=$(k -n "$namespace" get deployment "$deployment" -o jsonpath='{.spec.replicas}')
	ready=$(k -n "$namespace" get deployment "$deployment" -o jsonpath='{.status.readyReplicas}')
	[ "$ready" = "$desired" ] || fail 'controller is not fully Ready'
	restarts=$(k -n "$namespace" get pods -l app.kubernetes.io/instance="$release" -o json | jq '[.items[].status.containerStatuses[]?.restartCount] | add // 0')
	[ "$restarts" -eq 0 ] || fail 'controller restarted during release smoke'
	if k -n "$namespace" logs -l app.kubernetes.io/instance="$release" --all-containers --prefix 2>&1 | grep -E 'panic:|fatal error:|OOMKilled' >/dev/null; then fail 'controller logs contain panic/fatal/OOM'; fi
}

assert_recovery_state() {
	for resource in stringsecret/smoke-string basicauth/smoke-basic sshkeypair/smoke-ssh; do
		k -n "$namespace" wait --for='jsonpath={.status.conditions[?(@.type=="Ready")].status}=True' "$resource" --timeout=10s >/dev/null
	done
	assert_owners
	[ "$(secret_hash smoke-string)$(secret_hash smoke-basic)$(secret_hash smoke-ssh)$(secret_hash smoke-annotation)" = "$string_hash$basic_hash$ssh_hash$annotation_hash" ] || fail 'observed recovery state changed a credential'
}

upgrade_manager v4 "$candidate_local_image" false
for resource in stringsecret/smoke-string basicauth/smoke-basic sshkeypair/smoke-ssh; do
	k -n "$namespace" wait --for='jsonpath={.status.conditions[?(@.type=="Ready")].status}=True' "$resource" --timeout=60s >/dev/null
done
assert_owners
[ "$(secret_hash smoke-string)" = "$string_hash" ] || fail 'v4 upgrade rotated StringSecret'
[ "$(secret_hash smoke-basic)" = "$basic_hash" ] || fail 'v4 upgrade rotated BasicAuth'
[ "$(secret_hash smoke-ssh)" = "$ssh_hash" ] || fail 'v4 upgrade rotated SSHKeyPair'
[ "$(secret_hash smoke-annotation)" = "$annotation_hash" ] || fail 'v4 upgrade rotated annotation String Secret'
assert_controller_health

old_uid=$(k -n "$namespace" get secret smoke-string -o jsonpath='{.metadata.uid}')
k -n "$namespace" delete secret smoke-string --wait=true >/dev/null
i=0
while [ "$i" -lt 60 ]; do
	new_uid=$(k -n "$namespace" get secret smoke-string -o jsonpath='{.metadata.uid}' 2>/dev/null || true)
	[ -n "$new_uid" ] && [ "$new_uid" != "$old_uid" ] && break
	sleep 1
	i=$((i + 1))
done
[ -n "${new_uid:-}" ] && [ "$new_uid" != "$old_uid" ] || fail 'deleted String Secret was not recreated'
assert_owner stringsecret smoke-string StringSecret
string_hash=$(secret_hash smoke-string)

k -n "$namespace" patch secret smoke-basic --type merge -p '{"data":{"password":"ZHJpZnQ="}}' >/dev/null
healed=false
healed_hash=
i=0
while [ "$i" -lt 60 ]; do
	current_hash=$(secret_hash smoke-basic)
	drift_removed=$(k -n "$namespace" get secret smoke-basic -o json | jq -r '.data.password != null and .data.password != "ZHJpZnQ="')
	if [ "$current_hash" != "$basic_hash" ] && [ "$drift_removed" = true ]; then
		healed_hash=$current_hash
		healed=true
		break
	fi
	sleep 1
	i=$((i + 1))
done
[ "$healed" = true ] || fail 'BasicAuth drift was not healed'
k -n "$namespace" wait --for='jsonpath={.status.conditions[?(@.type=="Ready")].status}=True' basicauth/smoke-basic --timeout=10s >/dev/null
assert_owner basicauth smoke-basic BasicAuth
[ "$healed_hash" != "$basic_hash" ] || fail 'BasicAuth self-heal did not rotate credentials'
stable_rv=$(k -n "$namespace" get secret smoke-basic -o jsonpath='{.metadata.resourceVersion}')
sleep 5
[ "$(k -n "$namespace" get secret smoke-basic -o jsonpath='{.metadata.resourceVersion}')" = "$stable_rv" ] || fail 'BasicAuth self-heal caused an update storm'
[ "$(secret_hash smoke-basic)" = "$healed_hash" ] || fail 'BasicAuth self-heal did not become stable'
basic_hash=$healed_hash

crd_hash=$(k get crd basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de -o json | jq -cS '[.items[]|{name:.metadata.name,spec:.spec}]|sort_by(.name)' | digest)
counts=$(k -n "$namespace" get stringsecrets,basicauths,sshkeypairs,secrets -o json | jq -c '[.items|group_by(.kind)|map({(.[0].kind):length})|add]')
upgrade_manager v3.4.1 "$v3_local_image" true
[ "$(k get crd basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de -o json | jq -cS '[.items[]|{name:.metadata.name,spec:.spec}]|sort_by(.name)' | digest)" = "$crd_hash" ] || fail 'manager rollback changed v4 CRDs'
[ "$(k -n "$namespace" get stringsecrets,basicauths,sshkeypairs,secrets -o json | jq -c '[.items|group_by(.kind)|map({(.[0].kind):length})|add]')" = "$counts" ] || fail 'manager rollback changed object counts'
[ "$(secret_hash smoke-string)" = "$string_hash" ] || fail 'manager rollback rotated StringSecret'
[ "$(secret_hash smoke-basic)" = "$basic_hash" ] || fail 'manager rollback rotated BasicAuth'
[ "$(secret_hash smoke-ssh)" = "$ssh_hash" ] || fail 'manager rollback rotated SSHKeyPair'
[ "$(secret_hash smoke-annotation)" = "$annotation_hash" ] || fail 'manager rollback rotated annotation String Secret'

upgrade_manager v4 "$candidate_local_image" true
for resource in stringsecret/smoke-string basicauth/smoke-basic sshkeypair/smoke-ssh; do
	k -n "$namespace" wait --for='jsonpath={.status.conditions[?(@.type=="Ready")].status}=True' "$resource" --timeout=60s >/dev/null
done
assert_owners
[ "$(secret_hash smoke-string)$(secret_hash smoke-basic)$(secret_hash smoke-ssh)$(secret_hash smoke-annotation)" = "$string_hash$basic_hash$ssh_hash$annotation_hash" ] || fail 'v4 re-upgrade rotated a credential'

health_checks=0
observation_started=$(date +%s)
while :; do
	assert_controller_health
	assert_recovery_state
	health_checks=$((health_checks + 1))
	now=$(date +%s)
	observation_elapsed=$((now - observation_started))
	elapsed=$((now - started))
	[ "$elapsed" -lt 900 ] || fail 'release smoke exceeded 15 minutes'
	[ "$elapsed" -ge 600 ] && break
	remaining=$((600 - elapsed))
	if [ "$remaining" -lt 10 ]; then sleep "$remaining"; else sleep 10; fi
done

elapsed=$(( $(date +%s) - started ))
observation_elapsed=$(( $(date +%s) - observation_started ))
jq -cn --arg chart "$chart_digest" --arg candidate "$candidate_digest" --arg v3 "$v3_digest" \
	--argjson elapsed "$elapsed" --argjson observation "$observation_elapsed" --argjson checks "$health_checks" \
	'{status:"passed",objects:{customResources:3,managedSecrets:4},durationSeconds:$elapsed,observationSeconds:$observation,healthChecks:$checks,controller:{ready:true,restarts:0,panicOrOOM:false,recovered:true},digests:{chart:$chart,candidate:$candidate,v3Compatibility:$v3}}'
