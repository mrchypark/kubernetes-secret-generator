# v4.0.0-rc.1 requirements traceability

| Requirement | Automated check | Operator document |
|---|---|---|
| Read-only, explicit-target migration inventory | `scripts/test-preflight-v4.sh` | `docs/MIGRATION-v4.md` |
| Crypto baseline and redaction | `scripts/preflight-baseline-src/main_test.go`, `scripts/check-test-artifacts.sh` | `docs/MIGRATION-v4.md` |
| API/owner/type/regeneration behavior | unit and envtest suites | `docs/API.md`, `docs/TROUBLESHOOTING.md` |
| CRD-first install/upgrade | Helm and release guard tests | `docs/INSTALL.md`, `docs/UPGRADE.md` |
| Encrypted backup and manager rollback | backup/restore and rollback tests | `docs/BACKUP-RESTORE.md`, `docs/ROLLBACK.md` |
| Candidate build and manual promotion | source/security/chart workflow checks | `docs/RELEASE.md` |

Deferred availability, capacity, long-running resilience, and architecture/minor matrix
controls are not implied by this ledger. Reintroduction thresholds are recorded in the
[development directive](PRODUCTION_READINESS_DEVELOPMENT_DIRECTIVE.md).
