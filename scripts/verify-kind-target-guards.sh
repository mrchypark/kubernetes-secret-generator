#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
foundation=$repo_root/test/e2e/kind-foundation.sh
release=$repo_root/test/e2e/release-smoke.sh
benchmark=$repo_root/test/e2e/benchmark.sh
inventory_filter=$repo_root/test/e2e/release-smoke-inventory.jq
recreate_observer=$repo_root/test/e2e/recreate-observer.sh
recreate_observer_test=$repo_root/scripts/test-recreate-observer.sh

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }

for guarded in "$release" "$benchmark"; do
	if grep -E '^[[:space:]]*kubectl ' "$guarded" >/dev/null; then
		fail "$(basename "$guarded") contains kubectl outside its guarded helper"
	fi
done
if awk '
	/kubectl --/ && ($0 !~ /--kubeconfig "\$kubeconfig"/ || $0 !~ /--context /) { bad=1 }
	END { exit bad ? 0 : 1 }
' "$foundation"; then
	fail 'foundation contains kubectl without the run-owned kubeconfig/context'
fi

check_helm_commands() {
	awk '
		function check(command) {
			if (command ~ /helm (install|upgrade|uninstall|template)/ &&
			    (command !~ /--kubeconfig "\$(kubeconfig|KUBECONFIG)"/ || command !~ /--kube-context "\$(context|kube_context|KUBE_CONTEXT)"/)) exit 2
			if (command ~ /scripts\/helm-release\.sh/ &&
			    (command !~ /KUBECONFIG="\$kubeconfig"/ || command !~ /KUBE_CONTEXT="\$context"/)) exit 3
		}
		{
			current = current " " $0
			if ($0 ~ /\\[[:space:]]*$/) next
			check(current)
			current = ""
		}
		END { if (current != "") check(current) }
	' "$1" || fail "unguarded Helm mutation/template command in $1"
}

check_helm_commands "$foundation"
check_helm_commands "$release"
check_helm_commands "$benchmark"

# shellcheck disable=SC2016 # These are literal source contracts.
for contract in \
	'case "$server" in https://127.0.0.1:' \
	'ksg-test-owner=$run_id' \
	'sleep 900' \
	'pod-security.kubernetes.io/warn=restricted' \
	'V3_COMPAT_IMAGE differs from the locked amd64 v3.4.1 image' \
	'candidate local tag does not match exact candidate image ID' \
	'v3 local tag does not match exact v3 image ID' \
	'kind load docker-image "$v3_local_image"' \
	'kind load docker-image "$candidate_local_image"' \
	'git -C "$repo_root" archive b01e37dce377e5e4296392b7e4d823b6830b763e deploy' \
	'k apply -f "$repo_root/test/fixtures/v3.4.1/crds"' \
	'--skip-crds --set rbac.clusterRole=false' \
	'--set-string image.digest=' \
	'--set image.pullPolicy=IfNotPresent' \
	'v3_install_diagnostics' \
	'secret-generator.v1.mittwald.de/secure: "yes"' \
	'annotate secret smoke-string secret-generator.v1.mittwald.de/secure=yes' \
	'"$repo_root/scripts/preflight-v4.sh"' \
	'REPORT_FORMAT=markdown REPORT_FILE="$preflight_report"' \
	'read-only v4 preflight failed; sanitized report follows' \
	'crd.v3.4.1.basicauths.spec-sha256' \
	'does not match the pinned v3.4.1 CRD spec before ownership takeover' \
	'.manager == "kubectl-client-side-apply" and .operation == "Update"' \
	'--show-managed-fields=true' \
	'.manager == "kube-apiserver" and .operation == "Update"' \
	'.subresource == "status"' \
	'(.fieldsV1 | keys) == ["f:status"]' \
	'fieldPaths:' \
	'.metadata.resourceVersion' \
	'immediate legacy-adoption preflight reported blockers or an unstable snapshot' \
	'replace --dry-run=server --field-manager=kubernetes-secret-generator-crd-manager' \
	'replace --field-manager=kubernetes-secret-generator-crd-manager' \
	'old controller Pod did not disappear before offline replacement' \
	'v4 upgrade requires the previous controller Pod to be absent' \
	'v3 rollback requires the v4 controller Pod to be absent' \
	'more than one active controller Pod was observed' \
	'.spec.strategy.type' \
	'.metadata.ownerReferences[0].blockOwnerDeletion == true' \
	'BasicAuth self-heal did not rotate credentials' \
	'basic_hash=$healed_hash' \
	'BasicAuth self-heal caused an update storm'; do
	grep -F -q -- "$contract" "$release" || fail "release smoke safety assertion is missing: $contract"
