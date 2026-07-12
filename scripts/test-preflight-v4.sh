#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-preflight-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15
log=$tmp_dir/kubectl.log
mkdir "$tmp_dir/bin"

cat >"$tmp_dir/bin/kubectl" <<'EOF'
#!/bin/sh
printf '%s|%s\n' "${RUN_ID:-0}" "$*" >>"$KUBECTL_LOG"
case "${READ_FAIL:-}" in
	context) case "$*" in *'config view'*) exit 74 ;; esac ;;
	snapshot) case "$*" in *'stringsecrets.secretgenerator.mittwald.de,basicauths.secretgenerator.mittwald.de'*) exit 75 ;; esac ;;
	basicauths) case "$*" in *'get basicauths.secretgenerator.mittwald.de -A'*) exit 76 ;; esac ;;
esac
secret_json() {
	phase=${1:-report}
	generated=YWJjZGVmZ2g=
	literal=ZXhwZWN0ZWQ=
	extra=
	case "${BASELINE_MODE:-valid}" in
		valid) ;;
		invalid-string) generated=c2hvcnQ= ;;
		overwrite) literal=ZGlmZmVyZW50 ;;
		type-mismatch) ;;
		stale) extra=',"stale":"c3RhbGU="' ;;
		*) exit 72 ;;
	esac
	secure='"secret-generator.v1.mittwald.de/secure":"yes"'
	[ "${SECURE_MARKER:-present}" != missing ] || secure=
	regenerate=
	if [ "${ANNOTATION_REGENERATE:-false}" = true ]; then
		[ -z "$secure" ] || regenerate=,
		regenerate="${regenerate}\"secret-generator.v1.mittwald.de/regenerate\":\"false\""
	fi
	rv=1
	[ "${SNAPSHOT_MODE:-stable}" != changed ] || [ "$phase" != report ] || rv=2
	case "${OWNER_MODE:-exact}" in
		exact) refs='[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","name":"login","uid":"uid-1","controller":true}]' ;;
		wrong-uid) refs='[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","name":"login","uid":"old-uid","controller":true}]' ;;
		multiple) refs='[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","name":"login","uid":"uid-1","controller":true},{"apiVersion":"example.test/v1","kind":"Observer","name":"other","uid":"other"}]' ;;
		*) exit 71 ;;
	esac
	secret_type=Opaque
	[ "${BASELINE_MODE:-valid}" != type-mismatch ] || secret_type=example.test/mismatch
	printf '{"apiVersion":"v1","kind":"Secret","metadata":{"namespace":"app","name":"login","uid":"secret-uid","resourceVersion":"%s","labels":{"team":"platform"},"annotations":{%s%s},"ownerReferences":%s},"immutable":%s,"type":"%s","data":{"password":"%s","literal":"%s"%s}}' "$rv" "$secure" "$regenerate" "$refs" "${IMMUTABLE_SECRET:-false}" "$secret_type" "$generated" "$literal" "$extra"
}
case "$*" in
  *'config view'*'.server'*) printf '%s' 'https://test.example.invalid' ;;
  *'config view'*'certificate-authority-data'*) printf '%s' 'dGVzdC1jYQ==' ;;
  *'get deployment'*)
	deployment_rv=1
	deployment_calls=$(grep -c "^${RUN_ID:-0}|.*get deployment" "$KUBECTL_LOG")
	[ "${DEPLOYMENT_SNAPSHOT_MODE:-stable}" != changed ] || [ "$deployment_calls" -le 1 ] || deployment_rv=2
	printf '{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"ksg","namespace":"ksg-system","uid":"deployment-uid","resourceVersion":"%s"},"spec":{"template":{"spec":{"containers":[{"name":"kubernetes-secret-generator","env":[{"name":"WATCH_NAMESPACE","value":"%s"}]}]}}}}\n' "$deployment_rv" "${WATCH_NAMESPACE_VALUE:-ksg-system}"
    ;;
  *'get customresourcedefinitions'*) cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"basicauths.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}},
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"sshkeypairs.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{"algorithm":{},"privateKey":{},"privateKeyField":{},"publicKeyField":{}}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}},
{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"stringsecrets.secretgenerator.mittwald.de"},"spec":{"versions":[{"name":"v1alpha1","served":true,"storage":true,"subresources":{"status":{}},"schema":{"openAPIV3Schema":{"properties":{"spec":{"properties":{}},"status":{"properties":{"conditions":{},"observedGeneration":{}}}}}}}]}}
]}
JSON
    ;;
  *'get stringsecrets.secretgenerator.mittwald.de,basicauths.secretgenerator.mittwald.de,sshkeypairs.secretgenerator.mittwald.de,secrets'*)
	length=8
	[ "${SPEC_MODE:-valid}" != invalid ] || length=0
	printf '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","metadata":{"namespace":"app","name":"login","uid":"uid-1","labels":{"team":"platform"}},"spec":{"forceRegenerate":%s,"data":{"literal":"expected"},"fields":[{"fieldName":"password","encoding":"raw","length":"%s"}]}} ,' "${BLOCKED:-false}" "$length"
	secret_json baseline
	printf ']}\n'
	;;
  *'get stringsecrets'*)
	length=8
	[ "${SPEC_MODE:-valid}" != invalid ] || length=0
	printf '{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","metadata":{"namespace":"app","name":"login","uid":"uid-1","labels":{"team":"platform"}},"spec":{"forceRegenerate":%s,"data":{"literal":"expected"},"fields":[{"fieldName":"password","encoding":"raw","length":"%s"}]}}]}\n' "${BLOCKED:-false}" "$length"
	;;
  *'get basicauths'*) printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}' ;;
  *'get sshkeypairs'*) printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}' ;;
	*'get secrets'*)
		printf '{"apiVersion":"v1","kind":"List","items":['
		secret_json report
		printf ']}\n'
    ;;
  *) echo "unexpected kubectl invocation: $*" >&2; exit 70 ;;
