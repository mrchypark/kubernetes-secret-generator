# Offline rollback runbook

v4 does not provide automatic downgrade or rolling rollback. A v4 to v3.4.1 rollback is a
manual, downtime operation. Keep the v4 CRDs in place; never downgrade CRDs.

## Required artifacts and preflight

- Exact v4 release chart/bundle and the previously verified v3.4.1 chart and image digest.
- Encrypted backup proven restorable and an approved inventory of credentials rotated by v4.
- Every CR has `forceRegenerate=false`.
- Every managed Secret `regenerate` annotation, including `false`, is removed or explicitly
  approved because v3 BasicAuth/SSH treats any non-empty value as a trigger.
- Legacy String `secure` markers, immutable Secrets, pending drift, scope, and RBAC are
  inventoried without recording Secret values.

Any unresolved immutable object, missing secure marker without an approved rotation, pending
declarative drift, foreign owner, or unverified artifact blocks rollback.

## Downtime procedure

1. Freeze controller-related changes and verify the encrypted backup.
2. Scale the v4 Deployment to zero.
3. Wait until its desired, ready, and available replicas are zero and no matching Pod exists.
4. Install the verified v3.4.1 manager with the same watch scope and `--skip-crds`.
5. Wait for exactly one v3 Pod to become Ready.
6. Compare CR/Secret counts, owner UIDs, Ready state, and in-memory data fingerprints; require
   no unapproved credential rotation.

Do not use `helm rollback` as an automatic recovery action. Do not run v3 and v4 together.
The public v4 chart has no compatibility profile and the v4 binary has no leader-election
flags. Re-upgrading to v4 repeats the same downtime boundary: stop v3, prove Pod zero, then
install v4.

Rollback does not restore credentials already changed by v4. Use
[encrypted restore](BACKUP-RESTORE.md) when previous values are required. Abort on multiple
controller Pods, panic/fatal/OOM, event storms, unexpected rotation, scope/RBAC changes, or a
Secret change outside the approved set.