done
for contract in \
	'OLD_UID="$old_v4_uid" READY_FILE="$recreate_ready" SUMMARY_FILE="$recreate_summary"' \
	'"$repo_root/test/e2e/recreate-observer.sh" &' \
	'--set terminationGracePeriodSeconds=31 --wait --timeout 180s' \
	'wait --for=condition=Ready --timeout=60s "pod/$new_v4_pod"' \
	'rotation state did not settle before v4-to-v4 Recreate upgrade' \
	'v4-to-v4 Recreate upgrade rotated a credential' \
	'v4-to-v4 Recreate upgrade changed rotation state' \
	'--slurpfile recreate "$recreate_summary"'; do
	grep -F -q -- "$contract" "$release" || fail "v4 Recreate observation contract is missing: $contract"
done
[ -x "$recreate_observer" ] || fail 'Recreate observer is not executable'
[ -x "$recreate_observer_test" ] || fail 'Recreate observer negative-fixture test is not executable'
"$recreate_observer_test"

observer_line=$(grep -n -F '"$repo_root/test/e2e/recreate-observer.sh" &' "$release" | cut -d: -f1)
ready_line=$(grep -n -F '[ -e "$recreate_ready" ] || fail' "$release" | cut -d: -f1)
upgrade_line=$(grep -n -F -- '--set terminationGracePeriodSeconds=31 --wait --timeout 180s' "$release" | cut -d: -f1)
stop_line=$(grep -n -F ': >"$recreate_stop"' "$release" | tail -n 1 | cut -d: -f1)
wait_line=$(grep -n -F 'wait "$recreate_observer_pid"; then' "$release" | cut -d: -f1)
summary_line=$(grep -n -F 'Recreate observation summary is incomplete' "$release" | cut -d: -f1)
[ "$observer_line" -lt "$ready_line" ] && [ "$ready_line" -lt "$upgrade_line" ] && \
	[ "$upgrade_line" -lt "$stop_line" ] && [ "$stop_line" -lt "$wait_line" ] && [ "$wait_line" -lt "$summary_line" ] ||
	fail 'Recreate observer is not ordered around the live Helm upgrade'
! grep -F -q -- '--set installCRDs=true' "$release" || fail 'v3 runtime chart must not replace the original e159 CRD fixtures'
grep -F -q 'jq -cS -f "$repo_root/test/e2e/release-smoke-inventory.jq"' "$release" || fail 'release smoke does not use the managed fixture inventory filter'
grep -F -q "manager rollback changed managed fixture identity or ownership" "$release" || fail 'release smoke fixture rollback assertion is missing'
! grep -F -q 'manager rollback changed object counts' "$release" || fail 'release smoke still counts unrelated namespace objects'

