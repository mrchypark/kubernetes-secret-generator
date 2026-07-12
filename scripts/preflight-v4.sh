#!/bin/sh
set -eu

fail() { printf 'error: %s\n' "$*" >&2; exit 2; }
require() { eval "value=\${$1:-}"; [ -n "$value" ] || fail "$1 is required"; }

require KUBE_CONTEXT
require CONFIRM_CONTEXT
require EXPECTED_SERVER_URL
require EXPECTED_CA_SHA256
require NAMESPACE
require RELEASE_NAME
[ "$KUBE_CONTEXT" = "$CONFIRM_CONTEXT" ] || fail 'CONFIRM_CONTEXT must exactly match KUBE_CONTEXT'
command -v kubectl >/dev/null 2>&1 || fail 'kubectl is required'
command -v jq >/dev/null 2>&1 || fail 'jq is required'
command -v openssl >/dev/null 2>&1 || fail 'openssl is required'
command -v go >/dev/null 2>&1 || fail 'Go is required for in-memory bcrypt and SSH baseline validation'

scripts_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)

if ! server=$(kubectl --context "$KUBE_CONTEXT" config view --minify --raw -o jsonpath='{.clusters[0].cluster.server}'); then fail 'unable to read selected context server'; fi
[ "$server" = "$EXPECTED_SERVER_URL" ] || fail 'EXPECTED_SERVER_URL does not match selected context'
if ! ca_data=$(kubectl --context "$KUBE_CONTEXT" config view --minify --raw --flatten -o jsonpath='{.clusters[0].cluster.certificate-authority-data}'); then fail 'unable to read selected context CA'; fi
[ -n "$ca_data" ] || fail 'selected context has no embedded CA data'
ca_sha=$(printf '%s' "$ca_data" | openssl base64 -d -A | openssl dgst -sha256 -r | awk '{print $1}')
[ "$ca_sha" = "$EXPECTED_CA_SHA256" ] || fail 'EXPECTED_CA_SHA256 does not match selected context'

deployment_name=${DEPLOYMENT_NAME:-$RELEASE_NAME}
report_file=${REPORT_FILE:-}
report_format=${REPORT_FORMAT:-json}
case "$report_format" in json|markdown) ;; *) fail 'REPORT_FORMAT must be json or markdown' ;; esac
tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/ksg-preflight.XXXXXX")
trap 'rm -rf "$tmp_dir"' 0 1 2 15
report=$tmp_dir/report.json
baseline_findings=$tmp_dir/baseline-findings.json
baseline_input=$tmp_dir/baseline-input.fifo
baseline_deployment=$tmp_dir/baseline-deployment.fifo
baseline_objects=$tmp_dir/baseline-objects.fifo
mkfifo "$baseline_input" "$baseline_deployment" "$baseline_objects"

# Secret and private-key bytes flow only through pipes and process memory. The
# cached validator emits redacted object identities/reason codes to disk.
kubectl --context "$KUBE_CONTEXT" get deployment "$deployment_name" -n "$NAMESPACE" -o json >"$baseline_deployment" &
baseline_deployment_pid=$!
kubectl --context "$KUBE_CONTEXT" get \
	stringsecrets.secretgenerator.mittwald.de,basicauths.secretgenerator.mittwald.de,sshkeypairs.secretgenerator.mittwald.de,secrets \
	-A -o json >"$baseline_objects" &
baseline_objects_pid=$!
jq -s '{deployment:.[0],items:.[1].items}' "$baseline_deployment" "$baseline_objects" >"$baseline_input" &
baseline_jq_pid=$!
baseline_ok=true
if ! "$scripts_dir/preflight-baseline" <"$baseline_input" >"$baseline_findings"; then baseline_ok=false; fi
for pid in "$baseline_deployment_pid" "$baseline_objects_pid" "$baseline_jq_pid"; do
	if ! wait "$pid"; then baseline_ok=false; fi
done
[ "$baseline_ok" = true ] || fail 'unable to read a complete baseline CR/Secret snapshot'

