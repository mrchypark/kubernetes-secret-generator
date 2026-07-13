# Troubleshooting

Inspect Conditions and Warning events first. Never print `.data`, `stringData`, `spec.privateKey`, generated passwords, private keys, or controller checksum annotations.

| Condition/reason | Cause | Safe action |
|---|---|---|
| `InvalidSpec` | Invalid length, encoding, field/key, collision, username, algorithm, or key material | Correct the CR/annotation; no controller restart is needed |
| `SecretOwnershipConflict` | Same-name Secret is unowned or owned by another identity | Verify API version/kind/name/UID/controller bit; rename or repair ownership only through an approved migration |
| `ImmutableSecretConflict` | Convergence would mutate an immutable Secret | Use the offline encrypted remediation in `MIGRATION-v4.md`; never let the controller delete/recreate it |
| `TrackingStateConflict` | Managed lists/version/fingerprint bundle is partial or malformed | Restore tracking from encrypted backup; do not adopt current data as a new baseline |
| `RegenerationStateConflict` | Regeneration marker is malformed or ahead of the CR generation | Restore the authoritative marker from backup or perform reviewed repair; do not force another rotation |
| `SecretSizeConflict` | Projected serialized Secret exceeds the safe bound | Reduce managed/unmanaged data or metadata before retrying |
| `LegacyBaselineInvalid` | v3 data does not match effective generation inputs | Correct spec or approve one-time reapply/rotation before upgrade |

If a Secret was deleted, CR-owned Secrets recreate with new generated credentials; annotation-managed Secrets cannot recreate because the source object is gone. Secret-only keys/labels require encrypted backup restore.

If a consumer fails after self-heal, treat it as a credential rollout issue: verify the Ready condition, identify the exact consumer through metadata, use its supported reload/restart path, then test authentication without logging values.

If reconcile errors repeat, confirm the error is transient (API availability/conflict) rather than a terminal Condition. Verify that exactly one controller Pod exists, then check API throttling, RBAC `can-i`, and watch scope. Do not widen RBAC or switch to cluster scope as an incident shortcut.

Escalate with controller/chart version, image digest, Kubernetes version, redacted Conditions/events, time range, and object kinds/counts. Follow [SECURITY.md](../SECURITY.md) for suspected vulnerabilities.
