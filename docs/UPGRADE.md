# Upgrade and rollback

Read [MIGRATION-v4.md](MIGRATION-v4.md) and resolve the public read-only preflight before
upgrading an existing installation. Create the encrypted backup first.

## Upgrade order

1. Record the exact candidate image, chart, and source digests in the GitHub
   release issue.
2. Keep scope unchanged, scale the named v3 Deployment to zero, and wait for zero matching Pods.
3. Apply the verified CRDs first and wait for all three to become `Established`.
4. Install exactly one v4 manager Pod from the exact digest.
5. Verify Ready Conditions, owner identity, restart/error Events, and intended rotations.
6. Observe the amd64 candidate for 10–15 minutes. Build the arm64 image and verify its
   `--help` startup command. This is candidate validation, not a capacity or SLA claim.

This is a breaking offline upgrade with controller downtime. The v4 Deployment uses
`Recreate` and intentionally retains the three-label v3.4.1 selector (`name`,
`app.kubernetes.io/name`, and `app.kubernetes.io/instance`) because selectors are immutable.
Chart/version/management labels remain non-selector Deployment metadata, while Pod metadata
retains the compatible recommended name and instance labels. No live selector migration or
resource recreation is required.

Direct Helm upgrade:

```sh
export KUBE_CONTEXT=approved-cluster
export CONFIRM_CONTEXT="$KUBE_CONTEXT"
export NAMESPACE=secret-generator-system
export RELEASE_NAME=kubernetes-secret-generator
export DEPLOYMENT_NAME=kubernetes-secret-generator
export CHART_VERSION=4.0.0-rc.17
export IMAGE_DIGEST='sha256:<verified-64-hex-digest>'
export CRD_LIFECYCLE_MANAGER=direct
export SCOPE_MODE=ownNamespace
export CONFIRMED_SCOPE=ownNamespace
export CONFIRM_LEGACY_CRD_ADOPTION=v3.4.1
export RAW_V3_PREFLIGHT_REPORT=/absolute/path/to/preflight-report.json
export RAW_V3_PREFLIGHT_SHA256='<sha256-of-that-report>'
export EXPECTED_SERVER_URL='<exact-approved-api-server>'
export EXPECTED_CA_SHA256='<exact-approved-ca-sha256>'
export CONTROLLER_STOPPED_CONFIRM=true
make upgrade
```

When all three installed CRDs are the exact pinned v3.4.1 specs, the wrapper requires this
fresh zero-blocker report before using a server-side dry-run and scoped ownership takeover.
It never forces conflicts for marked v4 CRDs, partial sets, unknown schemas, active Flux ownership,
or a report for another target. At mutation time it reruns preflight, accepts only the exact
legacy `kubectl-client-side-apply` spec tuple plus `kube-apiserver` status tuple, and replaces each exact CRD with its captured UID and
resourceVersion before establishing normal non-forcing SSA ownership. Concurrent changes fail
the replacement. CRDs are updated in place and retained during manager rollback.

For CRDs orphaned after Flux was completely removed, set
`CONFIRM_ORPHANED_FLUX_OWNER='<kustomization-name>/<kustomization-namespace>'`. The wrapper
also requires `CONFIRM_ORPHANED_FLUX_DECOMMISSIONED` to match that exact value after the
GitOps/platform owner confirms the reconciliation source is permanently decommissioned. It
requires those exact owner labels and managedFields, then immediately verifies that no Flux
toolkit CRD or known controller Deployment exists. Normal active Flux remains rejected and
must remain the sole CRD manager.

All raw v3 migrations must set `CONTROLLER_STOPPED_CONFIRM=true` after scaling
`DEPLOYMENT_NAME` to zero and waiting for zero matching Pods. Orphaned Flux adoption also
requires independent non-secret `ORPHANED_FLUX_APPROVAL_REF` and
`ORPHANED_FLUX_APPROVER` values. The wrapper rechecks Deployment zero and Pod zero immediately
before its first CRD replacement and persists the approval evidence in the lifecycle owner
ConfigMap. A stale v3 Lease is not inspected or deleted because v4 does not use it.

All three targets pass server dry-run before the first write. If a replace conflicts, the
wrapper refetches managedFields and UID. It retries once with the current resourceVersion only
when the object is still the exact pinned orphan state; if the first request already produced
the exact rc12 direct-managed state, it continues to non-forcing SSA. Any changed spec, UID,
or manager fails closed. Helm is not invoked and the controller must remain stopped.

Never reuse a printed or retained resourceVersion-bound target: the wrapper deletes these
files on failure. Use the redacted identity/managedFields inventory to review the live state,
then refetch and revalidate every CRD before generating a new recovery request. After all
three CRDs converge and become `Established`, verify:

```sh
kubectl --context "$KUBE_CONTEXT" get crd/basicauths.secretgenerator.mittwald.de -o jsonpath='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.username.type}{"\n"}'
kubectl --context "$KUBE_CONTEXT" get crd/sshkeypairs.secretgenerator.mittwald.de -o jsonpath='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.algorithm.type}{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.privateKeyField.type}{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.publicKeyField.type}{"\n"}'
kubectl --context "$KUBE_CONTEXT" get crd/stringsecrets.secretgenerator.mittwald.de -o jsonpath='{.spec.versions[?(@.name=="v1alpha1")].schema.openAPIV3Schema.properties.spec.properties.fields.type}{"\n"}'
```

Expected outputs are `string`, `stringstringstring`, and `array`. Only after all three match
may the operator install v4 or deliberately restart the v3.4.1 manager.

The lifecycle owner ConfigMap preserves `orphanedFluxApprovalRef` and
`orphanedFluxApprover` automatically on ordinary upgrade or reinstall. To replace or clear
them, set `REPLACE_ORPHANED_FLUX_APPROVAL=true` and an independent
`ORPHANED_FLUX_APPROVAL_REPLACEMENT_REF`; supply both new approval fields to replace them or
leave both unset to clear them. The replacement audit reference is persisted. Any unreviewed
mismatch is rejected before mutation.

If the installation already uses Flux, keep Flux as the sole CRD manager and update CRDs
before the HelmRelease. A Flux rehearsal is useful but is not a universal rc.17 release
blocker. Never switch CRD managers during the controller upgrade.

## Rollback

Automatic downgrade and rolling rollback are unsupported. Keep the v4 CRDs, stop v4, prove
that no matching Pod remains, and only then install the verified v3.4.1 manager. Rollback
does not recover credentials already rotated by v4; use the encrypted restore runbook when
old values are required.

v4 supports exactly one replica and one active release per non-overlapping controller scope.
Do not attach an HPA, manually scale above one, or run multiple releases over the same objects.
`Recreate` prevents the Deployment controller from starting a replacement while an old Pod is
non-terminal, but it is not distributed fencing. A force-deleted Pod or a Pod on an unreachable
node may leave its controller process running after the API object is gone. Because v4 deliberately
has no HA or leader election, fence that node or otherwise prove the old process has stopped before
continuing; never start an overlapping release.

See [ROLLBACK.md](ROLLBACK.md) for the command checklist and [RELEASE.md](RELEASE.md) for
manual candidate promotion.
