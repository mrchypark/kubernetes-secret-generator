# Changelog

## 3.5.0

- Add default-off `spec.rotationInterval` to `StringSecret`, `BasicAuth`, and `SSHKeyPair`.
- Recreate deleted owned Secrets and additively repair missing or empty generated values.
- Preserve nonempty changed values, literal data, unrelated keys, type, labels, and ownership.
- Add multi-architecture signed images, SPDX SBOMs, vulnerability scanning, pinned container bases, and restricted Helm security defaults.
- Preserve the 3.4 Helm values, cluster-wide default scope, RBAC, leader election, CRD version, and normal in-place upgrade path.
