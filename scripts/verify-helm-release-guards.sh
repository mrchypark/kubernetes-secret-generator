#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmpdir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-helm-guards.XXXXXX")
trap 'rm -rf "$tmpdir"' 0 1 2 15
mkdir "$tmpdir/bin"
log=$tmpdir/calls.log
digest=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa

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
			if [ "$1" = --destination ]; then shift; touch "$1/ksg-4.0.0-rc.3.tgz"; break; fi
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
	*apply\ --server-side*) [ "${MOCK_SSA_CONFLICT:-false}" != true ] ;;
	*wait\ --for=condition=Established*) ;;
	*get\ customresourcedefinition/basicauths.*) printf string ;;
	*get\ customresourcedefinition/sshkeypairs.*) printf stringstringstring ;;
	*get\ customresourcedefinition/stringsecrets.*) printf array ;;
	*) exit 71 ;;
esac
EOF
chmod +x "$tmpdir/bin/helm" "$tmpdir/bin/kubectl"

common_env() {
	env PATH="$tmpdir/bin:$PATH" CALL_LOG="$log" \
		MOCK_CHART_DIR="$repo_root/deploy/helm-chart/kubernetes-secret-generator" \
		KUBE_CONTEXT=explicit-target CONFIRM_CONTEXT=explicit-target \
		NAMESPACE=ksg-system RELEASE_NAME=ksg CHART_VERSION=4.0.0-rc.3 \
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
grep -F -q 'kubectl --context explicit-target apply --server-side --dry-run=server' "$log" || fail 'CRD bundle did not pass a full server-side dry-run before mutation'
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
if common_env MOCK_RELEASE_EXISTS=true MOCK_OWNER_RECORD='ksg-system\towner\tflux\tksg-system\tksg\t4.0.0-rc.3\townNamespace\t\n' \
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
common_env MOCK_CRD_VERSION=4.0.0 MOCK_OWNER_RECORD='ksg-system\towner\tdirect\tksg-system\tksg\t4.0.0-rc.3\townNamespace\t\n' \
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
