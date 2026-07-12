#!/bin/sh
set -eu

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }
require() { eval "value=\${$1:-}"; [ -n "$value" ] || fail "$1 is required"; }

action=${1:-}
case "$action" in backup|verify|restore) ;; *) fail 'usage: backup-restore.sh backup|verify|restore' ;; esac
require ENCRYPTION_ADAPTER
case "$ENCRYPTION_ADAPTER" in /*) ;; *) fail 'ENCRYPTION_ADAPTER must be an absolute executable path' ;; esac
[ -x "$ENCRYPTION_ADAPTER" ] || fail 'ENCRYPTION_ADAPTER is not executable'
require BACKUP_FILE
require RELEASE_ISSUE
case "$RELEASE_ISSUE" in TODO|TBD|CHANGEME|PENDING) fail 'RELEASE_ISSUE must identify the reviewed GitHub release issue' ;; esac
case "$BACKUP_FILE" in /*) ;; *) fail 'BACKUP_FILE must be an absolute path' ;; esac
command -v jq >/dev/null 2>&1 || fail 'jq is required'

validate_payload() {
	jq -e '
      .schemaVersion == 1 and
      (.crds | type == "array" and length == 3) and
      (.crs | type == "array") and
      (.secrets | type == "array") and
      all(.secrets[]; (.mode == "cr" or .mode == "annotation") and (.object.kind == "Secret") and (.object.metadata.name|length > 0) and (.object.metadata.namespace|length > 0)) and
      ([.secrets[]|select(.mode=="cr")]|length) == (.crs|length)
    ' >/dev/null
}

validate_encrypted() {
	"$ENCRYPTION_ADAPTER" validate <"$BACKUP_FILE" >/dev/null
	"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | validate_payload
}

if [ "$action" = verify ]; then
	[ -f "$BACKUP_FILE" ] || fail 'BACKUP_FILE does not exist'
	validate_encrypted
	"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" |
		jq -c '{schemaVersion,crdCount:(.crds|length),crCount:(.crs|length),secretCount:(.secrets|length),payloadValid:true}'
	exit 0
fi

require KUBE_CONTEXT
require CONFIRM_CONTEXT
require EXPECTED_SERVER_URL
require EXPECTED_CA_SHA256
NAMESPACE=${NAMESPACE:-}
require NAMESPACE
require RELEASE_NAME
require CONTROLLER_STOPPED_CONFIRM
require KUBECONFIG
require SCOPE_MODE
[ "$KUBE_CONTEXT" = "$CONFIRM_CONTEXT" ] || fail 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT'
[ "$CONTROLLER_STOPPED_CONFIRM" = true ] || fail 'CONTROLLER_STOPPED_CONFIRM=true is required'
case "$KUBECONFIG" in /*) ;; *) fail 'KUBECONFIG must be an absolute run-owned path' ;; esac
[ -f "$KUBECONFIG" ] && [ ! -L "$KUBECONFIG" ] || fail 'KUBECONFIG must be a regular non-symlink file'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
command -v openssl >/dev/null 2>&1 || fail 'openssl is required'
k() { kubectl --kubeconfig "$KUBECONFIG" --context "$KUBE_CONTEXT" "$@"; }

server=$(k config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}')
[ "$server" = "$EXPECTED_SERVER_URL" ] || fail 'EXPECTED_SERVER_URL does not match selected context'
ca_data=$(k config view --minify --raw --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
[ -n "$ca_data" ] || fail 'run-owned kubeconfig must contain embedded CA data'
ca_sha=$(printf '%s' "$ca_data" | openssl base64 -d -A | openssl dgst -sha256 -r | awk '{print $1}')
[ "$ca_sha" = "$EXPECTED_CA_SHA256" ] || fail 'EXPECTED_CA_SHA256 does not match selected context'
approved_namespaces=
case "$SCOPE_MODE" in
	ownNamespace) approved_namespaces=$NAMESPACE ;;
	namespaces)
		require SCOPE_NAMESPACES
		case ",$SCOPE_NAMESPACES," in *,,*) fail 'SCOPE_NAMESPACES contains an empty entry' ;; esac
		approved_namespaces=$(printf '%s' "$SCOPE_NAMESPACES" | tr ',' '\n')
		[ "$(printf '%s\n' "$approved_namespaces" | LC_ALL=C sort -u | wc -l | tr -d ' ')" = "$(printf '%s\n' "$approved_namespaces" | wc -l | tr -d ' ')" ] || fail 'SCOPE_NAMESPACES contains duplicates'
		;;
	cluster) ;;
	*) fail 'SCOPE_MODE must be ownNamespace, namespaces, or cluster' ;;
esac

namespace_allowed() {
	[ "${SCOPE_MODE:-ownNamespace}" = cluster ] && return 0
	printf '%s\n' "$approved_namespaces" | grep -F -x -q "$1"
}

resource_json() {
	resource=$1
	case "${SCOPE_MODE:-ownNamespace}" in
		cluster) k get "$resource" -A -o json ;;
		ownNamespace) k -n "$NAMESPACE" get "$resource" -o json ;;
		namespaces)
			for ns in $approved_namespaces; do k -n "$ns" get "$resource" -o json; done | jq -s '{apiVersion:"v1",kind:"List",items:[.[].items[]]}'
			;;
	esac
}

deployment_name=${DEPLOYMENT_NAME:-$RELEASE_NAME}
# NAMESPACE is validated by the dynamic require helper above.
# shellcheck disable=SC2153
replicas=$(k -n "$NAMESPACE" get deployment "$deployment_name" -o jsonpath='{.spec.replicas}')
ready=$(k -n "$NAMESPACE" get deployment "$deployment_name" -o jsonpath='{.status.readyReplicas}')
[ "${replicas:-0}" -eq 0 ] && [ "${ready:-0}" -eq 0 ] || fail 'controller Deployment must be scaled to zero and fully stopped'

if [ "$action" = backup ]; then
	require BACKUP_CONFIRM
	[ "$BACKUP_CONFIRM" = "$KUBE_CONTEXT/$NAMESPACE/$RELEASE_NAME" ] || fail 'BACKUP_CONFIRM must equal KUBE_CONTEXT/NAMESPACE/RELEASE_NAME'
	[ ! -e "$BACKUP_FILE" ] || fail 'BACKUP_FILE already exists'
	tmp_cipher=$BACKUP_FILE.tmp.$$
	umask 077
	trap 'rm -f "$tmp_cipher"' 0 1 2 15
	{
		k get \
			customresourcedefinitions.apiextensions.k8s.io/basicauths.secretgenerator.mittwald.de \
			customresourcedefinitions.apiextensions.k8s.io/sshkeypairs.secretgenerator.mittwald.de \
			customresourcedefinitions.apiextensions.k8s.io/stringsecrets.secretgenerator.mittwald.de -o json
		resource_json stringsecrets.secretgenerator.mittwald.de
		resource_json basicauths.secretgenerator.mittwald.de
		resource_json sshkeypairs.secretgenerator.mittwald.de
		resource_json secrets
	} | jq -s --arg context "$KUBE_CONTEXT" --arg server "$server" --arg ca "$ca_sha" '
      def stableannotations: (. // {}) | del(.["kubectl.kubernetes.io/last-applied-configuration"]);
      def cleanmeta: {name:.name,namespace:(.namespace // ""),labels:(.labels // {}),annotations:(.annotations|stableannotations)};
      def cleancr: {apiVersion,kind,metadata:(.metadata|cleanmeta),spec};
      def cleansecret: {apiVersion:"v1",kind:"Secret",metadata:(.metadata|cleanmeta),type:(.type // "Opaque"),immutable:(.immutable // false),data:(.data // {})};
      .[0].items as $crds | (.[1].items + .[2].items + .[3].items) as $crs | .[4].items as $secrets |
      {
	        schemaVersion:1,createdAt:(now|todateiso8601),source:{context:$context,server:$server,caSHA256:$ca},
        crds:[$crds[]|{apiVersion,kind,metadata:{name:.metadata.name},spec}],
        crs:[$crs[]|cleancr],
        secrets:[
          $secrets[] as $s |
          (first($crs[] | . as $c | select($c.metadata.namespace==$s.metadata.namespace and $c.metadata.name==$s.metadata.name and any($s.metadata.ownerReferences[]?; .apiVersion==$c.apiVersion and .kind==$c.kind and .name==$c.metadata.name and .uid==$c.metadata.uid and .controller==true)) | $c) // null) as $cr |
          if $cr != null then {mode:"cr",owner:{apiVersion:$cr.apiVersion,kind:$cr.kind,namespace:$cr.metadata.namespace,name:$cr.metadata.name},object:($s|cleansecret)}
          elif (((($s.metadata.annotations // {})["secret-generator.v1.mittwald.de/type"] // "") != "") or ((($s.metadata.annotations // {})["secret-generator.v1.mittwald.de/autogenerate"] // "") != "")) and ([ $s.metadata.ownerReferences[]? | select(.controller==true) ]|length)==0 then {mode:"annotation",object:($s|cleansecret)}
          else empty end
        ]
      }
    ' | "$ENCRYPTION_ADAPTER" encrypt >"$tmp_cipher"
	"$ENCRYPTION_ADAPTER" validate <"$tmp_cipher" >/dev/null
	"$ENCRYPTION_ADAPTER" decrypt <"$tmp_cipher" | validate_payload
	mv "$tmp_cipher" "$BACKUP_FILE"
	trap - 0 1 2 15
	"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" |
		jq -c '{schemaVersion,crdCount:(.crds|length),crCount:(.crs|length),secretCount:(.secrets|length),encryptedBackupCreated:true}'
	exit 0
fi

require RESTORE_CONFIRM
require TARGET_CRD_DIR
[ "$RESTORE_CONFIRM" = "$KUBE_CONTEXT/$NAMESPACE/$RELEASE_NAME" ] || fail 'RESTORE_CONFIRM must equal KUBE_CONTEXT/NAMESPACE/RELEASE_NAME'
[ -f "$BACKUP_FILE" ] || fail 'BACKUP_FILE does not exist'
validate_encrypted

# Validate the complete decrypted contract before any target mutation.
"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | jq -e '
  . as $root |
  all(.crs[]; .apiVersion=="secretgenerator.mittwald.de/v1alpha1" and (.kind=="StringSecret" or .kind=="BasicAuth" or .kind=="SSHKeyPair") and
    (has("status")|not) and ((.metadata|keys)-["annotations","labels","name","namespace"]|length)==0) and
  all(.secrets[]; . as $record | .object.apiVersion=="v1" and .object.kind=="Secret" and
    ((.object.metadata|keys)-["annotations","labels","name","namespace"]|length)==0 and
    (if .mode=="annotation" then (.owner|not) and ((.object.metadata.ownerReferences // [])|length)==0
     else .owner.apiVersion=="secretgenerator.mittwald.de/v1alpha1" and
       ([.owner.kind]|inside(["StringSecret","BasicAuth","SSHKeyPair"])) and
       any($root.crs[]?; .apiVersion==$record.owner.apiVersion and .kind==$record.owner.kind and .metadata.namespace==$record.owner.namespace and .metadata.name==$record.owner.name) and
       .object.metadata.namespace==.owner.namespace and .object.metadata.name==.owner.name end)) and
  ([.secrets[]|select(.mode=="cr")]|length)==(.crs|length) and
  all(.crs[]; . as $c | ([$root.secrets[]|select(.mode=="cr" and .owner.apiVersion==$c.apiVersion and .owner.kind==$c.kind and .owner.namespace==$c.metadata.namespace and .owner.name==$c.metadata.name)]|length)==1) and
  (([.crs[]|(.kind+"\u0000"+.metadata.namespace+"\u0000"+.metadata.name)]|length)==([.crs[]|(.kind+"\u0000"+.metadata.namespace+"\u0000"+.metadata.name)]|unique|length)) and
  (([.secrets[]|(.object.metadata.namespace+"\u0000"+.object.metadata.name)]|length)==([.secrets[]|(.object.metadata.namespace+"\u0000"+.object.metadata.name)]|unique|length))
' >/dev/null || fail 'backup payload kinds or identities are invalid/duplicated'
# Scope and collision checks also run before CRD mutation.
"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | jq -r '.crs[] | [.kind,.metadata.namespace,.metadata.name] | @tsv' |
while IFS="$(printf '\t')" read -r kind namespace name; do
	case "$kind" in
		StringSecret) resource=stringsecrets.secretgenerator.mittwald.de ;;
		BasicAuth) resource=basicauths.secretgenerator.mittwald.de ;;
		SSHKeyPair) resource=sshkeypairs.secretgenerator.mittwald.de ;;
		*) fail "unsupported CR kind in backup: $kind" ;;
	esac
	namespace_allowed "$namespace" || fail 'backup contains a CR outside the approved scope'
	if k -n "$namespace" get "$resource/$name" >/dev/null 2>&1; then
		fail "restore destination CR already exists: $kind $namespace/$name"
	fi
done

"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | jq -r '.secrets[] | [.object.metadata.namespace,.object.metadata.name] | @tsv' |
while IFS="$(printf '\t')" read -r namespace name; do
	namespace_allowed "$namespace" || fail 'backup contains a Secret outside the approved scope'
	if k -n "$namespace" get "secret/$name" >/dev/null 2>&1; then
		fail "restore destination Secret already exists: $namespace/$name"
	fi
done

# Archived CRDs are evidence only. Restore applies the separately verified
# target release CRDs only after every payload/scope/collision check passed.
[ -d "$TARGET_CRD_DIR" ] || fail 'TARGET_CRD_DIR must be a directory containing verified target CRDs'
k apply --server-side --field-manager=kubernetes-secret-generator-restore -f "$TARGET_CRD_DIR" >/dev/null
for crd in basicauths.secretgenerator.mittwald.de sshkeypairs.secretgenerator.mittwald.de stringsecrets.secretgenerator.mittwald.de; do
	k wait --for=condition=Established --timeout=60s "customresourcedefinition/$crd" >/dev/null
done

"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" |
jq -c '.crs[] | .metadata.annotations = ((.metadata.annotations // {}) | del(.["kubectl.kubernetes.io/last-applied-configuration"]))' |
while IFS= read -r object; do
	printf '%s\n' "$object" | k create -f - >/dev/null
done

"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | jq -r '.secrets[] | @base64' |
while IFS= read -r encoded; do
	record=$(printf '%s' "$encoded" | openssl base64 -d -A)
	mode=$(printf '%s' "$record" | jq -r '.mode')
	object=$(printf '%s' "$record" | jq -c '.object | .metadata.annotations = ((.metadata.annotations // {}) | del(.["kubectl.kubernetes.io/last-applied-configuration"]))')
	if [ "$mode" = cr ]; then
		kind=$(printf '%s' "$record" | jq -r '.owner.kind')
		namespace=$(printf '%s' "$record" | jq -r '.owner.namespace')
		name=$(printf '%s' "$record" | jq -r '.owner.name')
		api_version=$(printf '%s' "$record" | jq -r '.owner.apiVersion')
		case "$kind" in
			StringSecret) resource=stringsecrets.secretgenerator.mittwald.de ;;
			BasicAuth) resource=basicauths.secretgenerator.mittwald.de ;;
			SSHKeyPair) resource=sshkeypairs.secretgenerator.mittwald.de ;;
		esac
		uid=$(k -n "$namespace" get "$resource/$name" -o jsonpath='{.metadata.uid}')
		object=$(printf '%s' "$object" | jq -c --arg apiVersion "$api_version" --arg kind "$kind" --arg name "$name" --arg uid "$uid" '.metadata.ownerReferences=[{apiVersion:$apiVersion,kind:$kind,name:$name,uid:$uid,controller:true,blockOwnerDeletion:true}]')
	fi
	printf '%s\n' "$object" | k create -f - >/dev/null
done

"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" | jq -r '.secrets[] | @base64' |
while IFS= read -r encoded; do
	record=$(printf '%s' "$encoded" | openssl base64 -d -A)
	namespace=$(printf '%s' "$record" | jq -r '.object.metadata.namespace')
	name=$(printf '%s' "$record" | jq -r '.object.metadata.name')
	mode=$(printf '%s' "$record" | jq -r '.mode')
	owner_uid=
	if [ "$mode" = cr ]; then
		kind=$(printf '%s' "$record" | jq -r '.owner.kind')
		case "$kind" in
			StringSecret) resource=stringsecrets.secretgenerator.mittwald.de ;;
			BasicAuth) resource=basicauths.secretgenerator.mittwald.de ;;
			SSHKeyPair) resource=sshkeypairs.secretgenerator.mittwald.de ;;
		esac
		owner_uid=$(k -n "$namespace" get "$resource/$(printf '%s' "$record" | jq -r '.owner.name')" -o jsonpath='{.metadata.uid}')
	fi
	{
		printf '%s' "$record"
		k -n "$namespace" get "secret/$name" -o json
	} | jq -s -e --arg ownerUID "$owner_uid" '
		def stableannotations: (. // {}) | del(.["kubectl.kubernetes.io/last-applied-configuration"]);
		.[0] as $record | $record.object as $expected | .[1] as $actual |
		$expected.data == $actual.data and $expected.type == $actual.type and
		$expected.immutable == ($actual.immutable // false) and
		$expected.metadata.labels == ($actual.metadata.labels // {}) and
		($expected.metadata.annotations|stableannotations) == ($actual.metadata.annotations|stableannotations) and
		(if $record.mode == "annotation" then (($actual.metadata.ownerReferences // [])|length)==0
		 else (($actual.metadata.ownerReferences // [])|length)==1 and
			$actual.metadata.ownerReferences[0].apiVersion==$record.owner.apiVersion and
			$actual.metadata.ownerReferences[0].kind==$record.owner.kind and
			$actual.metadata.ownerReferences[0].name==$record.owner.name and
			$actual.metadata.ownerReferences[0].uid==$ownerUID and
			$actual.metadata.ownerReferences[0].controller==true and
			$actual.metadata.ownerReferences[0].blockOwnerDeletion==true end)' >/dev/null || fail "restored Secret equality/owner check failed: $namespace/$name"
done

# POSIX pipelines may execute loops in subshells, so report the authoritative
# encrypted payload count instead of the loop-local counter.
	"$ENCRYPTION_ADAPTER" decrypt <"$BACKUP_FILE" |
	jq -c --arg issue "$RELEASE_ISSUE" '{restored:true,secretEquality:true,secretCount:(.secrets|length),controllerRemainsStopped:true,releaseIssue:$issue}'
