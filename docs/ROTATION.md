# Secret rotation in 3.5

`StringSecret`, `BasicAuth`, and `SSHKeyPair` accept an optional `spec.rotationInterval`. It uses Go duration syntax and must be between `1m` and `8760h`.

```yaml
spec:
  rotationInterval: 24h
```

The default is empty, so upgrading does not rotate existing credentials. On first enable, the controller writes the RFC3339Nano annotation `secretgenerator.mittwald.de/rotation-anchor` to the owned Secret and schedules the next reconcile. It does not rotate immediately. When due, it rotates generated credentials once and moves the anchor to the current time; missed intervals are coalesced into that one rotation. Changing the interval keeps the anchor, and removing the interval removes only the anchor.

Rotation preserves literal `spec.data`, unrelated data, type, labels, and ownership. It rejects invalid durations before changing credentials. `StringSecret` rotation requires a generated field, and `SSHKeyPair` rotation cannot be combined with a supplied `spec.privateKey` or `spec.ed25519Seed`.

The owned-Secret watch is deliberately additive: deletion and missing or empty managed literal/generated keys trigger repair, while nonempty changed values are left untouched. `forceRegenerate` keeps its existing 3.4 behavior.

## Upgrade from 3.4

Apply the updated CRDs before upgrading the chart because Helm does not upgrade CRDs already installed from a chart's `crds/` directory:

```sh
kubectl apply -f deploy/crds/
helm upgrade kubernetes-secret-generator deploy/helm-chart/kubernetes-secret-generator
```

No offline migration is required. Existing CRs and Secrets remain in place, and rotation stays disabled unless configured.
