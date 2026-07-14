# Using SSHKeyPair resources

`SSHKeyPair` is a namespaced custom resource in the `secretgenerator.mittwald.de/v1alpha1` API. The controller creates a `Secret` with the same name and namespace, adds the `SSHKeyPair` as its controller owner, and records an object reference in `status.secret`. Deleting the custom resource therefore normally lets Kubernetes garbage-collect the generated Secret. A same-name Secret that is not owned by an `SSHKeyPair` is not modified.

Published versions before v3.5.0 do not support `spec.ed25519Seed`. This page describes the current v3.5 source and does not imply that v3.5.0 has been released. Verify that both the installed CRD and controller contain the support before using it.

## Choose a key source

With no supplied key source, the controller generates a random key pair. The supported algorithms are:

- `rsa`: the default with the standard controller configuration; `length` is the key size in bits and defaults to 2048.
- `ecdsa`: supports lengths `256`, `384`, and `521`; an empty length defaults to 256.
- `ed25519`: generates a random key and ignores `length`.

This ordinary generated-key resource creates `generated-ssh` and stores the private key under the key required by the Kubernetes SSH Secret type:

```yaml
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: SSHKeyPair
metadata:
  name: generated-ssh
  namespace: default
spec:
  algorithm: ed25519
  type: kubernetes.io/ssh-auth
```

Alternatively, `privateKey` accepts an existing unencrypted PEM private key and derives its public key. For Ed25519 only, `ed25519Seed` accepts a canonical padded RFC 4648 standard-base64 string representing exactly 32 seed bytes. The same seed always produces the same PKCS#8 `PRIVATE KEY` PEM and OpenSSH public key.

## Create and use an Ed25519 seed

Generate the seed with OpenSSL into a mode-restricted temporary file. Run the generation, independent-backup verification, and apply commands below in the same POSIX `sh` session. They keep the seed out of process arguments and verify the required 44-character encoded length without printing it. There is deliberately no automatic cleanup trap: validation, backup-confirmation, apply, or interruption failures must leave the only local copy intact.

```sh
umask 077
seed_file=$(mktemp "${TMPDIR:-/tmp}/ksg-ed25519-seed.XXXXXX") || exit 1
echo "mode-0600 seed file created at $seed_file; content not displayed" >&2

openssl rand -base64 32 | tr -d '\n' >"$seed_file"
seed_length=$(wc -c <"$seed_file" | tr -d ' ')
[ "$seed_length" -eq 44 ] || {
  echo "unexpected Ed25519 seed length; seed file retained at $seed_file" >&2
  exit 1
}
```

Stop here. An independently encrypted copy in your organization's approved secret manager is mandatory for this workflow. Store the seed using an input mechanism that reads `"$seed_file"` without putting its contents in a command argument or terminal output. Then retrieve that backup into a separate mode-0600 temporary file, compare it to `"$seed_file"` with `cmp -s`, and remove the retrieved verification file. Do not continue unless creation, retrieval, and comparison all succeed. The custom resource and a backup of the same cluster are not independent copies.

After a human has completed those steps, set the exact confirmation below. This value is intentionally not inferred or set by the example because backup and retrieval procedures are provider-specific:

```sh
INDEPENDENT_SEED_BACKUP_CONFIRMED=true
```

Save this template as `sshkeypair-seeded.yaml`. The placeholder is not a valid seed and must be replaced before applying it:

```yaml
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: SSHKeyPair
metadata:
  name: seeded-ssh
  namespace: default
spec:
  algorithm: ed25519
  type: kubernetes.io/ssh-auth
  ed25519Seed: "<REPLACE_WITH_44_CHARACTER_BASE64_SEED>"
```

Substitute from the protected file on standard input instead of placing the seed in a command-line argument or a committed manifest. The guard requires the exact human confirmation above. A missing or different value stops before apply and retains the mode-0600 seed file:

```sh
if [ "${INDEPENDENT_SEED_BACKUP_CONFIRMED:-}" != true ]; then
  echo "independent encrypted seed backup and retrieval are not confirmed; seed file retained at $seed_file" >&2
  exit 1
fi

if ! awk 'NR == FNR { seed = $0; next } { gsub(/<REPLACE_WITH_44_CHARACTER_BASE64_SEED>/, seed); print }' \
  "$seed_file" sshkeypair-seeded.yaml | \
  kubectl apply --server-side --field-manager=kubernetes-secret-generator-seed -f -; then
  echo "server-side apply failed; seed file retained at $seed_file" >&2
  exit 1
fi

rm -f "$seed_file" || {
  echo "apply succeeded but seed file cleanup failed; file retained at $seed_file" >&2
  exit 1
}
unset seed_file seed_length INDEPENDENT_SEED_BACKUP_CONFIRMED
```

