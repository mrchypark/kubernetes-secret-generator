# Changelog

All notable changes are recorded here. Releases follow semantic versioning.

## [Unreleased]

The next artifact is `v4.0.0-rc.16`, a release candidate without SLA, capacity, HA, or
production-certification claims.

### Changed

- v4 makes CR literal data, type, and managed labels declarative.
- Generated credential drift and owned Secret deletion trigger self-heal with credential rotation.
- Exact controller owner identity is required before a Secret can be modified.
- Regeneration is idempotent once per CR generation; annotation values `true` and `yes` are equivalent.
- CRDs support optional `spec.rotationInterval` credential rotation from `1m` to `8760h`; it is
  disabled by default and safely coalesces missed intervals into one rotation.
- Encrypted backup/restore preserves user and KSG metadata exactly while excluding only
  kubectl's volatile `kubectl.kubernetes.io/last-applied-configuration` serialization.
- Helm is the sole deployment source, namespace scope is the default, and images are deployed by digest.
- v4 is explicitly single-controller: the binary has no leader election, the chart fixes one
  replica with `Recreate`, and v3 migration/rollback require downtime with Pod-zero gates.
- Orphaned Flux CRD adoption accepts the two exact Kubernetes label ownership encodings observed
  with and without the parent field marker, while continuing to reject any extra owner path.

### Security

- Input bounds, immutable Secret handling, least-privilege RBAC, restricted Pod security,
  candidate image scanning, SBOM, signature, and vulnerability checks are added.

Upgrade and rollback requirements are documented in [docs/MIGRATION-v4.md](docs/MIGRATION-v4.md).