legacy_ssh_fixture=$(awk '
	$0 == "kind: SSHKeyPair" { found=1 }
	found && $0 == "---" { exit }
	found { print }
' "$release")
printf '%s\n' "$legacy_ssh_fixture" | grep -F -q 'spec: {length: "2048"}' || fail 'legacy SSH fixture does not exercise the original v3 length field'
for v4_field in algorithm privateKeyField publicKeyField rotationInterval; do
	if printf '%s\n' "$legacy_ssh_fixture" | grep -E "(^|[,{[:space:]])${v4_field}:" >/dev/null; then
		fail "legacy SSH fixture uses v4-only field $v4_field"
	fi
done

inventory() { jq -cS -f "$inventory_filter"; }
baseline=$(inventory <<'EOF'
{"items":[
  {"kind":"StringSecret","metadata":{"name":"smoke-string","uid":"cr-string"}},
  {"kind":"BasicAuth","metadata":{"name":"smoke-basic","uid":"cr-basic"}},
  {"kind":"SSHKeyPair","metadata":{"name":"smoke-ssh","uid":"cr-ssh"}},
  {"kind":"Secret","metadata":{"name":"smoke-string","uid":"secret-string","ownerReferences":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","name":"smoke-string","uid":"cr-string","controller":true,"blockOwnerDeletion":true}]}},
  {"kind":"Secret","metadata":{"name":"smoke-basic","uid":"secret-basic","ownerReferences":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"BasicAuth","name":"smoke-basic","uid":"cr-basic","controller":true,"blockOwnerDeletion":true}]}},
  {"kind":"Secret","metadata":{"name":"smoke-ssh","uid":"secret-ssh","ownerReferences":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"SSHKeyPair","name":"smoke-ssh","uid":"cr-ssh","controller":true,"blockOwnerDeletion":true}]}},
  {"kind":"Secret","metadata":{"name":"smoke-annotation","uid":"secret-annotation"}}
]}
EOF
)
with_unrelated=$(printf '%s\n' "$baseline" | jq '{items: (map({kind,metadata:{name,uid,ownerReferences:.owners}}) + [
  {kind:"Secret",metadata:{name:"sh.helm.release.v1.ksg-release.v3",uid:"helm-history"}},
  {kind:"Secret",metadata:{name:"unrelated",uid:"unrelated"}},
  {kind:"Secret",metadata:{name:"smoke-unrelated",uid:"smoke-unrelated"}},
  {kind:"ConfigMap",metadata:{name:"smoke-config",uid:"unrelated-config"}}
])}' | inventory)
[ "$with_unrelated" = "$baseline" ] || fail 'fixture inventory includes Helm history or unrelated objects'
missing=$(printf '%s\n' "$baseline" | jq 'del(.[] | select(.kind == "Secret" and .name == "smoke-basic"))')
[ "$(printf '%s\n' "$missing" | jq '{items: map({kind,metadata:{name,uid,ownerReferences:.owners}})}' | inventory)" != "$baseline" ] || fail 'fixture inventory ignored managed fixture loss'
added=$(printf '%s\n' "$baseline" | jq '. + [{kind:"Secret",name:"owned-extra",uid:"secret-extra",owners:[{apiVersion:"secretgenerator.mittwald.de/v1alpha1",kind:"StringSecret",name:"smoke-string",uid:"cr-string",controller:true,blockOwnerDeletion:true}]}]')
[ "$(printf '%s\n' "$added" | jq '{items: map({kind,metadata:{name,uid,ownerReferences:.owners}})}' | inventory)" != "$baseline" ] || fail 'fixture inventory ignored managed fixture addition'
added_noncontroller=$(printf '%s\n' "$baseline" | jq '. + [{kind:"Secret",name:"owned-noncontroller",uid:"secret-noncontroller",owners:[{apiVersion:"secretgenerator.mittwald.de/v1alpha1",kind:"StringSecret",name:"smoke-string",uid:"cr-string",controller:false,blockOwnerDeletion:true}]}]')
[ "$(printf '%s\n' "$added_noncontroller" | jq '{items: map({kind,metadata:{name,uid,ownerReferences:.owners}})}' | inventory)" != "$baseline" ] || fail 'fixture inventory ignored non-controller fixture owner reference'
added_implicit_owner=$(printf '%s\n' "$baseline" | jq '. + [{kind:"Secret",name:"owned-implicit",uid:"secret-implicit",owners:[{apiVersion:"secretgenerator.mittwald.de/v1alpha1",kind:"StringSecret",name:"smoke-string",uid:"cr-string",blockOwnerDeletion:true}]}]')
[ "$(printf '%s\n' "$added_implicit_owner" | jq '{items: map({kind,metadata:{name,uid,ownerReferences:.owners}})}' | inventory)" != "$baseline" ] || fail 'fixture inventory ignored implicit non-controller fixture owner reference'
owner_changed=$(printf '%s\n' "$baseline" | jq '(.[] | select(.kind == "Secret" and .name == "smoke-basic") | .owners[0].blockOwnerDeletion) = false')
[ "$(printf '%s\n' "$owner_changed" | jq '{items: map({kind,metadata:{name,uid,ownerReferences:.owners}})}' | inventory)" != "$baseline" ] || fail 'fixture inventory ignored blockOwnerDeletion change'
preflight_line=$(grep -n -F 'adoption_preflight=$workdir/adoption-preflight.json' "$release" | cut -d: -f1)
identity_line=$(grep -n -F 'actual_spec_sha=$(jq' "$release" | cut -d: -f1)
replace_line=$(grep -n -F 'replace --field-manager=kubernetes-secret-generator-crd-manager -f' "$release" | cut -d: -f1)
[ "$identity_line" -lt "$preflight_line" ] && [ "$preflight_line" -lt "$replace_line" ] || fail 'legacy CRD replacement is not gated by exact identity and immediate preflight'
! grep -F -q -- '--force-conflicts' "$release" || fail 'release smoke must not force CRD field conflicts'
if grep -E 'kind load docker-image.*\$(CANDIDATE_IMAGE|V3_COMPAT_IMAGE)' "$release" >/dev/null; then
	fail 'release smoke must not kind-load digest references directly'
