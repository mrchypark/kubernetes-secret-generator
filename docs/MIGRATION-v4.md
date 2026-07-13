# v3.4.1 to v4 migration

## Read-only preflight

Run `scripts/preflight-v4.sh` before any API mutation. It requires explicit
`KUBE_CONTEXT`, matching `CONFIRM_CONTEXT`, `EXPECTED_SERVER_URL`,
`EXPECTED_CA_SHA256`, `NAMESPACE`, `RELEASE_NAME`, `SCOPE_MODE`, and
`CONFIRMED_SCOPE`. For `namespaces`, also set the exact namespace list and canonical
SHA-256 confirmation. Set `REPORT_FORMAT=json|markdown`; `REPORT_FILE` is optional and
must not already exist.

For a raw v3 migration, also set `RAW_V3_MIGRATION=true` and an explicit
`DEPLOYMENT_NAME`. The report binds that Deployment name and the release wrapper rejects a
report generated for any other Deployment.

The report inventories:

- exact CR owner identity and same-name Secret presence;
- Secret type mismatch and immutable managed Secrets;
- invalid String, BasicAuth, and SSH specifications;
- live generated-value, bcrypt, and SSH private/public baseline validity in memory;
- pending CR or annotation regeneration markers;
- stale data/label candidates requiring human review;
- running and requested scope, including namespace-list confirmation; and
- a stable UID/resourceVersion snapshot so concurrent changes fail closed.

No Secret or private-key value is written to the report. The command only uses `get` and
context inspection; it never applies, creates, deletes, patches, or replaces an object.

## Resolve findings

Blockers must be corrected before upgrade. Do not auto-adopt a foreign Secret. Type change
uses a new name by default. Immutable or same-name replacement is an offline operation:
encrypt the backup, stop the controller, preserve bytes and correct owner identity, perform
the explicitly reviewed replacement, verify equality in memory, then resume.

Warnings identify stale data or labels the controller cannot classify safely. Decide what
to keep or remove in the GitHub release issue without copying values.

## Upgrade

1. Freeze controller-related changes and rerun preflight until it reports zero blockers.
2. Create and verify the encrypted backup.
3. Keep the running scope and leader-election ID unchanged.
4. Apply the verified v4 CRDs first and wait for `Established`.
5. Upgrade only the manager to the exact candidate digest.
6. Confirm valid legacy values baseline without rotation; allow only explicitly intended
   rotations and reload consumers afterward.
7. Follow the 10–15 minute smoke and manual promotion process in [RELEASE.md](RELEASE.md).

Rollback changes the manager image only. Never downgrade CRDs. See
[ROLLBACK.md](ROLLBACK.md) and [BACKUP-RESTORE.md](BACKUP-RESTORE.md).