esac
EOF
chmod +x "$tmp_dir/bin/kubectl"

ca_sha=$(printf '%s' 'test-ca' | openssl dgst -sha256 -r | awk '{print $1}')
run_counter=0
run() {
	run_counter=$((run_counter + 1))
	env PATH="$tmp_dir/bin:$PATH" KUBECTL_LOG="$log" BLOCKED="${BLOCKED:-false}" OWNER_MODE="${OWNER_MODE:-exact}" \
		RUN_ID="$run_counter" \
		BASELINE_MODE="${BASELINE_MODE:-valid}" SPEC_MODE="${SPEC_MODE:-valid}" ANNOTATION_REGENERATE="${ANNOTATION_REGENERATE:-false}" \
		SECURE_MARKER="${SECURE_MARKER:-present}" \
		IMMUTABLE_SECRET="${IMMUTABLE_SECRET:-false}" \
		READ_FAIL="${READ_FAIL:-}" SNAPSHOT_MODE="${SNAPSHOT_MODE:-stable}" WATCH_NAMESPACE_VALUE="${WATCH_NAMESPACE_VALUE:-ksg-system}" \
		DEPLOYMENT_SNAPSHOT_MODE="${DEPLOYMENT_SNAPSHOT_MODE:-stable}" \
		KUBE_CONTEXT=test CONFIRM_CONTEXT=test EXPECTED_SERVER_URL=https://test.example.invalid \
		EXPECTED_CA_SHA256="$ca_sha" NAMESPACE=ksg-system RELEASE_NAME=ksg REPORT_FORMAT="${REPORT_FORMAT:-json}" \
		SCOPE_MODE="${SCOPE_MODE-ownNamespace}" CONFIRMED_SCOPE="${CONFIRMED_SCOPE-ownNamespace}" \
		SCOPE_NAMESPACES="${SCOPE_NAMESPACES:-}" CONFIRMED_NAMESPACES_SHA256="${CONFIRMED_NAMESPACES_SHA256:-}" \
		"$repo_root/scripts/preflight-v4.sh"
}