scope_mode=${SCOPE_MODE:-}
confirmed_scope=${CONFIRMED_SCOPE:-}
scope_namespaces=${SCOPE_NAMESPACES:-}
confirmed_namespaces_sha=${CONFIRMED_NAMESPACES_SHA256:-}
canonical_namespaces_sha=
namespace_input_valid=true
if [ "$scope_mode" = namespaces ] && [ -n "$scope_namespaces" ]; then
	case ",$scope_namespaces," in *',,'*) namespace_input_valid=false ;; esac
	namespace_count=$(printf '%s' "$scope_namespaces" | awk -F, '{print NF}')
	canonical_namespaces=$(printf '%s' "$scope_namespaces" | tr ',' '\n' | LC_ALL=C sort -u | paste -sd '\n' -)
	canonical_namespace_count=$(printf '%s\n' "$canonical_namespaces" | awk 'NF { count++ } END { print count+0 }')
	[ "$namespace_count" -eq "$canonical_namespace_count" ] || namespace_input_valid=false
	if command -v shasum >/dev/null 2>&1; then
		canonical_namespaces_sha=$(printf '%s' "$canonical_namespaces" | shasum -a 256 | awk '{print $1}')
	else
		canonical_namespaces_sha=$(printf '%s' "$canonical_namespaces" | sha256sum | awk '{print $1}')
	fi
fi

report_deployment=$tmp_dir/report-deployment.fifo
report_crds=$tmp_dir/report-crds.fifo
report_strings=$tmp_dir/report-strings.fifo
report_basics=$tmp_dir/report-basics.fifo
report_sshs=$tmp_dir/report-sshs.fifo
report_secrets=$tmp_dir/report-secrets.fifo
mkfifo "$report_deployment" "$report_crds" "$report_strings" "$report_basics" "$report_sshs" "$report_secrets"
kubectl --context "$KUBE_CONTEXT" get deployment "$deployment_name" -n "$NAMESPACE" -o json >"$report_deployment" & report_deployment_pid=$!
kubectl --context "$KUBE_CONTEXT" get \
	customresourcedefinitions.apiextensions.k8s.io/basicauths.secretgenerator.mittwald.de \
	customresourcedefinitions.apiextensions.k8s.io/sshkeypairs.secretgenerator.mittwald.de \
	customresourcedefinitions.apiextensions.k8s.io/stringsecrets.secretgenerator.mittwald.de -o json >"$report_crds" & report_crds_pid=$!
