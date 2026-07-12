# Upgrade and rollback

Read [MIGRATION-v4.md](MIGRATION-v4.md) and resolve the public read-only preflight before
upgrading an existing installation. Create the encrypted backup first.

## Upgrade order

1. Record the exact candidate image, chart, and source digests in the GitHub
   release issue.
2. Keep scope and `kubernetes-secret-generator-lock` unchanged.
3. Apply the verified CRDs first and wait for all three to become `Established`.
4. Upgrade the manager to the exact digest.
5. Verify Ready Conditions, owner identity, restart/error Events, and intended rotations.
6. Observe the amd64 candidate for 10–15 minutes. Build the arm64 image and verify its
   `--help` startup command. This is candidate validation, not a capacity or SLA claim.

Direct Helm upgrade:

```sh
export KUBE_CONTEXT=approved-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.5
export IMAGE_DIGEST='sha256:<verified-64-hex-digest>'
export CRD_LIFECYCLE_MANAGER=direct
export SCOPE_MODE=ownNamespace
export CONFIRMED_SCOPE=ownNamespace
export CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1
make upgrade
```

If the installation already uses Flux, keep Flux as the sole CRD manager and update CRDs
before the HelmRelease. A Flux rehearsal is useful but is not a universal rc.5 release
blocker. Never switch CRD managers during the controller upgrade.

## Rollback

Keep v4 CRDs and chart/RBAC values; change only the manager image to the verified v3.4.1
compatibility digest. The default shared leader ID permits a rolling manager rollback. A
custom leader ID requires downtime: stop v4 completely and verify its Lease is expired or
unowned before starting v3. Rollback does not recover credentials already rotated by v4;
use the encrypted restore runbook when old values are required.

See [ROLLBACK.md](ROLLBACK.md) for the command checklist and [RELEASE.md](RELEASE.md) for
manual candidate promotion.
