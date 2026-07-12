# Changelog

All notable changes are recorded here. Releases follow semantic versioning.

## [Unreleased]

The next artifact is `v4.0.0-rc.3`, a release candidate without SLA, capacity, HA, or
production-certification claims.

### Changed

- v4 makes CR literal data, type, and managed labels declarative.
- Generated credential drift and owned Secret deletion trigger self-heal with credential rotation.
- Exact controller owner identity is required before a Secret can be modified.
- Regeneration is idempotent once per CR generation; annotation values `true` and `yes` are equivalent.
- CRDs support optional `spec.rotationInterval` credential rotation from `1m` to `8760h`; it is
  disabled by default and safely coalesces missed intervals into one rotation.
- Helm is the sole deployment source, namespace scope is the default, and images are deployed by digest.

### Security

- Input bounds, immutable Secret handling, least-privilege RBAC, restricted Pod security,
  candidate image scanning, SBOM, signature, and vulnerability checks are added.

Upgrade and rollback requirements are documented in [docs/MIGRATION-v4.md](docs/MIGRATION-v4.md).