run >"$tmp_dir/report.json"
[ "$(jq -r '.blockerCount' "$tmp_dir/report.json")" -eq 0 ]
jq -e 'has("migrationApproval") | not' "$tmp_dir/report.json" >/dev/null
if grep -Eq ' apply | create | delete | patch | replace ' "$log"; then
	echo 'preflight invoked a mutating kubectl command' >&2
	exit 1
fi
if grep -Eq 'YWJjZGVmZ2g=|ZXhwZWN0ZWQ=' "$tmp_dir/report.json"; then
	echo 'preflight report exposed Secret data' >&2
	exit 1
fi
if BLOCKED=true run >"$tmp_dir/blocked.json" 2>/dev/null; then
	echo 'preflight accepted a pending force regeneration' >&2
	exit 1
fi
jq -e '.findings | any(.code == "ForceRegeneratePending")' "$tmp_dir/blocked.json" >/dev/null

for owner_mode in wrong-uid multiple; do
	if OWNER_MODE=$owner_mode run >"$tmp_dir/$owner_mode.json" 2>/dev/null; then
		echo "preflight accepted owner mode $owner_mode" >&2
		exit 1
	fi
	jq -e '.findings | any(.code == "OwnerMismatch")' "$tmp_dir/$owner_mode.json" >/dev/null
	if grep -Eq 'YWJjZGVmZ2g=|ZXhwZWN0ZWQ=' "$tmp_dir/$owner_mode.json"; then
		echo "preflight owner mismatch report exposed Secret data" >&2
		exit 1
	fi
done

BLOCKED=false
OWNER_MODE=exact

for mode in invalid-string overwrite; do
	if BASELINE_MODE=$mode run >"$tmp_dir/$mode.json" 2>/dev/null; then
		echo "preflight accepted baseline mode $mode" >&2
		exit 1
	fi
done
jq -e '.findings | any(.code == "LegacyBaselineInvalid")' "$tmp_dir/invalid-string.json" >/dev/null || {
	jq '.findings' "$tmp_dir/invalid-string.json" >&2
	exit 1
}
jq -e '.findings | any(.code == "DeclarativeOverwritePending")' "$tmp_dir/overwrite.json" >/dev/null || {
	jq '.findings' "$tmp_dir/overwrite.json" >&2
	exit 1
}

for immutable in false true; do
	if BASELINE_MODE=type-mismatch IMMUTABLE_SECRET=$immutable run >"$tmp_dir/type-mismatch-$immutable.json" 2>/dev/null; then
		echo "preflight accepted a type mismatch with immutable=$immutable" >&2
		exit 1
	fi
	jq -e '.findings | any(.code == "SecretTypeMismatch")' "$tmp_dir/type-mismatch-$immutable.json" >/dev/null
done
IMMUTABLE_SECRET=false
BASELINE_MODE=valid

if SPEC_MODE=invalid BASELINE_MODE=valid run >"$tmp_dir/invalid-spec.json" 2>/dev/null; then
	echo 'preflight accepted an invalid String field length' >&2
	exit 1
fi
jq -e '.findings | any(.code == "InvalidStringField")' "$tmp_dir/invalid-spec.json" >/dev/null
SPEC_MODE=valid

BASELINE_MODE=stale run >"$tmp_dir/stale.json"
jq -e '.findings | any(.code == "StaleDataCandidates")' "$tmp_dir/stale.json" >/dev/null

if ANNOTATION_REGENERATE=true run >"$tmp_dir/regenerate.json" 2>/dev/null; then
	echo 'preflight accepted a regenerate annotation whose value was false' >&2
	exit 1
fi
jq -e '.findings | any(.code == "RegenerateAnnotationPresent")' "$tmp_dir/regenerate.json" >/dev/null

if SECURE_MARKER=missing run >"$tmp_dir/secure-marker.json" 2>/dev/null; then
	echo 'preflight accepted a StringSecret whose reviewed secure marker was missing' >&2
	exit 1
fi
jq -e '.findings | any(.code == "SecureMarkerMissing")' "$tmp_dir/secure-marker.json" >/dev/null
if grep -Eq 'YWJjZGVmZ2g=|ZXhwZWN0ZWQ=' "$tmp_dir/secure-marker.json"; then
	echo 'secure-marker preflight report exposed Secret data' >&2
	exit 1
