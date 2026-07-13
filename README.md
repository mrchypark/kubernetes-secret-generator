# Kubernetes Secret Generator

Kubernetes controller that creates cryptographically secure random strings, BasicAuth credentials, and SSH key pairs. It supports annotation-managed `Secret` objects and the `StringSecret`, `BasicAuth`, and `SSHKeyPair` CRs.

## Install

Helm is the only supported deployment source. Use a release tag and its verified image digest; never deploy `latest`.

```sh
export KUBE_CONTEXT=my-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.13
export IMAGE_DIGEST='sha256:<64-hex-digest-from-v4.0.0-rc.13-candidate>'
export CRD_LIFECYCLE_MANAGER=direct
export SCOPE_MODE=ownNamespace

make install
```

`make install` applies the packaged CRDs first and then installs the manager. It rejects implicit contexts, unsafe namespaces, mutable image references, and a local chart version that differs from `CHART_VERSION`. See [installation](docs/INSTALL.md), [upgrade](docs/UPGRADE.md), and [migration](docs/MIGRATION-v4.md) before changing an existing installation.

## API summary

| Mode | Kind/type | Main inputs | Generated data |
|---|---|---|---|
| CR | `StringSecret` | `spec.fields`, `spec.data`, `spec.type`, regeneration settings | configured random fields |
| CR | `BasicAuth` | `spec.username`, `spec.length`, `spec.encoding`, `spec.data`, regeneration settings | `username`, `password`, `auth` |
| CR | `SSHKeyPair` | `spec.algorithm`, `spec.length`, `spec.privateKey`, field names, regeneration settings | private/public key fields |
| Annotation | `type=string` | `autogenerate`, `length`, `encoding` | named random fields |
| Annotation | `type=basic-auth` | `basic-auth-username`, `length`, `encoding` | `username`, `password`, `auth` |
| Annotation | `type=ssh-keypair` | `ssh-key-algorithm`, `length` | `ssh-privatekey`, `ssh-publickey` |

CR API group/version is `secretgenerator.mittwald.de/v1alpha1`. Supported encodings are `base64`, `base64url`, `base32`, `hex`, and `raw`. String length is `1..65536`; suffix `B` or `b` means input bytes rather than encoded output length. BasicAuth usernames must not contain `:` or line breaks and encoded passwords must fit bcrypt's 72-byte input limit. RSA keys must be at least 2048 bits. A resource may generate at most 64 fields and manage at most 256 keys; supplied private PEM is limited to 64 KiB and projected serialized Secret size to 768 KiB.

The complete field, annotation, default, and status contracts are in [API reference](docs/API.md).

### StringSecret example

Use the admission-tested [StringSecret manifest](docs/examples/stringsecret.yaml).

The generated Secret has the same name and is controller-owned by this exact CR identity. A same-name unowned or foreign-owned Secret is never adopted or modified.

### Annotation example

Use the admission-tested [annotation-managed Secret manifest](docs/examples/annotation-string-secret.yaml).

## Convergence and regeneration

CR literal data, type, CR-managed labels, and generated field configuration are declarative. Existing unmanaged Secret keys and labels are preserved during normal reconciliation. If an entire owned Secret is deleted, only CR-declared/generated content is recreated; Secret-only content requires backup restore.

Self-heal after generated credential drift causes safe rotation, not recovery of the previous value. Consumers must reload or restart after a rotation or recreation.

For CRs, `spec.forceRegenerate: true` rotates generated fields once per CR generation. Reconciliation, controller restart, or a status-write retry in the same generation does not rotate again. To rotate again, apply `false` and then `true`, or apply another spec change while it remains true.

CRs can also set `spec.rotationInterval` to a Go duration from `1m` through `8760h`, for example `24h`. Empty or omitted disables scheduled rotation. Enabling it starts a new interval without rotating immediately; missed intervals cause one safe rotation rather than catch-up rotations. This field is not supported on annotation-managed Secrets. See [credential rotation](docs/ROTATION.md) for the complete behavior and constraints.

For annotation-managed Secrets:

- `regenerate: "true"` or `"yes"` rotates every generated field.
- A comma-separated value rotates only those String fields.
- `"false"`, `"no"`, or empty is a no-op and is removed.
- Invalid or unknown selective fields produce one Warning event, remain for correction, and do not retry continuously.

```sh
kubectl --context "$KUBE_CONTEXT" -n application annotate secret application-login \
  secret-generator.v1.mittwald.de/regenerate=yes --overwrite
```

Never use `--all` for production rotation without a reviewed change plan.

## Scope and security

The default `scope.mode=ownNamespace` watches only the installation namespace. `namespaces` and `cluster` are explicit opt-ins and require matching RBAC and a migration confirmation. CRs and Secrets are sensitive: enable Kubernetes at-rest encryption, restrict RBAC, encrypt backups, and never place Secret values or controller checksums in logs or tickets.

`v4.0.0-rc.13` is a digest-addressed candidate for amd64 and arm64. It carries no SLA,
capacity, HA, or production-certification claim. Build/promotion steps and candidate status
are in [RELEASE.md](docs/RELEASE.md) and [SUPPORT.md](docs/SUPPORT.md).

Generated API artifacts are reproducible with the locked toolchain: use `make generate` for deepcopy code, `make manifests` for CRDs, and `make verify-generated` to prove the committed outputs are current.

## Operations

- [Operations and alerts](docs/OPERATIONS.md)
- [Scope and RBAC](docs/SCOPE-RBAC.md)
- [Credential rotation](docs/ROTATION.md)
- [Backup and restore](docs/BACKUP-RESTORE.md)
- [Rollback](docs/ROLLBACK.md)
- [Troubleshooting](docs/TROUBLESHOOTING.md)
- [Security policy](SECURITY.md)
- [Release process](docs/RELEASE.md)

## License

Apache License 2.0. See [LICENSE.txt](LICENSE.txt).