kubectl --context "$KUBE_CONTEXT" get stringsecrets.secretgenerator.mittwald.de -A -o json >"$report_strings" & report_strings_pid=$!
kubectl --context "$KUBE_CONTEXT" get basicauths.secretgenerator.mittwald.de -A -o json >"$report_basics" & report_basics_pid=$!
kubectl --context "$KUBE_CONTEXT" get sshkeypairs.secretgenerator.mittwald.de -A -o json >"$report_sshs" & report_sshs_pid=$!
kubectl --context "$KUBE_CONTEXT" get secrets -A -o json >"$report_secrets" & report_secrets_pid=$!
report_ok=true
if ! jq -s --slurpfile baselineResult "$baseline_findings" \
		--arg context "$KUBE_CONTEXT" --arg server "$server" --arg ca "$ca_sha" \
		--arg releaseNamespace "$NAMESPACE" --arg releaseName "$RELEASE_NAME" \
		--arg scopeMode "$scope_mode" --arg confirmedScope "$confirmed_scope" \
		--arg scopeNamespaces "$scope_namespaces" --arg confirmedNamespacesSHA "$confirmed_namespaces_sha" \
		--arg canonicalNamespacesSHA "$canonical_namespaces_sha" --argjson namespaceInputValid "$namespace_input_valid" '
  def finding($severity;$code;$kind;$o;$message;$field):
    {severity:$severity,code:$code,kind:$kind,namespace:($o.metadata.namespace // ""),name:($o.metadata.name // ""),message:$message}
    + (if $field == "" then {} else {field:$field} end);
  def validLength($v):
    ($v == "") or (($v | test("^[0-9]+[bB]?$")) and (($v | sub("[bB]$";"") | tonumber) >= 1) and (($v | sub("[bB]$";"") | tonumber) <= 65536));
  def validEncoding($v): ($v == "") or (["base64","base64url","base32","hex","raw"] | index($v) != null);
  def effective($v;$default): if $v == null or $v == "" then $default else $v end;
  def snapshot($deployment;$crs;$secrets):
    ([{kind:($deployment.kind // "Deployment"),namespace:($deployment.metadata.namespace // ""),name:($deployment.metadata.name // ""),uid:($deployment.metadata.uid // ""),resourceVersion:($deployment.metadata.resourceVersion // "")}] +
    [($crs + $secrets)[] | {kind,namespace:(.metadata.namespace // ""),name:(.metadata.name // ""),uid:(.metadata.uid // ""),resourceVersion:(.metadata.resourceVersion // "")}]) |
    sort_by(.kind,.namespace,.name);
  def encodedLength($v;$encoding):
    ($v | sub("[bB]$";"") | tonumber) as $n |
    if ($v|test("[bB]$"))|not then $n
    elif $encoding == "hex" then 2*$n
    elif $encoding == "base32" then 8*(($n+4)/5|floor)
    elif $encoding == "raw" then $n
    else 4*(($n+2)/3|floor) end;
  def validKey($v): ($v | type == "string") and (($v|length) >= 1) and (($v|length) <= 253) and ($v|test("^[A-Za-z0-9._-]+$"));
  def ownerExact($secret;$cr):
    (($secret.metadata.ownerReferences // []) | length) == 1 and
    all($secret.metadata.ownerReferences[]?;
      .apiVersion == $cr.apiVersion and .kind == $cr.kind and .name == $cr.metadata.name and
      .uid == $cr.metadata.uid and .controller == true);
  def sameSecret($secrets;$cr): first($secrets[] | select(.metadata.namespace == $cr.metadata.namespace and .metadata.name == $cr.metadata.name)) // null;
  def crFindings($crs;$secrets):
    [ $crs[] as $cr |
      (sameSecret($secrets;$cr)) as $secret |
      if $secret == null then finding("blocker";"MissingManagedSecret";$cr.kind;$cr;"same-name managed Secret is missing";"")
      elif ownerExact($secret;$cr) | not then finding("blocker";"OwnerMismatch";$cr.kind;$cr;"same-name Secret does not have the exact controller owner identity";"")
      else empty end,
      if ($cr.spec.forceRegenerate // false) then finding("blocker";"ForceRegeneratePending";$cr.kind;$cr;"spec.forceRegenerate must be reviewed and set false before upgrade";"spec.forceRegenerate") else empty end,
      if $cr.kind == "StringSecret" then
        if ([($cr.spec.fields // [])[].fieldName] | length) + (($cr.spec.data // {})|length) == 0 then finding("blocker";"EmptySpecification";$cr.kind;$cr;"at least one literal or generated field is required";"spec") else empty end,
        if (($cr.spec.fields // [])|length) > 64 or ((($cr.spec.fields // [])|length) + (($cr.spec.data // {})|length)) > 256 then finding("blocker";"ManagedFieldLimit";$cr.kind;$cr;"generated or total managed field count exceeds the v4 bound";"spec") else empty end,
        if any(($cr.spec.fields // [])[]; (validKey(.fieldName)|not) or (validLength(.length // "")|not) or (validEncoding(.encoding // "")|not)) then finding("blocker";"InvalidStringField";$cr.kind;$cr;"generated field name, length, or encoding is invalid";"spec.fields") else empty end,
        if (([($cr.spec.fields // [])[].fieldName] | length) != ([($cr.spec.fields // [])[].fieldName] | unique | length)) or any(($cr.spec.fields // [])[]; (.fieldName as $n | ($cr.spec.data // {}) | has($n))) then finding("blocker";"FieldCollision";$cr.kind;$cr;"generated fields are duplicated or collide with literal data";"spec.fields") else empty end,
        if $secret != null and ((($secret.metadata.annotations // {})["secret-generator.v1.mittwald.de/secure"] // "") == "") then finding("blocker";"SecureMarkerMissing";$cr.kind;$cr;"legacy String secure marker is missing; record the intended rotation decision in the release issue";"metadata.annotations") else empty end
      elif $cr.kind == "BasicAuth" then
		(if ($cr.spec.username // "") == "" then "admin" else $cr.spec.username end) as $username |
		if ($username | test("[:\\r\\n\\u0000]")) or ($username|length) > 255 then finding("blocker";"InvalidUsername";$cr.kind;$cr;"effective username is oversized or contains a forbidden character";"spec.username") else empty end,
        if (validLength($cr.spec.length // "")|not) or (validEncoding($cr.spec.encoding // "")|not) then finding("blocker";"InvalidCredentialShape";$cr.kind;$cr;"length or encoding is invalid";"spec")
		elif encodedLength(effective($cr.spec.length;"40");effective($cr.spec.encoding;"base64")) > 72 then finding("blocker";"BcryptInputTooLong";$cr.kind;$cr;"encoded password would exceed bcrypt 72-byte input limit";"spec.length") else empty end,
        if any(["auth","username","password"][]; . as $n | ($cr.spec.data // {}) | has($n)) then finding("blocker";"ReservedFieldCollision";$cr.kind;$cr;"literal data collides with generated BasicAuth fields";"spec.data") else empty end
      elif $cr.kind == "SSHKeyPair" then
        ($cr.spec.algorithm // "rsa") as $alg | ($cr.spec.length // "") as $len |
        if (((["rsa","ecdsa","ed25519"] | index($alg)) == null) or
           ($alg == "rsa" and ($len != "" and ((["2048","3072","4096"]|index($len)) == null))) or
		   ($alg == "ecdsa" and ($len != "" and ((["256","384","521"]|index($len)) == null)))) then finding("blocker";"InvalidSSHConfiguration";$cr.kind;$cr;"algorithm and length combination is invalid";"spec") else empty end,
        ($cr.spec.privateKeyField // "ssh-privatekey") as $priv | ($cr.spec.publicKeyField // "ssh-publickey") as $pub |
        if (validKey($priv)|not) or (validKey($pub)|not) or $priv == $pub or (($cr.spec.data // {})|has($priv)) or (($cr.spec.data // {})|has($pub)) then finding("blocker";"SSHFieldCollision";$cr.kind;$cr;"SSH key fields are invalid, equal, or collide with literal data";"spec") else empty end
      else empty end,
      if any((($cr.spec.data // {})|keys)[]; validKey(.)|not) then finding("blocker";"InvalidLiteralKey";$cr.kind;$cr;"literal data contains an invalid Secret key";"spec.data") else empty end,
      if $secret != null and ($secret.immutable // false) then finding("blocker";"ImmutableManagedSecret";$cr.kind;$cr;"managed Secret is immutable and requires approved offline remediation";"") else empty end,
      if $secret != null then
        ([($cr.spec.data // {})|keys[]] +
          (if $cr.kind == "StringSecret" then [($cr.spec.fields // [])[].fieldName]
           elif $cr.kind == "BasicAuth" then ["auth","username","password"]
           else [($cr.spec.privateKeyField // "ssh-privatekey"),($cr.spec.publicKeyField // "ssh-publickey")] end) | unique) as $expected |
        ((($secret.data // {})|keys) - $expected) as $stale |
        if ($stale|length) > 0 then finding("warning";"StaleDataCandidates";$cr.kind;$cr;("Secret has undeclared data-key candidates requiring human review: " + ($stale|join(",")));"") else empty end
      else empty end,
      if $secret != null then
		((($secret.metadata.labels // {})|keys) - (($cr.metadata.labels // {})|keys)) as $staleLabels |
        if ($staleLabels|length) > 0 then finding("warning";"StaleLabelCandidates";$cr.kind;$cr;("Secret has undeclared label-key candidates requiring human review: " + ($staleLabels|join(",")));"") else empty end
      else empty end
    ];
  .[0] as $deployment | .[1].items as $crds | .[2].items as $strings | .[3].items as $basics | .[4].items as $sshs | .[5].items as $secrets |
  ($strings + $basics + $sshs) as $crs |
  ([($deployment.spec.template.spec.containers[]?.env[]? | select(.name=="WATCH_NAMESPACE") | .value)] | first // "") as $watch |
  (if $watch == "" then "cluster" elif ($watch|contains(",")) or $watch != $releaseNamespace then "namespaces" else "ownNamespace" end) as $scope |
  ([ $crds[] as $crd |
    (first($crd.spec.versions[]? | select(.name=="v1alpha1")) // null) as $v |
    if $v == null or ($v.served|not) or ($v.storage|not) or ($v.subresources.status == null) then finding("blocker";"CRDSchemaIncomplete";"CustomResourceDefinition";$crd;"v1alpha1 must be served/storage with status subresource";"spec.versions") else empty end,
    if $crd.metadata.name == "sshkeypairs.secretgenerator.mittwald.de" and
       (($v.schema.openAPIV3Schema.properties.spec.properties.algorithm == null) or ($v.schema.openAPIV3Schema.properties.spec.properties.privateKey == null) or ($v.schema.openAPIV3Schema.properties.spec.properties.privateKeyField == null) or ($v.schema.openAPIV3Schema.properties.spec.properties.publicKeyField == null)) then finding("warning";"LegacySSHCRDSchema";"CustomResourceDefinition";$crd;"live SSH CRD lacks v4 fields; apply the verified target CRD before the manager";"spec.versions") else empty end,
    if ($v.schema.openAPIV3Schema.properties.status.properties.conditions == null) or ($v.schema.openAPIV3Schema.properties.status.properties.observedGeneration == null) then finding("warning";"LegacyStatusSchema";"CustomResourceDefinition";$crd;"live CRD lacks v4 status fields; apply the verified target CRD before the manager";"spec.versions") else empty end
  ]) as $crdFindings |
  (crFindings($crs;$secrets)) as $ownedFindings |
  ([ $secrets[] as $s | ($s.metadata.annotations // {}) as $a |
    if ($a | has("secret-generator.v1.mittwald.de/regenerate")) then finding("blocker";"RegenerateAnnotationPresent";"Secret";$s;"regenerate annotation is present and requires review even when its value is false";"metadata.annotations") else empty end
  ]) as $regenerateFindings |
  ([ $secrets[] as $s |
    ($s.metadata.annotations // {}) as $a |
    (($a["secret-generator.v1.mittwald.de/autogenerate"] // "") | split(",")) as $autoFields |
    select(($a["secret-generator.v1.mittwald.de/type"] // "") != "" or ($a | has("secret-generator.v1.mittwald.de/autogenerate"))) |
    if ($s.immutable // false) then finding("blocker";"ImmutableManagedSecret";"Secret";$s;"annotation-managed Secret is immutable and requires approved offline remediation";"") else empty end,
    if ([ $s.metadata.ownerReferences[]? | select(.controller==true) ]|length) > 0 then finding("blocker";"AnnotationOwnerConflict";"Secret";$s;"annotation-managed Secret must not have a controller owner";"metadata.ownerReferences") else empty end,
    (if ($a["secret-generator.v1.mittwald.de/type"] // "") == "" and ($a|has("secret-generator.v1.mittwald.de/autogenerate")) then "string" else ($a["secret-generator.v1.mittwald.de/type"] // "") end) as $annotationType |
    if (["string","basic-auth","ssh-keypair"] | index($annotationType)) == null then finding("blocker";"InvalidAnnotationType";"Secret";$s;"annotation Secret type is unknown";"metadata.annotations") else empty end,
    if $annotationType == "string" and (($a["secret-generator.v1.mittwald.de/secure"] // "") == "") then finding("blocker";"SecureMarkerMissing";"Secret";$s;"legacy String secure marker is missing; record the intended rotation decision in the release issue";"metadata.annotations") else empty end,
    if (($a["secret-generator.v1.mittwald.de/length"] // "") | validLength(.) | not) then finding("blocker";"InvalidAnnotationLength";"Secret";$s;"length annotation is invalid";"metadata.annotations") else empty end,
    if (($a["secret-generator.v1.mittwald.de/encoding"] // "") | validEncoding(.) | not) then finding("blocker";"InvalidAnnotationEncoding";"Secret";$s;"encoding annotation is invalid";"metadata.annotations") else empty end,
    if (($a["secret-generator.v1.mittwald.de/type"] // "string") == "string") and
       (($autoFields|length) > 64 or ($autoFields|length) != ($autoFields|unique|length) or any($autoFields[]; validKey(.)|not)) then finding("blocker";"InvalidAutogenerateFields";"Secret";$s;"autogenerate field list is empty, duplicated, invalid, or exceeds 64 fields";"metadata.annotations") else empty end,
    if $annotationType == "basic-auth" and (effective($a["secret-generator.v1.mittwald.de/basic-auth-username"];"admin") | length) > 255 or
       ($annotationType == "basic-auth" and (effective($a["secret-generator.v1.mittwald.de/basic-auth-username"];"admin") | test("[:\\r\\n\\u0000]"))) then finding("blocker";"InvalidUsername";"Secret";$s;"BasicAuth effective username is oversized or contains a forbidden character";"metadata.annotations") else empty end,
    if (($a["secret-generator.v1.mittwald.de/type"] // "") == "ssh-keypair") then
      ($a["secret-generator.v1.mittwald.de/ssh-key-algorithm"] // "rsa") as $alg | ($a["secret-generator.v1.mittwald.de/length"] // "") as $len |
      if (((["rsa","ecdsa","ed25519"]|index($alg)) == null) or
		 ($len != "" and (($alg=="rsa" and (["2048","3072","4096"]|index($len))==null) or ($alg=="ecdsa" and (["256","384","521"]|index($len))==null)))) then finding("blocker";"InvalidSSHConfiguration";"Secret";$s;"SSH algorithm and length annotations are incompatible";"metadata.annotations") else empty end
    else empty end
  ]) as $annotationFindings |
  (snapshot($deployment;$crs;$secrets)) as $reportSnapshot |
  ([if ($baselineResult[0].snapshot // []) != $reportSnapshot then finding("blocker";"SnapshotChanged";"Deployment";$deployment;"CR or Secret UID/resourceVersion changed during preflight; rerun against a stable snapshot";"") else empty end]) as $snapshotFindings |
  ([if ((["ownNamespace","namespaces","cluster"] | index($scopeMode)) == null) then finding("blocker";"ScopeConfirmationMissing";"Deployment";$deployment;"SCOPE_MODE must explicitly select ownNamespace, namespaces, or cluster";"scope.mode") else empty end,
    if $scopeMode != $confirmedScope then finding("blocker";"ScopeConfirmationMismatch";"Deployment";$deployment;"CONFIRMED_SCOPE must exactly match SCOPE_MODE";"migration.confirmedScope") else empty end,
    if $scopeMode == "namespaces" and ($namespaceInputValid|not) then finding("blocker";"NamespaceScopeInvalid";"Deployment";$deployment;"scope namespace list contains an empty or duplicate entry";"scope.namespaces") else empty end,
    if $scopeMode == "namespaces" and ($scopeNamespaces == "" or $canonicalNamespacesSHA == "" or $confirmedNamespacesSHA != $canonicalNamespacesSHA) then finding("blocker";"NamespaceScopeDigestMismatch";"Deployment";$deployment;"namespaces scope requires the canonical confirmed namespace SHA-256";"migration.confirmedNamespacesSHA256") else empty end,
    if $scopeMode != "" and $scopeMode != $scope then finding("blocker";"ScopeChangeCombinedWithUpgrade";"Deployment";$deployment;"requested scope differs from the running v3 scope; migrate scope as a separate approved change";"scope.mode")
    elif $scopeMode == "namespaces" and (($watch|split(",")|sort|unique) != ($scopeNamespaces|split(",")|sort|unique)) then finding("blocker";"ScopeChangeCombinedWithUpgrade";"Deployment";$deployment;"requested namespace set differs from the running v3 watch set";"scope.namespaces") else empty end
  ]) as $scopeFindings |
  ($crdFindings + $ownedFindings + $regenerateFindings + $annotationFindings + ($baselineResult[0].findings // []) + $snapshotFindings + $scopeFindings) as $findings |
  {
    schemaVersion:1,
    generatedAt:(now|todateiso8601),
    target:{context:$context,server:$server,caSHA256:$ca,releaseNamespace:$releaseNamespace,releaseName:$releaseName},
    deployment:{watchNamespace:$watch,inferredScope:$scope,requestedScope:$scopeMode,confirmedScope:$confirmedScope,confirmedNamespacesSHA256:$confirmedNamespacesSHA},
    crds:[$crds[] | (first(.spec.versions[]? | select(.name=="v1alpha1")) // {}) as $v | {name:.metadata.name,served:($v.served // false),storage:($v.storage // false),hasStatus:(($v.subresources.status // null) != null)}],
    counts:{crds:($crds|length),stringSecrets:($strings|length),basicAuths:($basics|length),sshKeyPairs:($sshs|length),managedSecretCandidates:([$secrets[] as $s | select(((($s.metadata.annotations // {})["secret-generator.v1.mittwald.de/type"] // "") != "") or (($s.metadata.annotations // {}) | has("secret-generator.v1.mittwald.de/autogenerate")) or any($crs[]; . as $c | $c.metadata.namespace == $s.metadata.namespace and $c.metadata.name == $s.metadata.name))]|length)},
    findings:$findings,
    blockerCount:([$findings[]|select(.severity=="blocker")]|length),
    warningCount:([$findings[]|select(.severity=="warning")]|length)
  }
' "$report_deployment" "$report_crds" "$report_strings" "$report_basics" "$report_sshs" "$report_secrets" >"$report"; then report_ok=false; fi
for pid in "$report_deployment_pid" "$report_crds_pid" "$report_strings_pid" "$report_basics_pid" "$report_sshs_pid" "$report_secrets_pid"; do
	if ! wait "$pid"; then report_ok=false; fi
done
[ "$report_ok" = true ] || { rm -f "$report"; fail 'unable to read a complete report CR/Secret snapshot'; }

"$scripts_dir/check-test-artifacts.sh" "$report"
output=$report
if [ "$report_format" = markdown ]; then
	output=$tmp_dir/report.md
	jq -r '
      "# Kubernetes Secret Generator v4 preflight\n",
      "- Generated: `\(.generatedAt)`",
      "- Release: `\(.target.releaseNamespace)/\(.target.releaseName)`",
      "- Inferred/requested scope: `\(.deployment.inferredScope)` / `\(.deployment.requestedScope)`",
      "- Blockers: **\(.blockerCount)**",
      "- Warnings: **\(.warningCount)**\n",
      "## Findings\n",
      (if (.findings|length)==0 then "No findings."
       else .findings[] | "- **\(.severity)** `\(.code)` — `\(.kind)` `\(.namespace)/\(.name)`: \(.message)" end)
    ' "$report" >"$output"
	"$scripts_dir/check-test-artifacts.sh" "$output"
fi
if [ -n "$report_file" ]; then
	[ ! -e "$report_file" ] || fail 'REPORT_FILE already exists'
	umask 077
	cp "$output" "$report_file"
fi
cat "$output"
[ "$(jq -r '.blockerCount' "$report")" -eq 0 ]
