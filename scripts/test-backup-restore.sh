#!/bin/sh
set -eu

repo_root=$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-backup-test.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15
mkdir "$tmp_dir/bin"
log=$tmp_dir/kubectl.log
state_dir=$tmp_dir/state
mkdir "$state_dir"
backup=$tmp_dir/backup.enc
kubeconfig=$tmp_dir/kubeconfig
: >"$kubeconfig"

cat >"$tmp_dir/adapter" <<'EOF'
#!/bin/sh
set -eu
case ${1:-} in
  encrypt) openssl enc -aes-256-cbc -salt -pbkdf2 -pass pass:local-test-only ;;
  decrypt) openssl enc -d -aes-256-cbc -pbkdf2 -pass pass:local-test-only ;;
  validate) openssl enc -d -aes-256-cbc -pbkdf2 -pass pass:local-test-only >/dev/null ;;
  *) exit 2 ;;
esac
EOF

cat >"$tmp_dir/bin/kubectl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >>"$KUBECTL_LOG"
case "$*" in
  *"config view"*".server"*) printf '%s' 'https://127.0.0.1:6443' ;;
  *"config view"*"certificate-authority-data"*) printf '%s' 'cmVzdG9yZS1jYQ==' ;;
  *"get namespace app"*"ksg-test-owner"*) printf '%s' "$RUN_OWNER_ID" ;;
  *"get deployment"*".spec.replicas"*) printf '0' ;;
  *"get deployment"*".status.readyReplicas"*) printf '0' ;;
  *"get customresourcedefinitions"*) cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"basicauths.secretgenerator.mittwald.de"},"spec":{}},{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"sshkeypairs.secretgenerator.mittwald.de"},"spec":{}},{"apiVersion":"apiextensions.k8s.io/v1","kind":"CustomResourceDefinition","metadata":{"name":"stringsecrets.secretgenerator.mittwald.de"},"spec":{}}]}
JSON
    ;;
  *"get stringsecrets.secretgenerator.mittwald.de/login"*"jsonpath"*) [ -f "$STATE_DIR/cr-login.json" ] && printf 'new-uid' || exit 1 ;;
  *"get stringsecrets"*"-o json"*) cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","metadata":{"namespace":"app","name":"login","uid":"old-uid","labels":{"team":"test"}},"spec":{"data":{"username":"service"},"fields":[{"fieldName":"password"}]}}]}
JSON
    ;;
  *"get basicauths"*"-o json"*|*"get sshkeypairs"*"-o json"*) printf '%s\n' '{"apiVersion":"v1","kind":"List","items":[]}' ;;
  *"get secrets"*"-o json"*) cat <<'JSON'
{"apiVersion":"v1","kind":"List","items":[
{"apiVersion":"v1","kind":"Secret","metadata":{"namespace":"app","name":"login","labels":{"team":"test"},"annotations":{"tracking":"kept"},"ownerReferences":[{"apiVersion":"secretgenerator.mittwald.de/v1alpha1","kind":"StringSecret","name":"login","uid":"old-uid","controller":true}]},"type":"Opaque","immutable":false,"data":{"username":"c2VydmljZQ==","password":"S1NHX1RFU1RfU0VDUkVUX25ldmVyX3BsYWludGV4dA=="}},
{"apiVersion":"v1","kind":"Secret","metadata":{"namespace":"app","name":"annotation","labels":{"team":"test"},"annotations":{"secret-generator.v1.mittwald.de/type":"string","secret-generator.v1.mittwald.de/autogenerate":"password"}},"type":"Opaque","immutable":false,"data":{"password":"S1NHX1RFU1RfU0VDUkVUX25ldmVyX3BsYWludGV4dA=="}}
]}
JSON
    ;;
  *"get stringsecrets.secretgenerator.mittwald.de/login"*) [ -f "$STATE_DIR/cr-login.json" ] ;;
  *"get secret/login"*"-o json"*) [ -f "$STATE_DIR/secret-login.json" ] && cat "$STATE_DIR/secret-login.json" || exit 1 ;;
  *"get secret/annotation"*"-o json"*) [ -f "$STATE_DIR/secret-annotation.json" ] && cat "$STATE_DIR/secret-annotation.json" || exit 1 ;;
  *"get secret/login"*|*"get secret/annotation"*)
    name=${*#*secret/}; name=${name%% *}; [ -f "$STATE_DIR/secret-$name.json" ]
    ;;
  *" create -f -"*)
    object=$(mktemp "$STATE_DIR/create.XXXXXX")
    trap 'rm -f "$object"' 0 1 2 15
    cat >"$object"
    kind=$(jq -r .kind "$object")
    name=$(jq -r .metadata.name "$object")
    case "$kind" in
      StringSecret) jq '.metadata.uid="new-uid"' "$object" >"$STATE_DIR/cr-$name.json" ;;
      Secret)
        if [ "${CORRUPT_CREATE:-false}" = true ]; then jq '.data={}' "$object" >"$STATE_DIR/secret-$name.json"
        else cp "$object" "$STATE_DIR/secret-$name.json"
        fi
        ;;
      *) exit 70 ;;
    esac
    ;;
	  *" apply "*|*" wait "*) : ;;
  *) echo "unexpected kubectl invocation: $*" >&2; exit 70 ;;