fi
if grep -F -q 'pod-security.kubernetes.io/enforce=restricted' "$release"; then
	fail 'release smoke must not enforce restricted Pod Security before the legacy v3 upgrade'
fi

# shellcheck disable=SC2016 # These are literal source contracts.
for contract in \
	'[ "$KUBE_CONTEXT" = "$CONFIRM_CONTEXT" ]' \
	'[ "$(k get namespace kube-system -o jsonpath=' \
	'[ "$secret_count" -eq 100 ] && [ "$ready_count" -eq 98 ]' \
	'controller restarted during benchmark'; do
	grep -F -q "$contract" "$benchmark" || fail "benchmark safety assertion is missing: $contract"
done

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ksg-kind-guards.XXXXXX")
trap 'rm -rf "$tmp"' 0 1 2 15

command -v helm >/dev/null 2>&1 || fail 'locked Helm is required to verify the v3 runtime Role'
locked_helm=$(awk -F= '$1=="helm.version" {print $2}' "$repo_root/tools.lock")
[ "$(helm version --template '{{.Version}}')" = "$locked_helm" ] || fail "Helm $locked_helm is required to verify the v3 runtime Role"
v3_runtime_tree=$tmp/v3-runtime
mkdir "$v3_runtime_tree"
git -C "$repo_root" archive b01e37dce377e5e4296392b7e4d823b6830b763e deploy | tar -x -C "$v3_runtime_tree"
for fixture in "$repo_root"/test/fixtures/v3.4.1/crds/*.yaml; do
	name=$(basename "$fixture")
	git -C "$repo_root" show "e15976ccd356c260be6e691b4d26d55005800b91:deploy/crds/$name" | cmp -s - "$fixture" ||
		fail "$name is not the original e159 v3.4.1 CRD fixture"
done
helm template ksg-v3-runtime "$v3_runtime_tree/deploy/helm-chart/kubernetes-secret-generator" \
	--namespace ksg-v3-runtime --set rbac.clusterRole=false --set-string watchNamespace=ksg-v3-runtime \
	--show-only templates/role.yaml >"$tmp/v3-role.yaml"
awk '
	function finish_rule() {
		if (api && resource && create && get && update && patch) found=1
		api=resource=create=get=update=patch=0
	}
	/^  - apiGroups:$/ { finish_rule(); section="api"; next }
	/^    resources:$/ { section="resource"; next }
	/^    verbs:$/ { section="verb"; next }
	/^      - / {
		value=$0
		sub(/^      - /, "", value)
		if (section == "api" && value == "coordination.k8s.io") api=1
		if (section == "resource" && value == "leases") resource=1
		if (section == "verb" && value == "create") create=1
		if (section == "verb" && value == "get") get=1
		if (section == "verb" && value == "update") update=1
		if (section == "verb" && value == "patch") patch=1
	}
	END { finish_rule(); exit found ? 0 : 1 }
' "$tmp/v3-role.yaml" || fail 'rendered b01 v3 Role lacks required coordination.k8s.io Lease permissions'

: >"$tmp/chart.tgz"
if CHART_TGZ="$tmp/chart.tgz" CANDIDATE_IMAGE=mutable RELEASE_TAG=v4.0.0 "$release" >"$tmp/release.out" 2>&1; then
	fail 'release smoke accepted a mutable candidate image'
fi
grep -F -q 'CANDIDATE_IMAGE must be an exact digest reference' "$tmp/release.out" || fail 'release smoke did not fail before tool or cluster mutation'
if KUBECONFIG="$tmp/chart.tgz" KUBE_CONTEXT=kind-owned CONFIRM_CONTEXT=kind-other RUN_OWNER_ID=test \
	CHART_TGZ="$tmp/chart.tgz" CANDIDATE_IMAGE=mutable "$benchmark" >"$tmp/benchmark.out" 2>&1; then
	fail 'benchmark accepted a mismatched context confirmation'
fi
grep -F -q 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT' "$tmp/benchmark.out" || fail 'benchmark did not fail before tool or cluster mutation'

printf 'kind target guard verification passed\n'