An interruption can leave the generated mode-0600 file at the path reported above. Do not display it. Only after independently encrypted backup retrieval has been verified, remove that exact path with `rm -f "$seed_file"` (or substitute the recorded path if the original shell ended).

Server-side apply is used so the complete manifest is not copied into the `kubectl.kubernetes.io/last-applied-configuration` annotation. If this resource was previously applied with client-side `kubectl apply`, remove that annotation without displaying its value:

```sh
kubectl annotate sshkeypair seeded-ssh -n default \
  kubectl.kubernetes.io/last-applied-configuration-
```

This removes only the live annotation. It cannot remove copies already retained in audit logs, backups, terminal capture, or Git history; handle those according to your data-retention and incident procedures.

Wait for the same-name Secret and inspect only non-sensitive metadata:

```sh
kubectl wait --for=jsonpath='{.status.secret.name}'=seeded-ssh \
  sshkeypair/seeded-ssh -n default --timeout=60s
kubectl get sshkeypair seeded-ssh -n default \
  -o jsonpath='{.status.secret.kind}{"/"}{.status.secret.name}{"\n"}'
kubectl get secret seeded-ssh -n default \
  -o jsonpath='{.type}{"\n"}'
```

Kubernetes serializes every `Secret.data` value as base64 in its API representation. That outer encoding is separate from `ed25519Seed`: the seed is base64 for 32 raw seed bytes, while `Secret.data.ssh-privatekey` is base64 for a complete PKCS#8 PEM document.

The following commands decode the Secret values into permission-restricted temporary files and verify that the public key matches the private key. They do not display the private key or seed:

```sh
umask 077
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/seeded-ssh-verify.XXXXXX") || exit 1
private_file=$work_dir/private
public_file=$work_dir/public
derived_file=$work_dir/derived
trap 'rm -rf "$work_dir"' 0 1 2 3 15

kubectl get secret seeded-ssh -n default \
  -o jsonpath='{.data.ssh-privatekey}' | openssl base64 -d -A >"$private_file" || exit 1
kubectl get secret seeded-ssh -n default \
  -o jsonpath='{.data.ssh-publickey}' | openssl base64 -d -A >"$public_file" || exit 1
ssh-keygen -y -f "$private_file" >"$derived_file" || exit 1
if cmp -s "$public_file" "$derived_file"; then
  echo "SSH key pair matches"
else
  echo "SSH key pair mismatch" >&2
  exit 1
fi
ssh-keygen -lf "$public_file"
rm -rf "$work_dir"
trap - 0 1 2 3 15
unset work_dir private_file public_file derived_file
```

## Fields

| Field | Default | Behavior |
| --- | --- | --- |
| `algorithm` | Controller default (`rsa` in the shipped configuration) | `rsa`, `ecdsa`, or `ed25519`. With `ed25519Seed`, it must be empty or `ed25519`. |
| `length` | RSA: controller setting (2048 shipped); ECDSA: 256 | RSA bit length or ECDSA curve size. Ignored for Ed25519. |
| `privateKey` | Empty | Supplied unencrypted PEM private key. Mutually exclusive with `ed25519Seed`. |
| `ed25519Seed` | Empty | Canonical padded standard base64 for exactly 32 Ed25519 seed bytes. |
| `privateKeyField` | `ssh-privatekey` | Secret data key for the PEM private key. |
| `publicKeyField` | `ssh-publickey` | Secret data key for the OpenSSH public key. |
| `type` | Empty | The controller leaves the Secret type unset. Use `kubernetes.io/ssh-auth` with the default private-key field name. |
| `data` | Empty map | Additional literal Secret data. With a supplied key source, entries matching the effective `privateKeyField` or `publicKeyField` are ignored; other missing or empty values are repaired additively. |
| `forceRegenerate` | `false` | Replaces both key fields. With a seed, the replacement is deterministic. It also overwrites values declared in `data`, except managed-key entries ignored for a supplied key source. |
| `rotationInterval` | Empty (disabled) | Go duration from `1m` through `8760h`. It cannot be combined with `privateKey` or `ed25519Seed`. See [rotation](ROTATION.md). |

Custom data-key names are useful with an `Opaque` Secret. Do not use `kubernetes.io/ssh-auth` here because that built-in type requires `ssh-privatekey`:

```yaml
apiVersion: secretgenerator.mittwald.de/v1alpha1
kind: SSHKeyPair
metadata:
  name: custom-fields
  namespace: default
spec:
  algorithm: ecdsa
  length: "384"
  type: Opaque
  privateKeyField: id_ecdsa
  publicKeyField: id_ecdsa.pub
  data:
    purpose: deployment
```

## Validation and conflicts

The CRD limits `ed25519Seed` to at most 44 characters. Kubernetes can therefore admit a malformed value that is 44 characters or shorter; the controller performs the complete runtime validation before writing a Secret. Values longer than 44 characters are rejected by the API server.