esac
EOF
chmod +x "$tmp_dir/adapter" "$tmp_dir/bin/kubectl"

ca_sha=$(printf '%s' 'restore-ca' | openssl dgst -sha256 -r | awk '{print $1}')
common="PATH=$tmp_dir/bin:$PATH KUBECTL_LOG=$log STATE_DIR=$state_dir ENCRYPTION_ADAPTER=$tmp_dir/adapter BACKUP_FILE=$backup RELEASE_ISSUE=https://github.com/mittwald/kubernetes-secret-generator/issues/123"
target="KUBECONFIG=$kubeconfig KUBE_CONTEXT=test CONFIRM_CONTEXT=test EXPECTED_SERVER_URL=https://127.0.0.1:6443 EXPECTED_CA_SHA256=$ca_sha NAMESPACE=app RELEASE_NAME=ksg CONTROLLER_STOPPED_CONFIRM=true SCOPE_MODE=ownNamespace"

# These variables intentionally contain multiple env NAME=value arguments.
# shellcheck disable=SC2086
env $common $target BACKUP_CONFIRM=test/app/ksg "$repo_root/scripts/backup-restore.sh" backup >"$tmp_dir/backup-report.json"
# shellcheck disable=SC2086
env $common "$repo_root/scripts/backup-restore.sh" verify >"$tmp_dir/verify-report.json"
grep -q 'KSG_TEST_SECRET' "$backup" && { echo 'encrypted backup contains plaintext sentinel' >&2; exit 1; }

: >"$log"
# shellcheck disable=SC2086
if env $common $target RESTORE_CONFIRM=wrong \
	TARGET_CRD_DIR="$repo_root/deploy/crds" "$repo_root/scripts/backup-restore.sh" restore >/dev/null 2>&1; then
	echo 'restore accepted a mismatched confirmation' >&2
	exit 1
fi
if grep -E -q ' apply | create | delete | patch | replace ' "$log"; then
	echo 'restore mutated the cluster before confirmation passed' >&2
	exit 1
fi

# shellcheck disable=SC2086
env $common $target RESTORE_CONFIRM=test/app/ksg \
	TARGET_CRD_DIR="$repo_root/deploy/crds" "$repo_root/scripts/backup-restore.sh" restore >"$tmp_dir/restore-report.json"
jq -e '.restored and .secretEquality and .secretCount == 2 and .controllerRemainsStopped' "$tmp_dir/restore-report.json" >/dev/null
if grep -q 'KSG_TEST_SECRET' "$tmp_dir"/*report.json; then echo 'backup report exposed plaintext' >&2; exit 1; fi

# The fake API stores the actual create stdin. Corrupting that payload must make
# the wrapper's subsequent live GET equality check fail.
rm -f "$state_dir"/*
# These variables intentionally contain multiple env NAME=value arguments.
# shellcheck disable=SC2086
if env $common $target CORRUPT_CREATE=true RESTORE_CONFIRM=test/app/ksg \
	TARGET_CRD_DIR="$repo_root/deploy/crds" "$repo_root/scripts/backup-restore.sh" restore >/dev/null 2>&1; then
	echo 'restore accepted a corrupted create payload' >&2
	exit 1
fi

# Invalid scope input must fail before any cluster mutation.
: >"$log"
# shellcheck disable=SC2086
if env $common BACKUP_FILE=$tmp_dir/wrong.enc KUBECONFIG=$kubeconfig SCOPE_MODE=namespaces SCOPE_NAMESPACES=app,app KUBE_CONTEXT=test CONFIRM_CONTEXT=test EXPECTED_SERVER_URL=https://127.0.0.1:6443 EXPECTED_CA_SHA256=$ca_sha NAMESPACE=app RELEASE_NAME=ksg CONTROLLER_STOPPED_CONFIRM=true BACKUP_CONFIRM=test/app/ksg "$repo_root/scripts/backup-restore.sh" backup >/dev/null 2>&1; then
	echo 'backup accepted duplicate scope namespaces' >&2
	exit 1
fi
if grep -E -q ' apply | create | delete | patch | replace ' "$log"; then echo 'scope validation failure mutated the cluster' >&2; exit 1; fi

echo 'encrypted backup/restore checks passed'
