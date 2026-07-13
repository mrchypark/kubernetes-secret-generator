# Manager rollback runbook

Rollback keeps the v4 chart, values schema, rendered RBAC, and CRDs. Only the manager image changes to the signed multi-architecture v3.4.1 compatibility digest. CRDs are never downgraded.

## Required artifacts and preflight

- Exact v4 release chart/bundle and the current values; do not re-render from another commit.
- A v3.4.1 compatibility digest built from tag commit `b01e37dce377e5e4296392b7e4d823b6830b763e` and checked for platform startup, SBOM, signature, and vulnerabilities. This image provenance is distinct from the original e15976c CRD fixture source.
- Encrypted backup proven restorable; approved list of credentials already rotated by v4.
- Every CR has `forceRegenerate=false`.
- Every managed Secret `regenerate` annotation, including `false`, is removed or explicitly approved for rotation because v3 BasicAuth/SSH treats any non-empty value as a trigger.
- Legacy String `secure` markers, immutable Secrets, pending data/type/metadata drift, scope, RBAC, and current leader-election ID are inventoried.

Any unresolved immutable object, missing secure marker without an approved rotation, pending declarative drift, foreign owner, or unverified artifact blocks rollback.

## Default leader ID: rolling manager-only rollback

Use only when both versions use `kubernetes-secret-generator-lock` and the same watch scope. Export the normal explicit context, namespace, release, chart version, confirmed scope, direct CRD manager, and the verified v3 compatibility digest, then run:

```sh
export COMPATIBILITY_PROFILE=v3.4.1
export LEADER_ELECTION_ENABLED=true
export LEADER_ELECTION_ID=kubernetes-secret-generator-lock
export IMAGE_DIGEST='sha256:<verified-v3.4.1-compatibility-digest>'
make upgrade
```

The compatibility profile omits v4-only manager flags and forces `REGENERATE_INSECURE=false`. Watch the Lease and Pods throughout the rolling update; exactly one live leader is allowed.

## Custom leader ID: approved downtime only

Automatic rolling rollback is prohibited. Scale the v4 Deployment to zero, wait for all Pods to terminate, and prove the custom Lease is expired or unowned. Only then install the v3 manager using the default lease. At no point may leaders under the custom and default leases overlap. Record the downtime decision and lease observations in the GitHub release issue without object data.

## Verification and re-upgrade

Compare CR/Secret counts, exact owner UIDs, Ready state, restart count, and redacted error/event rates. Confirm no unapproved rotation and authenticate consumers. v3 may remove v4 observational status fields; `status.secret` and Secret tracking must remain. A later v4 re-upgrade must reconstruct status without rotation.

Rollback does not restore credentials already changed by v4. Use [encrypted restore](BACKUP-RESTORE.md) for those values. Abort on dual leaders, panic/fatal/OOM, event storm, unexpected rotation, scope/RBAC change, or Secret change outside the approved set.
