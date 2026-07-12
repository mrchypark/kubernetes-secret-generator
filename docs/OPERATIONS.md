# Operations

## Routine checks

- Confirm the Deployment and expected Pods are Ready.
- Inspect CR `Ready` Conditions and deduplicated Warning Events before controller logs.
- Investigate `InvalidSpec`, owner conflicts, immutable/type conflicts, tracking conflicts,
  unexpected credential rotation, API connectivity failure, restart, panic, fatal, or OOM.
- Keep logs, metrics, issues, and reports free of Secret values, private keys, managed
  checksums, kubeconfigs, and plaintext backups.

The controller exposes `/healthz` and `/readyz` on port 8080 and controller-runtime metrics
on port 8383. Domain metrics describe controller-observed outcomes; they are not an
availability SLA. Labels are fixed low-cardinality values and never contain object names,
data keys, values, or checksums.

## Credential changes

Deleting or drifting generated data can rotate credentials during self-heal. The
controller does not restart consumers. Identify affected workloads, use the application's
supported reload path, and verify authentication. Restore the encrypted backup when the
previous credential is required.

## Upgrade checklist

1. Run `scripts/preflight-v4.sh` with explicit context/server/CA and save redacted JSON or
   Markdown. Resolve blockers and review stale-data warnings.
2. Create and verify the encrypted backup.
3. Record exact source, image, and chart digests in the GitHub release issue.
4. Apply CRDs first, then upgrade the manager without changing scope.
5. Observe the amd64 candidate for at least 10 and at most 15 minutes. Require Ready state, no restart/fatal error,
   no owner conflict, and no unexpected rotation.
6. Build the arm64 image and verify `docker run --rm --platform linux/arm64 IMAGE --help`.
7. Keep the manager rollback and restore commands ready until consumers are verified.

## Scope and sizing

Use `ownNamespace` unless inventory demonstrates a broader requirement. Treat scope change
as a separate operation. Resource requests and replica count are deployment choices, not a
capacity promise. An optional disposable 100-object benchmark may inform sizing but must
include its environment and must not be presented as an SLO or SLA.

Escalate unresolved findings with redacted Conditions, Events, version/digest, Kubernetes
version, scope, and time range. Suspected vulnerabilities follow [SECURITY.md](../SECURITY.md).