fi

if SCOPE_MODE='' CONFIRMED_SCOPE='' run >"$tmp_dir/scope.json" 2>/dev/null; then
	echo 'preflight accepted missing scope confirmation' >&2
	exit 1
fi
jq -e '.findings | any(.code == "ScopeConfirmationMissing")' "$tmp_dir/scope.json" >/dev/null

ANNOTATION_REGENERATE=false
SECURE_MARKER=present
IMMUTABLE_SECRET=false
BASELINE_MODE=valid
READ_FAIL=
SNAPSHOT_MODE=stable
for read_fail in context snapshot basicauths; do
	if READ_FAIL=$read_fail run >"$tmp_dir/read-$read_fail.json" 2>/dev/null; then
		echo "preflight accepted failed kubectl producer $read_fail" >&2
		exit 1
	fi
done
READ_FAIL=

if SNAPSHOT_MODE=changed run >"$tmp_dir/snapshot-changed.json" 2>/dev/null; then
	echo 'preflight accepted a changing CR/Secret snapshot' >&2
	exit 1
fi
jq -e '.findings | any(.code == "SnapshotChanged")' "$tmp_dir/snapshot-changed.json" >/dev/null
SNAPSHOT_MODE=stable

if DEPLOYMENT_SNAPSHOT_MODE=changed run >"$tmp_dir/deployment-changed.json" 2>/dev/null; then
	echo 'preflight accepted a changing Deployment snapshot' >&2
	exit 1
fi
jq -e '.findings | any(.code == "SnapshotChanged")' "$tmp_dir/deployment-changed.json" >/dev/null
DEPLOYMENT_SNAPSHOT_MODE=stable

namespace_sha=$(printf 'a\nb' | openssl dgst -sha256 -r | awk '{print $1}')
WATCH_NAMESPACE_VALUE=b,a SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces SCOPE_NAMESPACES=b,a CONFIRMED_NAMESPACES_SHA256="$namespace_sha" run >"$tmp_dir/namespaces.json"
[ "$(jq -r '.blockerCount' "$tmp_dir/namespaces.json")" -eq 0 ]
if WATCH_NAMESPACE_VALUE=b,a SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces SCOPE_NAMESPACES=b,a,a CONFIRMED_NAMESPACES_SHA256="$namespace_sha" run >"$tmp_dir/namespace-duplicate.json" 2>/dev/null; then
	echo 'preflight accepted duplicate namespaces' >&2
	exit 1
fi
jq -e '.findings | any(.code == "NamespaceScopeInvalid")' "$tmp_dir/namespace-duplicate.json" >/dev/null
if WATCH_NAMESPACE_VALUE=b,a SCOPE_MODE=namespaces CONFIRMED_SCOPE=namespaces SCOPE_NAMESPACES=b,a CONFIRMED_NAMESPACES_SHA256=ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff run >"$tmp_dir/namespace-mismatch.json" 2>/dev/null; then
	echo 'preflight accepted mismatched namespace digest' >&2
	exit 1
fi
jq -e '.findings | any(.code == "NamespaceScopeDigestMismatch")' "$tmp_dir/namespace-mismatch.json" >/dev/null
if grep -Eq ' apply | create | delete | patch | replace ' "$log"; then
	echo 'owner mismatch preflight invoked a mutating kubectl command' >&2
	exit 1
fi

WATCH_NAMESPACE_VALUE=ksg-system SCOPE_MODE=ownNamespace CONFIRMED_SCOPE=ownNamespace \
	SCOPE_NAMESPACES='' CONFIRMED_NAMESPACES_SHA256='' REPORT_FORMAT=markdown run >"$tmp_dir/report.md"
grep -F -q '# Kubernetes Secret Generator v4 preflight' "$tmp_dir/report.md"
grep -F -q 'Blockers: **0**' "$tmp_dir/report.md"
if grep -Eq 'YWJjZGVmZ2g=|ZXhwZWN0ZWQ=' "$tmp_dir/report.md"; then
	echo 'markdown preflight exposed Secret data' >&2
	exit 1
fi

echo 'v4 preflight checks passed'
