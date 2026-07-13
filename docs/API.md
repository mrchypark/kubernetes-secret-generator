# API and annotation reference

The CR API is `secretgenerator.mittwald.de/v1alpha1`; all CRs are namespaced and create a same-name Secret. CR-owned Secrets require an exact controller owner reference matching API version, kind, name, UID, and `controller=true`. The controller never adopts an unowned or foreign-owned Secret.

## CR specifications

| Kind | Field | Default | Contract |
|---|---|---|---|
| All | `spec.data` | empty | Declarative literal Secret data; keys are 1–253 characters matching `[A-Za-z0-9._-]+` |
| All | `spec.forceRegenerate` | `false` | When true, rotate generated fields once for that CR generation |
| All CRs | `spec.rotationInterval` | empty (disabled) | Go duration from `1m` through `8760h`; rotates generated credentials after each elapsed interval |
| `StringSecret` | `spec.type` | `Opaque` | Declarative Kubernetes Secret type |
| `StringSecret` | `spec.fields[]` | none | 1–64 generated fields; each needs a unique `fieldName` and must not collide with `spec.data` |
| `StringSecret` | `spec.fields[].encoding` | controller default (`base64`) | `base64`, `base64url`, `base32`, `hex`, or `raw` |
| `StringSecret` | `spec.fields[].length` | controller default (`40`) | `1..65536`; optional `B`/`b` suffix measures source bytes |
| `BasicAuth` | `spec.username` | `admin` | 1–255 Unicode characters in valid UTF-8; colon, CR, LF, and NUL are rejected |
| `BasicAuth` | `spec.encoding` | controller default (`base64`) | Same encoding set; encoded password must not exceed bcrypt's 72-byte input bound |
| `BasicAuth` | `spec.length` | controller default (`40`) | Same length syntax; generated keys are `username`, `password`, and `auth` and may not appear in `spec.data` |
| `SSHKeyPair` | `spec.algorithm` | `rsa` | `rsa`, `ecdsa`, or `ed25519` |
| `SSHKeyPair` | `spec.length` | algorithm default | RSA: `2048`, `3072`, `4096`; ECDSA: `256`, `384`, `521`; omitted for Ed25519 |
| `SSHKeyPair` | `spec.privateKey` | generated | PEM private key, at most 64 KiB; algorithm and public key must agree |
| `SSHKeyPair` | `spec.privateKeyField` | `ssh-privatekey` | Custom key matching the Secret key syntax |
| `SSHKeyPair` | `spec.publicKeyField` | `ssh-publickey` | Must differ from the private field and not collide with `spec.data` |
| `SSHKeyPair` | `spec.type` | `Opaque` | Declarative Kubernetes Secret type |

A resource may manage at most 256 keys and its projected serialized Secret may not exceed 768 KiB. Invalid desired state is terminal: the controller sets `Ready=False` and does not periodically retry until the object changes.

`spec.rotationInterval` applies only to the three CR kinds, not annotation-managed Secrets. Empty or omitted means no scheduled rotation. A non-empty value uses Go duration syntax and must be between `1m` and `8760h`, inclusive. `StringSecret` requires at least one generated `spec.fields` entry, and `SSHKeyPair` cannot combine scheduled rotation with a supplied `spec.privateKey`.

Enabling scheduled rotation establishes a new interval without immediately changing the Secret. Disabling and later re-enabling also starts a fresh interval. Changing an enabled interval keeps the current schedule anchor, so shortening it can make one rotation immediately due while lengthening it postpones the next rotation. If the controller misses multiple intervals, it rotates once and starts the next interval from that successful write. A successful force regeneration, generated-value drift repair, or other generated credential replacement also restarts the interval. Literal-only reconciliation does not. An immutable Secret that reaches its rotation time reports `ImmutableSecretConflict` instead of changing data. See [Credential rotation](ROTATION.md).

## Secret annotations

Every key below is prefixed by `secret-generator.v1.mittwald.de/`.

| Suffix | Applies to | Default | Contract |
|---|---|---|---|
| `type` | all | `string` | `string`, `basic-auth`, or `ssh-keypair` |
| `autogenerate` | string | required | Comma-separated unique generated field names; no whitespace normalization |
| `length` | all | `40` for strings/passwords; algorithm default for SSH | Same bounds and syntax as the corresponding CR field |
| `encoding` | string/basic-auth | `base64` | `base64`, `base64url`, `base32`, `hex`, or `raw` |
| `basic-auth-username` | basic-auth | `admin` | Same username contract as `BasicAuth.spec.username` |
| `ssh-key-algorithm` | SSH | `rsa` | `rsa`, `ecdsa`, or `ed25519` |
| `private-key-field` | SSH | `ssh-privatekey` | Secret key name for private material |
| `public-key-field` | SSH | `ssh-publickey` | Secret key name for authorized-key public material |
| `regenerate` | all | absent | `true`/`yes` rotates all generated fields; `false`/`no`/empty is a no-op; String alone accepts a comma-separated generated-field subset |

The controller owns its generated-at, secure, regeneration, and tracking annotations. Operators must not edit controller tracking or copy tracking checksums into logs, tickets, or metrics. Tracking checksums detect accidental drift; they are not an authorization or durable integrity boundary. Informer-observed data plus valid-tracking changes that do not match an exact in-process controller write are rejected. If an actor with Secret write access changes both while the controller is stopped, the Secret-local tracking model has no independent historical authority to prove the forgery after cold start; mitigate this boundary with least-privilege Secret RBAC and Kubernetes audit retention. Independent durable history or signing is a future design, not part of this contract.

## Status contract

All CRs expose `status.secret`, `status.observedGeneration`, `status.conditions`, `status.lastRegeneratedGeneration`, and `status.trackingInitialized`. `Ready` is the supported Condition type. Common reasons are `Reconciled`, `InvalidSpec`, `LegacyBaselineInvalid`, `SecretOwnershipConflict`, `RegenerationStateConflict`, `TrackingStateConflict`, `SecretSizeConflict`, `ImmutableSecretConflict`, `GenerationFailed`, and `ApplyFailed`.

Status is observational. Secret tracking is authoritative for regeneration idempotency; deleting or editing status must not rotate credentials. See [Troubleshooting](TROUBLESHOOTING.md) for reason-specific recovery.
