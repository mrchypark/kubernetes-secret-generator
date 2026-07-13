# v3.4.1 to v4 migration

v4 is a breaking, offline major-version upgrade. It is not a rolling upgrade. The v3
controller must be completely stopped before any v4 CRD mutation or v4 controller start.
Expect controller downtime.

## Read-only preflight

Run `scripts/preflight-v4.sh` before any API mutation. It requires explicit
`KUBE_CONTEXT`, matching `CONFIRM_CONTEXT`, `EXPECTED_SERVER_URL`, `EXPECTED_CA_SHA256`,
`NAMESPACE`, `RELEASE_NAME`, `SCOPE_MODE`, and `CONFIRMED_SCOPE`. For `namespaces`, also set
the exact namespace list and canonical SHA-256 confirmation. A raw v3 migration additionally
requires `RAW_V3_MIGRATION=true` and the exact `DEPLOYMENT_NAME`; the report is bound to that
Deployment.

The report inventories owner identity, Secret conflicts, input validity, regeneration
markers, drift candidates, scope, and a stable UID/resourceVersion snapshot. It writes no
Secret or private-key value and performs only reads.

## Resolve findings

Resolve every blocker before the upgrade. Do not auto-adopt a foreign Secret. Immutable or
same-name replacement is an offline operation using the encrypted backup. Record warnings and
rotation decisions in the release issue without copying values.

## Offline upgrade

1. Freeze controller-related changes and rerun preflight until it has zero blockers.
2. Create and verify the encrypted backup.
3. Scale the explicitly named v3 Deployment to zero.
4. Verify desired, ready, and available replicas are zero and no matching v3 Pod exists.
5. Set `CONTROLLER_STOPPED_CONFIRM=true` and run the verified migration wrapper. It rechecks
   Deployment zero and Pod zero immediately before CRD replacement.
6. Update the exact pinned v3 CRDs in place and wait for all three to be `Established`.
7. Install v4 as exactly one Pod using the immutable candidate digest.
8. Confirm valid legacy values without rotation and run the 10–15 minute smoke.

The old v3 Lease may remain as historical state; it is not a v4 install blocker because v4
does not use leader election. Never run HPA, manually increase replicas, or run multiple v4
releases against overlapping scope.

There is no automatic downgrade. A v4 to v3 rollback is another downtime operation and keeps
the v4 CRDs. See [ROLLBACK.md](ROLLBACK.md) and [BACKUP-RESTORE.md](BACKUP-RESTORE.md).
