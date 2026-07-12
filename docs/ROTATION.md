# Credential rotation runbook

Any generated-value drift, owned Secret deletion, approved regeneration, scheduled rotation, or approved legacy reapply creates new credential material. The controller updates Kubernetes Secret state; it does not reload applications or restore a prior random value.

## Scheduled rotation

`StringSecret`, `BasicAuth`, and `SSHKeyPair` can rotate generated credentials after an elapsed interval:

```yaml
spec:
  rotationInterval: 24h
```

The value uses Go duration syntax and must be from `1m` through `8760h`, inclusive. Empty or omitted disables scheduled rotation. It is an elapsed interval, not a cron schedule, and is unavailable for annotation-managed Secrets.

Enabling it on an existing CR starts the interval without rotating immediately. Disabling and later re-enabling starts a fresh interval. Changing an enabled interval retains the current schedule anchor: shortening it may make one rotation immediately due, while lengthening it postpones the next rotation. If downtime spans several intervals, reconciliation performs one rotation, not one per missed interval.

Each successful generated credential replacement restarts the interval. This includes scheduled or forced rotation, generated-value drift repair, and a generation-setting change that replaces credentials. A literal or label-only update does not restart it. Secret and schedule tracking are committed together, so a controller restart or status retry does not cause an extra rotation.

Scheduled rotation is invalid for a `StringSecret` with no generated `spec.fields`, or for an `SSHKeyPair` with a supplied `spec.privateKey`. When an immutable Secret becomes due, the controller reports `ImmutableSecretConflict` and leaves its data unchanged.

## Before rotation

1. Record the exact CR/Secret identity, owner UID, consumer inventory, maintenance window, and change decision without reading or copying Secret values.
2. Confirm the object is `Ready=True`, tracking is complete, and no ownership or immutable conflict exists.
3. Verify an encrypted backup and restore rehearsal exists when rollback to the prior credential is required.
4. Choose the smallest rotation: one String field, one CR generation, or one object. Never rotate every namespace with `--all` in an ordinary change.

## Manual trigger

For a CR, patch `forceRegenerate` in a new generation. `true` rotates once for that generation; a retry or controller restart does not rotate again.

```sh
kubectl --context "$KUBE_CONTEXT" -n "$WORKLOAD_NAMESPACE" patch stringsecret "$NAME" \
  --type=merge -p '{"spec":{"forceRegenerate":true}}'
```

After success, return the desired spec to false. That spec update does not itself rotate.

For annotation-managed String fields, select exact generated fields; use `yes` only when all fields must rotate:

```sh
kubectl --context "$KUBE_CONTEXT" -n "$WORKLOAD_NAMESPACE" annotate secret "$NAME" \
  secret-generator.v1.mittwald.de/regenerate=password --overwrite
```

BasicAuth and SSH do not support selective regeneration. Their value must be `true` or `yes`.

## Consumer rollout and verification

Wait for `Ready=True` and a single expected rotation outcome. Use each application's documented reload mechanism; restart a Deployment/StatefulSet only when it cannot reload mounted/projected credentials. Verify authentication end-to-end without placing credentials in command arguments, rollout annotations, logs, or tickets.

Stop and investigate on repeated rotation events, unexpected Secret resourceVersion changes, consumer authentication failure, owner conflict, or controller error storm. Removing the trigger does not restore the previous value. Restore the encrypted backup when the prior credential is required, then re-run in-memory equality and consumer tests from [BACKUP-RESTORE.md](BACKUP-RESTORE.md).
