#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
foundation=$repo_root/test/e2e/kind-foundation.sh
release=$repo_root/test/e2e/release-smoke.sh
benchmark=$repo_root/test/e2e/benchmark.sh

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
	'--set image.pullPolicy=IfNotPresent' \
	'v3_install_diagnostics' \
	'"$repo_root/scripts/preflight-v4.sh"' \
	'compatibilityProfile=$profile' \
	'BasicAuth self-heal did not rotate credentials' \
	'basic_hash=$healed_hash' \
	'BasicAuth self-heal caused an update storm'; do
	grep -F -q -- "$contract" "$release" || fail "release smoke safety assertion is missing: $contract"
done
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
