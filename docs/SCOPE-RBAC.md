# Scope and RBAC runbook

Scope controls both informer visibility and generated RBAC. Use `ownNamespace` unless a reviewed workload inventory proves another mode is required.

| Mode | Watched namespaces | Access objects |
|---|---|---|
| `ownNamespace` | Helm release namespace | Namespaced Roles/RoleBindings only in that namespace |
| `namespaces` | Exact `scope.namespaces` list | One Role/RoleBinding per listed namespace; leader Lease remains in the release namespace |
| `cluster` | All namespaces | ClusterRole/ClusterRoleBinding for managed resources; leader Lease remains namespaced |

The manager may get/list/watch and create/update/patch Secrets; get/list/watch CRs; update/patch CR status; and create/patch Events. It must not update CR specs, delete Pods, manage ServiceMonitors, or mutate unrelated Kubernetes APIs.

## Scope change procedure

1. Inventory managed CRs, annotation-managed Secrets, consuming workloads, and current RoleBindings without reading Secret data.
2. Record scope migration as a separate GitHub issue decision; do not combine it with a controller version upgrade.
3. Set `SCOPE_MODE` and identical `CONFIRMED_SCOPE`.
4. For `namespaces`, provide a comma-separated, already unique list. Sort the UTF-8 names bytewise, join them with `\n` without a trailing newline, and record its lowercase SHA-256 as `CONFIRMED_NAMESPACES_SHA256`.
5. Render and inspect RBAC before mutation. Apply through `make upgrade`; the guard rejects missing or mismatched confirmation before Kubernetes API writes.
6. Verify positive access only in selected namespaces and negative access elsewhere using the controller ServiceAccount impersonation.

Example hash preparation:

```sh
export SCOPE_NAMESPACES='application-a,application-b'
canonical_namespaces=$(printf '%s' "$SCOPE_NAMESPACES" | tr ',' '\n' | LC_ALL=C sort)
export CONFIRMED_NAMESPACES_SHA256="$(printf '%s' "$canonical_namespaces" | openssl dgst -sha256 -r | awk '{print $1}')"
```

Do not add duplicates or a trailing newline to the hash input. The deployment script independently canonicalizes and verifies the value.

## RBAC verification

Set `SERVICE_ACCOUNT` to the rendered chart ServiceAccount name, then verify representative permissions:

```sh
kubectl --context "$KUBE_CONTEXT" auth can-i get secrets -n application-a \
  --as="system:serviceaccount:$NAMESPACE:$SERVICE_ACCOUNT"
kubectl --context "$KUBE_CONTEXT" auth can-i patch stringsecrets/status -n application-a \
  --as="system:serviceaccount:$NAMESPACE:$SERVICE_ACCOUNT"
kubectl --context "$KUBE_CONTEXT" auth can-i update stringsecrets -n application-a \
  --as="system:serviceaccount:$NAMESPACE:$SERVICE_ACCOUNT"
kubectl --context "$KUBE_CONTEXT" auth can-i delete pods -n application-a \
  --as="system:serviceaccount:$NAMESPACE:$SERVICE_ACCOUNT"
```

The first two answers must be `yes`; the last two must be `no`. For `ownNamespace` and `namespaces`, repeat a Secret read against an out-of-scope namespace and require `no`. A mismatch blocks rollout; do not widen to cluster scope as a workaround.