| Configuration or state | Result |
| --- | --- |
| `privateKey` and `ed25519Seed` are both set | Rejected by the controller without a Secret write. |
| Seed with empty `algorithm` or `algorithm: ed25519` | Accepted; the seed deterministically supplies the key pair. |
| Seed with `algorithm: rsa` or `algorithm: ecdsa` | Rejected by the controller without a Secret write. |
| Seed is short, malformed, unpadded, non-canonical, or does not decode to 32 bytes | Rejected by the controller without a Secret write. |
| `rotationInterval` with `privateKey` or `ed25519Seed` | Rejected without changing credentials. |
| Seed is set, keys already exist, and `forceRegenerate: false` | Existing nonempty keys are preserved. |
| Seed is set and `forceRegenerate: true` | Both key fields are replaced by the deterministic pair from the seed. Repeated reconciles produce the same values. |
| A supplied key source and `data` both declare an effective private- or public-key field | The managed `data` entries are ignored. The supplied source determines both key fields. |
| Private key is missing but the existing public key matches the supplied key or seed | The supplied private key is restored and the public key is preserved. |
| Private key is missing and the existing public key does not match the supplied key or seed | Reconciliation fails and Secret data is not written. |
| Public key is missing but a valid private key exists | The public key is derived from the existing private key; the private key is preserved. |
| Both key fields are missing or empty | The pair is recreated from the supplied source, or randomly generated when no source is supplied. |

The repair policy is additive. Missing or empty managed key and `data` entries are repaired, unrelated Secret data is preserved, and nonempty changed values are left alone unless `forceRegenerate` is true. Deleting the owned Secret recreates it; a seed makes that recreation deterministic. A same-name Secret without an `SSHKeyPair` owner is not adopted or modified.

## Security

Base64 is an encoding, not encryption. A seed stored in the custom resource can be exposed through:

- `get`, `list`, or `watch` access to `SSHKeyPair` resources;
- Git history or rendered GitOps artifacts;
- API audit records, depending on the cluster audit policy;
- etcd and cluster or namespace backups.

Do not commit a real seed. Supply it through an appropriately protected secret-delivery workflow, restrict `SSHKeyPair` read permissions with least-privilege RBAC, limit access to controller logs and backups, and apply the same protection used for private keys. This API stores the seed directly; it does not currently support a Secret reference.

## Upgrade an existing installation

Helm installs files from a chart's `crds/` directory on initial installation, but does not upgrade existing CRDs during `helm upgrade`. From a source checkout containing this feature, update the shipped SSHKeyPair CRD before deploying the matching controller or chart:

```sh
RELEASE_NAME='<existing-release-name>'
RELEASE_NAMESPACE='<existing-release-namespace>'

kubectl apply -f deploy/crds/secretgenerator.mittwald.de_sshkeypairs_crd.yaml
helm upgrade "$RELEASE_NAME" deploy/helm-chart/kubernetes-secret-generator \
  --namespace "$RELEASE_NAMESPACE" \
  --reuse-values
```

Replace both placeholders with the release and namespace reported for the existing installation by `helm list --all-namespaces`; do not use a different namespace or add `--install` during this upgrade. `--reuse-values` preserves the installed configuration. If your release is managed from explicit values files instead, pass the same reviewed `-f` files and overrides used for that release rather than `--reuse-values`.

Do not create an `ed25519Seed` resource until both steps are complete. Applying only the CRD allows the field to be stored, but an older controller does not implement it. Deploying only the controller while the old CRD remains causes the API server to prune or reject the unknown field. No offline migration is required for existing resources or Secrets.

## Troubleshooting without exposing the seed

Confirm field availability and inspect status, Secret keys, Events, and controller logs without retrieving the custom resource YAML:

```sh
kubectl explain sshkeypair.spec.ed25519Seed
kubectl get sshkeypair seeded-ssh -n default \
  -o jsonpath='{.metadata.generation}{"\t"}{.status.secret.name}{"\n"}'
kubectl get secret seeded-ssh -n default \
  -o go-template='{{range $key, $value := .data}}{{$key}}{{"\n"}}{{end}}'
kubectl get events -n default \
  --field-selector involvedObject.kind=SSHKeyPair,involvedObject.name=seeded-ssh
kubectl logs -n CONTROLLER_NAMESPACE deployment/CONTROLLER_DEPLOYMENT --since=10m | \
  grep 'seeded-ssh'
```

This controller does not currently emit dedicated Kubernetes Events for seed validation failures, so the controller log is the authoritative runtime error source. Validation messages identify the violated rule without including the seed value. Avoid `kubectl get sshkeypair ... -o yaml` and `kubectl describe sshkeypair ...` in shared terminals or support transcripts because both can display `spec.ed25519Seed`.
