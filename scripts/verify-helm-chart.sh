#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
chart=$repo_root/deploy/helm-chart/kubernetes-secret-generator
flux_example=$repo_root/docs/examples/flux-helmrelease.yaml
digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-helm.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15

fail() {
	printf 'error: %s\n' "$*" >&2
	exit 2
}

command -v helm >/dev/null 2>&1 || fail 'helm is required'
command -v openssl >/dev/null 2>&1 || fail 'openssl is required'
helm_version=$(helm version --template '{{.Version}}' | sed 's/^v//; s/+.*//; s/-.*//')
locked_helm_version=$(awk -F= '$1 == "helm.version" { print substr($2, 2); found=1 } END { if (!found) exit 1 }' "$repo_root/tools.lock")
[ "$helm_version" = "$locked_helm_version" ] || fail "Helm $locked_helm_version from tools.lock is required (found $helm_version)"
chart_version=$(awk '$1 == "version:" { print $2; exit }' "$chart/Chart.yaml")
schema_version=${chart_version%%-*}
for crd in "$chart"/crds/*_crd.yaml; do
	grep -F -q "secretgenerator.mittwald.de/schema-release: v$schema_version" "$crd" || fail "$(basename "$crd") schema-release annotation differs from the Chart base version"
done
grep -F -q 'chartVersion: {{ .Chart.Version | quote }}' "$chart/templates/crd-lifecycle-owner.yaml" || fail 'lifecycle owner does not persist Chart.version'
grep -F -q 'helm.sh/resource-policy: keep' "$chart/templates/crd-lifecycle-owner.yaml" || fail 'lifecycle owner evidence is not retained on uninstall'

render() {
	profile=$1
	scope=$2
	output=$3
	case "$scope" in
		namespaces)
			helm template ksg "$chart" --namespace ksg-system \
				--set "profile=$profile" --set "scope.mode=namespaces" \
				--set 'scope.namespaces={alpha,beta}' --set-string "image.digest=$digest" >"$output"
			;;
		ownNamespace|cluster)
			helm template ksg "$chart" --namespace ksg-system \
				--set "profile=$profile" --set "scope.mode=$scope" \
				--set-string "image.digest=$digest" >"$output"
			;;
	esac
}

helm lint "$chart" --set profile=dev >/dev/null
for profile in production dev; do
	for scope in ownNamespace namespaces cluster; do
		render "$profile" "$scope" "$tmpdir/$profile-$scope.yaml"
	done
done

production=$tmpdir/production-ownNamespace.yaml
development=$tmpdir/dev-ownNamespace.yaml
grep -F -q "image: \"ghcr.io/mrchypark/kubernetes-secret-generator@$digest\"" "$production" || fail 'production image is not digest-pinned'
grep -F -q 'replicas: 2' "$production" || fail 'production profile is not HA by default'
grep -F -q 'minDomains: 2' "$production" || fail 'production topology spread is missing'
grep -F -q 'kind: PodDisruptionBudget' "$production" || fail 'production PDB is missing'
grep -F -q -- '- --leader-elect=true' "$production" || fail 'leader election is not enabled'
grep -F -q -- '- --leader-election-id=kubernetes-secret-generator-lock' "$production" || fail 'default leader election ID is missing'
grep -F -q 'replicas: 1' "$development" || fail 'dev profile is not single-node'
if grep -F -q 'kind: PodDisruptionBudget' "$development"; then fail 'dev profile unexpectedly renders a PDB'; fi
for field in 'runAsNonRoot: true' 'allowPrivilegeEscalation: false' 'readOnlyRootFilesystem: true' 'type: RuntimeDefault' 'drop:' 'cpu: 50m' 'memory: 64Mi'; do
	grep -F -q "$field" "$production" || fail "production pod security/resources missing: $field"
done
grep -A1 -F 'name: WATCH_NAMESPACE' "$production" | grep -F -q 'value: "ksg-system"' || fail 'ownNamespace watch scope is incorrect'
grep -A1 -F 'name: WATCH_NAMESPACE' "$tmpdir/production-namespaces.yaml" | grep -F -q 'value: "alpha,beta"' || fail 'namespaces watch scope is not canonical'
grep -A1 -F 'name: WATCH_NAMESPACE' "$tmpdir/production-cluster.yaml" | grep -F -q 'value: ""' || fail 'cluster watch scope is not cluster-wide'
if grep -F -q 'kind: ClusterRole' "$production"; then fail 'ownNamespace scope rendered cluster-wide RBAC'; fi
grep -F -q 'kind: ClusterRole' "$tmpdir/production-cluster.yaml" || fail 'cluster scope lacks ClusterRole RBAC'
for rendered in "$tmpdir"/*.yaml; do
	if grep -E -q 'image:.*:latest|namespace: default|^[[:space:]]*command:' "$rendered"; then
		fail "unsafe deployment field rendered in $rendered"
	fi
done

for scope in ownNamespace namespaces cluster; do
	rendered=$tmpdir/production-$scope.yaml
	rbac=$tmpdir/rbac-$scope.yaml
	awk '
		/^---$/ { if (is_rbac) printf "%s", document; document=""; is_rbac=0; next }
		{ document=document $0 "\n" }
		$0 == "kind: Role" || $0 == "kind: ClusterRole" { is_rbac=1 }
		END { if (is_rbac) printf "%s", document }
	' "$rendered" >"$rbac"
	for forbidden in 'resources: ["pods"]' 'resources: ["deployments"]' 'resources: ["replicasets"]' 'resources: ["services"]' 'resources: ["servicemonitors"]' 'verbs: ["delete"]'; do
		if grep -F -q "$forbidden" "$rbac"; then fail "$scope RBAC contains forbidden permission: $forbidden"; fi
	done
	grep -F -q 'resources: ["secrets"]' "$rbac" || fail "$scope RBAC lacks Secret access"
	grep -F -q 'verbs: ["get", "list", "watch", "create", "update", "patch"]' "$rbac" || fail "$scope Secret verbs differ from the contract"
	grep -F -q 'resources: ["basicauths", "sshkeypairs", "stringsecrets"]' "$rbac" || fail "$scope RBAC lacks read-only main CR access"
	grep -F -q 'verbs: ["get", "list", "watch"]' "$rbac" || fail "$scope main CR verbs differ from the contract"
	grep -F -q 'resources: ["basicauths/status", "sshkeypairs/status", "stringsecrets/status"]' "$rbac" || fail "$scope RBAC lacks status access"
done
grep -F -q 'resourceNames: ["kubernetes-secret-generator-lock"]' "$tmpdir/rbac-ownNamespace.yaml" || fail 'leader Lease is not resource-name restricted'
grep -F -q 'verbs: ["get", "update"]' "$tmpdir/rbac-ownNamespace.yaml" || fail 'leader Lease verbs are not minimal'

if helm template ksg "$chart" --namespace ksg-system --set profile=production >/dev/null 2>&1; then
	fail 'production profile accepted a mutable image reference'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set scope.mode=namespaces >/dev/null 2>&1; then
	fail 'namespaces mode accepted an empty namespace list'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set scope.mode=namespaces --set 'scope.namespaces={alpha,alpha}' >/dev/null 2>&1; then
	fail 'namespaces mode accepted duplicate namespaces'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set-string image.tag=latest >/dev/null 2>&1; then
	fail 'chart accepted image.tag=latest'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set replicaCount=2 --set pdb.enabled=false >/dev/null 2>&1; then
	fail 'multi-replica dev profile accepted pdb.enabled=false'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set replicaCount=2 --set pdb.minAvailable=2 >/dev/null 2>&1; then
	fail 'multi-replica dev profile accepted pdb.minAvailable other than 1'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=production --set-string "image.digest=$digest" --set pdb.enabled=false >/dev/null 2>&1; then
	fail 'production profile accepted pdb.enabled=false'
fi
if helm template ksg "$chart" --namespace ksg-system --set profile=dev --set installCRDs=true >/dev/null 2>&1; then
	fail 'chart accepted removed installCRDs value'
fi
if helm template ksg "$chart" --namespace ksg-system --is-upgrade --set profile=dev >/dev/null 2>&1; then
	fail 'upgrade accepted missing scope confirmation'
fi
namespace_digest=$(printf 'alpha\nbeta' | openssl dgst -sha256 -r | awk '{print $1}')
helm template ksg "$chart" --namespace ksg-system --is-upgrade --set profile=dev \
	--set scope.mode=namespaces --set 'scope.namespaces={beta,alpha}' \
	--set migration.confirmedScope=namespaces \
	--set-string "migration.confirmedNamespacesSHA256=$namespace_digest" >/dev/null
if helm template ksg "$chart" --namespace ksg-system --is-upgrade --set profile=dev \
	--set scope.mode=namespaces --set 'scope.namespaces={beta,alpha}' \
	--set migration.confirmedScope=namespaces \
	--set-string migration.confirmedNamespacesSHA256=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa >/dev/null 2>&1; then
	fail 'namespaces upgrade accepted an incorrect canonical confirmation digest'
fi

helm template ksg "$chart" --namespace ksg-system --set profile=dev \
	--set metrics.serviceMonitor.enabled=true >"$tmpdir/monitor.yaml"
grep -F -q 'kind: ServiceMonitor' "$tmpdir/monitor.yaml" || fail 'ServiceMonitor opt-in did not render'

package_dir=$tmpdir/package
mkdir "$package_dir"
helm package "$chart" --destination "$package_dir" >/dev/null
package=$(find "$package_dir" -type f -name '*.tgz' -print | head -n 1)
[ -n "$package" ] || fail 'Helm chart package was not created'
helm show crds "$package" >"$tmpdir/package-crds.yaml"
LC_ALL=C
export LC_ALL
for crd in "$chart"/crds/*_crd.yaml; do
	[ -f "$crd" ] || fail 'canonical generated CRDs are missing'
	cat "$crd"
	printf '\n'
done >"$tmpdir/canonical-crds.yaml"
cmp -s "$tmpdir/canonical-crds.yaml" "$tmpdir/package-crds.yaml" || fail 'packaged CRDs differ from canonical generated CRDs'

[ "$(grep -F -c 'crds: CreateReplace' "$flux_example")" -eq 2 ] || fail 'Flux install and upgrade must both use CreateReplace'
grep -F -q 'apiVersion: helm.toolkit.fluxcd.io/v2' "$flux_example" || fail 'Flux example is not HelmRelease v2'
grep -F -q 'manager: flux' "$flux_example" || fail 'Flux example does not select the Flux CRD manager'
grep -F -q 'commit: "0000000000000000000000000000000000000000"' "$flux_example" || fail 'Flux example must expose an immutable commit prerequisite'
grep -F -q 'reconcileStrategy: Revision' "$flux_example" || fail 'GitRepository chart changes are not reconciled by source revision'
if grep -Eq '^[[:space:]]+(tag|branch):|^[[:space:]]+version:' "$flux_example"; then fail 'Flux Git source contains a mutable/ignored selector'; fi

printf 'Helm chart verification passed\n'
